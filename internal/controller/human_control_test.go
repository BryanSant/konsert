package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- helpers ----------------------------------------------------------------------------

func userCtx() context.Context {
	return withAuditActor(context.Background(), "tester", auditSourceREST)
}

func orchestratorCtx(chatID string) context.Context {
	ctx := withAuditActor(context.Background(), "orchestrator", auditSourceOrchestrator)
	if chatID != "" {
		ctx = withChatOrigin(ctx, chatID, true)
	}
	return ctx
}

// expectWhere returns the next frame of msgType satisfying pred (others are skipped).
func expectWhere(t *testing.T, fw *fakeWorker, msgType string, timeout time.Duration, pred func(protocol.Envelope) bool) protocol.Envelope {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		env := fw.expect(msgType, time.Until(deadline))
		if pred(env) {
			return env
		}
	}
	t.Fatalf("no %s frame matching the predicate", msgType)
	return protocol.Envelope{}
}

// startTask dispatches a mock task pinned to workerID and waits for its task.start; the
// fake worker then reports it running.
func startTask(t *testing.T, svc *FleetService, fw *fakeWorker, ctx context.Context, workerID, prompt string, mutate func(*DispatchRequest)) store.Task {
	t.Helper()
	req := DispatchRequest{WorkerSelector: WorkerSelector{WorkerID: workerID}, Agent: "mock", RepoPath: "/tmp", Prompt: prompt, Mode: protocol.TaskModeOneshot}
	if mutate != nil {
		mutate(&req)
	}
	res, err := svc.DispatchTask(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	expectWhere(t, fw, protocol.MsgTaskStart, 3*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.TaskStartPayload](env)
		return p.TaskID == res.TaskID
	})
	fw.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: res.TaskID, Status: protocol.TaskStatusRunning})
	waitFor(t, 3*time.Second, "task running", func() bool { return mustTask(t, svc.store, res.TaskID).Status == protocol.TaskStatusRunning })
	return mustTask(t, svc.store, res.TaskID)
}

func taskEvents(t *testing.T, st store.Store, taskID string) []store.FleetEvent {
	t.Helper()
	evs, err := st.ListFleetEvents(context.Background(), store.FleetEventFilter{TaskID: taskID, Kind: store.EventKindStatus, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	// oldest first
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	return evs
}

func activeHolds(t *testing.T, st store.Store) []store.Hold {
	t.Helper()
	hs, err := st.ListHolds(context.Background(), store.HoldFilter{ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return hs
}

func operatorCmd(taskID, cmd string, exit int) protocol.OperatorCommandPayload {
	now := time.Now().UTC()
	end := now.Add(100 * time.Millisecond)
	return protocol.OperatorCommandPayload{TaskID: taskID, Command: cmd, ExitCode: &exit, Cwd: "/home/u", StartedAt: now, EndedAt: &end,
		CommandID: "01" + strings.ReplaceAll(now.Format("150405.000000000"), ".", "") + cmd[:min(3, len(cmd))]}
}

// --- cause model ---------------------------------------------------------------------------

func TestCauseRecordedOnEveryTaskTransition(t *testing.T) {
	th := newTestHub(t)
	w := th.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")

	// Dispatched by the user (UI), started by the worker, canceled by the user.
	task := startTask(t, th.svc, w, userCtx(), "w1", "build it", nil)
	if _, err := th.svc.CancelTask(userCtx(), task.ID); err != nil {
		t.Fatal(err)
	}
	w.expect(protocol.MsgTaskCancel, 3*time.Second)
	w.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: task.ID, Status: protocol.TaskStatusCanceled})
	waitFor(t, 3*time.Second, "canceled", func() bool { return mustTask(t, th.st, task.ID).Status == protocol.TaskStatusCanceled })

	evs := taskEvents(t, th.st, task.ID)
	type tr struct{ from, to, actor, source string }
	var got []tr
	for _, e := range evs {
		got = append(got, tr{e.FromStatus, e.ToStatus, e.Cause.Actor, e.Cause.Source})
		if e.Cause.Confidence == "" || e.Summary == "" {
			t.Fatalf("event without confidence or summary: %+v", e)
		}
	}
	want := []tr{
		{"", protocol.TaskStatusAssigned, store.ActorUser, store.SourceUI},
		{protocol.TaskStatusAssigned, protocol.TaskStatusRunning, store.ActorSystem, "worker"},
		{protocol.TaskStatusRunning, protocol.TaskStatusCanceled, store.ActorUser, store.SourceUI},
	}
	if len(got) != len(want) {
		t.Fatalf("transitions %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transition %d = %+v, want %+v (all %+v)", i, got[i], want[i], got)
		}
	}
	if c := mustTask(t, th.st, task.ID).Cause; c == nil || c.Actor != store.ActorUser || c.ActorID != "tester" {
		t.Fatalf("task cause %+v", c)
	}
	items, _ := th.svc.ListTasks(context.Background(), "", "w1", 10)
	if len(items) == 0 || items[0].LastCause == nil || items[0].LastCause.Actor != store.ActorUser {
		t.Fatalf("list_tasks last cause: %+v", items)
	}
	res, _ := th.svc.GetTaskResult(context.Background(), task.ID, false, 0)
	if res.LastCause == nil || res.LastCause.Actor != store.ActorUser || len(res.Holds) != 1 {
		t.Fatalf("get_task_result cause/holds: %+v", res)
	}

	// An agent task that finishes by itself: the agent did it.
	t2 := startTask(t, th.svc, w, orchestratorCtx("orch-1"), "w1", "do it", nil)
	code := 0
	w.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: t2.ID, Status: protocol.TaskStatusCompleted, ExitCode: &code})
	waitFor(t, 3*time.Second, "completed", func() bool { return mustTask(t, th.st, t2.ID).Status == protocol.TaskStatusCompleted })
	evs = taskEvents(t, th.st, t2.ID)
	if evs[0].Cause.Actor != store.ActorOrchestrator || evs[0].Cause.ActorID != "orch-1" || evs[0].ChatID != "orch-1" {
		t.Fatalf("orchestrator dispatch cause: %+v", evs[0])
	}
	if last := evs[len(evs)-1]; last.Cause.Actor != store.ActorAgent || last.ToStatus != protocol.TaskStatusCompleted {
		t.Fatalf("completion cause: %+v", last)
	}
	if mustTask(t, th.st, t2.ID).ParentChatID != "orch-1" {
		t.Fatal("parent chat not recorded")
	}

	// Queued work placed later: the scheduler did it.
	res2, err := th.svc.DispatchTask(userCtx(), DispatchRequest{WorkerSelector: WorkerSelector{WorkerID: "w2"}, Agent: "mock", RepoPath: "/tmp", Prompt: "later"})
	if err != nil || res2.Status != protocol.TaskStatusQueued {
		t.Fatalf("queued dispatch: %+v %v", res2, err)
	}
	w2 := th.connectWorker(t, testWorkerInfo("w2", "box2"), nil, "w2")
	w2.expect(protocol.MsgTaskStart, 3*time.Second)
	waitFor(t, 3*time.Second, "scheduler transition", func() bool {
		evs := taskEvents(t, th.st, res2.TaskID)
		return len(evs) == 2 && evs[1].Cause.Actor == store.ActorScheduler && evs[1].ToStatus == protocol.TaskStatusAssigned
	})

	// A worker lost while the task ran: the reaper (system) failed it.
	t3 := startTask(t, th.svc, w, userCtx(), "w1", "long", nil)
	th.h.handleOrphanTasks(context.Background(), "w1")
	evs = taskEvents(t, th.st, t3.ID)
	if last := evs[len(evs)-1]; last.ToStatus != protocol.TaskStatusFailed || last.Cause.Actor != store.ActorSystem || last.Cause.Source != "reaper" {
		t.Fatalf("orphan cause: %+v", last)
	}

	// Audit rows carry the cause too.
	audits, _ := th.st.ListAudit(context.Background(), store.AuditFilter{Action: AuditCancel})
	if len(audits) == 0 || audits[0].Cause == nil || audits[0].Cause.Actor != store.ActorUser {
		t.Fatalf("audit cause: %+v", audits)
	}
}

