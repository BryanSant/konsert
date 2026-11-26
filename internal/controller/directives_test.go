package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// clockBox is an injectable clock. The directive loop reads it from another goroutine.
type clockBox struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clockBox) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clockBox) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func upsertBox(t *testing.T, st store.Store, id, name string) {
	t.Helper()
	if err := st.UpsertWorker(context.Background(), store.Worker{
		ID: id, Name: name, HostGroup: name, OS: "linux", Arch: "amd64",
		Agents: []string{"mock"}, MaxConcurrent: 4, Status: "online", LastSeenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func mustDirective(t *testing.T, h *Hub, in DirectiveInput) store.Directive {
	t.Helper()
	d, err := h.CreateDirective(userCtx(), in)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func pulseSummaries(t *testing.T, st store.Store) []string {
	t.Helper()
	evs, err := st.ListFleetEvents(context.Background(), store.FleetEventFilter{Kind: store.EventKindPulse, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Summary
	}
	return out
}

func countContaining(ss []string, sub string) int {
	n := 0
	for _, s := range ss {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func TestDirectiveExpiryTiming(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	upsertBox(t, th.st, "w1", "box")
	start := time.Date(2026, 11, 25, 14, 0, 0, 0, time.UTC)
	clk := &clockBox{t: start}
	th.h.directiveNow = clk.now

	if got := th.h.nextDirectiveWake(ctx, start); got != directiveIdleDelay {
		t.Fatalf("idle wake %s, want %s", got, directiveIdleDelay)
	}

	until := start.Add(30 * time.Minute)
	d := mustDirective(t, th.h, DirectiveInput{
		Kind: store.DirectiveMachinePause, Target: "w1", Until: &until, Reason: "lunch",
	})
	if got := th.h.nextDirectiveWake(ctx, start); got != 25*time.Minute {
		t.Fatalf("wake at start %s, want 25m", got)
	}

	clk.set(until.Add(-6 * time.Minute))
	th.h.runDirectivePass(ctx)
	if n := countContaining(pulseSummaries(t, th.st), ") expiring"); n != 0 {
		t.Fatalf("warning %s before the window: %d pulses", until.Add(-6*time.Minute), n)
	}

	clk.set(until.Add(-4 * time.Minute))
	if got := th.h.nextDirectiveWake(ctx, clk.now()); got != 4*time.Minute {
		t.Fatalf("wake inside the window %s, want 4m (expiry, warning still pending)", got)
	}
	th.h.runDirectivePass(ctx)
	th.h.runDirectivePass(ctx)
	sums := pulseSummaries(t, th.st)
	if n := countContaining(sums, ") expiring"); n != 1 {
		t.Fatalf("warning count %d, want 1 (second pass must not repeat): %v", n, sums)
	}
	if n := countContaining(sums, ") expired"); n != 0 {
		t.Fatalf("expired early: %v", sums)
	}
	if cur, err := th.st.GetDirective(ctx, d.ID); err != nil || cur.EndedAt != nil {
		t.Fatalf("directive ended inside the warning window: %+v %v", cur, err)
	}

	clk.set(until)
	if got := th.h.nextDirectiveWake(ctx, clk.now()); got != 0 {
		t.Fatalf("wake at until %s, want 0", got)
	}
	if got := th.h.nextDirectiveDelay(ctx); got != time.Second {
		t.Fatalf("due delay %s, want 1s", got)
	}
	th.h.runDirectivePass(ctx)
	cur, err := th.st.GetDirective(ctx, d.ID)
	if err != nil || cur.EndedAt == nil || cur.EndedBy != "system" {
		t.Fatalf("after until: %+v %v", cur, err)
	}
	sums = pulseSummaries(t, th.st)
	if n := countContaining(sums, ") expiring"); n != 1 {
		t.Fatalf("expiring pulses %d, want 1: %v", n, sums)
	}
	if n := countContaining(sums, ") expired"); n != 1 {
		t.Fatalf("expired pulses %d, want 1: %v", n, sums)
	}
	audits, err := th.st.ListAudit(ctx, store.AuditFilter{Action: AuditDirectiveExpire, Limit: 10})
	if err != nil || len(audits) != 1 || audits[0].Cause == nil || audits[0].Cause.Actor != store.ActorSystem || audits[0].Cause.Source != "directive" {
		t.Fatalf("expire audit: %+v %v", audits, err)
	}
	evs, err := th.st.ListFleetEvents(ctx, store.FleetEventFilter{Kind: store.EventKindDirective, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var expired bool
	for _, e := range evs {
		if e.Action == directiveActionExpired && e.WorkerID == "w1" {
			expired = true
		}
	}
	if !expired {
		t.Fatalf("no expired fleet event: %+v", evs)
	}

	// A clock jump past until expires and notifies expiry only.
	jumpStart := until.Add(time.Hour)
	clk.set(jumpStart)
	jumpUntil := jumpStart.Add(30 * time.Minute)
	d2 := mustDirective(t, th.h, DirectiveInput{Kind: store.DirectiveMachineDrain, Target: "w1", Until: &jumpUntil})
	clk.set(jumpUntil.Add(time.Minute))
	th.h.runDirectivePass(ctx)
	cur, err = th.st.GetDirective(ctx, d2.ID)
	if err != nil || cur.EndedAt == nil || cur.EndedBy != "system" {
		t.Fatalf("jump expiry: %+v %v", cur, err)
	}
	sums = pulseSummaries(t, th.st)
	if n := countContaining(sums, ") expiring"); n != 1 {
		t.Fatalf("clock jump sent a warning (expiring count %d): %v", n, sums)
	}
	if n := countContaining(sums, ") expired"); n != 2 {
		t.Fatalf("clock jump expired count %d, want 2: %v", n, sums)
	}
}

func TestSchedulerSkipsPausedAndDrainedMachines(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	info := testWorkerInfo("w1", "box")
	info.MaxConcurrent = 4
	fw := th.connectWorker(t, info, nil, "w1")

	pause := mustDirective(t, th.h, DirectiveInput{Kind: store.DirectiveMachinePause, Target: "box"})
	createQueuedTask(t, th.st, "t-pause-a", WorkerSelector{}, false)
	createQueuedTask(t, th.st, "t-pause-b", WorkerSelector{}, false)
	th.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgTaskStart, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("paused machine received %d tasks", len(got))
	}
	if got := mustTask(t, th.st, "t-pause-a"); got.Status != protocol.TaskStatusQueued {
		t.Fatalf("paused task status %s", got.Status)
	}
	w, err := th.st.GetWorker(ctx, "w1")
	if err != nil || w.Drained {
		t.Fatalf("pause set workers.drained: %+v %v", w, err)
	}
	if _, err := th.h.EndDirective(userCtx(), pause.ID); err != nil {
		t.Fatal(err)
	}
	th.h.SchedulePending(ctx)
	expectTaskStarts(t, fw, "t-pause-a", "t-pause-b")

	// A drain directive blocks new work and does not stick to workers.drained.
	// A manual drain still blocks after that directive ends.
	createQueuedTask(t, th.st, "t-drain", WorkerSelector{}, false)
	drain := mustDirective(t, th.h, DirectiveInput{Kind: store.DirectiveMachineDrain, Target: "w1"})
	th.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgTaskStart, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("drain directive dispatched %d tasks", len(got))
	}
	w, err = th.st.GetWorker(ctx, "w1")
	if err != nil || w.Drained {
		t.Fatalf("drain directive wrote workers.drained: %+v %v", w, err)
	}
	if err := th.st.SetWorkerDrained(ctx, "w1", true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := th.h.EndDirective(userCtx(), drain.ID); err != nil {
		t.Fatal(err)
	}
	th.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgTaskStart, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("ending a drain directive cleared a manual drain")
	}
	w, _ = th.st.GetWorker(ctx, "w1")
	if !w.Drained {
		t.Fatal("manual drain was cleared")
	}
	if err := th.st.SetWorkerDrained(ctx, "w1", false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	th.h.SchedulePending(ctx)
	expectTaskStarts(t, fw, "t-drain")
}

func TestSchedulerCapsThrottledMachines(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	info := testWorkerInfo("w1", "box")
	info.MaxConcurrent = 4
	fw := th.connectWorker(t, info, nil, "w1")

	zero := mustDirective(t, th.h, DirectiveInput{
		Kind: store.DirectiveMachineThrottle, Target: "w1", Params: store.DirectiveParams{MaxConcurrency: intPtr(0)},
	})
	createQueuedTask(t, th.st, "t-zero", WorkerSelector{}, false)
	th.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgTaskStart, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("max_concurrency 0 still dispatched")
	}
	if _, err := th.h.EndDirective(userCtx(), zero.ID); err != nil {
		t.Fatal(err)
	}
	// Ending the cap lets the waiting task go. Retire it so it does not fill the next cap.
	th.h.SchedulePending(ctx)
	expectTaskStarts(t, fw, "t-zero")
	fw.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: "t-zero", Status: protocol.TaskStatusCompleted})
	waitFor(t, 3*time.Second, "t-zero completed", func() bool {
		return mustTask(t, th.st, "t-zero").Status == protocol.TaskStatusCompleted
	})

	throttle := mustDirective(t, th.h, DirectiveInput{
		Kind: store.DirectiveMachineThrottle, Target: "w1", Params: store.DirectiveParams{MaxConcurrency: intPtr(1)},
	})
	createQueuedTask(t, th.st, "t-one", WorkerSelector{}, false)
	createQueuedTask(t, th.st, "t-two", WorkerSelector{}, false)
	th.h.SchedulePending(ctx)
	first, err := protocol.DecodePayload[protocol.TaskStartPayload](fw.expect(protocol.MsgTaskStart, 3*time.Second))
	if err != nil || (first.TaskID != "t-one" && first.TaskID != "t-two") {
		t.Fatalf("throttle dispatched %+v %v", first, err)
	}
	other := "t-one"
	if first.TaskID == "t-one" {
		other = "t-two"
	}
	if got := fw.drain(protocol.MsgTaskStart, 250*time.Millisecond); len(got) != 0 {
		t.Fatal("throttle max 1 dispatched a second task")
	}
	if got := mustTask(t, th.st, other); got.Status != protocol.TaskStatusQueued {
		t.Fatalf("second task status %s", got.Status)
	}
	if _, err := th.h.EndDirective(userCtx(), throttle.ID); err != nil {
		t.Fatal(err)
	}
	th.h.SchedulePending(ctx)
	expectTaskStarts(t, fw, other)
}

func TestSchedulerIgnoresDirectivePastUntil(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 25, 14, 0, 0, 0, time.UTC)
	clk := &clockBox{t: start}
	th.h.directiveNow = clk.now
	fw := th.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	until := start.Add(time.Hour)
	mustDirective(t, th.h, DirectiveInput{Kind: store.DirectiveMachinePause, Target: "w1", Until: &until})
	createQueuedTask(t, th.st, "t-later", WorkerSelector{}, false)
	th.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgTaskStart, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("pause still in the future dispatched")
	}
	clk.set(until.Add(time.Second))
	th.h.SchedulePending(ctx)
	expectTaskStarts(t, fw, "t-later")
	list, err := th.st.ListDirectives(ctx, store.DirectiveFilter{OpenOnly: true, Limit: 10})
	if err != nil || len(list) != 1 || list[0].EndedAt != nil {
		t.Fatalf("row should still be open until the reaper: %+v %v", list, err)
	}
}

func TestAutopilotPauseStopsTicketDispatch(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	proj, repo := createProjectAndRepo(t, th.st, "Billing", true, false)
	mk := func(title string) store.Ticket {
		t.Helper()
		tk, err := th.st.CreateTicket(ctx, store.Ticket{
			ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: title,
			Status: store.TicketStatusReady, Hints: store.TicketHints{Agent: "mock"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repo.ID}); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	first := mk("First")
	fw := th.connectWorker(t, testWorkerInfo("w-1", "box-1"), nil, "w-1")
	open, err := protocol.DecodePayload[protocol.AgentChatOpen](fw.expect(protocol.MsgAgentChatOpen, 3*time.Second))
	if err != nil || open.TicketSID != first.SID {
		t.Fatalf("happy path did not dispatch: %+v %v", open, err)
	}
	if cur, _ := th.st.GetTicket(ctx, first.ID); cur.Status != store.TicketStatusRunning {
		t.Fatalf("first ticket status %s", cur.Status)
	}

	second := mk("Second")
	d := mustDirective(t, th.h, DirectiveInput{Kind: store.DirectiveAutopilotPause, Target: proj.ID, Reason: "hold the board"})
	th.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgAgentChatOpen, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("autopilot.pause still dispatched")
	}
	if cur, _ := th.st.GetTicket(ctx, second.ID); cur.Status != store.TicketStatusReady || cur.Lease != nil {
		t.Fatalf("paused ticket: %+v", cur)
	}
	if _, err := th.h.EndDirective(userCtx(), d.ID); err != nil {
		t.Fatal(err)
	}
	th.h.SchedulePending(ctx)
	open, err = protocol.DecodePayload[protocol.AgentChatOpen](fw.expect(protocol.MsgAgentChatOpen, 3*time.Second))
	if err != nil || open.TicketSID != second.SID {
		t.Fatalf("dispatch after end: %+v %v", open, err)
	}
}

func TestDirectiveSuspendResumeOnPause(t *testing.T) {
	th := newTestHub(t)
	w := th.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	task := startTask(t, th.svc, w, userCtx(), "w1", "work", nil)

	first := mustDirective(t, th.h, DirectiveInput{
		Kind: store.DirectiveMachinePause, Target: "w1", Params: store.DirectiveParams{SuspendRunning: true},
	})
	if p, _ := protocol.DecodePayload[protocol.TaskSuspendPayload](w.expect(protocol.MsgTaskSuspend, 3*time.Second)); p.TaskID != task.ID {
		t.Fatalf("suspend payload %+v", p)
	}
	w.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: task.ID, Status: protocol.TaskStatusSuspended})
	waitFor(t, 3*time.Second, "suspended by directive", func() bool {
		tk := mustTask(t, th.st, task.ID)
		return tk.Status == protocol.TaskStatusSuspended && tk.SuspendedBy == store.SuspendedByDirective
	})
	if hs := activeHolds(t, th.st); len(hs) != 0 {
		t.Fatalf("directive suspend placed holds: %+v", hs)
	}
	var susCause store.Cause
	for _, e := range taskEvents(t, th.st, task.ID) {
		if e.ToStatus == protocol.TaskStatusSuspended {
			susCause = e.Cause
		}
	}
	if susCause.Actor != store.ActorSystem || susCause.Source != "directive" {
		t.Fatalf("suspend cause %+v", susCause)
	}

	second := mustDirective(t, th.h, DirectiveInput{
		Kind: store.DirectiveMachinePause, Target: "w1", Params: store.DirectiveParams{SuspendRunning: true},
	})
	if _, err := th.h.EndDirective(userCtx(), first.ID); err != nil {
		t.Fatal(err)
	}
	if got := w.drain(protocol.MsgTaskResume, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("ending one of two suspend pauses resumed the session")
	}
	if tk := mustTask(t, th.st, task.ID); tk.Status != protocol.TaskStatusSuspended {
		t.Fatalf("status after ending one pause: %s", tk.Status)
	}
	if _, err := th.h.EndDirective(userCtx(), second.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := protocol.DecodePayload[protocol.TaskSuspendPayload](w.expect(protocol.MsgTaskResume, 3*time.Second)); p.TaskID != task.ID {
		t.Fatalf("resume payload %+v", p)
	}
	w.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: task.ID, Status: protocol.TaskStatusRunning})
	waitFor(t, 3*time.Second, "running after last pause ended", func() bool {
		tk := mustTask(t, th.st, task.ID)
		return tk.Status == protocol.TaskStatusRunning && tk.SuspendedBy == ""
	})

	// Expiry of a suspend_running pause resumes what it froze.
	start := time.Date(2026, 11, 25, 16, 0, 0, 0, time.UTC)
	clk := &clockBox{t: start}
	th.h.directiveNow = clk.now
	until := start.Add(10 * time.Minute)
	mustDirective(t, th.h, DirectiveInput{
		Kind: store.DirectiveMachinePause, Target: "w1", Until: &until,
		Params: store.DirectiveParams{SuspendRunning: true},
	})
	w.expect(protocol.MsgTaskSuspend, 3*time.Second)
	w.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: task.ID, Status: protocol.TaskStatusSuspended})
	waitFor(t, 3*time.Second, "suspended until expiry", func() bool {
		return mustTask(t, th.st, task.ID).Status == protocol.TaskStatusSuspended
	})
	clk.set(until.Add(time.Second))
	th.h.runDirectivePass(context.Background())
	w.expect(protocol.MsgTaskResume, 3*time.Second)
	w.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: task.ID, Status: protocol.TaskStatusRunning})
	waitFor(t, 3*time.Second, "resumed on expiry", func() bool {
		tk := mustTask(t, th.st, task.ID)
		return tk.Status == protocol.TaskStatusRunning && tk.SuspendedBy == ""
	})
}

