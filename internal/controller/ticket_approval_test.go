package controller

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestTicketApprovalModeSelectionAndSandboxCap(t *testing.T) {
	claudeModes := []protocol.AgentApprovalMode{
		{ID: "plan", Risk: 0},
		{ID: "default", Risk: 1},
		{ID: "acceptEdits", Risk: 2},
		{ID: "auto", Risk: 3},
		{ID: "bypassPermissions", Risk: 4},
	}
	claudeSandboxed := []protocol.AgentApprovalMode{
		{ID: "plan", Risk: 0},
		{ID: "default", Risk: 1},
		{ID: "acceptEdits", Risk: 2},
		{ID: "auto", Risk: 3},
		{ID: "bypassPermissions", Risk: 4, Disabled: protocol.SandboxDisabledReason},
	}

	acpModes := &protocol.AgentAccount{Approvals: []protocol.AgentApprovalMode{
		{ID: "default", Risk: 1}, {ID: "accept-edits", Risk: 2}, {ID: "auto", Risk: 3}, {ID: "full-access", Risk: 4},
	}}
	cases := []struct {
		name      string
		agent     string
		sandboxed bool
		acct      *protocol.AgentAccount
		want      string
	}{
		// Claude stays where commands still prompt, so the workdir guard sees them.
		{"claude_full_access", "claude", false, &protocol.AgentAccount{Approvals: claudeModes}, "acceptEdits"},
		{"claude_sandboxed", "claude", true, &protocol.AgentAccount{Approvals: claudeSandboxed}, "acceptEdits"},
		// Codex full-auto is confined by its own OS sandbox.
		{"codex_full_access", "codex", false, nil, "full-auto"},
		{"codex_sandboxed", "codex", true, nil, "full-auto"},
		// ACP agents get the highest mode that still asks before commands (risk <= 2), never auto.
		{"agy_full_access", "agy", false, acpModes, "accept-edits"},
		{"agy_sandboxed", "agy", true, acpModes, "accept-edits"},
		{"grok_unknown_modes", "grok", false, nil, ""},
		{"mock", "mock", false, acpModes, "accept-edits"},
		{
			name:      "sandbox_cap_fallback_on_disabled_mode",
			agent:     "agy",
			sandboxed: true,
			acct: &protocol.AgentAccount{Approvals: []protocol.AgentApprovalMode{
				{ID: "default", Risk: 1},
				{ID: "accept-edits", Risk: 2},
				{ID: "auto", Risk: 4, Disabled: protocol.SandboxDisabledReason},
			}},
			want: "accept-edits",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ticketApprovalMode(tc.agent, tc.sandboxed, tc.acct); got != tc.want {
				t.Fatalf("ticketApprovalMode(%q, sandboxed=%v) = %q, want %q", tc.agent, tc.sandboxed, got, tc.want)
			}
		})
	}
}