// --- suspend / resume --------------------------------------------------------------------

func TestSuspendResumeTaskPlacesAndReleasesHold(t *testing.T) {
	th := newTestHub(t)
	w := th.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	task := startTask(t, th.svc, w, orchestratorCtx("orch-1"), "w1", "work", nil)

	res, err := th.h.SuspendTask(userCtx(), task.ID)
	if err != nil || res.Status != protocol.TaskStatusSuspended {
		t.Fatalf("suspend: %+v %v", res, err)
	}
	p, _ := protocol.DecodePayload[protocol.TaskSuspendPayload](w.expect(protocol.MsgTaskSuspend, 3*time.Second))
	if p.TaskID != task.ID {
		t.Fatalf("suspend payload %+v", p)
	}
	w.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: task.ID, Status: protocol.TaskStatusSuspended})
	waitFor(t, 3*time.Second, "suspended", func() bool { return mustTask(t, th.st, task.ID).Status == protocol.TaskStatusSuspended })
	cur := mustTask(t, th.st, task.ID)
	if cur.SuspendedBy != store.SuspendedByUser || cur.Cause == nil || cur.Cause.Actor != store.ActorUser {
		t.Fatalf("suspended task: %+v cause %+v", cur, cur.Cause)
	}
	holds := activeHolds(t, th.st)
	if len(holds) != 1 || holds[0].ScopeKind != store.HoldScopeSession || holds[0].TaskID != task.ID {
		t.Fatalf("holds after suspend: %+v", holds)
	}
	// The orchestrator may not steer a session the user suspended.
	if _, err := th.svc.SteerTask(orchestratorCtx("orch-1"), task.ID, "keep going", true); err == nil || !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("orchestrator steer of a suspended task: %v", err)
	}

	if _, err := th.h.ResumeTask(userCtx(), task.ID); err != nil {
		t.Fatal(err)
	}
	w.expect(protocol.MsgTaskResume, 3*time.Second)
	w.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: task.ID, Status: protocol.TaskStatusRunning})
	waitFor(t, 3*time.Second, "running again", func() bool { return mustTask(t, th.st, task.ID).Status == protocol.TaskStatusRunning })
	cur = mustTask(t, th.st, task.ID)
	if cur.SuspendedBy != "" || cur.Cause == nil || cur.Cause.Actor != store.ActorUser {
		t.Fatalf("resumed task: %+v", cur)
	}
	if hs := activeHolds(t, th.st); len(hs) != 0 {
		t.Fatalf("resume left holds: %+v", hs)
	}
	if _, err := th.svc.SteerTask(orchestratorCtx("orch-1"), task.ID, "keep going", true); err != nil {
		t.Fatalf("steer after resume: %v", err)
	}
}

