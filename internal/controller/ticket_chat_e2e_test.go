package controller

import (
	"context"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/gitutil"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/worker"
	"github.com/BryanSant/syre/internal/worker/drivers"
)

func initTestGitRepo(t *testing.T, dir, branch string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	run("init", "-b", branch)
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")
}

// startTicketTestDaemon boots a real syred daemon with the mock chat engine and an
// allowed workdir root, waiting until the mock chat account is registered on the hub.
func startTicketTestDaemon(t *testing.T, cs *chatServer, workdirRoot string) (*worker.Daemon, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	wCfg := config.DefaultWorkerConfig()
	wCfg.ControllerURL = cs.wsURL("/ws/worker")
	wCfg.WorkerID = "worker-ticket-e2e"
	wCfg.Name = "ticket-e2e"
	wCfg.HostGroup = "ticket-e2e"
	wCfg.EnableMockDriver = true
	wCfg.MaxConcurrent = 4
	wCfg.AllowedRoots = []string{workdirRoot}
	daemon := newTestDaemon(t, wCfg, drivers.DefaultRegistry, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = daemon.Run(ctx)
	}()

	waitFor(t, 10*time.Second, "mock chat account on worker", func() bool {
		views, _ := cs.h.agentAccountViews(ctx, wCfg.WorkerID)
		for _, v := range views {
			if v.Agent == "mock" && v.ChatSupported {
				return true
			}
		}
		return false
	})

	stop := func() {
		cancel()
		<-done
	}
	return daemon, stop
}

