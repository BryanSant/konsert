package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// orchestratorSetup connects an agent worker (slot limit maxConcurrent), opens a browser
// and creates the orchestrator chat, idle after a first turn. finish ends a chat turn
// normally and waits until the hub has applied it.
type orchestratorSetup struct {
	cs     *chatServer
	w, b   *fakeWorker
	orch   store.ChatSession
	finish func(chatID, turnID string)
}

func newOrchestratorSetup(t *testing.T, maxConcurrent int) orchestratorSetup {
	t.Helper()
	old := deliveryDebounce
	deliveryDebounce = 50 * time.Millisecond
	t.Cleanup(func() { deliveryDebounce = old })
	cs := newChatServer(t)
	info := testWorkerInfo("w1", "box")
	info.MaxConcurrent = maxConcurrent
	w := cs.connectWorker(t, info, nil, "w1")
	w.send(protocol.MsgAgentCapabilities, protocol.AgentCapabilitiesReport{Accounts: []protocol.AgentAccount{claudeAccount("claude", true)}})
	waitFor(t, 3*time.Second, "accounts stored", func() bool {
		recs, _ := cs.st.ListAgentAccounts(context.Background(), "w1")
		return len(recs) == 1
	})
	b := cs.dialBrowser(t)
	var seq atomic.Int64
	finish := func(chatID, turn string) {
		w.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: chatID, Seq: seq.Add(1), Kind: protocol.EventTurnEnd, StopReason: "end_turn", TurnID: turn})
		waitFor(t, 3*time.Second, "turn end", func() bool { return mustSession(t, cs.st, chatID).Status == protocol.ChatStatusDone })
	}
	_, raw := cs.api(t, http.MethodPost, "/api/chats/orchestrator", nil)
	var orch store.ChatSession
	if err := json.Unmarshal(raw, &orch); err != nil || orch.ID == "" {
		t.Fatalf("orchestrator chat: %v %s", err, raw)
	}
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: orch.ID, Content: "keep the fleet busy"})
	p1, _ := protocol.DecodePayload[protocol.AgentChatPrompt](w.expect(protocol.MsgAgentChatPrompt, 3*time.Second))
	finish(orch.ID, p1.TurnID)
	return orchestratorSetup{cs: cs, w: w, b: b, orch: orch, finish: finish}
}

// nextOrchestratorPrompt waits for the next prompt Konsert sends the orchestrator.
func (o orchestratorSetup) nextOrchestratorPrompt(t *testing.T) protocol.AgentChatPrompt {
	t.Helper()
	env := expectWhere(t, o.w, protocol.MsgAgentChatPrompt, 5*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.AgentChatPrompt](env)
		return p.ChatID == o.orch.ID
	})
	p, _ := protocol.DecodePayload[protocol.AgentChatPrompt](env)
	return p
}