func TestChatSuspendResumeMidTurn(t *testing.T) {
	cs := newChatServer(t)
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)
	chat, _, turn := openChat(t, cs, w, b, nil)
	w.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: chat.ID, Seq: 1, Kind: protocol.EventStatus, Status: protocol.ChatStatusWorking, TurnID: turn})
	waitFor(t, 3*time.Second, "working", func() bool { return mustSession(t, cs.st, chat.ID).Status == protocol.ChatStatusWorking })

	code, raw := cs.api(t, http.MethodPost, "/api/chats/"+chat.SID+"/suspend", nil)
	if code != 200 || !strings.Contains(string(raw), `"suspended"`) {
		t.Fatalf("suspend: %d %s", code, raw)
	}
	if p, _ := protocol.DecodePayload[protocol.AgentChatSuspend](w.expect(protocol.MsgAgentChatSuspend, 3*time.Second)); p.ChatID != chat.ID {
		t.Fatalf("suspend payload %+v", p)
	}
	s := mustSession(t, cs.st, chat.ID)
	if s.Status != protocol.ChatStatusSuspended || s.SuspendedBy != store.SuspendedByUser || s.SuspendedPrev != protocol.ChatStatusWorking || s.Cause == nil || s.Cause.Actor != store.ActorUser {
		t.Fatalf("suspended chat: %+v cause %+v", s, s.Cause)
	}
	// The frozen engine's stale reports don't un-suspend it; new prompts are refused.
	w.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: chat.ID, Seq: 2, Kind: protocol.EventStatus, Status: protocol.ChatStatusWorking, TurnID: turn})
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat.ID, Content: "hello?"})
	expectChatEvent(t, b, protocol.EventError)
	if got := w.drain(protocol.MsgAgentChatPrompt, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("prompt forwarded to a suspended chat: %d", len(got))
	}
	if s := mustSession(t, cs.st, chat.ID); s.Status != protocol.ChatStatusSuspended {
		t.Fatalf("status after stale report: %s", s.Status)
	}

	code, raw = cs.api(t, http.MethodPost, "/api/chats/"+chat.ID+"/resume", nil)
	if code != 200 {
		t.Fatalf("resume: %d %s", code, raw)
	}
	w.expect(protocol.MsgAgentChatResume, 3*time.Second)
	s = mustSession(t, cs.st, chat.ID)
	if s.Status != protocol.ChatStatusWorking || s.SuspendedBy != "" {
		t.Fatalf("resumed chat: %+v", s)
	}
	if hs := activeHolds(t, cs.st); len(hs) != 0 {
		t.Fatalf("resume left holds: %+v", hs)
	}
	// Both transitions are in the timeline, attributed to the user.
	evs, _ := cs.st.ListFleetEvents(context.Background(), store.FleetEventFilter{ChatID: chat.ID, Kind: store.EventKindStatus})
	var sus, res bool
	for _, e := range evs {
		sus = sus || (e.ToStatus == protocol.ChatStatusSuspended && e.Cause.Actor == store.ActorUser)
		res = res || (e.FromStatus == protocol.ChatStatusSuspended && e.ToStatus == protocol.ChatStatusWorking && e.Cause.Actor == store.ActorUser)
	}
	if !sus || !res {
		t.Fatalf("chat transitions not attributed: %+v", evs)
	}
}

// --- drain and pause ------------------------------------------------------------------------

func TestDrainAndPauseBlockDispatchAndSurviveRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	th := newTestHubAt(t, dbPath)
	w := th.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")

	// Drained: work pinned to the machine waits in the queue until undrained.
	if _, err := th.h.DrainWorker(userCtx(), "w1", true); err != nil {
		t.Fatal(err)
	}
	res, err := th.svc.DispatchTask(userCtx(), DispatchRequest{WorkerSelector: WorkerSelector{WorkerID: "w1"}, Agent: "mock", RepoPath: "/tmp", Prompt: "x"})
	if err != nil || res.Status != protocol.TaskStatusQueued {
		t.Fatalf("dispatch to drained: %+v %v", res, err)
	}
	th.h.kickScheduler()
	if got := w.drain(protocol.MsgTaskStart, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("drained machine received work")
	}
	if _, err := th.h.DrainWorker(userCtx(), "w1", false); err != nil {
		t.Fatal(err)
	}
	expectWhere(t, w, protocol.MsgTaskStart, 3*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.TaskStartPayload](env)
		return p.TaskID == res.TaskID
	})
	w.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: res.TaskID, Status: protocol.TaskStatusRunning})
	waitFor(t, 3*time.Second, "running", func() bool { return mustTask(t, th.st, res.TaskID).Status == protocol.TaskStatusRunning })

	// Pause everything: the running task is suspended and automatic dispatch refused.
	ps, err := th.h.PauseEverything(userCtx())
	if err != nil || !ps.Paused || ps.Affected != 1 {
		t.Fatalf("pause: %+v %v", ps, err)
	}
	w.expect(protocol.MsgTaskSuspend, 3*time.Second)
	w.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: res.TaskID, Status: protocol.TaskStatusSuspended})
	waitFor(t, 3*time.Second, "suspended by pause", func() bool {
		tk := mustTask(t, th.st, res.TaskID)
		return tk.Status == protocol.TaskStatusSuspended && tk.SuspendedBy == store.SuspendedByPause
	})
	if _, err := th.svc.DispatchTask(orchestratorCtx("orch"), DispatchRequest{Agent: "mock", RepoPath: "/tmp", Prompt: "more"}); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("orchestrator dispatch while paused: %v", err)
	}
	// Queued work stays queued while paused.
	queued := store.Task{Agent: "mock", Workdir: "/tmp", Prompt: "q", Status: protocol.TaskStatusQueued}
	if err := th.st.CreateTask(context.Background(), queued); err != nil {
		t.Fatal(err)
	}
	th.h.kickScheduler()
	if got := w.drain(protocol.MsgTaskStart, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("scheduler dispatched while paused")
	}
	if holds := activeHolds(t, th.st); len(holds) != 0 {
		t.Fatalf("pause placed holds: %+v", holds)
	}

	// The hub restarts: still paused, and Resume restores what the pause suspended.
	w.close()
	th.stop()
	th2 := newTestHubAt(t, dbPath)
	if !th2.h.fleetPaused(context.Background()) {
		t.Fatal("pause did not survive a hub restart")
	}
	w2 := th2.connectWorker(t, testWorkerInfo("w1", "box"), []protocol.ActiveTaskInfo{{TaskID: res.TaskID, Status: protocol.TaskStatusSuspended}}, "w1")
	if tk := mustTask(t, th2.st, res.TaskID); tk.Status != protocol.TaskStatusSuspended || tk.SuspendedBy != store.SuspendedByPause {
		t.Fatalf("after restart: %+v", tk)
	}
	ps, err = th2.h.ResumeEverything(userCtx())
	if err != nil || ps.Paused || ps.Affected != 1 {
		t.Fatalf("resume: %+v %v", ps, err)
	}
	w2.expect(protocol.MsgTaskResume, 3*time.Second)
	// Dispatching restarts: the queued task is placed.
	w2.expect(protocol.MsgTaskStart, 3*time.Second)
	// The whole story is in the timeline.
	evs, _ := th2.st.ListFleetEvents(context.Background(), store.FleetEventFilter{Kind: store.EventKindAction, Actor: store.ActorUser})
	var actions []string
	for _, e := range evs {
		actions = append(actions, e.Action)
	}
	if got := strings.Join(actions, ","); got != "unpause,pause,undrain,drain" {
		t.Fatalf("user actions %q", got)
	}
}