func TestHelloPushesDirectivesAndResuspends(t *testing.T) {
	th := newTestHub(t)
	info := testWorkerInfo("w1", "box")
	w := th.connectWorker(t, info, nil, "w1")
	task := startTask(t, th.svc, w, userCtx(), "w1", "work", nil)
	mustDirective(t, th.h, DirectiveInput{
		Kind: store.DirectiveMachinePause, Target: "w1", Params: store.DirectiveParams{SuspendRunning: true},
	})
	dir := w.expect(protocol.MsgWorkerDirectives, 3*time.Second)
	payload, err := protocol.DecodePayload[protocol.WorkerDirectivesPayload](dir)
	if err != nil || len(payload.Directives) != 1 || payload.Directives[0].Kind != protocol.DirectiveKindMachinePause || !payload.Directives[0].Params.SuspendRunning {
		t.Fatalf("pushed directives: %+v %v", payload, err)
	}
	w.expect(protocol.MsgTaskSuspend, 3*time.Second)
	w.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: task.ID, Status: protocol.TaskStatusSuspended})
	waitFor(t, 3*time.Second, "suspended", func() bool {
		return mustTask(t, th.st, task.ID).Status == protocol.TaskStatusSuspended
	})

	w.close()
	waitFor(t, 3*time.Second, "worker to disconnect", func() bool { return !th.h.IsWorkerConnected("w1") })
	w2 := th.connectWorker(t, info, []protocol.ActiveTaskInfo{{TaskID: task.ID, Status: protocol.TaskStatusRunning}}, "w1")
	payload, err = protocol.DecodePayload[protocol.WorkerDirectivesPayload](w2.expect(protocol.MsgWorkerDirectives, 3*time.Second))
	if err != nil || len(payload.Directives) != 1 || payload.Directives[0].Kind != protocol.DirectiveKindMachinePause {
		t.Fatalf("hello directives: %+v %v", payload, err)
	}
	if p, _ := protocol.DecodePayload[protocol.TaskSuspendPayload](w2.expect(protocol.MsgTaskSuspend, 3*time.Second)); p.TaskID != task.ID {
		t.Fatalf("re-suspend payload %+v", p)
	}
}