func TestTicketChatEndToEndWithMockEngine(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()

	workdirRoot := t.TempDir()
	repoOrigin := filepath.Join(t.TempDir(), "app-origin")
	initTestGitRepo(t, repoOrigin, "main")

	daemon, stop := startTicketTestDaemon(t, cs, workdirRoot)
	defer stop()

	// Create autopilot project + repo pointing to local git origin.
	if err := cs.st.CreateProject(ctx, store.Project{Name: "TicketChatE2E", Autopilot: true}); err != nil {
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

	// 1. Ticket opens a chat with ticket tools, calls ticket_check (via mock keyword check_item_0),
	//    provisions worktree under <workdirRoot>/.syre/tickets/<ticket sid>/app, stores RunDigest, and moves to review.
	tk1, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Implement handler (check_item_0)",
		Body:      "Add MockHandler and check acceptance item 0.",
		Acceptance: []store.AcceptanceItem{
			{Text: "Add MockHandler"},
			{Text: "Write unit tests"},
		},
		Status: store.TicketStatusReady,
		Hints:  store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
		TicketID: tk1.ID, RepoID: repo.ID, BaseBranch: "main", TargetBranch: "feat/handler",
	}); err != nil {
		t.Fatal(err)
	}

	cs.h.SchedulePending(ctx)

	// Wait for turn end -> ticket review and acceptance item 0 persisted as done.
	waitFor(t, 15*time.Second, "ticket 1 reaches review with acceptance[0] checked", func() bool {
		cur, err := cs.st.GetTicket(ctx, tk1.ID)
		return err == nil && cur.Status == store.TicketStatusReview && len(cur.Acceptance) == 2 && cur.Acceptance[0].Done
	})

	curTk1, _ := cs.st.GetTicket(ctx, tk1.ID)
	if curTk1.Acceptance[1].Done {
		t.Fatalf("acceptance[1] should still be unchecked")
	}
	chats1, err := cs.st.ListChatSessionsByTicket(ctx, tk1.ID)
	if err != nil || len(chats1) != 1 {
		t.Fatalf("expected 1 chat session for tk1, got %d (%v)", len(chats1), err)
	}
	chat1 := chats1[0]
	if chat1.FleetTools {
		t.Fatalf("ticket chat must not have fleet_tools=true")
	}
	resolvedRoot, _ := filepath.EvalSymlinks(workdirRoot)
	wantDir := gitutil.TicketWorkdir(resolvedRoot, tk1.SID)
	if chat1.Cwd != wantDir {
		t.Fatalf("chat1.Cwd = %q, want %q", chat1.Cwd, wantDir)
	}
	if _, err := os.Stat(filepath.Join(wantDir, "app", "README.md")); err != nil {
		t.Fatalf("expected provisioned worktree at %s: %v", filepath.Join(wantDir, "app"), err)
	}

	// Verify task row has worktrees + persisted RunDigest with interface line.
	tasks1, err := cs.st.ListTasksByTicket(ctx, tk1.ID)
	if err != nil || len(tasks1) == 0 {
		t.Fatalf("expected task row for tk1, got %+v (%v)", tasks1, err)
	}
	t1 := tasks1[0]
	if len(t1.Worktrees) != 1 || t1.Worktrees[0].RepoName != "app" {
		t.Fatalf("expected 1 worktree recorded on task, got %+v", t1.Worktrees)
	}
	var dig1 protocol.RunDigest
	if err := json.Unmarshal([]byte(t1.DigestJSON), &dig1, protocol.JSONOptions); err != nil {
		t.Fatalf("unmarshal digest %q: %v", t1.DigestJSON, err)
	}
	if dig1.TicketSID != tk1.SID || dig1.Status != protocol.DigestStatusReview {
		t.Fatalf("unexpected digest header: %+v", dig1)
	}
	if !strings.Contains(dig1.Interface, "MockHandler") {
		t.Fatalf("expected interface extracted into digest, got %+v", dig1)
	}

	// Session must stay open in review so SendTicketTurn can deliver a second turn.
	isOpen := func(chatID string) bool {
		for _, ci := range daemon.ChatManager().Chats() {
			if ci.ChatID == chatID && ci.Open {
				return true
			}
		}
		return false
	}
	if !isOpen(chat1.ID) {
		t.Fatalf("expected chat1 to remain open while ticket is in review")
	}

	// 2. SendTicketTurn delivers a second turn to the same chat session and returns to review.
	beforeEvents, _ := cs.st.ListChatEvents(ctx, chat1.ID, 0, 500)
	if err := cs.h.SendTicketTurn(ctx, tk1.ID, "Please tighten error handling"); err != nil {
		t.Fatalf("SendTicketTurn failed: %v", err)
	}
	waitFor(t, 10*time.Second, "second turn completes and ticket returns to review", func() bool {
		cur, err := cs.st.GetTicket(ctx, tk1.ID)
		if err != nil || cur.Status != store.TicketStatusReview {
			return false
		}
		evs, _ := cs.st.ListChatEvents(ctx, chat1.ID, 0, 500)
		return len(evs) > len(beforeEvents)+1
	})

	// 3. Engine error -> ticket failed with failure category + chat closed.
	// Wait for autopilot auto-off after tk1 emptied the board, then re-enable Play.
	waitFor(t, 3*time.Second, "autopilot auto-off after tk1", func() bool {
		p, err := cs.st.GetProject(ctx, proj.ID)
		return err == nil && !p.Autopilot
	})
	if _, err := cs.h.SetProjectAutopilot(ctx, proj.ID, true); err != nil {
		t.Fatal(err)
	}
	tkErr, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindBug,
		Title:     "Trigger fail auth",
		Status:    store.TicketStatusReady,
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tkErr.ID, RepoID: repo.ID})
	cs.h.SchedulePending(ctx)

	waitFor(t, 10*time.Second, "error ticket reaches failed with auth digest", func() bool {
		cur, err := cs.st.GetTicket(ctx, tkErr.ID)
		if err != nil || cur.Status != store.TicketStatusFailed {
			return false
		}
		tasks, _ := cs.st.ListTasksByTicket(ctx, tkErr.ID)
		return len(tasks) > 0 && tasks[0].DigestJSON != ""
	})
	tasksErr, _ := cs.st.ListTasksByTicket(ctx, tkErr.ID)
	var digErr protocol.RunDigest
	if err := json.Unmarshal([]byte(tasksErr[0].DigestJSON), &digErr, protocol.JSONOptions); err != nil {
		t.Fatalf("unmarshal error digest: %v", err)
	}
	if digErr.Status != protocol.DigestStatusFailed || digErr.Failure == nil || digErr.Failure.Category != protocol.ErrKindAuth {
		t.Fatalf("expected failed digest with auth category, got %+v", digErr)
	}
	chatsErr, _ := cs.st.ListChatSessionsByTicket(ctx, tkErr.ID)
	waitFor(t, 5*time.Second, "failed ticket chat closed on daemon", func() bool {
		return len(chatsErr) > 0 && !isOpen(chatsErr[0].ID)
	})

	// 4. Elicitation ("ask") -> ticket needs_you, answered -> running -> review.
	waitFor(t, 3*time.Second, "autopilot auto-off after tkErr", func() bool {
		p, err := cs.st.GetProject(ctx, proj.ID)
		return err == nil && !p.Autopilot
	})
	if _, err := cs.h.SetProjectAutopilot(ctx, proj.ID, true); err != nil {
		t.Fatal(err)
	}
	tkAsk, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Please ask user",
		Status:    store.TicketStatusReady,
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tkAsk.ID, RepoID: repo.ID})
	cs.h.SchedulePending(ctx)

	waitFor(t, 10*time.Second, "ask ticket enters needs_you", func() bool {
		cur, err := cs.st.GetTicket(ctx, tkAsk.ID)
		return err == nil && cur.Status == store.TicketStatusNeedsYou
	})
	chatsAsk, _ := cs.st.ListChatSessionsByTicket(ctx, tkAsk.ID)
	if len(chatsAsk) == 0 {
		t.Fatalf("expected chat session for tkAsk")
	}
	// Find the pending elicitation ID from chat events and answer it.
	var elicitID string
	evsAsk, _ := cs.st.ListChatEvents(ctx, chatsAsk[0].ID, 0, 200)
	for _, ce := range evsAsk {
		if ce.Kind == protocol.EventElicitation {
			var ev protocol.AgentChatEvent
			if json.Unmarshal([]byte(ce.JSON), &ev, protocol.JSONOptions) == nil && ev.Elicitation != nil {
				elicitID = ev.Elicitation.ID
			}
		}
	}
	if elicitID == "" {
		t.Fatalf("expected elicitation event for tkAsk")
	}
	if err := cs.h.respondChat(ctx, chatRespondRequest{
		SessionID: chatsAsk[0].ID, ElicitationID: elicitID, OptionID: "blue",
	}); err != nil {
		t.Fatalf("respondChat failed: %v", err)
	}
	waitFor(t, 10*time.Second, "answered ask ticket completes to review", func() bool {
		cur, err := cs.st.GetTicket(ctx, tkAsk.ID)
		return err == nil && cur.Status == store.TicketStatusReview
	})

	// 5. Requeue closes the chat.
	waitFor(t, 3*time.Second, "autopilot auto-off after tkAsk", func() bool {
		p, err := cs.st.GetProject(ctx, proj.ID)
		return err == nil && !p.Autopilot
	})
	if _, err := cs.h.SetProjectAutopilot(ctx, proj.ID, true); err != nil {
		t.Fatal(err)
	}
	tkSlow, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Run slow job",
		Status:    store.TicketStatusReady,
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tkSlow.ID, RepoID: repo.ID})
	cs.h.SchedulePending(ctx)

	var slowChatID string
	waitFor(t, 10*time.Second, "slow ticket chat opens on daemon", func() bool {
		chats, _ := cs.st.ListChatSessionsByTicket(ctx, tkSlow.ID)
		if len(chats) == 0 {
			return false
		}
		slowChatID = chats[0].ID
		return isOpen(slowChatID)
	})

	// Disable autopilot before requeueing so the scheduler doesn't immediately re-dispatch it.
	proj.Autopilot = false
	if err := cs.st.UpdateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	requeued, err := cs.h.RequeueTicket(ctx, tkSlow.ID, "operator requeue")
	if err != nil {
		t.Fatalf("RequeueTicket failed: %v", err)
	}
	if requeued.Status != store.TicketStatusReady || requeued.Lease != nil {
		t.Fatalf("expected ready/unleased after requeue, got %+v", requeued)
	}
	waitFor(t, 5*time.Second, "requeued ticket chat closed on daemon", func() bool {
		return !isOpen(slowChatID)
	})
}
