package gitutil

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateWorkdirAllowedRoots(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	sub := filepath.Join(allowed, "repo")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{sub, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Symlink inside allowed pointing outside
	escapeLink := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, escapeLink); err != nil {
		t.Fatal(err)
	}

	// Default full_access (empty allowedRoots) permits any existing dir.
	if _, err := ValidateWorkdir(outside, nil); err != nil {
		t.Fatalf("expected outside allowed when allowedRoots empty: %v", err)
	}

	// Sandboxed allowedRoots permits sub.
	if _, err := ValidateWorkdir(sub, []string{allowed}); err != nil {
		t.Fatalf("expected sub allowed: %v", err)
	}

	// Sandboxed rejects outside.
	if _, err := ValidateWorkdir(outside, []string{allowed}); !errors.Is(err, ErrWorkdirNotAllowed) {
		t.Fatalf("expected ErrWorkdirNotAllowed for outside, got %v", err)
	}

	// Sandboxed rejects symlink escape.
	if _, err := ValidateWorkdir(escapeLink, []string{allowed}); !errors.Is(err, ErrWorkdirNotAllowed) {
		t.Fatalf("expected ErrWorkdirNotAllowed for symlink escape, got %v", err)
	}
}

func TestWorktreeAndCaptureSummary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	ctx := context.Background()
	repo := t.TempDir()

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	run(repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "README.md")
	run(repo, "commit", "-m", "initial commit")

	root, ok := IsGitRepo(ctx, repo)
	if !ok || root == "" {
		t.Fatalf("expected git repo")
	}

	wtPath, baseSHA, err := CreateWorktree(ctx, root, "task-1", "feat/test-wt")
	if err != nil {
		t.Fatalf("CreateWorktree error: %v", err)
	}
	if baseSHA == "" {
		t.Fatalf("expected non-empty baseSHA")
	}
	if !strings.HasSuffix(wtPath, filepath.Join(".syre-worktrees", "task-1")) {
		t.Fatalf("unexpected wtPath: %s", wtPath)
	}

	// Make a commit and an uncommitted file in the worktree.
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(wtPath, "add", "README.md")
	run(wtPath, "commit", "-m", "add world")

	if err := os.WriteFile(filepath.Join(wtPath, "untracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	summary, err := CaptureSummary(ctx, wtPath, baseSHA)
	if err != nil {
		t.Fatalf("CaptureSummary error: %v", err)
	}
	if summary == nil {
		t.Fatalf("expected non-nil summary")
	}
	if summary.BaseSHA != baseSHA || summary.HeadSHA == baseSHA {
		t.Fatalf("expected head != base: %+v", summary)
	}
	if len(summary.Commits) != 1 || !strings.Contains(summary.Commits[0], "add world") {
		t.Fatalf("unexpected commits: %+v", summary.Commits)
	}
	if !strings.Contains(summary.DiffStat, "README.md") {
		t.Fatalf("expected README.md in diff stat, got %q", summary.DiffStat)
	}
	if len(summary.DirtyFiles) != 1 || !strings.Contains(summary.DirtyFiles[0], "untracked.txt") {
		t.Fatalf("unexpected dirty files: %+v", summary.DirtyFiles)
	}

	// Test live InspectWorkingTree + CommitChanges + CleanupSyreWorktree
	insp, err := InspectWorkingTree(ctx, wtPath)
	if err != nil || !insp.IsGitRepo || !insp.IsWorktree || len(insp.Files) != 1 || insp.Files[0].Path != "untracked.txt" {
		t.Fatalf("InspectWorkingTree mismatch: %+v err=%v", insp, err)
	}
	if !strings.Contains(insp.Files[0].Diff, "+dirty") {
		t.Fatalf("expected diff preview for untracked file, got %q", insp.Files[0].Diff)
	}

	// Configure git user locally so CommitChanges works in CI/temp repos
	run(wtPath, "config", "user.name", "Test")
	run(wtPath, "config", "user.email", "test@example.com")
	commitRes, err := CommitChanges(ctx, wtPath, "commit untracked file", true)
	if err != nil || !commitRes.OK || commitRes.CommitSHA == "" {
		t.Fatalf("CommitChanges failed: %+v err=%v", commitRes, err)
	}
	insp2, _ := InspectWorkingTree(ctx, wtPath)
	if len(insp2.Files) != 0 {
		t.Fatalf("expected clean tree after commit, got %+v", insp2.Files)
	}

	if err := CleanupSyreWorktree(ctx, wtPath); err != nil {
		t.Fatalf("CleanupSyreWorktree error: %v", err)
	}
}