// --- holds -----------------------------------------------------------------------------------

func TestHoldsRefuseAutomaticActionsUntilReleased(t *testing.T) {
	cs := newChatServer(t)
	w := cs.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	term := startTask(t, cs.svc, w, userCtx(), "w1", "", func(r *DispatchRequest) { r.SessionKind, r.Mode = "terminal", protocol.TaskModeInteractive })

	// The user stops nginx in a Konsert terminal, unrequested: nginx on box is held.
	w.send(protocol.MsgWorkerOperatorCommand, operatorCmd(term.ID, "sudo systemctl stop nginx", 0))
	waitFor(t, 3*time.Second, "hold", func() bool { return len(activeHolds(t, cs.st)) == 1 })
	hd := activeHolds(t, cs.st)[0]
	if hd.ScopeKind != store.HoldScopeResource || hd.WorkerID != "w1" || hd.Resource == nil || hd.Resource.Kind != "service" || hd.Resource.Name != "nginx" ||
		hd.Cause.Actor != store.ActorUser || hd.Cause.Source != store.SourceTerminal || hd.Cause.Confidence != store.ConfidenceHigh {
		t.Fatalf("hold: %+v", hd)
	}

	// The orchestrator may not touch nginx there...
	_, err := cs.svc.DispatchTask(orchestratorCtx("orch"), DispatchRequest{WorkerSelector: WorkerSelector{WorkerID: "w1"}, Agent: "mock", RepoPath: "/tmp", Prompt: "restart nginx and check the logs"})
	if err == nil || !strings.Contains(err.Error(), "on hold") || !strings.Contains(err.Error(), "ask the user") || !strings.Contains(err.Error(), hd.SID) {
		t.Fatalf("orchestrator dispatch on held resource: %v", err)
	}
	// ...but unrelated work is fine, and the user can do anything.
	if _, err := cs.svc.DispatchTask(orchestratorCtx("orch"), DispatchRequest{WorkerSelector: WorkerSelector{WorkerID: "w1"}, Agent: "mock", RepoPath: "/tmp", Prompt: "run the unit tests"}); err != nil {
		t.Fatalf("unrelated dispatch refused: %v", err)
	}
	if _, err := cs.svc.DispatchTask(userCtx(), DispatchRequest{WorkerSelector: WorkerSelector{WorkerID: "w1"}, Agent: "mock", RepoPath: "/tmp", Prompt: "restart nginx"}); err != nil {
		t.Fatalf("user dispatch refused: %v", err)
	}
	// No automatic restart of work on the machine while held.
	orphan := startTask(t, cs.svc, w, orchestratorCtx("orch"), "w1", "serve", func(r *DispatchRequest) { r.OrphanPolicy = protocol.OrphanPolicyRequeue })
	if cs.h.requeueOrphan(context.Background(), mustTask(t, cs.st, orphan.ID), "requeued") {
		t.Fatal("orphan requeued despite the hold")
	}
	if tk := mustTask(t, cs.st, orphan.ID); tk.Status != protocol.TaskStatusFailed || !strings.Contains(tk.Summary, hd.SID) || tk.Cause.HoldID != hd.ID {
		t.Fatalf("held orphan: %+v %+v", tk, tk.Cause)
	}
	// Fan-out skips the held machine.
	fo, err := cs.svc.FanOutFleet(orchestratorCtx("orch"), FanOutRequest{Kind: "agent_sweep", Agent: "mock", Prompt: "reload nginx"})
	if err != nil || len(fo.DispatchedTaskIDs) != 0 || len(fo.Machines) != 1 || fo.Machines[0].Status != "held" {
		t.Fatalf("fan-out on held machine: %+v %v", fo, err)
	}

	// The user releases it in the UI: automatic actions are allowed again.
	code, raw := cs.api(t, http.MethodPost, "/api/holds/"+hd.SID+"/release", map[string]string{"note": "go ahead"})
	if code != 200 || !strings.Contains(string(raw), `"released_at"`) {
		t.Fatalf("release: %d %s", code, raw)
	}
	if _, err := cs.svc.DispatchTask(orchestratorCtx("orch"), DispatchRequest{WorkerSelector: WorkerSelector{WorkerID: "w1"}, Agent: "mock", RepoPath: "/tmp", Prompt: "restart nginx"}); err != nil {
		t.Fatalf("dispatch after release: %v", err)
	}
	evs, _ := cs.st.ListFleetEvents(context.Background(), store.FleetEventFilter{Kind: store.EventKindHold})
	if len(evs) != 2 || evs[0].Action != "released" || evs[1].Action != "created" {
		t.Fatalf("hold events: %+v", evs)
	}

	// A terminal intervention without a known target holds only automatic restarts.
	w.send(protocol.MsgWorkerOperatorCommand, operatorCmd(term.ID, "sudo reboot", 0))
	waitFor(t, 3*time.Second, "restart hold", func() bool { return len(activeHolds(t, cs.st)) == 1 })
	rh := activeHolds(t, cs.st)[0]
	if rh.ScopeKind != store.HoldScopeMachine || rh.Resource == nil || rh.Resource.Kind != ResourceRestarts {
		t.Fatalf("restart hold: %+v", rh)
	}
	if _, held := cs.h.holdBlocking(context.Background(), holdCheck{workerID: "w1", text: "anything"}); held {
		t.Fatal("restart-only hold blocked new work")
	}
	if _, held := cs.h.holdBlocking(context.Background(), holdCheck{workerID: "w1", restart: true}); !held {
		t.Fatal("restart-only hold did not block a restart")
	}
}