func mustOverride(t *testing.T, st store.Store, id string) store.Override {
	t.Helper()
	o, err := st.GetOverride(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func mustVeto(t *testing.T, st store.Store) store.Veto {
	t.Helper()
	vs, err := st.ListVetoes(context.Background(), store.VetoFilter{})
	if err != nil || len(vs) != 1 {
		t.Fatalf("vetoes: %+v %v", vs, err)
	}
	return vs[0]
}

func dispatchOn(t *testing.T, cs *chatServer, ctx context.Context, prompt string) (DispatchResult, error) {
	t.Helper()
	return cs.svc.DispatchTask(ctx, DispatchRequest{WorkerSelector: WorkerSelector{WorkerID: "w1"}, Agent: "mock", RepoPath: "/tmp", Prompt: prompt, Mode: protocol.TaskModeOneshot})
}

// The orchestrator asks for a 2nd session on a one-slot machine; once the user approves,
// the queued task is placed (enforced), the use is counted (measured), the orchestrator
// is told, and when the override expires the limit is back and the orchestrator is told.
func TestOverrideSlotsRequestedEnforcedMeasuredAndExpired(t *testing.T) {
	o := newOrchestratorSetup(t, 1)
	cs, w := o.cs, o.w
	octx := orchestratorCtx(o.orch.ID)
	startTask(t, cs.svc, w, octx, "w1", "build the site", nil)
	second, err := dispatchOn(t, cs, octx, "run the e2e suite")
	if err != nil || second.Status != protocol.TaskStatusQueued {
		t.Fatalf("second task on a full machine: %+v %v", second, err)
	}

	// request_override from the orchestrator's MCP endpoint returns at once.
	open := decodeOpenFor(t, cs, o.orch)
	sess, err := connectChatMCP(t, w.mcpURL(open.MCPServers[0]), strings.TrimPrefix(open.MCPServers[0].Headers["Authorization"], "Bearer "))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "request_override", Arguments: map[string]any{
		"scope": "machine_slots", "machine": "box", "duration_minutes": 30, "reason": "The e2e suite must run next to the build to finish before the release",
	}})
	if err != nil || res.IsError {
		t.Fatalf("request_override: %v %+v", err, res)
	}
	if txt := toolText(res); !strings.Contains(txt, "o-") || !strings.Contains(txt, "Needs you") || !strings.Contains(txt, "Don't act as if it were approved") {
		t.Fatalf("request_override reply: %s", txt)
	}
	code, raw := cs.api(t, http.MethodGet, "/api/overrides?status=pending", nil)
	var list struct {
		Overrides []store.Override `json:"overrides"`
	}
	if err := json.Unmarshal(raw, &list); code != 200 || err != nil || len(list.Overrides) != 1 {
		t.Fatalf("pending overrides: %d %s", code, raw)
	}
	ov := list.Overrides[0]
	if ov.ChatID != o.orch.ID || ov.ScopeKind != store.OverrideScopeMachineSlots || ov.WorkerID != "w1" || ov.ExtraSlots != 1 ||
		ov.DurationSeconds != 1800 || !strings.Contains(ov.Consequences, "up to 2 sessions at once instead of 1") || ov.Status != store.OverridePending {
		t.Fatalf("override: %+v", ov)
	}
	// Bad requests are refused with a reason the agent can act on.
	res, _ = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "request_override", Arguments: map[string]any{
		"scope": "machine_slots", "machine": "box", "duration_minutes": 0, "reason": "x",
	}})
	if res == nil || !res.IsError || !strings.Contains(toolText(res), "duration_minutes") {
		t.Fatalf("zero duration accepted: %+v", res)
	}

	// The user approves: the queued task is placed and the use is measured.
	if code, raw := cs.api(t, http.MethodPost, "/api/overrides/"+ov.SID+"/approve", map[string]string{"note": "ok, just today"}); code != 200 {
		t.Fatalf("approve: %d %s", code, raw)
	}
	expectWhere(t, w, protocol.MsgTaskStart, 3*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.TaskStartPayload](env)
		return p.TaskID == second.TaskID
	})
	waitFor(t, 3*time.Second, "override used", func() bool { return mustOverride(t, cs.st, ov.ID).UseCount == 1 })
	ov = mustOverride(t, cs.st, ov.ID)
	if ov.Status != store.OverrideApproved || ov.ApprovedAt == nil || ov.ExpiresAt == nil || ov.FirstUsedAt == nil ||
		ov.ExpiresAt.Sub(*ov.ApprovedAt) != 30*time.Minute || ov.DecisionNote != "ok, just today" {
		t.Fatalf("approved override: %+v", ov)
	}
	if code, _ := cs.api(t, http.MethodPost, "/api/overrides/"+ov.SID+"/deny", nil); code != http.StatusConflict {
		t.Fatalf("deciding twice: %d", code)
	}
	// The orchestrator is told the decision in a turn of its own.
	p := o.nextOrchestratorPrompt(t)
	if !strings.Contains(p.Text, "approved override "+ov.SID) || !strings.Contains(p.Text, "ok, just today") {
		t.Fatalf("approval note: %q", p.Text)
	}
	o.finish(o.orch.ID, p.TurnID)

	// It expires: the orchestrator hears it, with the usage, and the limit is back.
	cs.h.sweepControl(context.Background(), ov.ExpiresAt.Add(time.Second))
	ov = mustOverride(t, cs.st, ov.ID)
	if ov.Status != store.OverrideExpired || ov.EndedAt == nil || ov.UseCount != 1 {
		t.Fatalf("expired override: %+v", ov)
	}
	p = o.nextOrchestratorPrompt(t)
	if !strings.Contains(p.Text, "Override "+ov.SID) || !strings.Contains(p.Text, "expired") || !strings.Contains(p.Text, "used 1 time") {
		t.Fatalf("expiry note: %q", p.Text)
	}
	o.finish(o.orch.ID, p.TurnID)
	w.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: second.TaskID, Status: protocol.TaskStatusCompleted})
	waitFor(t, 3*time.Second, "second done", func() bool { return mustTask(t, cs.st, second.TaskID).Status == protocol.TaskStatusCompleted })
	third, err := dispatchOn(t, cs, octx, "lint")
	if err != nil || third.Status != protocol.TaskStatusQueued {
		t.Fatalf("third task after expiry (one slot, one running): %+v %v", third, err)
	}
	evs, _ := cs.st.ListFleetEvents(context.Background(), store.FleetEventFilter{Kind: EventKindOverride})
	var actions []string
	for i := len(evs) - 1; i >= 0; i-- {
		actions = append(actions, evs[i].Action)
	}
	if strings.Join(actions, ",") != "created,approved,used,expired" {
		t.Fatalf("override events: %v", actions)
	}
}