func TestBuildChatOpenTicketApprovalAndProjectOverride(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()

	// Register a non-sandboxed worker and a sandboxed worker.
	cs.connectAgentWorker(t, "w-full", "full-box", claudeAccount("claude", true))
	cs.connectAgentWorker(t, "w-sand", "sand-box", claudeAccount("claude", true))
	wSand, err := cs.st.GetWorker(ctx, "w-sand")
	if err != nil {
		t.Fatal(err)
	}
	wSand.PermissionMode = store.PermissionSandboxed
	if err := cs.st.UpsertWorker(ctx, wSand); err != nil {
		t.Fatal(err)
	}

	// Default project (autonomy.approval_mode unset => autonomous).
	if err := cs.st.CreateProject(ctx, store.Project{Name: "AutoProj", Autopilot: true}); err != nil {
		t.Fatal(err)
	}
	// Project with approval_mode = "ask".
	if err := cs.st.CreateProject(ctx, store.Project{
		Name:      "AskProj",
		Autopilot: true,
		Autonomy:  store.AutonomyPolicy{ApprovalMode: store.AutonomyApprovalAsk},
	}); err != nil {
		t.Fatal(err)
	}
	projs, _ := cs.st.ListProjects(ctx)
	var autoProj, askProj store.Project
	for _, p := range projs {
		switch p.Name {
		case "AutoProj":
			autoProj = p
		case "AskProj":
			askProj = p
		}
	}

	tkAuto, err := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: autoProj.ID, Kind: store.TicketKindFeature, Title: "Auto ticket", Status: store.TicketStatusReady})
	if err != nil {
		t.Fatal(err)
	}
	tkAsk, err := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: askProj.ID, Kind: store.TicketKindFeature, Title: "Ask ticket", Status: store.TicketStatusReady})
	if err != nil {
		t.Fatal(err)
	}
	tkAskOverride, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: askProj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Ask project but autonomous ticket",
		Status:    store.TicketStatusReady,
		Autonomy:  store.AutonomyPolicy{ApprovalMode: store.AutonomyApprovalAutonomous},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Non-sandboxed claude ticket chat -> acceptEdits + AutoApproveWorkdir (commands still reach the workdir guard).
	sessFull := store.ChatSession{ID: "c-full", Backend: store.ChatBackendAgent, WorkerID: "w-full", Agent: "claude", ApprovalMode: "default", TicketID: tkAuto.ID}
	if err := cs.st.CreateChatSession(ctx, sessFull); err != nil {
		t.Fatal(err)
	}
	openFull, err := cs.h.buildChatOpen(ctx, sessFull)
	if err != nil {
		t.Fatal(err)
	}
	if openFull.ApprovalMode != "default" || !openFull.AutoApproveWorkdir {
		t.Fatalf("non-sandboxed claude ticket open = (mode=%q, auto=%v), want (default, true): the fixture account offers only default and plan", openFull.ApprovalMode, openFull.AutoApproveWorkdir)
	}

	// Sandboxed claude ticket chat -> acceptEdits (respects sandbox cap) + AutoApproveWorkdir.
	sessSand := store.ChatSession{ID: "c-sand", Backend: store.ChatBackendAgent, WorkerID: "w-sand", Agent: "claude", ApprovalMode: "default", TicketID: tkAuto.ID}
	if err := cs.st.CreateChatSession(ctx, sessSand); err != nil {
		t.Fatal(err)
	}
	openSand, err := cs.h.buildChatOpen(ctx, sessSand)
	if err != nil {
		t.Fatal(err)
	}
	if openSand.ApprovalMode != "default" || !openSand.AutoApproveWorkdir {
		t.Fatalf("sandboxed claude ticket open = (mode=%q, auto=%v), want (default, true): the fixture account offers only default and plan", openSand.ApprovalMode, openSand.AutoApproveWorkdir)
	}

	// Project override approval_mode=ask keeps today's behaviour (default, AutoApproveWorkdir=false).
	sessAsk := store.ChatSession{ID: "c-ask", Backend: store.ChatBackendAgent, WorkerID: "w-full", Agent: "claude", ApprovalMode: "default", TicketID: tkAsk.ID}
	if err := cs.st.CreateChatSession(ctx, sessAsk); err != nil {
		t.Fatal(err)
	}
	openAsk, err := cs.h.buildChatOpen(ctx, sessAsk)
	if err != nil {
		t.Fatal(err)
	}
	if openAsk.ApprovalMode != "default" || openAsk.AutoApproveWorkdir {
		t.Fatalf("ask project ticket open = (mode=%q, auto=%v), want (default, false)", openAsk.ApprovalMode, openAsk.AutoApproveWorkdir)
	}

	// Per-ticket override back to autonomous beats project ask.
	sessOverride := store.ChatSession{ID: "c-ov", Backend: store.ChatBackendAgent, WorkerID: "w-full", Agent: "claude", ApprovalMode: "default", TicketID: tkAskOverride.ID}
	if err := cs.st.CreateChatSession(ctx, sessOverride); err != nil {
		t.Fatal(err)
	}
	openOv, err := cs.h.buildChatOpen(ctx, sessOverride)
	if err != nil {
		t.Fatal(err)
	}
	if openOv.ApprovalMode != "default" || !openOv.AutoApproveWorkdir {
		t.Fatalf("ticket override open = (mode=%q, auto=%v), want (default, true): the fixture account offers only default and plan", openOv.ApprovalMode, openOv.AutoApproveWorkdir)
	}
}