// Taking control of a terminal holds it; handing back releases the hold.
func TestTakeControlHoldsSession(t *testing.T) {
	cs := newChatServer(t)
	w := cs.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	task := startTask(t, cs.svc, w, orchestratorCtx("orch"), "w1", "x", nil)
	code, raw := cs.api(t, http.MethodPost, "/api/tasks/"+task.SID+"/control", map[string]bool{"enabled": true})
	var resp struct {
		HoldID string `json:"hold_id"`
	}
	_ = json.Unmarshal(raw, &resp)
	if code != 200 || resp.HoldID == "" {
		t.Fatalf("take control: %d %s", code, raw)
	}
	if _, err := cs.svc.SteerTask(orchestratorCtx("orch"), task.ID, "ls", true); err == nil || !strings.Contains(err.Error(), "on hold") {
		t.Fatalf("orchestrator steer while the user has control: %v", err)
	}
	cs.api(t, http.MethodPost, "/api/tasks/"+task.ID+"/control", map[string]bool{"enabled": false})
	if hs := activeHolds(t, cs.st); len(hs) != 0 {
		t.Fatalf("hand back left holds: %+v", hs)
	}
}

// --- veto --------------------------------------------------------------------------------------

func TestVetoCancelsAndIsRememberedForTheOrchestrator(t *testing.T) {
	cs := newChatServer(t)
	w := cs.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	task := startTask(t, cs.svc, w, orchestratorCtx("orch"), "w1", "deploy to prod from the laptop", nil)
	code, raw := cs.api(t, http.MethodPost, "/api/tasks/"+task.SID+"/veto", map[string]string{"reason": "wrong_machine", "note": "use the linux build box"})
	if code != 200 {
		t.Fatalf("veto: %d %s", code, raw)
	}
	w.expect(protocol.MsgTaskCancel, 3*time.Second)
	if code, _ := cs.api(t, http.MethodPost, "/api/tasks/"+task.ID+"/veto", map[string]string{"reason": "bogus"}); code != http.StatusConflict {
		t.Fatalf("bad veto reason accepted: %d", code)
	}
	mem, _ := cs.st.ListMemory(context.Background(), "project:"+store.DefaultProjectID, 10)
	if len(mem) != 1 || !strings.Contains(mem[0].Summary, "wrong machine") || !strings.Contains(mem[0].Summary, "use the linux build box") || !strings.Contains(mem[0].Summary, task.SID) {
		t.Fatalf("veto memory: %+v", mem)
	}
	evs, _ := cs.st.ListFleetEvents(context.Background(), store.FleetEventFilter{Kind: store.EventKindVeto})
	if len(evs) != 1 || evs[0].Action != "wrong_machine" || evs[0].Cause.Actor != store.ActorUser || evs[0].TaskID != task.ID {
		t.Fatalf("veto event: %+v", evs)
	}
	if hs := activeHolds(t, cs.st); len(hs) != 1 || hs[0].TaskID != task.ID {
		t.Fatalf("veto hold: %+v", hs)
	}
}

// --- operator commands, requests and delivery -----------------------------------------------

