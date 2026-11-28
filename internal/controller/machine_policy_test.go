package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func setPolicy(t *testing.T, cs *chatServer, machine string, p protocol.MachinePolicy) {
	t.Helper()
	if code, raw := cs.api(t, http.MethodPut, "/api/workers/"+machine+"/policy", p); code != http.StatusOK {
		t.Fatalf("set policy: %d %s", code, raw)
	}
}

func TestMachinePolicyRESTAuditAndPush(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	fw := cs.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	fw.drain(protocol.MsgWorkerDirectives, 200*time.Millisecond) // the hello push

	want := protocol.MachinePolicy{MaxSessions: 2, ReserveRAMMB: 4096, AllowedWork: []string{protocol.WorkChat, protocol.WorkTicket},
		TimeZone: "America/Denver", QuietHours: []protocol.QuietWindow{{Start: "22:00", End: "07:00"}}}
	setPolicy(t, cs, "w1", want)

	// Stored, readable, and on the machine's view (what fleet_status shows).
	code, raw := cs.api(t, http.MethodGet, "/api/workers/w1/policy", nil)
	var got struct {
		Policy protocol.MachinePolicy `json:"policy"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &got) != nil || got.Policy.MaxSessions != 2 || got.Policy.TimeZone != "America/Denver" {
		t.Fatalf("get: %d %s", code, raw)
	}
	w, _ := cs.st.GetWorker(ctx, "w1")
	if v := toWorkerView(w, 0, nil); v.Policy == nil || v.Policy.ReserveRAMMB != 4096 {
		t.Fatalf("worker view policy: %+v", v.Policy)
	}
	// Pushed to the daemon with the directives.
	payload, err := protocol.DecodePayload[protocol.WorkerDirectivesPayload](fw.expect(protocol.MsgWorkerDirectives, 3*time.Second))
	if err != nil || payload.Policy == nil || payload.Policy.MaxSessions != 2 || payload.Policy.ReserveRAMMB != 4096 {
		t.Fatalf("pushed: %+v %v", payload, err)
	}
	// Audited with before and after.
	audits, err := cs.st.ListAudit(ctx, store.AuditFilter{Action: AuditMachinePolicy, Limit: 10})
	if err != nil || len(audits) != 1 || audits[0].WorkerID != "w1" || !strings.Contains(audits[0].MetaJSON, `"after"`) ||
		!strings.Contains(audits[0].MetaJSON, "America/Denver") {
		t.Fatalf("audit: %+v %v", audits, err)
	}

	// Invalid policies are refused and change nothing.
	for _, bad := range []map[string]any{
		{"role": "server"},
		{"quiet_hours": []map[string]any{{"start": "22:00", "end": "07:00"}}}, // no time zone
		{"max_sessions": -1},
	} {
		if code, raw := cs.api(t, http.MethodPut, "/api/workers/w1/policy", bad); code != http.StatusBadRequest {
			t.Fatalf("bad policy %v: %d %s", bad, code, raw)
		}
	}
	// An empty policy clears it, and the daemon is told.
	setPolicy(t, cs, "w1", protocol.MachinePolicy{})
	if w, _ := cs.st.GetWorker(ctx, "w1"); w.PolicyJSON != "" {
		t.Fatalf("cleared policy stored as %q", w.PolicyJSON)
	}
	payload, _ = protocol.DecodePayload[protocol.WorkerDirectivesPayload](fw.expect(protocol.MsgWorkerDirectives, 3*time.Second))
	if payload.Policy != nil {
		t.Fatalf("cleared policy pushed as %+v", payload.Policy)
	}
	if code, _ := cs.api(t, http.MethodPut, "/api/workers/missing/policy", protocol.MachinePolicy{}); code != http.StatusNotFound {
		t.Fatalf("unknown machine: %d", code)
	}
}

func TestSchedulerRespectsMachinePolicy(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	info := testWorkerInfo("w1", "box")
	info.MaxConcurrent = 4
	fw := cs.connectWorker(t, info, nil, "w1")

	// Control-only: nothing is placed there.
	setPolicy(t, cs, "w1", protocol.MachinePolicy{Role: protocol.MachineRoleControlOnly})
	createQueuedTask(t, cs.st, "t-a", WorkerSelector{}, false)
	createQueuedTask(t, cs.st, "t-b", WorkerSelector{}, false)
	cs.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgTaskStart, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("control-only machine received %d tasks", len(got))
	}
	// Chats only: tasks still wait.
	setPolicy(t, cs, "w1", protocol.MachinePolicy{AllowedWork: []string{protocol.WorkChat}})
	cs.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgTaskStart, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("chat-only machine received %d tasks", len(got))
	}

	// At most one session. A set_policy throttle with a higher cap (what the Commander
	// can do over MCP) does not raise it: directives only tighten on top of the policy.
	setPolicy(t, cs, "w1", protocol.MachinePolicy{MaxSessions: 1})
	mustDirective(t, cs.h, DirectiveInput{Kind: store.DirectiveMachineThrottle, Target: "w1",
		Params: store.DirectiveParams{MaxConcurrency: intPtr(3)}})
	cs.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgTaskStart, 500*time.Millisecond); len(got) != 1 {
		t.Fatalf("max_sessions 1 dispatched %d tasks", len(got))
	}

	// Pinned dispatch to a machine whose policy refuses it says why.
	setPolicy(t, cs, "w1", protocol.MachinePolicy{AllowedWork: []string{protocol.WorkTicket}})
	_, err := cs.svc.DispatchTask(userCtx(), DispatchRequest{WorkerSelector: WorkerSelector{WorkerID: "w1"}, Agent: "mock", RepoPath: "/repo", Prompt: "p"})
	var refused *errPolicyRefused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "runs only ticket work") {
		t.Fatalf("pinned dispatch: %v", err)
	}
}

func TestMachinePolicyMemoryAndLoad(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	upsertBox(t, th.st, "w1", "box")
	facts, _ := json.Marshal(protocol.MachineFacts{CPUCores: 8, SurvivalReserveMB: 800})
	w, _ := th.st.GetWorker(ctx, "w1")
	w.FactsJSON = string(facts)
	pol, _ := json.Marshal(protocol.MachinePolicy{ReserveRAMMB: 4000, ReserveCPU: 2})
	w.PolicyJSON = string(pol)

	th.h.mu.Lock()
	th.h.workerMetrics["w1"] = &protocol.WorkerMetrics{MemTotalMB: 16000, MemUsedMB: 12000, LoadAvg: 1}
	th.h.mu.Unlock()
	if v, _ := th.h.policyViolation(ctx, w, protocol.WorkTask, 0, "t"); v == nil || !strings.Contains(v.Message, "4800 MB kept free") {
		t.Fatalf("4000 MB free under a 4800 MB floor: %+v", v)
	}
	th.h.mu.Lock()
	th.h.workerMetrics["w1"] = &protocol.WorkerMetrics{MemTotalMB: 16000, MemUsedMB: 8000, LoadAvg: 6.5}
	th.h.mu.Unlock()
	if v, _ := th.h.policyViolation(ctx, w, protocol.WorkTask, 0, "t"); v == nil || !strings.Contains(v.Message, "cores for the user") {
		t.Fatalf("load 6.5 of 8 cores with 2 reserved: %+v", v)
	}
	th.h.mu.Lock()
	th.h.workerMetrics["w1"] = &protocol.WorkerMetrics{MemTotalMB: 16000, MemUsedMB: 8000, LoadAvg: 3}
	th.h.mu.Unlock()
	if v, _ := th.h.policyViolation(ctx, w, protocol.WorkTask, 0, "t"); v != nil {
		t.Fatalf("refused with room: %s", v.Message)
	}
}
