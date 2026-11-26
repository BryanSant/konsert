package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func readyTicket(t *testing.T, st store.Store, proj store.Project, repo store.Repo, title, agent string) store.Ticket {
	t.Helper()
	tk, err := st.CreateTicket(context.Background(), store.Ticket{
		ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: title,
		Status: store.TicketStatusReady, Hints: store.TicketHints{Agent: agent},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutTicketRepoBinding(context.Background(), store.TicketRepoBinding{TicketID: tk.ID, RepoID: repo.ID}); err != nil {
		t.Fatal(err)
	}
	return tk
}

func accountStamp(t *testing.T, st store.Store, workerID string) time.Time {
	t.Helper()
	recs, err := st.ListAgentAccounts(context.Background(), workerID)
	if err != nil {
		t.Fatal(err)
	}
	var latest time.Time
	for _, r := range recs {
		if r.UpdatedAt.After(latest) {
			latest = r.UpdatedAt
		}
	}
	return latest
}

func providerDirectives(t *testing.T, st store.Store) []store.Directive {
	t.Helper()
	list, err := st.ListDirectives(context.Background(), store.DirectiveFilter{
		Kind: store.DirectiveProviderDisable, ActiveOnly: true, Now: time.Now(), Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestTicketFallbackSubstitution(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	proj, repo := createProjectAndRepo(t, th.st, "Billing", true, false)
	tk := readyTicket(t, th.st, proj, repo, "Rotate tokens", "codex")
	until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	mustDirective(t, th.h, DirectiveInput{
		Kind: store.DirectiveProviderDisable, Target: "codex", Until: &until,
		Params: store.DirectiveParams{Fallback: "claude"},
	})
	info := testWorkerInfo("w-1", "box-1")
	info.Agents = []string{"codex", "claude"}
	fw := th.connectWorker(t, info, nil, "w-1")
	open, err := protocol.DecodePayload[protocol.AgentChatOpen](fw.expect(protocol.MsgAgentChatOpen, 3*time.Second))
	if err != nil || open.TicketSID != tk.SID || open.Agent != "claude" {
		t.Fatalf("fallback dispatch: %+v %v", open, err)
	}
	atts, err := th.st.ListAttempts(ctx, tk.ID, "")
	if err != nil || len(atts) == 0 || !strings.Contains(atts[0].Substitution, "substituted claude for codex") {
		t.Fatalf("attempt substitution: %+v %v", atts, err)
	}
	audits, err := th.st.ListAudit(ctx, store.AuditFilter{Action: AuditPlacementSubstitute, Limit: 10})
	if err != nil || len(audits) != 1 || !strings.Contains(audits[0].Message, "substituted claude for codex") {
		t.Fatalf("audit: %+v %v", audits, err)
	}
}

func TestPinnedTicketWaitsAndPulse(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	proj, repo := createProjectAndRepo(t, th.st, "Billing", true, false)
	tk := readyTicket(t, th.st, proj, repo, "Stay put", "codex")
	until := time.Now().Add(2 * time.Hour).In(time.Local).Truncate(time.Minute)
	mustDirective(t, th.h, DirectiveInput{
		Kind: store.DirectiveProviderDisable, Target: "codex", Until: &until,
	})
	info := testWorkerInfo("w-1", "box-1")
	info.Agents = []string{"codex", "claude"}
	fw := th.connectWorker(t, info, nil, "w-1")
	if got := fw.drain(protocol.MsgAgentChatOpen, 400*time.Millisecond); len(got) != 0 {
		t.Fatal("pinned ticket dispatched while codex was disabled")
	}
	if cur, _ := th.st.GetTicket(ctx, tk.ID); cur.Status != store.TicketStatusReady || cur.Lease != nil {
		t.Fatalf("pinned ticket: %+v", cur)
	}
	pulse := th.h.buildBoardPulse(ctx, nil)
	want := "waiting: codex disabled until " + until.Format("15:04")
	if !strings.Contains(pulse.Text(), want) {
		t.Fatalf("pulse %q, want %q", pulse.Text(), want)
	}

	// A fallback the worker does not have is the same as no fallback.
	th2 := newTestHub(t)
	proj2, repo2 := createProjectAndRepo(t, th2.st, "Billing", true, false)
	tk2 := readyTicket(t, th2.st, proj2, repo2, "No grok", "codex")
	mustDirective(t, th2.h, DirectiveInput{
		Kind: store.DirectiveProviderDisable, Target: "codex", Until: &until,
		Params: store.DirectiveParams{Fallback: "grok"},
	})
	info2 := testWorkerInfo("w-2", "box-2")
	info2.Agents = []string{"codex", "claude"}
	fw2 := th2.connectWorker(t, info2, nil, "w-2")
	if got := fw2.drain(protocol.MsgAgentChatOpen, 400*time.Millisecond); len(got) != 0 {
		t.Fatal("unusable fallback still dispatched")
	}
	if cur, _ := th2.st.GetTicket(ctx, tk2.ID); cur.Status != store.TicketStatusReady {
		t.Fatalf("unusable fallback ticket: %+v", cur)
	}
}

func TestUnpinnedTaskPicksAnotherAgent(t *testing.T) {
	th := newTestHub(t)
	mustDirective(t, th.h, DirectiveInput{Kind: store.DirectiveProviderDisable, Target: "codex"})
	info := testWorkerInfo("w1", "box")
	info.Agents = []string{"claude", "codex"}
	fw := th.connectWorker(t, info, nil, "w1")
	res, err := th.svc.DispatchTask(userCtx(), DispatchRequest{Agent: "codex", RepoPath: "/tmp", Prompt: "x"})
	if err != nil || res.Agent != "claude" || res.Status != protocol.TaskStatusAssigned {
		t.Fatalf("dispatch: %+v %v", res, err)
	}
	start, err := protocol.DecodePayload[protocol.TaskStartPayload](fw.expect(protocol.MsgTaskStart, 3*time.Second))
	if err != nil || start.Agent != "claude" {
		t.Fatalf("task.start: %+v %v", start, err)
	}
}

func TestUsageHeldTaskKeepsRequestedAgent(t *testing.T) {
	th := newTestHub(t)
	resets := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	info := testWorkerInfo("w1", "box")
	info.Agents = []string{"claude", "mock"}
	fw := th.connectWorker(t, info, nil, "w1")
	exhausted := func(id string) protocol.AgentAccount {
		return protocol.AgentAccount{
			ID: id, Agent: "mock", Auth: protocol.AgentAuthStatus{State: protocol.AuthOK},
			Usage: []protocol.AgentRateLimit{{Window: "five_hour", State: "exhausted", ResetsAt: &resets}},
		}
	}
	fw.send(protocol.MsgAgentCapabilities, protocol.AgentCapabilitiesReport{Accounts: []protocol.AgentAccount{exhausted("mock"), exhausted("mock2")}})
	waitFor(t, 3*time.Second, "both accounts disabled", func() bool { return len(providerDirectives(t, th.st)) == 2 })

	res, err := th.svc.DispatchTask(userCtx(), DispatchRequest{Agent: "mock", RepoPath: "/tmp", Prompt: "WAIT_INPUT", Mode: "interactive"})
	if err != nil || res.Agent != "mock" || res.Status != protocol.TaskStatusAssigned {
		t.Fatalf("dispatch: %+v %v", res, err)
	}
	start, err := protocol.DecodePayload[protocol.TaskStartPayload](fw.expect(protocol.MsgTaskStart, 3*time.Second))
	if err != nil || start.Agent != "mock" {
		t.Fatalf("task.start: %+v %v", start, err)
	}
}

func TestAutoPlacementSkipsUsageHeldAgent(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	proj, repo := createProjectAndRepo(t, th.st, "Billing", true, false)
	resets := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	info := testWorkerInfo("w-1", "box-1")
	info.Agents = []string{"claude", "codex"}
	fw := th.connectWorker(t, info, nil, "w-1")
	fw.send(protocol.MsgAgentCapabilities, protocol.AgentCapabilitiesReport{Accounts: []protocol.AgentAccount{
		{
			ID: "cl1", Agent: "claude", ChatSupported: true, Auth: protocol.AgentAuthStatus{State: protocol.AuthOK},
			Usage: []protocol.AgentRateLimit{{Window: "five_hour", State: "exhausted", ResetsAt: &resets}},
		},
		{
			ID: "cx1", Agent: "codex", ChatSupported: true, Auth: protocol.AgentAuthStatus{State: protocol.AuthOK},
		},
	}})
	waitFor(t, 3*time.Second, "claude usage disable", func() bool {
		list := providerDirectives(t, th.st)
		return len(list) == 1 && list[0].Target == "cl1" && list[0].Provenance == store.ProvenanceSystem
	})
	tk := readyTicket(t, th.st, proj, repo, "Any healthy agent", "")
	th.h.SchedulePending(ctx)
	open, err := protocol.DecodePayload[protocol.AgentChatOpen](fw.expect(protocol.MsgAgentChatOpen, 3*time.Second))
	if err != nil || open.TicketSID != tk.SID || open.Agent != "codex" {
		t.Fatalf("auto placement: %+v %v", open, err)
	}
}

func TestPinnedTicketWaitsOnUsageWindow(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	proj, repo := createProjectAndRepo(t, th.st, "Billing", true, false)
	resets := time.Now().Add(2 * time.Hour).In(time.Local).Truncate(time.Minute)
	info := testWorkerInfo("w-1", "box-1")
	info.Agents = []string{"codex", "claude"}
	fw := th.connectWorker(t, info, nil, "w-1")
	fw.send(protocol.MsgAgentCapabilities, protocol.AgentCapabilitiesReport{Accounts: []protocol.AgentAccount{{
		ID: "cx1", Agent: "codex", Auth: protocol.AgentAuthStatus{State: protocol.AuthOK},
		Usage: []protocol.AgentRateLimit{{Window: "five_hour", State: "exhausted", ResetsAt: &resets}},
	}}})
	waitFor(t, 3*time.Second, "usage disable", func() bool {
		list := providerDirectives(t, th.st)
		return len(list) == 1 && list[0].Target == "cx1" && list[0].Provenance == store.ProvenanceSystem
	})
	tk := readyTicket(t, th.st, proj, repo, "Stay on codex", "codex")
	th.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgAgentChatOpen, 400*time.Millisecond); len(got) != 0 {
		t.Fatal("pinned ticket dispatched while its only account was usage-disabled")
	}
	if cur, _ := th.st.GetTicket(ctx, tk.ID); cur.Status != store.TicketStatusReady || cur.Lease != nil {
		t.Fatalf("pinned ticket: %+v", cur)
	}
	pulse := th.h.buildBoardPulse(ctx, nil)
	want := "waiting: codex disabled until " + resets.Format("15:04")
	if !strings.Contains(pulse.Text(), want) {
		t.Fatalf("pulse %q, want %q", pulse.Text(), want)
	}
}

func TestUsageDirectivesCreateDedupeAndEnd(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	fw := th.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	resets := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	noReset := protocol.AgentAccount{ID: "nore", Agent: "claude", Usage: []protocol.AgentRateLimit{{Window: "five_hour", State: "exhausted"}}}
	codex := protocol.AgentAccount{ID: "codex", Agent: "codex", Usage: []protocol.AgentRateLimit{{Window: "five_hour", State: "exhausted", ResetsAt: &resets}}}
	sendCaps := func(accts ...protocol.AgentAccount) {
		t.Helper()
		fw.send(protocol.MsgAgentCapabilities, protocol.AgentCapabilitiesReport{Accounts: accts})
	}
	sendCaps(noReset, codex)
	waitFor(t, 3*time.Second, "system disable", func() bool {
		list := providerDirectives(t, th.st)
		return len(list) == 1 && list[0].Target == "codex" && list[0].Params.Scope == store.DirectiveScopeAccount &&
			list[0].Provenance == store.ProvenanceSystem && list[0].Until != nil && list[0].Until.Unix() == resets.Unix()
	})
	prev := accountStamp(t, th.st, "w1")
	sendCaps(noReset, codex)
	waitFor(t, 3*time.Second, "duplicate report stored", func() bool {
		return accountStamp(t, th.st, "w1").After(prev) && len(providerDirectives(t, th.st)) == 1
	})

	later := resets.Add(time.Hour)
	codex.Usage[0].ResetsAt = &later
	sendCaps(codex)
	waitFor(t, 3*time.Second, "reset moved", func() bool {
		list := providerDirectives(t, th.st)
		return len(list) == 1 && list[0].Until != nil && list[0].Until.Unix() == later.Unix()
	})
	all, err := th.st.ListDirectives(ctx, store.DirectiveFilter{Kind: store.DirectiveProviderDisable, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	endedOld := false
	for _, d := range all {
		if d.Until != nil && d.Until.Unix() == resets.Unix() {
			if d.EndedAt == nil {
				t.Fatalf("old reset still active: %+v", d)
			}
			endedOld = true
		}
	}
	if !endedOld {
		t.Fatalf("old directive was not ended: %+v", all)
	}

	fresh := protocol.AgentAccount{ID: "codex", Agent: "codex", Usage: []protocol.AgentRateLimit{{Window: "five_hour", State: "ok"}}}
	sendCaps(fresh)
	waitFor(t, 3*time.Second, "ended early", func() bool { return len(providerDirectives(t, th.st)) == 0 })

	keep := mustDirective(t, th.h, DirectiveInput{
		Kind: store.DirectiveProviderDisable, Target: "keep-me",
		Params: store.DirectiveParams{Scope: store.DirectiveScopeAccount},
	})
	prev = accountStamp(t, th.st, "w1")
	sendCaps(protocol.AgentAccount{ID: "keep-me", Agent: "claude", Usage: []protocol.AgentRateLimit{{Window: "five_hour", State: "ok"}}})
	waitFor(t, 3*time.Second, "recovery stored", func() bool { return accountStamp(t, th.st, "w1").After(prev) })
	cur, err := th.st.GetDirective(ctx, keep.ID)
	if err != nil || cur.EndedAt != nil || cur.Provenance != store.ProvenanceUser {
		t.Fatalf("user disable ended: %+v %v", cur, err)
	}
}

func TestSetPolicyModesAttributionAndUndo(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	upsertBox(t, th.st, "w1", "box")
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	in := SetPolicyInput{
		Target: "codex", Directive: store.DirectiveProviderDisable, Until: until.Format(time.RFC3339), Reason: "quiet",
		Constraints: &PolicyConstraints{Fallback: "claude"},
	}
	res, err := th.h.SetPolicy(commanderCtx("chat-1"), in)
	if err != nil || res.Loosens || res.Pending != nil || res.Applied == nil {
		t.Fatalf("tighten: %+v %v", res, err)
	}
	d := *res.Applied
	if d.Provenance != store.ProvenanceUser || d.Actor != "local" || d.Params.Fallback != "claude" || !strings.Contains(res.Message, "the user via Commander") {
		t.Fatalf("applied: %+v %q", d, res.Message)
	}
	if got := describeActor(th.h.commanderPolicyCause(commanderCtx(""))); got != "the user via Commander" {
		t.Fatalf("describeActor: %q", got)
	}
	evs, err := th.st.ListFleetEvents(ctx, store.FleetEventFilter{Kind: store.EventKindDirective, Limit: 10})
	if err != nil || len(evs) == 0 || evs[0].Cause.Actor != store.ActorUser || evs[0].Cause.Source != store.SourceCommander || evs[0].Action != "created" {
		t.Fatalf("receipt: %+v %v", evs, err)
	}
	audits, err := th.st.ListAudit(ctx, store.AuditFilter{Action: AuditDirectiveCreate, Limit: 10})
	if err != nil || len(audits) != 1 || audits[0].Cause == nil || audits[0].Cause.Actor != store.ActorUser || audits[0].Cause.Source != store.SourceCommander || audits[0].Cause.ActorID != "local" {
		t.Fatalf("audit cause: %+v %v", audits, err)
	}

	endRes, err := th.h.SetPolicy(commanderCtx(""), SetPolicyInput{Directive: "end", ID: d.SID})
	if err != nil || !endRes.Loosens || !strings.Contains(endRes.Message, "request_override") || endRes.Pending != nil {
		t.Fatalf("end loosen: %+v %v", endRes, err)
	}
	if cur, _ := th.st.GetDirective(ctx, d.ID); cur.EndedAt != nil {
		t.Fatal("end applied the loosen")
	}
	if _, err := th.h.SetPolicy(commanderCtx(""), SetPolicyInput{Directive: "end"}); err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Fatalf("missing id: %v", err)
	}
	ended, err := th.h.EndDirective(userCtx(), d.ID)
	if err != nil || ended.EndedAt == nil || ended.EndedBy != "tester" {
		t.Fatalf("undo: %+v %v", ended, err)
	}
	endAudits, err := th.st.ListAudit(ctx, store.AuditFilter{Action: AuditDirectiveEnd, Limit: 10})
	if err != nil || len(endAudits) != 1 || endAudits[0].Cause == nil || endAudits[0].Cause.Actor != store.ActorUser || endAudits[0].Cause.ActorID != "tester" {
		t.Fatalf("undo cause: %+v %v", endAudits, err)
	}

	if err := th.st.SetSettings(ctx, map[string]string{commanderPolicyWritesSetting: policyWritesConfirm}); err != nil {
		t.Fatal(err)
	}
	pending, err := th.h.SetPolicy(commanderCtx("chat-9"), in)
	if err != nil || pending.Applied != nil || pending.Pending == nil || pending.Pending.Status != store.PolicyProposalPending || pending.Pending.ChatID != "chat-9" || pending.Pending.ActorID != "local" {
		t.Fatalf("confirm: %+v %v", pending, err)
	}
	if list := providerDirectives(t, th.st); len(list) != 0 {
		t.Fatalf("confirm applied early: %+v", list)
	}
	// Ending still asks for request_override and does not file a card.
	active := mustDirective(t, th.h, DirectiveInput{Kind: store.DirectiveMachinePause, Target: "w1"})
	loose, err := th.h.SetPolicy(commanderCtx(""), SetPolicyInput{Directive: "end", ID: active.SID})
	if err != nil || !loose.Loosens || loose.Pending != nil || !strings.Contains(loose.Message, "request_override") {
		t.Fatalf("confirm end: %+v %v", loose, err)
	}
	props, err := th.st.ListPolicyProposals(ctx, store.PolicyProposalFilter{Status: store.PolicyProposalPending})
	if err != nil || len(props) != 1 {
		t.Fatalf("proposals: %+v %v", props, err)
	}
	approved, err := th.h.ApprovePolicyProposal(userCtx(), pending.Pending.SID)
	if err != nil || approved.Provenance != store.ProvenanceUser || approved.Actor != "local" || approved.Kind != store.DirectiveProviderDisable {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	deniedIn, err := th.h.SetPolicy(commanderCtx(""), SetPolicyInput{Target: "grok", Directive: store.DirectiveProviderDisable})
	if err != nil || deniedIn.Pending == nil {
		t.Fatalf("second proposal: %+v %v", deniedIn, err)
	}
	denied, err := th.h.DenyPolicyProposal(userCtx(), deniedIn.Pending.ID)
	if err != nil || denied.Status != store.PolicyProposalDenied {
		t.Fatalf("deny: %+v %v", denied, err)
	}
	if _, err := th.h.EndDirective(userCtx(), approved.ID); err != nil {
		t.Fatal(err)
	}

	if err := th.st.SetSettings(ctx, map[string]string{commanderPolicyWritesSetting: policyWritesNone}); err != nil {
		t.Fatal(err)
	}
	if _, err := th.h.SetPolicy(commanderCtx(""), in); err == nil || !strings.Contains(err.Error(), "commander_policy_writes is none") || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("none: %v", err)
	}

	if err := th.st.SetSettings(ctx, map[string]string{commanderPolicyWritesSetting: policyWritesTighten}); err != nil {
		t.Fatal(err)
	}
	high := 8
	raised, err := th.h.SetPolicy(commanderCtx(""), SetPolicyInput{
		Target: "box", Directive: store.DirectiveMachineThrottle, Constraints: &PolicyConstraints{MaxConcurrency: &high},
	})
	if err != nil || !raised.Loosens || !strings.Contains(raised.Message, "request_override") || raised.Applied != nil {
		t.Fatalf("raise cap: %+v %v", raised, err)
	}
	low := 1
	capped, err := th.h.SetPolicy(commanderCtx(""), SetPolicyInput{
		Target: "box", Directive: store.DirectiveMachineThrottle, Constraints: &PolicyConstraints{MaxConcurrency: &low},
	})
	if err != nil || capped.Applied == nil || capped.Applied.Target != "w1" || capped.Applied.Params.MaxConcurrency == nil || *capped.Applied.Params.MaxConcurrency != 1 {
		t.Fatalf("lower cap: %+v %v", capped, err)
	}
}
