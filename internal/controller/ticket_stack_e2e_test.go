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

	"github.com/BryanSant/syre/internal/gitutil"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestTicketStackingEndToEndWithMockEngine(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()

	workdirRoot := t.TempDir()
	repoOrigin := filepath.Join(t.TempDir(), "app-origin")
	initTestGitRepo(t, repoOrigin, "main")

	_, stop := startTicketTestDaemon(t, cs, workdirRoot)
	defer stop()

	if err := cs.st.CreateProject(ctx, store.Project{Name: "StackE2E", Autopilot: true}); err != nil {
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

	// Ticket A commits a change on its branch syre/<A sid> using the mock engine.
	tkA, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindBug,
		Title:     "Fix Add (mock_commit_change)",
		Status:    store.TicketStatusReady,
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
		TicketID:   tkA.ID,
		RepoID:     repo.ID,
		BaseBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	// Ticket B is blocked by A on the same repo.
	tkB, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Add Sub",
		Status:    store.TicketStatusReady,
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
		TicketID:   tkB.ID,
		RepoID:     repo.ID,
		BaseBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	if err := cs.st.AddTicketEdge(ctx, tkB.ID, tkA.ID); err != nil {
		t.Fatal(err)
	}

	cs.h.SchedulePending(ctx)

	// Wait for A to complete to review (with its committed file on syre/<tkA.SID>).
	waitFor(t, 15*time.Second, "ticket A reaches review", func() bool {
		cur, err := cs.st.GetTicket(ctx, tkA.ID)
		return err == nil && cur.Status == store.TicketStatusReview
	})

	// Confirm B is still ready (not dispatched before A is done).
	curB, _ := cs.st.GetTicket(ctx, tkB.ID)
	if curB.Status != store.TicketStatusReady {
		t.Fatalf("expected B ready while A is in review, got %s", curB.Status)
	}

	// Capture A's committed HEAD SHA on its worktree.
	resolvedRoot, _ := filepath.EvalSymlinks(workdirRoot)
	wtA := filepath.Join(gitutil.TicketWorkdir(resolvedRoot, tkA.SID), "app")
	wantFile := "change_" + strings.ToLower(tkA.SID) + ".txt"
	if _, err := os.Stat(filepath.Join(wtA, wantFile)); err != nil {
		t.Fatalf("expected A's worktree to contain %s: %v", wantFile, err)
	}
	headOut, err := exec.Command("git", "-C", wtA, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD in A: %v", err)
	}
	headA := strings.TrimSpace(string(headOut))

	// Commander/test marks A done WITHOUT merging A's branch into main.
	curA, _ := cs.st.GetTicket(ctx, tkA.ID)
	curA.Status = store.TicketStatusDone
	if _, err := cs.st.UpdateTicket(ctx, curA); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.h.SetProjectAutopilot(ctx, proj.ID, true); err != nil {
		t.Fatal(err)
	}
	cs.h.SchedulePending(ctx)

	// Wait for B to dispatch and reach review.
	waitFor(t, 15*time.Second, "ticket B dispatches and reaches review", func() bool {
		cur, err := cs.st.GetTicket(ctx, tkB.ID)
		return err == nil && cur.Status == store.TicketStatusReview
	})

	// B's worktree must contain A's change!
	wtB := filepath.Join(gitutil.TicketWorkdir(resolvedRoot, tkB.SID), "app")
	gotBytes, err := os.ReadFile(filepath.Join(wtB, wantFile))
	if err != nil {
		t.Fatalf("expected B's stacked worktree to contain A's file %s: %v", wantFile, err)
	}
	if !strings.Contains(string(gotBytes), tkA.SID) {
		t.Fatalf("B's %s content = %q, want %s", wantFile, string(gotBytes), tkA.SID)
	}

	// Verify B's task recorded BaseSHA == A's HEAD and its prompt included the stacked line.
	tasksB, err := cs.st.ListTasksByTicket(ctx, tkB.ID)
	if err != nil || len(tasksB) == 0 {
		t.Fatalf("expected task for B, got %+v (%v)", tasksB, err)
	}
	if len(tasksB[0].Worktrees) != 1 || tasksB[0].Worktrees[0].BaseSHA != headA {
		t.Fatalf("expected B task worktree BaseSHA=%q, got %+v", headA, tasksB[0].Worktrees)
	}
	wantStackLine := "app: stacked on " + tkA.SID + " (syre/" + tkA.SID + ")"
	if !strings.Contains(tasksB[0].Prompt, wantStackLine) {
		t.Fatalf("expected B prompt to contain %q, got:\n%s", wantStackLine, tasksB[0].Prompt)
	}
}

func TestTicketStackConflictMovesToNeedsYouWithDigestQuestion(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()

	workdirRoot := t.TempDir()
	repoOrigin := filepath.Join(t.TempDir(), "conflict-origin")
	initTestGitRepo(t, repoOrigin, "main")

	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repoOrigin}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	// Create two conflicting blocker branches on origin.
	gitRun("checkout", "-b", "syre/tk-c1")
	if err := os.WriteFile(filepath.Join(repoOrigin, "README.md"), []byte("version from c1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", "README.md")
	gitRun("commit", "-m", "c1 edit")

	gitRun("checkout", "main")
	gitRun("checkout", "-b", "syre/tk-c2")
	if err := os.WriteFile(filepath.Join(repoOrigin, "README.md"), []byte("version from c2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", "README.md")
	gitRun("commit", "-m", "c2 edit")
	gitRun("checkout", "main")

	_, stop := startTicketTestDaemon(t, cs, workdirRoot)
	defer stop()

	if err := cs.st.CreateProject(ctx, store.Project{Name: "ConflictE2E", Autopilot: true}); err != nil {
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

	mkDoneBlocker := func(sid, branch string) store.Ticket {
		tk, err := cs.st.CreateTicket(ctx, store.Ticket{
			SID:       sid,
			ProjectID: proj.ID,
			Kind:      store.TicketKindFeature,
			Title:     "Blocker " + sid,
			Status:    store.TicketStatusDone,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
			TicketID:     tk.ID,
			RepoID:       repo.ID,
			BaseBranch:   "main",
			TargetBranch: branch,
		}); err != nil {
			t.Fatal(err)
		}
		taskID := "task-" + sid
		if err := cs.st.CreateTask(ctx, store.Task{
			ID:       taskID,
			Agent:    "mock",
			Workdir:  workdirRoot,
			Prompt:   "blocker",
			Status:   protocol.TaskStatusCompleted,
			TicketID: tk.ID,
			RepoID:   repo.ID,
			Branch:   branch,
		}); err != nil {
			t.Fatal(err)
		}
		if err := cs.st.UpdateTaskWorktrees(ctx, taskID, []store.TaskWorktree{{
			RepoName: "app",
			Branch:   branch,
		}}); err != nil {
			t.Fatal(err)
		}
		return tk
	}

	c1 := mkDoneBlocker("tk-c1", "syre/tk-c1")
	c2 := mkDoneBlocker("tk-c2", "syre/tk-c2")

	dep, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Dependent with conflict",
		Status:    store.TicketStatusReady,
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
		TicketID:   dep.ID,
		RepoID:     repo.ID,
		BaseBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	if err := cs.st.AddTicketEdge(ctx, dep.ID, c1.ID); err != nil {
		t.Fatal(err)
	}
	if err := cs.st.AddTicketEdge(ctx, dep.ID, c2.ID); err != nil {
		t.Fatal(err)
	}

	cs.h.SchedulePending(ctx)

	waitFor(t, 15*time.Second, "conflicting dependent enters needs_you with digest question", func() bool {
		cur, err := cs.st.GetTicket(ctx, dep.ID)
		if err != nil || cur.Status != store.TicketStatusNeedsYou || cur.Lease != nil {
			return false
		}
		tasks, _ := cs.st.ListTasksByTicket(ctx, dep.ID)
		return len(tasks) > 0 && tasks[0].DigestJSON != ""
	})

	tasks, _ := cs.st.ListTasksByTicket(ctx, dep.ID)
	var dig protocol.RunDigest
	if err := json.Unmarshal([]byte(tasks[0].DigestJSON), &dig, protocol.JSONOptions); err != nil {
		t.Fatalf("unmarshal digest: %v", err)
	}
	if dig.Status != protocol.DigestStatusNeedsYou || dig.Question == nil {
		t.Fatalf("expected needs_you digest with question, got %+v", dig)
	}
	if !strings.Contains(dig.Question.Text, "README.md") {
		t.Fatalf("expected question to list conflicting file README.md, got %q", dig.Question.Text)
	}
}
