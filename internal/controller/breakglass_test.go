package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// A machine whose policy refuses tasks; a commander asks to suspend the rule.
func breakGlassSetup(t *testing.T) (*chatServer, *fakeWorker) {
	t.Helper()
	cs := newChatServer(t)
	info := testWorkerInfo("w1", "box")
	info.MaxConcurrent = 4
	fw := cs.connectWorker(t, info, nil, "w1")
	setPolicy(t, cs, "w1", protocol.MachinePolicy{AllowedWork: []string{protocol.WorkChat}})
	fw.drain(protocol.MsgWorkerDirectives, 300*time.Millisecond)
	return cs, fw
}

func requestPolicyOverride(t *testing.T, cs *chatServer, minutes int) store.Override {
	t.Helper()
	o, err := cs.h.createOverride(commanderCtx("c-commander"), "c-commander", requestOverrideInput{
		Scope: store.OverrideScopePolicy, Machine: "box", Rules: []string{"allowed_work"},
		DurationMinutes: minutes, Reason: "the release build must run on box tonight",
	})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestPolicyOverrideNeedsApprovalAndExpires(t *testing.T) {
	cs, fw := breakGlassSetup(t)
	ctx := context.Background()

	// Requested, not approved: the rule still holds.
	o := requestPolicyOverride(t, cs, 30)
	if o.Status != store.OverridePending || !strings.Contains(o.Consequences, "allowed_work") {
		t.Fatalf("request: %+v", o)
	}
	createQueuedTask(t, cs.st, "t-bg", WorkerSelector{}, false)
	cs.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgTaskStart, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("dispatched before the user approved the override")
	}
	// The commander cannot decide it, nor turn on its own approvals over MCP.
	if _, err := cs.h.decideOverride(commanderCtx("c-commander"), o.ID, true, ""); err == nil {
		t.Fatal("the commander approved its own override")
	}
	mcpBackend := serverControlPlane{FleetService: cs.svc, srv: cs.srv, admin: true}
	if _, err := mcpBackend.UpdateSettingsMap(ctx, map[string]string{overrideSelfApproveSetting: "true"}); err == nil {
		t.Fatal("override_self_approve was set over MCP")
	}
	if cs.h.overrideSelfApprove(ctx) {
		t.Fatal("self-approval on")
	}

	// The user approves: the hub places the task and the daemon is told the lift.
	if _, err := cs.h.decideOverride(userCtx(), o.ID, true, "ok for tonight"); err != nil {
		t.Fatal(err)
	}
	payload, err := protocol.DecodePayload[protocol.WorkerDirectivesPayload](fw.expect(protocol.MsgWorkerDirectives, 3*time.Second))
	if err != nil || len(payload.PolicyLifts) != 1 || payload.PolicyLifts[0].Rules[0] != "allowed_work" || payload.PolicyLifts[0].OverrideSID != o.SID {
		t.Fatalf("lift pushed: %+v %v", payload, err)
	}
	cs.h.SchedulePending(ctx)
	expectTaskStarts(t, fw, "t-bg")
	if cur, _ := cs.st.GetOverride(ctx, o.ID); cur.UseCount != 1 {
		t.Fatalf("use count %d", cur.UseCount)
	}

	// Past its end the rule applies again, before and after the expiry pass.
	after := time.Now().Add(31 * time.Minute)
	cs.h.setDirectiveClock(func() time.Time { return after })
	createQueuedTask(t, cs.st, "t-late", WorkerSelector{}, false)
	cs.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgTaskStart, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("dispatched after the override's end")
	}
	cs.h.expireOverrides(ctx, after)
	if cur, _ := cs.st.GetOverride(ctx, o.ID); cur.Status != store.OverrideExpired {
		t.Fatalf("status %s", cur.Status)
	}
	payload, _ = protocol.DecodePayload[protocol.WorkerDirectivesPayload](fw.expect(protocol.MsgWorkerDirectives, 3*time.Second))
	if len(payload.PolicyLifts) != 0 {
		t.Fatalf("lift still pushed after expiry: %+v", payload.PolicyLifts)
	}

	// The report: the rule blocked work, an override followed, was approved and used.
	code, raw := cs.api(t, http.MethodGet, "/api/policy/report?days=7", nil)
	if code != http.StatusOK {
		t.Fatalf("report: %d %s", code, raw)
	}
	rep, err := cs.h.policyReport(ctx, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var row *PolicyReportRow
	for i := range rep.Rows {
		if rep.Rows[i].Rule == "allowed_work" && rep.Rows[i].WorkerID == "w1" {
			row = &rep.Rows[i]
		}
	}
	if row == nil || row.Blocks < 2 || row.Requested != 1 || row.Approved != 1 || row.Used != 1 || !row.Followed {
		t.Fatalf("report row: %+v (all %+v)", row, rep.Rows)
	}
	if rep.Totals.Requested != 1 || len(rep.Overrides) != 1 || rep.Overrides[0].Reason == "" {
		t.Fatalf("report totals: %+v", rep.Totals)
	}
}

func TestOverrideSelfApproveSetting(t *testing.T) {
	cs, fw := breakGlassSetup(t)
	ctx := context.Background()
	if code, raw := cs.api(t, http.MethodPut, "/api/settings", map[string]any{"override_self_approve": true}); code != http.StatusOK {
		t.Fatalf("settings: %d %s", code, raw)
	}
	o := requestPolicyOverride(t, cs, 10)
	if o.Status != store.OverrideApproved || !strings.Contains(o.DecidedBy, "commander") {
		t.Fatalf("self-approved: %+v", o)
	}
	createQueuedTask(t, cs.st, "t-self", WorkerSelector{}, false)
	cs.h.SchedulePending(ctx)
	expectTaskStarts(t, fw, "t-self")
	evs, _ := cs.st.ListFleetEvents(ctx, store.FleetEventFilter{Kind: EventKindOverride, Limit: 10})
	found := false
	for _, e := range evs {
		found = found || strings.Contains(e.Summary, "self-approval is on")
	}
	if !found {
		t.Fatal("no fleet event says the commander approved it")
	}
}

func TestPolicyOverrideRulesValidated(t *testing.T) {
	cs, _ := breakGlassSetup(t)
	for _, rules := range [][]string{nil, {"bedtime"}} {
		if _, err := cs.h.createOverride(commanderCtx("c"), "c", requestOverrideInput{
			Scope: store.OverrideScopePolicy, Machine: "box", Rules: rules, DurationMinutes: 5, Reason: "x",
		}); err == nil {
			t.Fatalf("rules %v accepted", rules)
		}
	}
}