func TestOperatorCommandIngestion(t *testing.T) {
	th := newTestHub(t)
	w := th.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	other := th.connectWorker(t, testWorkerInfo("w2", "box2"), nil, "w2")
	b := th.dialBrowser(t)
	term := startTask(t, th.svc, w, userCtx(), "w1", "", func(r *DispatchRequest) { r.SessionKind, r.Mode = "terminal", protocol.TaskModeInteractive })

	// A start report alone records nothing (the effect is judged when it ends).
	start := operatorCmd(term.ID, "docker stop web", 0)
	start.ExitCode, start.EndedAt = nil, nil
	w.send(protocol.MsgWorkerOperatorCommand, start)
	// The end report: a user action, broadcast, on the task's timeline, holding the container.
	end := start
	code := 0
	now := time.Now().UTC()
	end.ExitCode, end.EndedAt = &code, &now
	w.send(protocol.MsgWorkerOperatorCommand, end)
	env := expectWhere(t, b, protocol.MsgFleetUpdate, 3*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.FleetUpdatePayload](env)
		return p.Kind == "fleet_event" && strings.Contains(string(env.Payload), `"action":"command"`)
	})
	p, _ := protocol.DecodePayload[struct {
		FleetEvent store.FleetEvent `json:"fleet_event"`
	}](env)
	fe := p.FleetEvent
	if fe.Cause.Actor != store.ActorUser || fe.Cause.Source != store.SourceTerminal || fe.Cause.Confidence != store.ConfidenceHigh ||
		fe.Cause.CommandID != start.CommandID || fe.TaskID != term.ID || fe.WorkerID != "w1" || !strings.Contains(fe.Summary, "container web") {
		t.Fatalf("command event: %+v", fe)
	}
	waitFor(t, 3*time.Second, "container hold", func() bool { return len(activeHolds(t, th.st)) == 1 })
	if hd := activeHolds(t, th.st)[0]; hd.Resource == nil || hd.Resource.Kind != "container" || hd.Resource.Name != "web" || hd.Cause.CommandID != start.CommandID {
		t.Fatalf("container hold: %+v", hd)
	}
	tevs, _ := th.st.ListEvents(context.Background(), term.ID, 0, 50)
	if len(tevs) == 0 || tevs[len(tevs)-1].Kind != "command" || !strings.Contains(tevs[len(tevs)-1].Message, "docker stop web") {
		t.Fatalf("task timeline: %+v", tevs)
	}
	// Re-sent (outbox) reports are de-duplicated.
	w.send(protocol.MsgWorkerOperatorCommand, end)
	// A failed command changes nothing: recorded, no hold.
	w.send(protocol.MsgWorkerOperatorCommand, operatorCmd(term.ID, "systemctl stop postgres", 1))
	// Another worker cannot report commands for this terminal.
	other.send(protocol.MsgWorkerOperatorCommand, operatorCmd(term.ID, "systemctl stop redis", 0))
	time.Sleep(300 * time.Millisecond)
	cmds, _ := th.st.ListFleetEvents(context.Background(), store.FleetEventFilter{Kind: store.EventKindAction, TaskID: term.ID})
	if len(cmds) != 2 {
		t.Fatalf("command events: %+v", cmds)
	}
	if !strings.Contains(cmds[0].Summary, "postgres") || strings.Contains(cmds[0].MetaJSON, `"intervention":true`) {
		t.Fatalf("failed command: %+v", cmds[0])
	}
	if hs := activeHolds(t, th.st); len(hs) != 1 {
		t.Fatalf("holds: %+v", hs)
	}
}

func TestAnalyzeCommand(t *testing.T) {
	cases := []struct {
		cmd, verb, kind, name string
		port                  int
		intervention          bool
	}{
		{"sudo systemctl stop nginx.service", "stop", "service", "nginx", 0, true},
		{"service postgresql restart", "restart", "service", "postgresql", 0, true},
		{"launchctl kickstart -k gui/501/com.example.agent", "kickstart", "service", "com.example.agent", 0, true},
		{"brew services restart redis", "restart", "service", "redis", 0, true},
		{"Stop-Service -Name Spooler", "stop", "service", "Spooler", 0, true},
		{"sc stop W32Time", "stop", "service", "W32Time", 0, true},
		{"pkill -f node", "kill", "process", "node", 0, true},
		{"kill -9 4242", "kill", "process", "pid 4242", 0, true},
		{"sudo fuser -k 80/tcp", "kill", "port", "", 80, true},
		{"docker container stop web", "stop", "container", "web", 0, true},
		{"FOO=1 sudo -E nginx -s reload", "reload", "service", "nginx", 0, true},
		{"sudo reboot", "reboot", "", "", 0, true},
		{"ls -la && systemctl stop x", "", "", "", 0, false},
		{"git status", "", "", "", 0, false},
		{"systemctl status nginx", "", "", "", 0, false},
	}
	for _, c := range cases {
		eff := analyzeCommand(c.cmd)
		if eff.intervention != c.intervention || eff.verb != c.verb {
			t.Errorf("%q: got %+v", c.cmd, eff)
			continue
		}
		if c.kind == "" {
			if eff.resource != nil {
				t.Errorf("%q: unexpected resource %+v", c.cmd, eff.resource)
			}
			continue
		}
		if eff.resource == nil || eff.resource.Kind != c.kind || eff.resource.Name != c.name || eff.resource.Port != c.port {
			t.Errorf("%q: resource %+v", c.cmd, eff.resource)
		}
	}
}