// A hold override: denied (with a note, reported back) leaves the hold blocking;
// approved lets the orchestrator through until it expires; the hold itself stays.
func TestOverrideHoldDeniedThenApprovedAndExpired(t *testing.T) {
	cs := newChatServer(t)
	w := cs.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	hd, ok := cs.h.holdAfterIntervention(userCtx(), causeFrom(userCtx()), holdSpec{scope: store.HoldScopeMachine, workerID: "w1", summary: "The user stopped everything on box"})
	if !ok {
		t.Fatal("no hold")
	}
	octx := orchestratorCtx("orch")
	if _, err := dispatchOn(t, cs, octx, "deploy"); err == nil || !strings.Contains(err.Error(), "on hold") {
		t.Fatalf("dispatch on held machine: %v", err)
	}
	create := func() store.Override {
		t.Helper()
		code, raw := cs.api(t, http.MethodPost, "/api/overrides", map[string]any{"scope": "hold", "hold_id": hd.SID, "duration_minutes": 15, "reason": "hotfix must ship"})
		var o store.Override
		if err := json.Unmarshal(raw, &o); code != 200 || err != nil {
			t.Fatalf("create override: %d %s", code, raw)
		}
		if o.HoldID != hd.ID || o.WorkerID != "w1" || !strings.Contains(o.Consequences, hd.SID) || !strings.HasPrefix(o.SID, "o-") {
			t.Fatalf("hold override: %+v", o)
		}
		return o
	}
	for _, bad := range []map[string]any{
		{"scope": "bogus", "duration_minutes": 5, "reason": "x"},
		{"scope": "hold", "hold_id": "h-zzzzz", "duration_minutes": 5, "reason": "x"},
		{"scope": "machine_slots", "machine": "box", "duration_minutes": 5},
		{"scope": "machine_slots", "machine": "box", "duration_minutes": 5000, "reason": "x"},
	} {
		if code, raw := cs.api(t, http.MethodPost, "/api/overrides", bad); code != http.StatusBadRequest {
			t.Fatalf("bad override %v accepted: %d %s", bad, code, raw)
		}
	}

	denied := create()
	code, raw := cs.api(t, http.MethodPost, "/api/overrides/"+denied.SID+"/deny", map[string]string{"note": "not on a Friday"})
	if code != 200 || !strings.Contains(string(raw), `"status":"denied"`) {
		t.Fatalf("deny: %d %s", code, raw)
	}
	if _, err := dispatchOn(t, cs, octx, "deploy"); err == nil {
		t.Fatal("denied override let the dispatch through")
	}
	evs, _ := cs.st.ListFleetEvents(context.Background(), store.FleetEventFilter{Kind: EventKindOverride, Limit: 1})
	if len(evs) != 1 || evs[0].Action != store.OverrideDenied || !strings.Contains(evs[0].Summary, "not on a Friday") || evs[0].Cause.Actor != store.ActorUser {
		t.Fatalf("denied event: %+v", evs)
	}

	approved := create()
	if code, raw := cs.api(t, http.MethodPost, "/api/overrides/"+approved.SID+"/approve", nil); code != 200 {
		t.Fatalf("approve: %d %s", code, raw)
	}
	res, err := dispatchOn(t, cs, octx, "deploy")
	if err != nil {
		t.Fatalf("dispatch with an approved override: %v", err)
	}
	expectWhere(t, w, protocol.MsgTaskStart, 3*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.TaskStartPayload](env)
		return p.TaskID == res.TaskID
	})
	if got := mustOverride(t, cs.st, approved.ID); got.UseCount != 1 {
		t.Fatalf("use not measured: %+v", got)
	}
	// The user's own dispatch never needs (or counts) the override.
	if _, err := dispatchOn(t, cs, userCtx(), "look around"); err != nil {
		t.Fatal(err)
	}
	if got := mustOverride(t, cs.st, approved.ID); got.UseCount != 1 {
		t.Fatalf("user dispatch counted as a use: %+v", got)
	}

	cs.h.sweepControl(context.Background(), mustOverride(t, cs.st, approved.ID).ExpiresAt.Add(time.Second))
	if _, err := dispatchOn(t, cs, octx, "deploy again"); err == nil || !strings.Contains(err.Error(), hd.SID) {
		t.Fatalf("dispatch after expiry: %v", err)
	}
	if hs := activeHolds(t, cs.st); len(hs) != 1 || hs[0].ID != hd.ID {
		t.Fatalf("hold released by the override: %+v", hs)
	}
}