func TestTicketMockPermissionAutonomousVsAskEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX git repo fixtures")
	}

	cs := newChatServer(t)
	ctx := context.Background()

	workdirRoot := t.TempDir()
	repoOrigin := filepath.Join(t.TempDir(), "app-origin")
	initTestGitRepo(t, repoOrigin, "main")

	_, stop := startTicketTestDaemon(t, cs, workdirRoot)
	defer stop()

	// 1. Default (autonomous) project: mock agent requests permission for an in-workdir command
	//    ("permission" keyword -> rm -rf build/) and proceeds straight to review without needs_you.
	if err := cs.st.CreateProject(ctx, store.Project{Name: "AutoPermE2E", Autopilot: true}); err != nil {
		t.Fatal(err)
	}
	if err := cs.st.CreateProject(ctx, store.Project{
		Name:      "AskPermE2E",
		Autopilot: true,
		Autonomy:  store.AutonomyPolicy{ApprovalMode: store.AutonomyApprovalAsk},
	}); err != nil {
		t.Fatal(err)
	}
	projs, _ := cs.st.ListProjects(ctx)
	var autoProj, askProj store.Project
	for _, p := range projs {
		switch p.Name {
		case "AutoPermE2E":
			autoProj = p
		case "AskPermE2E":
			askProj = p
		}
	}

	if err := cs.st.CreateRepo(ctx, store.Repo{ProjectID: autoProj.ID, Name: "app", RemoteURL: repoOrigin, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := cs.st.CreateRepo(ctx, store.Repo{ProjectID: askProj.ID, Name: "app", RemoteURL: repoOrigin, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	autoRepos, _ := cs.st.ListRepos(ctx, autoProj.ID)
	askRepos, _ := cs.st.ListRepos(ctx, askProj.ID)

	tkAuto, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: autoProj.ID,
		Kind:      store.TicketKindChore,
		Title:     "Clean build with permission",
		Status:    store.TicketStatusReady,
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
		TicketID: tkAuto.ID, RepoID: autoRepos[0].ID, BaseBranch: "main", TargetBranch: "feat/auto-perm",
	}); err != nil {
		t.Fatal(err)
	}

	cs.h.SchedulePending(ctx)

	waitFor(t, 15*time.Second, "autonomous ticket with in-workdir permission reaches review", func() bool {
		cur, err := cs.st.GetTicket(ctx, tkAuto.ID)
		return err == nil && cur.Status == store.TicketStatusReview
	})

	// Verify no elicitation / needs_you occurred on the autonomous ticket's chat.
	autoChats, _ := cs.st.ListChatSessionsByTicket(ctx, tkAuto.ID)
	if len(autoChats) != 1 {
		t.Fatalf("expected 1 chat session for tkAuto, got %d", len(autoChats))
	}
	autoEvs, _ := cs.st.ListChatEvents(ctx, autoChats[0].ID, 0, 500)
	for _, ev := range autoEvs {
		if ev.Kind == protocol.EventElicitation {
			t.Fatalf("unexpected elicitation on autonomous in-workdir ticket chat: %s", ev.JSON)
		}
	}

	// 2. Ask project: same in-workdir permission keyword pauses in needs_you.
	tkAsk, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: askProj.ID,
		Kind:      store.TicketKindChore,
		Title:     "Clean build with permission",
		Status:    store.TicketStatusReady,
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
		TicketID: tkAsk.ID, RepoID: askRepos[0].ID, BaseBranch: "main", TargetBranch: "feat/ask-perm",
	}); err != nil {
		t.Fatal(err)
	}

	cs.h.SchedulePending(ctx)

	waitFor(t, 15*time.Second, "ask-mode ticket pauses in needs_you", func() bool {
		cur, err := cs.st.GetTicket(ctx, tkAsk.ID)
		return err == nil && cur.Status == store.TicketStatusNeedsYou
	})
}
