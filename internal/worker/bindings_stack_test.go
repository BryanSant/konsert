package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/gitutil"
	"github.com/BryanSant/syre/internal/protocol"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func TestProvisionBindingsStackSingleBlockerLocalAndRemote(t *testing.T) {
	ctx := context.Background()
	workdirRoot := t.TempDir()

	// 1. Local checkout: blocker A creates syre/tk-a with a committed change.
	repoLocal := filepath.Join(workdirRoot, "checkouts", "demo")
	initTestRepo(t, repoLocal, "main")
	gitRun(t, repoLocal, "checkout", "-b", "syre/tk-a")
	if err := os.WriteFile(filepath.Join(repoLocal, "fix_add.txt"), []byte("fixed add\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repoLocal, "add", "fix_add.txt")
	gitRun(t, repoLocal, "commit", "-m", "fix add")
	blockerSHA := gitRun(t, repoLocal, "rev-parse", "HEAD")
	gitRun(t, repoLocal, "checkout", "main")

	prov, err := provisionBindings(ctx, []string{workdirRoot}, workdirRoot, "tk-b", []protocol.TaskRepoBinding{{
		RepoName:     "demo",
		CheckoutPath: repoLocal,
		BaseBranch:   "main",
		TargetBranch: "syre/tk-b",
		StackOn:      []string{"syre/tk-a"},
	}}, false, nil)
	if err != nil {
		t.Fatalf("provisionBindings single local blocker: %v", err)
	}
	if len(prov.worktrees) != 1 {
		t.Fatalf("expected 1 worktree, got %+v", prov.worktrees)
	}
	wt := prov.worktrees[0]
	if wt.BaseSHA != blockerSHA {
		t.Fatalf("worktree BaseSHA = %q, want blocker SHA %q", wt.BaseSHA, blockerSHA)
	}
	got, err := os.ReadFile(filepath.Join(wt.WorktreePath, "fix_add.txt"))
	if err != nil || string(got) != "fixed add\n" {
		t.Fatalf("expected B worktree to contain A's change, got %q (%v)", string(got), err)
	}

	// 2. Remote-only blocker branch (no local checkout; syred clones into .syre/clones/remote-demo).
	remoteOrigin := filepath.Join(t.TempDir(), "remote-demo")
	initTestRepo(t, remoteOrigin, "main")
	gitRun(t, remoteOrigin, "checkout", "-b", "syre/tk-rem")
	if err := os.WriteFile(filepath.Join(remoteOrigin, "from_remote.txt"), []byte("remote fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, remoteOrigin, "add", "from_remote.txt")
	gitRun(t, remoteOrigin, "commit", "-m", "remote fix")
	remoteSHA := gitRun(t, remoteOrigin, "rev-parse", "HEAD")
	gitRun(t, remoteOrigin, "checkout", "main")

	provRem, err := provisionBindings(ctx, []string{workdirRoot}, workdirRoot, "tk-b2", []protocol.TaskRepoBinding{{
		RepoName:     "remote-demo",
		RemoteURL:    remoteOrigin,
		BaseBranch:   "main",
		TargetBranch: "syre/tk-b2",
		StackOn:      []string{"syre/tk-rem"},
	}}, false, nil)
	if err != nil {
		t.Fatalf("provisionBindings single remote blocker: %v", err)
	}
	if provRem.worktrees[0].BaseSHA != remoteSHA {
		t.Fatalf("remote worktree BaseSHA = %q, want %q", provRem.worktrees[0].BaseSHA, remoteSHA)
	}
	gotRem, err := os.ReadFile(filepath.Join(provRem.worktrees[0].WorktreePath, "from_remote.txt"))
	if err != nil || string(gotRem) != "remote fix\n" {
		t.Fatalf("expected remote-stacked worktree to contain remote fix, got %q (%v)", string(gotRem), err)
	}
}

func TestProvisionBindingsStackTwoBlockersMerged(t *testing.T) {
	ctx := context.Background()
	workdirRoot := t.TempDir()
	repo := filepath.Join(workdirRoot, "checkouts", "demo")
	initTestRepo(t, repo, "main")

	// Blocker 1 adds a.txt.
	gitRun(t, repo, "checkout", "-b", "syre/tk-a1")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("from a1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "a.txt")
	gitRun(t, repo, "commit", "-m", "add a.txt")

	// Blocker 2 (also from main) adds b.txt.
	gitRun(t, repo, "checkout", "main")
	gitRun(t, repo, "checkout", "-b", "syre/tk-a2")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("from a2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "b.txt")
	gitRun(t, repo, "commit", "-m", "add b.txt")
	gitRun(t, repo, "checkout", "main")

	prov, err := provisionBindings(ctx, []string{workdirRoot}, workdirRoot, "tk-c", []protocol.TaskRepoBinding{{
		RepoName:     "demo",
		CheckoutPath: repo,
		BaseBranch:   "main",
		TargetBranch: "syre/tk-c",
		StackOn:      []string{"syre/tk-a1", "syre/tk-a2"},
	}}, false, nil)
	if err != nil {
		t.Fatalf("provisionBindings two blockers: %v", err)
	}
	wt := prov.worktrees[0]
	headSHA := gitRun(t, wt.WorktreePath, "rev-parse", "HEAD")
	if wt.BaseSHA != headSHA {
		t.Fatalf("expected BaseSHA %q == merged HEAD %q", wt.BaseSHA, headSHA)
	}
	for _, want := range []struct {
		file, body string
	}{
		{"a.txt", "from a1\n"},
		{"b.txt", "from a2\n"},
	} {
		got, err := os.ReadFile(filepath.Join(wt.WorktreePath, want.file))
		if err != nil || string(got) != want.body {
			t.Fatalf("worktree missing %s: got %q (%v)", want.file, string(got), err)
		}
	}
}

func TestProvisionBindingsStackConflictReturnsFilesAndCleansUp(t *testing.T) {
	ctx := context.Background()
	workdirRoot := t.TempDir()
	repo := filepath.Join(workdirRoot, "checkouts", "demo")
	initTestRepo(t, repo, "main")

	// Both blockers edit file.txt incompatibly.
	gitRun(t, repo, "checkout", "-b", "syre/tk-x1")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("change from x1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "file.txt")
	gitRun(t, repo, "commit", "-m", "x1 edit")

	gitRun(t, repo, "checkout", "main")
	gitRun(t, repo, "checkout", "-b", "syre/tk-x2")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("change from x2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "file.txt")
	gitRun(t, repo, "commit", "-m", "x2 edit")
	gitRun(t, repo, "checkout", "main")

	_, err := provisionBindings(ctx, []string{workdirRoot}, workdirRoot, "tk-conf", []protocol.TaskRepoBinding{{
		RepoName:     "demo",
		CheckoutPath: repo,
		BaseBranch:   "main",
		TargetBranch: "syre/tk-conf",
		StackOn:      []string{"syre/tk-x1", "syre/tk-x2"},
	}}, false, nil)
	if err == nil {
		t.Fatal("expected stack conflict error, got nil")
	}
	var conflictErr *gitutil.StackConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected *gitutil.StackConflictError, got %T: %v", err, err)
	}
	if conflictErr.RepoName != "demo" {
		t.Errorf("RepoName = %q, want demo", conflictErr.RepoName)
	}
	if conflictErr.Branch != "syre/tk-x2" {
		t.Errorf("Branch = %q, want syre/tk-x2", conflictErr.Branch)
	}
	if !slices.Contains(conflictErr.Files, "file.txt") {
		t.Errorf("Files = %v, want file.txt", conflictErr.Files)
	}

	// Conflicting worktree must have been cleaned up and target branch deleted.
	resolvedRoot, _ := filepath.EvalSymlinks(workdirRoot)
	wtPath := filepath.Join(gitutil.TicketWorkdir(resolvedRoot, "tk-conf"), "demo")
	if _, statErr := os.Stat(wtPath); !os.IsNotExist(statErr) {
		t.Errorf("expected conflicting worktree %s to be removed", wtPath)
	}
	if err := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/syre/tk-conf").Run(); err == nil {
		t.Errorf("expected aborted target branch syre/tk-conf to be deleted")
	}
}