// An agent asks the user to restart nginx; the user does it in a Konsert terminal: the
// request is resolved (no hold) and the agent's tool call returns so it continues.
func TestRequestUserActionResolvedByTerminalCommand(t *testing.T) {
	cs := newChatServer(t)
	cs.h.AskUserTimeout = 10 * time.Second
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)
	chat, open, _ := openChat(t, cs, w, b, nil)
	term := startTask(t, cs.svc, w, userCtx(), "w1", "", func(r *DispatchRequest) { r.SessionKind, r.Mode = "terminal", protocol.TaskModeInteractive })
	tok := strings.TrimPrefix(open.MCPServers[0].Headers["Authorization"], "Bearer ")
	sess, err := connectChatMCP(t, open.MCPServers[0].URL, tok)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	done := make(chan string, 1)
	go func() {
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "request_user_action", Arguments: map[string]any{
			"resource": "nginx", "description": "Restart nginx so it loads the new config (I have no sudo)",
		}})
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		done <- toolText(res)
	}()
	var req store.UserRequest
	waitFor(t, 5*time.Second, "open request", func() bool {
		rs, _ := cs.st.ListUserRequests(context.Background(), store.UserRequestFilter{Status: store.RequestOpen})
		if len(rs) == 1 {
			req = rs[0]
		}
		return len(rs) == 1
	})
	if req.ChatID != chat.ID || req.WorkerID != "w1" || req.Resource == nil || req.Resource.Name != "nginx" {
		t.Fatalf("request: %+v", req)
	}
	// It shows in the chat as a question the user can answer, too.
	el := expectChatEvent(t, b, protocol.EventElicitation)
	if el.Elicitation == nil || el.Elicitation.ID != "req-"+req.ID || !strings.Contains(el.Elicitation.Title, "nginx") {
		t.Fatalf("request elicitation: %+v", el.Elicitation)
	}

	cmd := operatorCmd(term.ID, "sudo systemctl restart nginx", 0)
	w.send(protocol.MsgWorkerOperatorCommand, cmd)
	select {
	case out := <-done:
		if !strings.Contains(out, "The user did it") || !strings.Contains(out, "systemctl restart nginx") || !strings.Contains(out, "Continue") {
			t.Fatalf("tool result: %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request_user_action did not return after the matching command")
	}
	got, _ := cs.st.GetUserRequest(context.Background(), req.ID)
	if got.Status != store.RequestDone || got.CommandID != cmd.CommandID {
		t.Fatalf("resolved request: %+v", got)
	}
	if hs := activeHolds(t, cs.st); len(hs) != 0 {
		t.Fatalf("a requested action placed a hold: %+v", hs)
	}
	evs, _ := cs.st.ListFleetEvents(context.Background(), store.FleetEventFilter{Kind: store.EventKindAction, TaskID: term.ID})
	if len(evs) != 1 || evs[0].Cause.RequestID != req.ID || !strings.Contains(evs[0].Summary, req.SID) {
		t.Fatalf("command not linked to the request: %+v", evs)
	}

	// The user can also decline in the UI.
	go func() {
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "request_user_action", Arguments: map[string]any{
			"description": "Plug in the hardware key",
		}})
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		done <- toolText(res)
	}()
	waitFor(t, 5*time.Second, "second request", func() bool {
		rs, _ := cs.st.ListUserRequests(context.Background(), store.UserRequestFilter{Status: store.RequestOpen})
		if len(rs) == 1 {
			req = rs[0]
		}
		return len(rs) == 1
	})
	code, raw := cs.api(t, http.MethodPost, "/api/requests/"+req.SID+"/resolve", map[string]string{"status": "declined", "note": "not today"})
	if code != 200 {
		t.Fatalf("decline: %d %s", code, raw)
	}
	select {
	case out := <-done:
		if !strings.Contains(out, "declined") || !strings.Contains(out, "not today") {
			t.Fatalf("decline result: %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request_user_action did not return after decline")
	}
}

