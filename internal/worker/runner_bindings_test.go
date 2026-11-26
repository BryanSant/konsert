package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/gitutil"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/drivers"
)

func initTestRepo(t *testing.T, dir, branch string) {
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
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "file.txt")
	run("commit", "-m", "init")
}

func TestRunnerMultiRepoTicketWorktreesAndCleanup(t *testing.T) {
	workdirRoot := t.TempDir()
	// Repo A: already checked out locally under workdirRoot.
	repoA := filepath.Join(workdirRoot, "checkouts", "api")
	initTestRepo(t, repoA, "main")

	// Repo B: bare/remote repo that is NOT checked out locally (daemon must clone into .syre/clones/web).
	remoteB := filepath.Join(t.TempDir(), "web-origin")
	initTestRepo(t, remoteB, "main")

	reg := drivers.NewRegistry()
	reg.Register(&drivers.MockDriver{})

	var (
		mu       sync.Mutex
		statuses []protocol.TaskStatusPayload
		doneCh   = make(chan struct{}, 1)
	)
	runner := NewRunner(reg, false, []string{workdirRoot}, RunnerCallbacks{
		OnTaskStatus: func(p protocol.TaskStatusPayload) {
			mu.Lock()
			statuses = append(statuses, p)
			mu.Unlock()
			if protocol.IsTerminalTaskStatus(p.Status) {
				select {
				case doneCh <- struct{}{}:
				default:
				}
			}
		},
	}, nil)

	err := runner.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID:    "task-multi-1",
		SID:       "t-m1",
		TicketSID: "SYR-MULTI",
		Agent:     "mock",
		Prompt:    "EXIT=0",
		Mode:      protocol.TaskModeOneshot,
		Bindings: []protocol.TaskRepoBinding{
			{
				RepoName:     "api",
				RemoteURL:    "https://github.com/acme/api.git",
				CheckoutPath: repoA,
				BaseBranch:   "main",
				TargetBranch: "feat/multi-api",
			},
			{
				RepoName:     "web",
				RemoteURL:    remoteB,
				CheckoutPath: "", // forces clone into <workdirRoot>/.syre/clones/web
				BaseBranch:   "main",
				TargetBranch: "feat/multi-web",
			},
		},
	})
	if err != nil {
		t.Fatalf("StartTask failed: %v", err)
	}

	select {
	case <-doneCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for task completion")
	}

	mu.Lock()
	var last protocol.TaskStatusPayload
	if len(statuses) > 0 {
		last = statuses[len(statuses)-1]
	}
	mu.Unlock()

	resolvedRoot, _ := filepath.EvalSymlinks(workdirRoot)
	wantTicketDir := gitutil.TicketWorkdir(resolvedRoot, "SYR-MULTI")
	if last.Status != protocol.TaskStatusCompleted {
		t.Fatalf("expected completed status, got %+v", last)
	}
	if last.Digest == nil {
		t.Fatalf("expected non-nil Digest on final TaskStatusPayload")
	}
	if last.Digest.TicketSID != "SYR-MULTI" || last.Digest.Status != protocol.DigestStatusReview {
		t.Fatalf("unexpected digest header: %+v", last.Digest)
	}
	if last.EffectiveDir != wantTicketDir {
		t.Fatalf("EffectiveDir = %q, want %q", last.EffectiveDir, wantTicketDir)
	}
	if len(last.Worktrees) != 2 {
		t.Fatalf("expected 2 reported worktrees, got %+v", last.Worktrees)
	}

	// Verify both worktrees exist under <workdirRoot>/.syre/tickets/SYR-MULTI/<repoName> on their target branches.
	wantWT := map[string]string{
		"api": "feat/multi-api",
		"web": "feat/multi-web",
	}
	for _, wt := range last.Worktrees {
		wantBranch, ok := wantWT[wt.RepoName]
		if !ok {
			t.Fatalf("unexpected worktree repo %q", wt.RepoName)
		}
		if wt.WorktreePath != filepath.Join(wantTicketDir, wt.RepoName) {
			t.Errorf("worktree path = %q, want %q", wt.WorktreePath, filepath.Join(wantTicketDir, wt.RepoName))
		}
		if wt.Branch != wantBranch {
			t.Errorf("worktree branch = %q, want %q", wt.Branch, wantBranch)
		}
		out, err := exec.Command("git", "-C", wt.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD").Output()
		if err != nil {
			t.Fatalf("git rev-parse in %s: %v", wt.WorktreePath, err)
		}
		if got := string(out); got != wantBranch+"\n" {
			t.Errorf("worktree %s checked out on %q, want %q", wt.RepoName, got, wantBranch)
		}
	}

	// Verify repo B was cloned into <workdirRoot>/.syre/clones/web.
	clonePath := gitutil.CloneDir(resolvedRoot, "web")
	if _, ok := gitutil.IsGitRepo(context.Background(), clonePath); !ok {
		t.Fatalf("expected cloned repo at %s", clonePath)
	}

	// Cleanup both worktrees: directories removed, parent ticket dir pruned, branches KEPT.
	for _, wt := range last.Worktrees {
		if err := gitutil.CleanupWorktree(context.Background(), wt.CheckoutPath, wt.WorktreePath, true); err != nil {
			t.Fatalf("CleanupWorktree(%s): %v", wt.RepoName, err)
		}
		if _, err := os.Stat(wt.WorktreePath); !os.IsNotExist(err) {
			t.Errorf("expected worktree %s removed", wt.WorktreePath)
		}
		// Branch must still exist in the primary repo.
		if err := exec.Command("git", "-C", wt.CheckoutPath, "show-ref", "--verify", "--quiet", "refs/heads/"+wt.Branch).Run(); err != nil {
			t.Errorf("expected branch %s to be kept in %s after cleanup", wt.Branch, wt.CheckoutPath)
		}
	}
	if _, err := os.Stat(wantTicketDir); !os.IsNotExist(err) {
		t.Errorf("expected empty ticket workdir %s to be pruned", wantTicketDir)
	}
}
