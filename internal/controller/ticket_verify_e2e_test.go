package controller

import (
	"context"
	"encoding/json/v2"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestFormatVerifyRepairPromptTokenBudget(t *testing.T) {
	var longTail []string
	for i := 0; i < 40; i++ {
		longTail = append(longTail, "pkg/auth/handler_test.go:42: expected status 200 OK but got 500 Internal Server Error on iteration")
	}
	dig := &protocol.RunDigest{
		Verify: &protocol.DigestVerify{Cmd: "go test ./pkg/auth/...", Exit: 1, Passed: 3, Failed: 2},
		Failure: &protocol.DigestFailure{
			Category: protocol.ErrKindVerify,
			Tests:    []string{"TestRotateToken", "TestReuseInvalidatesFamily"},
			Tail:     longTail,
		},
	}
	prompt := formatVerifyRepairPrompt(1, 2, dig)
	if !strings.HasPrefix(prompt, "Verification failed (attempt 1/2). Fix the failures below, then end your turn.") {
		t.Fatalf("unexpected prompt prefix: %q", prompt)
	}
	if !strings.Contains(prompt, "go test ./pkg/auth/...") {
		t.Fatalf("expected verify cmd in repair prompt: %q", prompt)
	}
	if !strings.Contains(prompt, "TestRotateToken") {
		t.Fatalf("expected failing test in repair prompt: %q", prompt)
	}
	if tok := protocol.EstimateTokens(prompt); tok > verifyRepairMaxTokens {
		t.Fatalf("repair prompt exceeded %d token cap: got %d tokens\n%s", verifyRepairMaxTokens, tok, prompt)
	}
}

func TestTicketVerifySelfRepairEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh verify commands")
	}

	cs := newChatServer(t)
	ctx := context.Background()

	workdirRoot := t.TempDir()
	repoOrigin := filepath.Join(t.TempDir(), "app-origin")
	initTestGitRepo(t, repoOrigin, "main")

	daemon, stop := startTicketTestDaemon(t, cs, workdirRoot)
	defer stop()

	if err := cs.st.CreateProject(ctx, store.Project{Name: "VerifyE2E", Autopilot: true}); err != nil {
		t.Fatal(err)
	}
	projs, _ := cs.st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	if err := cs.st.CreateRepo(ctx, store.Repo{
		ProjectID:     proj.ID,
		Name:          "app",
		RemoteURL:     repoOrigin,
		DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	repos, _ := cs.st.ListRepos(ctx, proj.ID)
	repo := repos[0]

	isOpen := func(chatID string) bool {
		for _, ci := range daemon.ChatManager().Chats() {
			if ci.ChatID == chatID && ci.Open {
				return true
			}
		}
		return false
	}

	// 1. Verify fails on attempt 1, mock creates verify_fixed.ok on repair prompt,
	//    verify passes on attempt 2 -> ticket ends in review with attempt 2.
	tkRepair, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Fix flaky check",
		Status:    store.TicketStatusReady,
		VerifyCmd: "test -f verify_fixed.ok # mock_fix_file",
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
		TicketID: tkRepair.ID, RepoID: repo.ID, BaseBranch: "main", TargetBranch: "feat/repair",
	}); err != nil {
		t.Fatal(err)
	}

	cs.h.SchedulePending(ctx)

	waitFor(t, 15*time.Second, "repaired ticket reaches review on attempt 2", func() bool {
		cur, err := cs.st.GetTicket(ctx, tkRepair.ID)
		return err == nil && cur.Status == store.TicketStatusReview && cur.Attempt == 2
	})

	curRepair, _ := cs.st.GetTicket(ctx, tkRepair.ID)
	if curRepair.Lease != nil {
		t.Fatalf("expected lease released in review, got %+v", curRepair.Lease)
	}
	tasksRepair, _ := cs.st.ListTasksByTicket(ctx, tkRepair.ID)
	if len(tasksRepair) == 0 {
		t.Fatalf("expected task row for tkRepair")
	}
	var digRepair protocol.RunDigest
	if err := json.Unmarshal([]byte(tasksRepair[0].DigestJSON), &digRepair, protocol.JSONOptions); err != nil {
		t.Fatalf("unmarshal repair digest: %v", err)
	}
	if digRepair.Status != protocol.DigestStatusReview || digRepair.Attempt != 2 {
		t.Fatalf("expected review digest attempt 2, got %+v", digRepair)
	}
	if digRepair.Verify == nil || digRepair.Verify.Exit != 0 || digRepair.Verify.Cmd != tkRepair.VerifyCmd {
		t.Fatalf("expected passing verify in digest, got %+v", digRepair.Verify)
	}
	// Verify the repair prompt was actually sent to the same chat session.
	chatsRepair, _ := cs.st.ListChatSessionsByTicket(ctx, tkRepair.ID)
	if len(chatsRepair) != 1 {
		t.Fatalf("expected 1 chat session, got %d", len(chatsRepair))
	}
	evsRepair, _ := cs.st.ListChatEvents(ctx, chatsRepair[0].ID, 0, 500)
	sawRepairPrompt := false
	for _, ce := range evsRepair {
		if ce.Kind == eventUserMessage && strings.Contains(ce.JSON, "Verification failed (attempt 1/2).") {
			sawRepairPrompt = true
			break
		}
	}
	if !sawRepairPrompt {
		t.Fatalf("expected repair prompt user_message event in chat history, got %d events", len(evsRepair))
	}

	// 2. Always-failing verify command with default retry_limit=2 -> retries exhausted -> failed.
	alwaysFailCmd := "printf '%s\\n' '--- FAIL: TestAlwaysBroken (0.00s)' 'FAIL\texample.com/app\t0.01s' && exit 1"
	tkFail, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindBug,
		Title:     "Broken verify exhausts retries",
		Status:    store.TicketStatusReady,
		VerifyCmd: alwaysFailCmd,
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tkFail.ID, RepoID: repo.ID})
	cs.h.SchedulePending(ctx)

	waitFor(t, 15*time.Second, "exhausted ticket reaches failed on attempt 2", func() bool {
		cur, err := cs.st.GetTicket(ctx, tkFail.ID)
		return err == nil && cur.Status == store.TicketStatusFailed && cur.Attempt == 2
	})

	curFail, _ := cs.st.GetTicket(ctx, tkFail.ID)
	if curFail.Lease != nil {
		t.Fatalf("expected lease released on failed ticket")
	}
	tasksFail, _ := cs.st.ListTasksByTicket(ctx, tkFail.ID)
	if len(tasksFail) == 0 {
		t.Fatalf("expected task row for tkFail")
	}
	var digFail protocol.RunDigest
	if err := json.Unmarshal([]byte(tasksFail[0].DigestJSON), &digFail, protocol.JSONOptions); err != nil {
		t.Fatalf("unmarshal failed digest: %v", err)
	}
	if digFail.Attempt != 2 || digFail.Status != protocol.DigestStatusFailed {
		t.Fatalf("expected failed digest attempt 2, got %+v", digFail)
	}
	if digFail.Verify == nil || digFail.Verify.Exit != 1 || digFail.Verify.Failed != 1 {
		t.Fatalf("expected verify failure details, got %+v", digFail.Verify)
	}
	if digFail.Failure == nil || digFail.Failure.Category != protocol.ErrKindVerify || !slices.Contains(digFail.Failure.Tests, "TestAlwaysBroken") {
		t.Fatalf("expected verify failure category + TestAlwaysBroken, got %+v", digFail.Failure)
	}
	chatsFail, _ := cs.st.ListChatSessionsByTicket(ctx, tkFail.ID)
	waitFor(t, 5*time.Second, "failed ticket chat closed on daemon", func() bool {
		return len(chatsFail) > 0 && !isOpen(chatsFail[0].ID)
	})

	// 3. Always-failing verify command with autonomy.escalate_on_blocker=true -> needs_you.
	escalate := true
	tkEsc, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindBug,
		Title:     "Broken verify escalates to needs_you",
		Status:    store.TicketStatusReady,
		VerifyCmd: alwaysFailCmd,
		Autonomy:  store.AutonomyPolicy{EscalateOnBlocker: &escalate},
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tkEsc.ID, RepoID: repo.ID})
	cs.h.SchedulePending(ctx)

	waitFor(t, 15*time.Second, "escalated ticket reaches needs_you on attempt 2", func() bool {
		cur, err := cs.st.GetTicket(ctx, tkEsc.ID)
		return err == nil && cur.Status == store.TicketStatusNeedsYou && cur.Attempt == 2
	})

	curEsc, _ := cs.st.GetTicket(ctx, tkEsc.ID)
	if curEsc.Lease != nil {
		t.Fatalf("expected lease released on escalated needs_you ticket")
	}
	tasksEsc, _ := cs.st.ListTasksByTicket(ctx, tkEsc.ID)
	if len(tasksEsc) == 0 {
		t.Fatalf("expected task row for tkEsc")
	}
	var digEsc protocol.RunDigest
	if err := json.Unmarshal([]byte(tasksEsc[0].DigestJSON), &digEsc, protocol.JSONOptions); err != nil {
		t.Fatalf("unmarshal escalated digest: %v", err)
	}
	if digEsc.Attempt != 2 || digEsc.Verify == nil || digEsc.Verify.Exit != 1 || digEsc.Failure == nil || digEsc.Failure.Category != protocol.ErrKindVerify {
		t.Fatalf("unexpected escalated digest: %+v", digEsc)
	}
	chatsEsc, _ := cs.st.ListChatSessionsByTicket(ctx, tkEsc.ID)
	if len(chatsEsc) == 0 || !isOpen(chatsEsc[0].ID) {
		t.Fatalf("expected escalated ticket chat to remain open for operator follow-up")
	}
}