func TestDirectiveRESTAuditAndFleet(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	upsertBox(t, cs.st, "w1", "box")
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	code, raw := cs.api(t, http.MethodPost, "/api/directives", map[string]any{
		"kind": "machine.throttle", "target": "box", "until": until, "reason": "quiet hours",
		"params": map[string]any{"max_concurrency": 1, "cpu_ceiling_pct": 40},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	var d store.Directive
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if !ids.Valid(ids.Directive, d.SID) || d.Kind != store.DirectiveMachineThrottle || d.Target != "w1" ||
		d.Provenance != store.ProvenanceUser || d.Actor != "local" || d.Reason != "quiet hours" ||
		d.Params.MaxConcurrency == nil || *d.Params.MaxConcurrency != 1 || d.Params.CPUCeilingPct == nil || *d.Params.CPUCeilingPct != 40 ||
		d.Until == nil {
		t.Fatalf("created directive: %+v", d)
	}

	code, raw = cs.api(t, http.MethodGet, "/api/directives?target=box", nil)
	if code != http.StatusOK || !strings.Contains(string(raw), d.SID) {
		t.Fatalf("list: %d %s", code, raw)
	}
	code, raw = cs.api(t, http.MethodGet, "/api/fleet", nil)
	if code != http.StatusOK || !strings.Contains(string(raw), d.SID) {
		t.Fatalf("fleet snapshot missing directive: %d %s", code, raw)
	}

	code, raw = cs.api(t, http.MethodPost, "/api/directives/"+d.SID+"/end", nil)
	if code != http.StatusOK {
		t.Fatalf("end: %d %s", code, raw)
	}
	var ended store.Directive
	if err := json.Unmarshal(raw, &ended); err != nil || ended.EndedAt == nil || ended.EndedBy != "local" {
		t.Fatalf("ended: %+v %v %s", ended, err, raw)
	}
	code, raw = cs.api(t, http.MethodPost, "/api/directives/"+d.ID+"/end", nil)
	if code != http.StatusOK {
		t.Fatalf("idempotent end: %d %s", code, raw)
	}
	created, err := cs.st.ListAudit(ctx, store.AuditFilter{Action: AuditDirectiveCreate, Limit: 10})
	endedAudits, err2 := cs.st.ListAudit(ctx, store.AuditFilter{Action: AuditDirectiveEnd, Limit: 10})
	if err != nil || err2 != nil || len(created) != 1 || len(endedAudits) != 1 {
		t.Fatalf("audit create %d end %d (%v %v)", len(created), len(endedAudits), err, err2)
	}
	if endedAudits[0].Cause == nil || endedAudits[0].Cause.Actor != store.ActorUser || endedAudits[0].Cause.ActorID != "local" {
		t.Fatalf("end audit cause: %+v", endedAudits[0].Cause)
	}
	code, raw = cs.api(t, http.MethodGet, "/api/directives", nil)
	if code != http.StatusOK || strings.Contains(string(raw), d.SID) {
		t.Fatalf("active list still has ended directive: %d %s", code, raw)
	}
	code, raw = cs.api(t, http.MethodGet, "/api/directives?active=0", nil)
	if code != http.StatusOK || !strings.Contains(string(raw), d.SID) {
		t.Fatalf("all list: %d %s", code, raw)
	}

	code, raw = cs.api(t, http.MethodPost, "/api/directives", map[string]any{"kind": store.DirectiveProviderDisable, "target": "w1"})
	if code != http.StatusBadRequest || !strings.Contains(string(raw), "not available yet") {
		t.Fatalf("provider.disable: %d %s", code, raw)
	}
	code, raw = cs.api(t, http.MethodPost, "/api/directives", map[string]any{
		"kind": store.DirectiveMachinePause, "target": "w1", "until": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	})
	if code != http.StatusBadRequest || !strings.Contains(string(raw), "future") {
		t.Fatalf("past until: %d %s", code, raw)
	}
	code, raw = cs.api(t, http.MethodPost, "/api/directives", map[string]any{"kind": store.DirectiveMachinePause, "target": "missing-box"})
	if code != http.StatusBadRequest || !strings.Contains(string(raw), "not found") {
		t.Fatalf("unknown machine: %d %s", code, raw)
	}
	code, raw = cs.api(t, http.MethodPost, "/api/directives/does-not-exist/end", nil)
	if code != http.StatusNotFound {
		t.Fatalf("end missing: %d %s", code, raw)
	}
}

func TestBoardPulseFleetLineUsesDirectives(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	loc := time.Local
	now := time.Date(2026, 11, 25, 14, 32, 0, 0, loc)
	until := time.Date(2026, 11, 25, 18, 0, 0, 0, loc)
	clk := &clockBox{t: now}
	th.h.directiveNow = clk.now
	th.h.pulseNow = clk.now

	upsertBox(t, th.st, "mini", "mini-mac")
	upsertBox(t, th.st, "win", "win-desk")
	upsertBox(t, th.st, "box", "box-2")
	upsertBox(t, th.st, "lab", "lab")
	if err := th.st.SetWorkerDrained(ctx, "box", true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := th.st.CreateHold(ctx, store.Hold{
		ScopeKind: store.HoldScopeMachine, WorkerID: "win", Summary: "user hold",
		Cause: store.Cause{Actor: store.ActorUser, Confidence: store.ConfidenceCertain},
	}); err != nil {
		t.Fatal(err)
	}
	mustDirective(t, th.h, DirectiveInput{
		Kind: store.DirectiveMachineThrottle, Target: "mini", Until: &until,
		Params: store.DirectiveParams{MaxConcurrency: intPtr(1)},
	})
	// Pause wins over a throttle on the same machine. The hold still appends.
	mustDirective(t, th.h, DirectiveInput{
		Kind: store.DirectiveMachineThrottle, Target: "win", Until: &until,
		Params: store.DirectiveParams{MaxConcurrency: intPtr(2)},
	})
	mustDirective(t, th.h, DirectiveInput{Kind: store.DirectiveMachinePause, Target: "win", Until: &until})
	mustDirective(t, th.h, DirectiveInput{Kind: store.DirectiveMachinePause, Target: "lab"})

	proj, _ := createProjectAndRepo(t, th.st, "Billing", true, false)
	mustDirective(t, th.h, DirectiveInput{Kind: store.DirectiveAutopilotPause, Target: proj.ID})

	text := th.h.buildBoardPulse(ctx, nil).Text()
	want := "Fleet: 0/4 up (box-2 drained; lab paused; mini-mac throttled max=1 until 18:00; win-desk paused until 18:00, held)"
	if !strings.Contains(text, want) {
		t.Fatalf("pulse text:\n%s\nwant fragment:\n%s", text, want)
	}
	if strings.Contains(text, "win-desk throttled") || strings.Contains(text, "lab paused until") {
		t.Fatalf("pulse kept a lower-priority or open-ended clock: %s", text)
	}
	if strings.Contains(text, "autopilot on") || strings.Contains(text, "autopilot paused") {
		t.Fatalf("a project directive should leave the header off, not paused: %s", text)
	}
}

func expectTaskStarts(t *testing.T, fw *fakeWorker, ids ...string) {
	t.Helper()
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	deadline := time.Now().Add(4 * time.Second)
	for len(want) > 0 {
		if !time.Now().Before(deadline) {
			t.Fatalf("missing task.start for %v", want)
		}
		env := fw.expect(protocol.MsgTaskStart, time.Until(deadline))
		p, err := protocol.DecodePayload[protocol.TaskStartPayload](env)
		if err != nil {
			t.Fatal(err)
		}
		if !want[p.TaskID] {
			t.Fatalf("unexpected task.start %s, still want %v", p.TaskID, want)
		}
		delete(want, p.TaskID)
	}
}