func TestOverrideLifts(t *testing.T) {
	session := store.Hold{ID: "h1", ScopeKind: store.HoldScopeSession, WorkerID: "w1", TaskID: "t1"}
	machine := store.Hold{ID: "h2", ScopeKind: store.HoldScopeMachine, WorkerID: "w1"}
	resource := store.Hold{ID: "h3", ScopeKind: store.HoldScopeResource, WorkerID: "w1", Resource: &store.Resource{Kind: "service", Name: "nginx"}}
	cases := []struct {
		o    store.Override
		hd   store.Hold
		want bool
	}{
		{store.Override{ScopeKind: store.OverrideScopeHold, HoldID: "h2"}, machine, true},
		{store.Override{ScopeKind: store.OverrideScopeHold, HoldID: "h2"}, resource, false},
		{store.Override{ScopeKind: store.OverrideScopeSession, TaskID: "t1"}, session, true},
		{store.Override{ScopeKind: store.OverrideScopeSession, TaskID: "t2"}, session, false},
		{store.Override{ScopeKind: store.OverrideScopeSession, TaskID: "t1"}, machine, false},
		{store.Override{ScopeKind: store.OverrideScopeMachine, WorkerID: "w1"}, machine, true},
		{store.Override{ScopeKind: store.OverrideScopeMachine, WorkerID: "w1"}, resource, true},
		{store.Override{ScopeKind: store.OverrideScopeMachine, WorkerID: "w1"}, session, false},
		{store.Override{ScopeKind: store.OverrideScopeMachine, WorkerID: "w2"}, machine, false},
		{store.Override{ScopeKind: store.OverrideScopeMachineSlots, WorkerID: "w1"}, machine, false},
	}
	for i, c := range cases {
		if got := overrideLifts(c.o, c.hd); got != c.want {
			t.Errorf("case %d: overrideLifts(%+v, %s) = %v, want %v", i, c.o, c.hd.ID, got, c.want)
		}
	}
	for secs, want := range map[int]string{60: "1m", 1800: "30m", 3600: "1h", 5400: "1h30m"} {
		if got := formatMinutes(secs); got != want {
			t.Errorf("formatMinutes(%d) = %q, want %q", secs, got, want)
		}
	}
}

