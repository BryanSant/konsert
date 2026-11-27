package controller

import (
	"context"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/worker/guard"
)

func setWorkerGuardFacts(t *testing.T, st store.Store, id, baseMode string) {
	t.Helper()
	ctx := context.Background()
	w, err := st.GetWorker(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(protocol.MachineFacts{GuardrailBaseMode: baseMode, GuardrailMode: baseMode})
	if err != nil {
		t.Fatal(err)
	}
	w.FactsJSON = string(raw)
	if err := st.UpsertWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
}

func TestMachineGuardrailsDirectiveValidation(t *testing.T) {
	th := newTestHub(t)
	upsertBox(t, th.st, "w1", "box")
	if _, err := th.h.CreateDirective(userCtx(), DirectiveInput{Kind: store.DirectiveMachineGuardrails, Target: "w1"}); err == nil {
		t.Fatal("machine.guardrails without a mode accepted")
	}
	if _, err := th.h.CreateDirective(userCtx(), DirectiveInput{Kind: store.DirectiveMachineGuardrails, Target: "w1", Params: store.DirectiveParams{Guardrails: "hermetic"}}); err == nil {
		t.Fatal("unknown guardrail mode accepted")
	}
	d := mustDirective(t, th.h, DirectiveInput{Kind: store.DirectiveMachineGuardrails, Target: "w1", Params: store.DirectiveParams{Guardrails: " Passthrough "}})
	if d.Params.Guardrails != "passthrough" {
		t.Fatalf("mode not normalized: %+v", d.Params)
	}
	if got := th.h.effectiveGuardrails(context.Background(), "w1"); got != "passthrough" {
		t.Fatalf("effective = %q, want the directive's mode", got)
	}
}

func TestSetPolicyGuardrailsTightenFreelyLoosenWithApproval(t *testing.T) {
	th := newTestHub(t)
	upsertBox(t, th.st, "w1", "box")
	setWorkerGuardFacts(t, th.st, "w1", "adaptive")

	loose, err := th.h.SetPolicy(commanderCtx(""), SetPolicyInput{
		Target: "w1", Directive: store.DirectiveMachineGuardrails, Constraints: &PolicyConstraints{Guardrails: "passthrough"},
	})
	if err != nil || !loose.Loosens || loose.Applied != nil {
		t.Fatalf("passthrough below adaptive must need request_override: %+v %v", loose, err)
	}

	tight, err := th.h.SetPolicy(commanderCtx(""), SetPolicyInput{
		Target: "w1", Directive: store.DirectiveMachineGuardrails, Reason: "keeps wedging",
		Constraints: &PolicyConstraints{Guardrails: "adaptive_strict"},
	})
	if err != nil || tight.Loosens || tight.Applied == nil || tight.Applied.Params.Guardrails != "adaptive_strict" {
		t.Fatalf("tightening to adaptive_strict: %+v %v", tight, err)
	}
	// With adaptive_strict now in force, adaptive is a loosening.
	again, err := th.h.SetPolicy(commanderCtx(""), SetPolicyInput{
		Target: "w1", Directive: store.DirectiveMachineGuardrails, Constraints: &PolicyConstraints{Guardrails: "adaptive"},
	})
	if err != nil || !again.Loosens {
		t.Fatalf("adaptive under an adaptive_strict directive must loosen: %+v %v", again, err)
	}
}

func TestThrottleAfterShed(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	upsertBox(t, th.st, "w1", "box") // MaxConcurrent 4

	th.h.throttleAfterShed(ctx, "w1", &protocol.ShedEvidence{ActiveSessions: 3})
	list, err := th.st.ListDirectives(ctx, store.DirectiveFilter{Target: "w1", Kind: store.DirectiveMachineThrottle, ActiveOnly: true, Limit: 10})
	if err != nil || len(list) != 1 {
		t.Fatalf("throttle directives: %+v %v", list, err)
	}
	d := list[0]
	if d.Params.MaxConcurrency == nil || *d.Params.MaxConcurrency != 2 || d.Until == nil || d.Provenance != store.ProvenanceSystem {
		t.Fatalf("throttle = %+v", d)
	}
	if left := time.Until(*d.Until); left < 25*time.Minute || left > 31*time.Minute {
		t.Fatalf("throttle lasts %s, want ~30m", left)
	}
	// A second shed with the cap already at or below what it would file adds nothing.
	th.h.throttleAfterShed(ctx, "w1", &protocol.ShedEvidence{ActiveSessions: 3})
	if list, _ := th.st.ListDirectives(ctx, store.DirectiveFilter{Target: "w1", Kind: store.DirectiveMachineThrottle, ActiveOnly: true, Limit: 10}); len(list) != 1 {
		t.Fatalf("duplicate throttle filed: %+v", list)
	}
	// One session running: a cap of 1 does not tighten a one-session machine.
	upsertBox(t, th.st, "w2", "small")
	w2, _ := th.st.GetWorker(ctx, "w2")
	w2.MaxConcurrent = 1
	_ = th.st.UpsertWorker(ctx, w2)
	th.h.throttleAfterShed(ctx, "w2", &protocol.ShedEvidence{ActiveSessions: 1})
	if list, _ := th.st.ListDirectives(ctx, store.DirectiveFilter{Target: "w2", Kind: store.DirectiveMachineThrottle, ActiveOnly: true, Limit: 10}); len(list) != 0 {
		t.Fatalf("no-op throttle filed: %+v", list)
	}
}

// TestTicketShedRequeuesEndToEnd runs a ticket on a real syred with the mock engine,
// has the guardrails shed it mid-turn, and checks the hub requeues it without failing
// it, throttles the machine, and hands it to the user after the third shed.
func TestTicketShedRequeuesEndToEnd(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	workdirRoot := t.TempDir()
	repoOrigin := filepath.Join(t.TempDir(), "app-origin")
	initTestGitRepo(t, repoOrigin, "main")
	daemon, stop := startTicketTestDaemon(t, cs, workdirRoot)
	defer stop()

	if err := cs.st.CreateProject(ctx, store.Project{Name: "ShedE2E", Autopilot: true}); err != nil {
		t.Fatal(err)
	}
	projs, _ := cs.st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	if err := cs.st.CreateRepo(ctx, store.Repo{ProjectID: proj.ID, Name: "app", RemoteURL: repoOrigin, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	repos, _ := cs.st.ListRepos(ctx, proj.ID)
	// "activity" keeps the mock's turn running for a few seconds.
	tk, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "Long activity build",
		Status: store.TicketStatusReady, Hints: store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repos[0].ID, BaseBranch: "main", TargetBranch: "feat/shed"}); err != nil {
		t.Fatal(err)
	}

	runningChat := func() string {
		cur, err := cs.st.GetTicket(ctx, tk.ID)
		if err != nil || cur.Status != store.TicketStatusRunning {
			return ""
		}
		for _, ci := range daemon.ChatManager().Chats() {
			if ci.TurnActive {
				return ci.ChatID
			}
		}
		return ""
	}
	shed := func(round int) {
		cs.h.SchedulePending(ctx)
		var chatID string
		waitFor(t, 15*time.Second, "ticket running a turn", func() bool {
			chatID = runningChat()
			return chatID != ""
		})
		guard.Announce(guard.Evidence{
			Owner: chatID, Kind: "chat", Reason: guard.ReasonBelowReserve,
			RSS: 3 << 30, HostAvail: 90 << 20, Reserve: 410 << 20, ActiveSessions: 2,
		})
		waitFor(t, 15*time.Second, "shed attempt recorded", func() bool {
			return cs.h.ticketShedCount(ctx, tk.ID) == round
		})
	}

	shed(1)
	waitFor(t, 10*time.Second, "ticket requeued to ready (or re-leased)", func() bool {
		cur, _ := cs.st.GetTicket(ctx, tk.ID)
		return cur.Status == store.TicketStatusReady || cur.Status == store.TicketStatusRunning
	})
	rows, _ := cs.st.ListAttempts(ctx, tk.ID, "")
	if len(rows) == 0 || rows[0].FailureCategory != protocol.ErrKindMemoryPressureShed {
		t.Fatalf("attempts: %+v", rows)
	}
	if got := attemptStatusLabel(rows[0]); got != "requeued/memory_pressure_shed" {
		t.Fatalf("attempt label = %q", got)
	}
	tasks, _ := cs.st.ListTasksByTicket(ctx, tk.ID)
	var dig protocol.RunDigest
	found := false
	for _, task := range tasks {
		if task.DigestJSON == "" {
			continue
		}
		if err := json.Unmarshal([]byte(task.DigestJSON), &dig, protocol.JSONOptions); err == nil && dig.TerminationReason == protocol.TerminationMemoryPressureShed {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no shed digest among %d tasks", len(tasks))
	}
	if dig.HostAvailRAMAtExitMB != 90 || dig.PeakRSSMB < 3072 || !strings.Contains(dig.Summary, "Shed by host memory pressure") {
		t.Fatalf("shed digest = %+v", dig)
	}
	throttles, _ := cs.st.ListDirectives(ctx, store.DirectiveFilter{Target: "worker-ticket-e2e", Kind: store.DirectiveMachineThrottle, ActiveOnly: true, Limit: 10})
	if len(throttles) != 1 || *throttles[0].Params.MaxConcurrency != 1 {
		t.Fatalf("auto throttle = %+v", throttles)
	}

	shed(2)
	shed(3)
	waitFor(t, 10*time.Second, "third shed hands the ticket to the user", func() bool {
		cur, _ := cs.st.GetTicket(ctx, tk.ID)
		return cur.Status == store.TicketStatusNeedsYou && cur.Lease == nil
	})
}
