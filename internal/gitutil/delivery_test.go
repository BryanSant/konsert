package gitutil

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
)

func initTestRepo(t *testing.T, withBareRemote bool) (repoDir, bareDir string) {
	t.Helper()
	base, err := os.MkdirTemp("/var/tmp", "konsert-gitutil-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	ctx := context.Background()
	repoDir = filepath.Join(base, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repoDir, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	_, _ = runGit(ctx, repoDir, "config", "user.name", "Test User")
	_, _ = runGit(ctx, repoDir, "config", "user.email", "test@users.noreply.github.com")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repoDir, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repoDir, "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	if withBareRemote {
		bareDir = filepath.Join(base, "origin.git")
		if err := os.MkdirAll(bareDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := runGit(ctx, bareDir, "init", "--bare", "-b", "main"); err != nil {
			t.Fatal(err)
		}
		if _, err := runGit(ctx, repoDir, "remote", "add", "origin", bareDir); err != nil {
			t.Fatal(err)
		}
		if _, err := runGit(ctx, repoDir, "push", "-u", "origin", "main"); err != nil {
			t.Fatal(err)
		}
	}
	return repoDir, bareDir
}

func TestRepoGitInfo(t *testing.T) {
	ctx := context.Background()
	repo, bare := initTestRepo(t, true)
	info := RepoGitInfo(ctx, repo)
	if !info.OK || !info.IsGitRepo || info.Branch != "main" || info.DefaultBranch != "main" || !info.HasRemote || info.RemoteURL != bare || info.Dirty {
		t.Fatalf("RepoGitInfo = %+v", info)
	}

	localOnly, _ := initTestRepo(t, false)
	info2 := RepoGitInfo(ctx, localOnly)
	if !info2.OK || info2.HasRemote || info2.DefaultBranch != "main" {
		t.Fatalf("local RepoGitInfo = %+v", info2)
	}
}

func TestPushBranchToOriginAndForkURL(t *testing.T) {
	ctx := context.Background()
	repo, bare := initTestRepo(t, true)
	wt, _, err := CreateWorktree(ctx, repo, "t-push", "konsert/t-push")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("new feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// PushBranch auto-commits dirty files when CommitMessage is provided.
	res := PushBranch(ctx, wt, protocol.GitPushRequest{CommitMessage: "Add feature"})
	if !res.OK || res.Branch != "konsert/t-push" || res.RemoteURL != bare || res.HeadSHA == "" {
		t.Fatalf("PushBranch(origin) = %+v", res)
	}
	// Verify branch reached bare remote.
	if _, err := runGit(ctx, bare, "show-ref", "--verify", "--quiet", "refs/heads/konsert/t-push"); err != nil {
		t.Fatalf("branch not in bare remote: %v", err)
	}

	// Push to an explicit fork URL.
	forkDir := filepath.Join(filepath.Dir(bare), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, forkDir, "init", "--bare"); err != nil {
		t.Fatal(err)
	}
	forkRes := PushBranch(ctx, wt, protocol.GitPushRequest{Branch: "konsert/t-push", RemoteURL: forkDir})
	if !forkRes.OK || forkRes.RemoteURL != forkDir {
		t.Fatalf("PushBranch(fork) = %+v", forkRes)
	}
	if _, err := runGit(ctx, forkDir, "show-ref", "--verify", "--quiet", "refs/heads/konsert/t-push"); err != nil {
		t.Fatalf("branch not in fork remote: %v", err)
	}
}

func TestMergeBranchStylesAndLocalOnly(t *testing.T) {
	ctx := context.Background()
	for _, style := range []string{"squash", "merge", "rebase"} {
		t.Run(style, func(t *testing.T) {
			repo, bare := initTestRepo(t, true)
			wt, _, err := CreateWorktree(ctx, repo, "t-"+style, "konsert/"+style)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, style+".txt"), []byte(style+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			res := MergeBranch(ctx, wt, protocol.GitMergeRequest{
				Style:         style,
				Push:          true,
				CommitMessage: "Deliver via " + style,
			})
			if !res.OK || !res.Merged || !res.Pushed || res.Conflict || res.TargetBranch != "main" || res.MergeSHA == "" {
				t.Fatalf("MergeBranch(%s) = %+v", style, res)
			}
			// Verify remote main has the new file.
			files, err := runGit(ctx, bare, "ls-tree", "-r", "--name-only", "main")
			if err != nil || !strings.Contains(files, style+".txt") {
				t.Fatalf("bare main files = %q, err = %v", files, err)
			}
		})
	}

	t.Run("local_only_no_remote", func(t *testing.T) {
		repo, _ := initTestRepo(t, false)
		wt, _, err := CreateWorktree(ctx, repo, "t-local", "konsert/local")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, "local.txt"), []byte("ok\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := MergeBranch(ctx, wt, protocol.GitMergeRequest{
			Style:         "squash",
			Push:          true,
			CommitMessage: "Local delivery",
		})
		if !res.OK || !res.Merged || res.Pushed || res.MergeSHA == "" {
			t.Fatalf("MergeBranch(local) = %+v", res)
		}
		if _, err := os.Stat(filepath.Join(repo, "local.txt")); err != nil {
			t.Fatalf("merged file missing in main checkout: %v", err)
		}
	})
}

func TestMergeBranchConflictIsReportedAndAborted(t *testing.T) {
	ctx := context.Background()
	repo, _ := initTestRepo(t, false)
	wt, _, err := CreateWorktree(ctx, repo, "t-conflict", "konsert/conflict")
	if err != nil {
		t.Fatal(err)
	}
	// Divergent edits to README.md on main and task branch.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("main change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repo, "commit", "-am", "main edit"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("task change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, wt, "commit", "-am", "task edit"); err != nil {
		t.Fatal(err)
	}

	for _, style := range []string{"squash", "merge", "rebase"} {
		res := MergeBranch(ctx, wt, protocol.GitMergeRequest{
			Style:         style,
			CommitMessage: "Should conflict",
		})
		if res.OK || !res.Conflict || len(res.ConflictFiles) == 0 || res.ConflictFiles[0] != "README.md" {
			t.Fatalf("MergeBranch(%s) conflict = %+v", style, res)
		}
		// Repo should have no tracked/conflicted changes after abort.
		st, _ := runGit(ctx, repo, "status", "--porcelain", "--untracked-files=no")
		if st != "" {
			t.Fatalf("repo dirty after %s conflict abort: %q", style, st)
		}
	}
}