// A veto is delivered into the orchestrator's next turn; that turn ending normally is the
// acknowledgement, and the veto resolves itself.
func TestVetoAcknowledgedOnTheAgentsNextTurn(t *testing.T) {
	o := newOrchestratorSetup(t, 4)
	cs := o.cs
	task := startTask(t, cs.svc, o.w, orchestratorCtx(o.orch.ID), "w1", "deploy to prod from the laptop", nil)
	if code, raw := cs.api(t, http.MethodPost, "/api/tasks/"+task.SID+"/veto", map[string]string{"reason": "wrong_machine", "note": "use the build box"}); code != 200 {
		t.Fatalf("veto: %d %s", code, raw)
	}
	v := mustVeto(t, cs.st)
	if !v.Open() || v.TaskID != task.ID || v.ChatID != o.orch.ID || v.Reason != "wrong_machine" || v.Note != "use the build box" || !strings.HasPrefix(v.SID, "v-") {
		t.Fatalf("tracked veto: %+v", v)
	}

	// The veto's cancel and hold are recorded first and may start a turn of their own before
	// the veto event exists; the veto then rides in the next turn. Finish earlier turns until
	// the prompt carrying the veto arrives.
	p := o.nextOrchestratorPrompt(t)
	for i := 0; i < 3 && !strings.Contains(p.Text, "vetoed"); i++ {
		o.finish(o.orch.ID, p.TurnID)
		p = o.nextOrchestratorPrompt(t)
	}
	if !strings.Contains(p.Text, "vetoed") || !strings.Contains(p.Text, "use the build box") {
		t.Fatalf("veto note: %q", p.Text)
	}
	v = mustVeto(t, cs.st)
	if v.DeliveredChatID != o.orch.ID || v.DeliveredTurnID != p.TurnID || v.DeliveredAt == nil || v.AckedAt != nil {
		t.Fatalf("delivered veto: %+v", v)
	}
	code, raw := cs.api(t, http.MethodGet, "/api/vetoes?open=1", nil)
	if code != 200 || !strings.Contains(string(raw), `"status":"delivered"`) || !strings.Contains(string(raw), "the orchestrator (chat "+o.orch.SID+")") {
		t.Fatalf("open vetoes: %d %s", code, raw)
	}

	o.finish(o.orch.ID, p.TurnID)
	waitFor(t, 3*time.Second, "veto acknowledged", func() bool { return mustVeto(t, cs.st).AckedAt != nil })
	if v = mustVeto(t, cs.st); v.AckedChatID != o.orch.ID {
		t.Fatalf("acknowledged veto: %+v", v)
	}
	if _, raw := cs.api(t, http.MethodGet, "/api/vetoes?open=1", nil); !strings.Contains(string(raw), `"vetoes":[]`) {
		t.Fatalf("acknowledged veto still open: %s", raw)
	}
	// The notice is recorded just after the acknowledgement is stored.
	waitFor(t, 3*time.Second, "veto_ack notice", func() bool {
		notices, _ := cs.st.ListFleetEvents(context.Background(), store.FleetEventFilter{Kind: store.EventKindNotice})
		for _, e := range notices {
			if e.Action == "veto_ack" && strings.Contains(e.Summary, v.SID) {
				return true
			}
		}
		return false
	})
	// An acknowledged veto never escalates.
	cs.h.sweepControl(context.Background(), time.Now().Add(time.Hour))
	if v = mustVeto(t, cs.st); v.EscalatedAt != nil {
		t.Fatalf("acknowledged veto escalated: %+v", v)
	}
}

