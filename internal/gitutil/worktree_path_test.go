package gitutil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWorktreePathContainment(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"", ".", "..", "../x", "../../etc", "a/b", `a\b`, "/abs", "c:x"} {
		if p, err := WorktreePath(root, bad); !errors.Is(err, ErrInvalidWorktreeName) {
			t.Fatalf("WorktreePath(%q) = %q, %v; want ErrInvalidWorktreeName", bad, p, err)
		}
	}
	p, err := WorktreePath(root, "11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != filepath.Join(root, ".konsert-worktrees") {
		t.Fatalf("unexpected path %s", p)
	}
}

func TestCreateWorktreeRejectsTraversal(t *testing.T) {
	repo, run := gitTestRepo(t)
	writeFile(t, filepath.Join(repo, "f"), "x\n")
	run("add", "-A")
	run("commit", "-q", "-m", "init")

	_, _, err := CreateWorktree(context.Background(), repo, "../../escaped", "feat/x")
	if !errors.Is(err, ErrInvalidWorktreeName) {
		t.Fatalf("expected ErrInvalidWorktreeName, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(repo), "escaped")); !os.IsNotExist(statErr) {
		t.Fatal("worktree was created outside .konsert-worktrees")
	}
}

func TestRemoveTaskWorktreeDeletesCreatedBranch(t *testing.T) {
	repo, run := gitTestRepo(t)
	writeFile(t, filepath.Join(repo, "f"), "x\n")
	run("add", "-A")
	run("commit", "-q", "-m", "init")

	info, err := CreateWorktreeInfo(context.Background(), repo, "task-9", "feat/new-branch")
	if err != nil {
		t.Fatal(err)
	}
	if !info.CreatedBranch {
		t.Fatal("expected CreatedBranch for a new branch")
	}
	if err := RemoveTaskWorktree(context.Background(), info); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(info.Path); !os.IsNotExist(err) {
		t.Fatal("worktree directory still present")
	}
	if _, err := runGit(context.Background(), repo, "show-ref", "--verify", "--quiet", "refs/heads/feat/new-branch"); err == nil {
		t.Fatal("branch created for the worktree still exists")
	}
}