// User actions reach the orchestrator (a turn starts when its work is affected and it
// is idle) and, as a note before their next prompt, other agent chats on the machine.
func TestEventDeliveryToOrchestratorAndNextTurnNote(t *testing.T) {
	old := deliveryDebounce
	deliveryDebounce = 100 * time.Millisecond
	t.Cleanup(func() { deliveryDebounce = old })
	cs := newChatServer(t)
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)
	var seq atomic.Int64
	finishTurn := func(chatID, turn string) {
		w.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: chatID, Seq: seq.Add(1), Kind: protocol.EventTurnEnd, StopReason: "end_turn", TurnID: turn})
		waitFor(t, 3*time.Second, "turn end", func() bool { return mustSession(t, cs.st, chatID).Status == protocol.ChatStatusDone })
	}

	// The orchestrator chat, idle after a first turn, with work it dispatched on box.
	_, raw := cs.api(t, http.MethodPost, "/api/chats/orchestrator", nil)
	var orch store.ChatSession
	_ = json.Unmarshal(raw, &orch)
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: orch.ID, Content: "deploy the site"})
	p1, _ := protocol.DecodePayload[protocol.AgentChatPrompt](w.expect(protocol.MsgAgentChatPrompt, 3*time.Second))
	finishTurn(orch.ID, p1.TurnID)
	startTask(t, cs.svc, w, orchestratorCtx(orch.ID), "w1", "serve the site with nginx", nil)
	// Another agent chat on the same machine, idle.
	chat := cs.createChat(t, map[string]any{"title": "worker chat"})
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat.ID, Content: "tail the logs"})
	p2, _ := protocol.DecodePayload[protocol.AgentChatPrompt](expectWhere(t, w, protocol.MsgAgentChatPrompt, 3*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.AgentChatPrompt](env)
		return p.ChatID == chat.ID
	}))
	if strings.Contains(p2.Text, eventNotePrefix) {
		t.Fatalf("first prompt carried an old note: %q", p2.Text)
	}
	finishTurn(chat.ID, p2.TurnID)

	// The user stops nginx in a Konsert terminal on box.
	term := startTask(t, cs.svc, w, userCtx(), "w1", "", func(r *DispatchRequest) { r.SessionKind, r.Mode = "terminal", protocol.TaskModeInteractive })
	w.send(protocol.MsgWorkerOperatorCommand, operatorCmd(term.ID, "sudo systemctl stop nginx", 0))

	// (a) The orchestrator gets a turn with a concise system note.
	env := expectWhere(t, w, protocol.MsgAgentChatPrompt, 5*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.AgentChatPrompt](env)
		return p.ChatID == orch.ID
	})
	op, _ := protocol.DecodePayload[protocol.AgentChatPrompt](env)
	if !strings.HasPrefix(op.Text, eventNotePrefix) || !strings.Contains(op.Text, "nginx") || !strings.Contains(op.Text, "Hold") || !strings.Contains(op.Text, "ask") {
		t.Fatalf("orchestrator note: %q", op.Text)
	}
	notices, _ := cs.st.ListFleetEvents(context.Background(), store.FleetEventFilter{Kind: store.EventKindNotice})
	if len(notices) != 1 || notices[0].ChatID != orch.ID {
		t.Fatalf("notice events: %+v", notices)
	}
	finishTurn(orch.ID, op.TurnID)
	// No second turn for the same events.
	if extra := w.drain(protocol.MsgAgentChatPrompt, 400*time.Millisecond); len(extra) != 0 {
		t.Fatalf("orchestrator notified twice: %d", len(extra))
	}

	// (b) The other chat on box is told before its next prompt (once).
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat.ID, Content: "continue"})
	p3, _ := protocol.DecodePayload[protocol.AgentChatPrompt](expectWhere(t, w, protocol.MsgAgentChatPrompt, 3*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.AgentChatPrompt](env)
		return p.ChatID == chat.ID
	}))
	if !strings.HasPrefix(p3.Text, eventNotePrefix) || !strings.Contains(p3.Text, "stopped service nginx") || !strings.HasSuffix(p3.Text, "continue") {
		t.Fatalf("next-turn note: %q", p3.Text)
	}
	finishTurn(chat.ID, p3.TurnID)
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat.ID, Content: "again"})
	p4, _ := protocol.DecodePayload[protocol.AgentChatPrompt](expectWhere(t, w, protocol.MsgAgentChatPrompt, 3*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.AgentChatPrompt](env)
		return p.ChatID == chat.ID
	}))
	if p4.Text != "again" {
		t.Fatalf("note repeated: %q", p4.Text)
	}

	// (c) recent_events from the chat's MCP endpoint explains the change.
	open := decodeOpenFor(t, cs, chat)
	tok := strings.TrimPrefix(open.MCPServers[0].Headers["Authorization"], "Bearer ")
	sess, err := connectChatMCP(t, open.MCPServers[0].URL, tok)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "recent_events", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("recent_events: %v %+v", err, res)
	}
	text := toolText(res)
	if !strings.Contains(text, "stopped service nginx") || !strings.Contains(text, `"actor":"user"`) || !strings.Contains(text, "active_holds") {
		t.Fatalf("recent_events: %s", text)
	}
	// (d) Every managed session carries the operator protocol.
	if !strings.Contains(open.SystemPrompt, "recent_events") || !strings.Contains(open.SystemPrompt, "ask the user") {
		t.Fatalf("system prompt lacks the operator protocol: %q", open.SystemPrompt)
	}
}

// decodeOpenFor builds the agent.chat.open the hub would send for chat (MCP URL + token).
func decodeOpenFor(t *testing.T, cs *chatServer, chat store.ChatSession) protocol.AgentChatOpen {
	t.Helper()
	cur := mustSession(t, cs.st, chat.ID)
	open, err := cs.h.buildChatOpen(context.Background(), cur, cs.h.workerConnFor(cur.WorkerID))
	if err != nil || len(open.MCPServers) == 0 {
		t.Fatalf("build open: %v %+v", err, open)
	}
	return open
}

// release_hold needs the user's confirmation in the chat.
func TestReleaseHoldToolAsksTheUser(t *testing.T) {
	cs := newChatServer(t)
	cs.h.AskUserTimeout = 10 * time.Second
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)
	chat, open, _ := openChat(t, cs, w, b, nil)
	hd, ok := cs.h.holdAfterIntervention(userCtx(), causeFrom(userCtx()), holdSpec{scope: store.HoldScopeMachine, workerID: "w1", summary: "The user stopped everything on box"})
	if !ok {
		t.Fatal("no hold")
	}
	tok := strings.TrimPrefix(open.MCPServers[0].Headers["Authorization"], "Bearer ")
	sess, err := connectChatMCP(t, open.MCPServers[0].URL, tok)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	done := make(chan *mcp.CallToolResult, 1)
	call := func() {
		res, _ := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "release_hold", Arguments: map[string]any{"hold_id": hd.SID}})
		done <- res
	}
	go call()
	el := expectChatEvent(t, b, protocol.EventElicitation)
	b.send(msgChatRespond, chatRespondRequest{SessionID: chat.ID, ElicitationID: el.Elicitation.ID, OptionID: "option_2"}) // "No"
	if res := <-done; res == nil || !res.IsError || !strings.Contains(toolText(res), "did not confirm") {
		t.Fatalf("unconfirmed release: %+v", res)
	}
	if len(activeHolds(t, cs.st)) != 1 {
		t.Fatal("hold released without confirmation")
	}
	go call()
	el = expectChatEvent(t, b, protocol.EventElicitation)
	b.send(msgChatRespond, chatRespondRequest{SessionID: chat.ID, ElicitationID: el.Elicitation.ID, OptionID: "option_1"}) // "Yes"
	if res := <-done; res == nil || res.IsError {
		t.Fatalf("confirmed release: %+v", res)
	}
	if hs := activeHolds(t, cs.st); len(hs) != 0 {
		t.Fatalf("hold not released: %+v", hs)
	}
	rel, _ := cs.st.GetHold(context.Background(), hd.ID)
	if !strings.Contains(rel.ReleaseNote, chat.SID) {
		t.Fatalf("release note: %+v", rel)
	}
}