// A veto the orchestrator can't take yet (mid-turn) escalates after the configured
// deadline; a turn that ends in error is no acknowledgement; the user can dismiss it.
func TestVetoEscalatesWhenNotAcknowledgedAndCanBeDismissed(t *testing.T) {
	o := newOrchestratorSetup(t, 4)
	cs := o.cs
	if code, raw := cs.api(t, http.MethodPut, "/api/settings", map[string]any{"veto_ack_minutes": 2}); code != 200 {
		t.Fatalf("settings: %d %s", code, raw)
	}
	if code, _ := cs.api(t, http.MethodPut, "/api/settings", map[string]any{"veto_ack_minutes": 0}); code != http.StatusBadRequest {
		t.Fatalf("zero ack minutes accepted: %d", code)
	}
	// The orchestrator is busy with a turn of its own.
	o.b.send(protocol.MsgChatSend, chatSendRequest{SessionID: o.orch.ID, Content: "plan the release"})
	busy := o.nextOrchestratorPrompt(t)
	task := startTask(t, cs.svc, o.w, userCtx(), "w1", "rewrite the parser", nil)
	if code, raw := cs.api(t, http.MethodPost, "/api/tasks/"+task.ID+"/veto", map[string]string{"reason": "wrong_approach"}); code != 200 {
		t.Fatalf("veto: %d %s", code, raw)
	}
	v := mustVeto(t, cs.st)
	code, raw := cs.api(t, http.MethodGet, "/api/vetoes?open=1", nil)
	var resp struct {
		Vetoes     []VetoView `json:"vetoes"`
		AckMinutes int        `json:"ack_minutes"`
	}
	if err := json.Unmarshal(raw, &resp); code != 200 || err != nil || len(resp.Vetoes) != 1 || resp.AckMinutes != 2 ||
		resp.Vetoes[0].Status != VetoSent || !resp.Vetoes[0].AckDeadline.Equal(v.CreatedAt.Add(2*time.Minute)) {
		t.Fatalf("open vetoes: %d %s", code, raw)
	}

	cs.h.sweepControl(context.Background(), v.CreatedAt.Add(time.Minute))
	if mustVeto(t, cs.st).EscalatedAt != nil {
		t.Fatal("escalated before the deadline")
	}
	// The busy turn ends in error: no acknowledgement (the veto wasn't in it anyway).
	o.w.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: o.orch.ID, Seq: 900, Kind: protocol.EventTurnEnd, StopReason: "error", TurnID: busy.TurnID})
	waitFor(t, 3*time.Second, "turn failed", func() bool { return mustSession(t, cs.st, o.orch.ID).Status == protocol.ChatStatusFailed })
	if mustVeto(t, cs.st).AckedAt != nil {
		t.Fatal("a failed turn acknowledged the veto")
	}
	cs.h.sweepControl(context.Background(), v.CreatedAt.Add(2*time.Minute+time.Second))
	v = mustVeto(t, cs.st)
	if v.EscalatedAt == nil || !v.Open() {
		t.Fatalf("veto not escalated: %+v", v)
	}
	notices, _ := cs.st.ListFleetEvents(context.Background(), store.FleetEventFilter{Kind: store.EventKindNotice})
	found := false
	for _, e := range notices {
		found = found || (e.Action == "veto_unacknowledged" && strings.Contains(e.Summary, v.SID) && strings.Contains(e.Summary, "2m"))
	}
	if !found {
		t.Fatalf("no escalation notice: %+v", notices)
	}

	code, raw = cs.api(t, http.MethodPost, "/api/vetoes/"+v.SID+"/dismiss", nil)
	if code != 200 || !strings.Contains(string(raw), `"status":"dismissed"`) {
		t.Fatalf("dismiss: %d %s", code, raw)
	}
	if v = mustVeto(t, cs.st); v.Open() || v.DismissedBy == "" {
		t.Fatalf("dismissed veto: %+v", v)
	}
}

// With nobody to tell (no orchestrator, a task no chat dispatched) a veto is closed at
// once instead of escalating.
func TestVetoWithNoAgentToTellClosesAtOnce(t *testing.T) {
	cs := newChatServer(t)
	w := cs.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	task := startTask(t, cs.svc, w, userCtx(), "w1", "deploy", nil)
	if code, raw := cs.api(t, http.MethodPost, "/api/tasks/"+task.ID+"/veto", map[string]string{"reason": "not_now"}); code != 200 {
		t.Fatalf("veto: %d %s", code, raw)
	}
	v := mustVeto(t, cs.st)
	if v.Open() || v.DismissedBy != vetoNoAgent {
		t.Fatalf("veto without recipient: %+v", v)
	}
	cs.h.sweepControl(context.Background(), time.Now().Add(time.Hour))
	if v = mustVeto(t, cs.st); v.EscalatedAt != nil {
		t.Fatalf("escalated: %+v", v)
	}
}
