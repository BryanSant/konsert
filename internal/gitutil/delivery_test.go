package gitutil

import (
	"bytes"
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
	_, _ = runGit(ctx, repoDir, "config", "user.email", "test@example.invalid")
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

type checkoutSnapshot struct {
	head    string
	branch  string
	status  string
	content map[string][]byte
}

func snapshotCheckout(t *testing.T, dir string, files ...string) checkoutSnapshot {
	t.Helper()
	ctx := context.Background()
	head, err := CurrentSHA(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	branch, _ := runGit(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	status, err := runGitRaw(ctx, dir, "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	content := make(map[string][]byte, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		content[f] = raw
	}
	return checkoutSnapshot{head: head, branch: branch, status: status, content: content}
}

func assertCheckoutUnchanged(t *testing.T, dir string, before checkoutSnapshot) {
	t.Helper()
	files := make([]string, 0, len(before.content))
	for f := range before.content {
		files = append(files, f)
	}
	after := snapshotCheckout(t, dir, files...)
	if after.head != before.head {
		t.Fatalf("main checkout HEAD changed: %s -> %s", before.head, after.head)
	}
	if after.branch != before.branch {
		t.Fatalf("main checkout branch changed: %q -> %q", before.branch, after.branch)
	}
	if after.status != before.status {
		t.Fatalf("main checkout status changed:\nbefore: %q\nafter:  %q", before.status, after.status)
	}
	for f, want := range before.content {
		if !bytes.Equal(after.content[f], want) {
			t.Fatalf("main checkout file %s changed:\nbefore: %q\nafter:  %q", f, want, after.content[f])
		}
	}
}

func assertNoTempWorktrees(t *testing.T, repoDir string) {
	t.Helper()
	out, err := runGitRaw(context.Background(), repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "konsert-merge-") {
		t.Fatalf("temp merge worktree was not removed:\n%s", out)
	}
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

	// PushBranch auto-commits dirty files inside a Konsert task worktree when CommitMessage is provided.
	res := PushBranch(ctx, wt, protocol.GitPushRequest{CommitMessage: "Add feature"})
	if !res.OK || res.Branch != "konsert/t-push" || res.RemoteURL != bare || res.HeadSHA == "" {
		t.Fatalf("PushBranch(origin) = %+v", res)
	}
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

func TestMergeBranchPreservesMainCheckoutAllStyles(t *testing.T) {
	ctx := context.Background()
	styles := []string{"squash", "merge", "rebase"}

	// 1. With remote, main checked out in user's checkout with uncommitted edit:
	//    pushed to origin/main, user's checkout completely untouched, LocalBehind reported.
	for _, style := range styles {
		t.Run("remote_main_checked_out_dirty_"+style, func(t *testing.T) {
			repo, bare := initTestRepo(t, true)
			wt, _, err := CreateWorktree(ctx, repo, "sid-"+style, "konsert/"+style)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, style+".txt"), []byte(style+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// User has uncommitted edits in their primary checkout on main.
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("user wip edit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("untracked wip\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			before := snapshotCheckout(t, repo, "README.md", "scratch.txt")

			res := MergeBranch(ctx, wt, protocol.GitMergeRequest{
				Style:         style,
				Push:          true,
				CommitMessage: "Deliver via " + style,
				TaskSID:       "sid-" + style,
			})
			if !res.OK || !res.Merged || !res.Pushed || res.Conflict || !res.LocalBehind || res.TargetBranch != "main" || res.MergeSHA == "" {
				t.Fatalf("MergeBranch(%s) = %+v", style, res)
			}
			wantMsg := "pushed to origin/main; your local main is behind — pull when ready"
			if res.Message != wantMsg {
				t.Fatalf("Message = %q, want %q", res.Message, wantMsg)
			}
			// Bare remote has the merged file.
			files, err := runGit(ctx, bare, "ls-tree", "-r", "--name-only", "main")
			if err != nil || !strings.Contains(files, style+".txt") {
				t.Fatalf("bare main files = %q, err = %v", files, err)
			}
			// User's primary checkout is byte-for-byte unchanged.
			assertCheckoutUnchanged(t, repo, before)
			assertNoTempWorktrees(t, repo)
		})
	}

	// 2. With remote, main NOT checked out (user is on a feature branch with uncommitted edit):
	//    pushed to origin/main, local refs/heads/main updated via update-ref, user's checkout untouched.
	for _, style := range styles {
		t.Run("remote_main_not_checked_out_"+style, func(t *testing.T) {
			repo, bare := initTestRepo(t, true)
			wt, _, err := CreateWorktree(ctx, repo, "sid-"+style, "konsert/"+style)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, style+".txt"), []byte(style+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// User switched their primary checkout to another branch and has uncommitted edits.
			if _, err := runGit(ctx, repo, "checkout", "-b", "user/wip"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("user branch edit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			before := snapshotCheckout(t, repo, "README.md")

			res := MergeBranch(ctx, wt, protocol.GitMergeRequest{
				Style:         style,
				Push:          true,
				CommitMessage: "Deliver via " + style,
				TaskSID:       "sid-" + style,
			})
			if !res.OK || !res.Merged || !res.Pushed || res.LocalBehind || res.MergeSHA == "" {
				t.Fatalf("MergeBranch(%s) = %+v", style, res)
			}
			// Both remote main and local refs/heads/main point to MergeSHA.
			bareSHA, _ := runGit(ctx, bare, "rev-parse", "refs/heads/main")
			localSHA, _ := runGit(ctx, repo, "rev-parse", "refs/heads/main")
			if bareSHA != res.MergeSHA || localSHA != res.MergeSHA {
				t.Fatalf("bareSHA=%s localSHA=%s want=%s", bareSHA, localSHA, res.MergeSHA)
			}
			assertCheckoutUnchanged(t, repo, before)
			assertNoTempWorktrees(t, repo)
		})
	}

	// 3. Without remote (local-only), main NOT checked out (user is on user/wip with uncommitted edit):
	//    refs/heads/main updated via update-ref, user's checkout byte-for-byte unchanged.
	for _, style := range styles {
		t.Run("local_main_not_checked_out_"+style, func(t *testing.T) {
			repo, _ := initTestRepo(t, false)
			wt, _, err := CreateWorktree(ctx, repo, "sid-"+style, "konsert/"+style)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, style+".txt"), []byte(style+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := runGit(ctx, repo, "checkout", "-b", "user/wip"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("user local edit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			before := snapshotCheckout(t, repo, "README.md")

			res := MergeBranch(ctx, wt, protocol.GitMergeRequest{
				Style:         style,
				Push:          false,
				CommitMessage: "Local " + style,
				TaskSID:       "sid-" + style,
			})
			if !res.OK || !res.Merged || res.Pushed || res.MergedBranch != "" || res.MergeSHA == "" {
				t.Fatalf("MergeBranch(%s) = %+v", style, res)
			}
			localSHA, _ := runGit(ctx, repo, "rev-parse", "refs/heads/main")
			if localSHA != res.MergeSHA {
				t.Fatalf("local main=%s want=%s", localSHA, res.MergeSHA)
			}
			files, _ := runGit(ctx, repo, "ls-tree", "-r", "--name-only", "refs/heads/main")
			if !strings.Contains(files, style+".txt") {
				t.Fatalf("local main missing %s.txt: %q", style, files)
			}
			assertCheckoutUnchanged(t, repo, before)
			assertNoTempWorktrees(t, repo)
		})
	}

	// 4. Without remote (local-only), main checked out and DIRTY:
	//    do NOT touch main checkout; leave result on konsert/merged/<sid>.
	for _, style := range styles {
		t.Run("local_main_checked_out_dirty_"+style, func(t *testing.T) {
			repo, _ := initTestRepo(t, false)
			sid := "T-" + style
			wt, _, err := CreateWorktree(ctx, repo, sid, "konsert/"+style)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, style+".txt"), []byte(style+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("dirty main edit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			before := snapshotCheckout(t, repo, "README.md")

			res := MergeBranch(ctx, wt, protocol.GitMergeRequest{
				Style:         style,
				Push:          false,
				CommitMessage: "Local dirty " + style,
				TaskSID:       sid,
			})
			wantBranch := "konsert/merged/" + sid
			wantMsg := "main is checked out with uncommitted changes; the merge is ready on branch " + wantBranch + " — fast-forward main when you're ready"
			if !res.OK || !res.Merged || res.Pushed || res.MergedBranch != wantBranch || res.Message != wantMsg || res.MergeSHA == "" {
				t.Fatalf("MergeBranch(%s) = %+v", style, res)
			}
			branchSHA, err := runGit(ctx, repo, "rev-parse", "--verify", "refs/heads/"+wantBranch)
			if err != nil || branchSHA != res.MergeSHA {
				t.Fatalf("%s SHA=%s err=%v want=%s", wantBranch, branchSHA, err, res.MergeSHA)
			}
			files, _ := runGit(ctx, repo, "ls-tree", "-r", "--name-only", wantBranch)
			if !strings.Contains(files, style+".txt") {
				t.Fatalf("%s missing %s.txt: %q", wantBranch, style, files)
			}
			assertCheckoutUnchanged(t, repo, before)
			assertNoTempWorktrees(t, repo)
		})
	}
}

func TestMergeBranchLocalOnlyCheckedOutAndCleanFastForwards(t *testing.T) {
	ctx := context.Background()
	for _, style := range []string{"squash", "merge", "rebase"} {
		t.Run(style, func(t *testing.T) {
			repo, _ := initTestRepo(t, false)
			wt, _, err := CreateWorktree(ctx, repo, "t-clean-"+style, "konsert/"+style)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, style+".txt"), []byte("ok\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			res := MergeBranch(ctx, wt, protocol.GitMergeRequest{
				Style:         style,
				Push:          false,
				CommitMessage: "Local clean " + style,
			})
			if !res.OK || !res.Merged || res.Pushed || res.MergedBranch != "" || res.MergeSHA == "" {
				t.Fatalf("MergeBranch(%s) = %+v", style, res)
			}
			head, _ := CurrentSHA(ctx, repo)
			if head != res.MergeSHA {
				t.Fatalf("main checkout HEAD=%s want=%s", head, res.MergeSHA)
			}
			if _, err := os.Stat(filepath.Join(repo, style+".txt")); err != nil {
				t.Fatalf("merged file missing in clean main checkout: %v", err)
			}
			assertNoTempWorktrees(t, repo)
		})
	}
}

func TestMergeBranchNonFastForwardPushRaceRetry(t *testing.T) {
	ctx := context.Background()

	pushConcurrentCommit := func(t *testing.T, bareDir, filename, content string) {
		t.Helper()
		cloneDir, err := os.MkdirTemp("/var/tmp", "konsert-race-clone-*")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(cloneDir) })
		if _, err := runGit(ctx, cloneDir, "clone", bareDir, "."); err != nil {
			t.Fatal(err)
		}
		_, _ = runGit(ctx, cloneDir, "config", "user.name", "Other Dev")
		_, _ = runGit(ctx, cloneDir, "config", "user.email", "other@example.invalid")
		if err := os.WriteFile(filepath.Join(cloneDir, filename), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runGit(ctx, cloneDir, "add", filename); err != nil {
			t.Fatal(err)
		}
		if _, err := runGit(ctx, cloneDir, "commit", "-m", "concurrent "+filename); err != nil {
			t.Fatal(err)
		}
		if _, err := runGit(ctx, cloneDir, "push", "origin", "main"); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("retries_once_and_succeeds", func(t *testing.T) {
		repo, bare := initTestRepo(t, true)
		wt, _, err := CreateWorktree(ctx, repo, "t-race", "konsert/race")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, "task.txt"), []byte("from task\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("untouched\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := snapshotCheckout(t, repo, "README.md")

		attempts := 0
		prev := beforeDirectPushHook
		beforeDirectPushHook = func(attempt int) {
			attempts++
			if attempt == 0 {
				pushConcurrentCommit(t, bare, "concurrent.txt", "from teammate\n")
			}
		}
		t.Cleanup(func() { beforeDirectPushHook = prev })

		res := MergeBranch(ctx, wt, protocol.GitMergeRequest{
			Style:         "squash",
			Push:          true,
			CommitMessage: "Deliver with race",
		})
		if !res.OK || !res.Merged || !res.Pushed || !res.LocalBehind {
			t.Fatalf("MergeBranch race = %+v", res)
		}
		if attempts != 2 {
			t.Fatalf("push attempts = %d, want 2", attempts)
		}
		files, err := runGit(ctx, bare, "ls-tree", "-r", "--name-only", "main")
		if err != nil || !strings.Contains(files, "task.txt") || !strings.Contains(files, "concurrent.txt") {
			t.Fatalf("bare main missing expected files: %q (err=%v)", files, err)
		}
		assertCheckoutUnchanged(t, repo, before)
		assertNoTempWorktrees(t, repo)
	})

	t.Run("fails_after_single_retry", func(t *testing.T) {
		repo, bare := initTestRepo(t, true)
		wt, _, err := CreateWorktree(ctx, repo, "t-race2", "konsert/race2")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, "task.txt"), []byte("from task\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		attempts := 0
		prev := beforeDirectPushHook
		beforeDirectPushHook = func(attempt int) {
			attempts++
			pushConcurrentCommit(t, bare, "concurrent-"+string(rune('a'+attempt))+".txt", "race\n")
		}
		t.Cleanup(func() { beforeDirectPushHook = prev })

		res := MergeBranch(ctx, wt, protocol.GitMergeRequest{
			Style:         "squash",
			Push:          true,
			CommitMessage: "Deliver with double race",
		})
		if res.OK || res.Pushed {
			t.Fatalf("expected failure after retry exhausted, got %+v", res)
		}
		if attempts != 2 {
			t.Fatalf("push attempts = %d, want 2", attempts)
		}
		assertNoTempWorktrees(t, repo)
	})
}

func TestMergeBranchConflictIsReportedAbortedAndCleansTempWorktree(t *testing.T) {
	ctx := context.Background()
	for _, withRemote := range []bool{false, true} {
		name := "local"
		if withRemote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			repo, _ := initTestRepo(t, withRemote)
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
			if withRemote {
				if _, err := runGit(ctx, repo, "push", "origin", "main"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("task change\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := runGit(ctx, wt, "commit", "-am", "task edit"); err != nil {
				t.Fatal(err)
			}
			// Also add an uncommitted edit in the main checkout to verify it is untouched on conflict.
			if err := os.WriteFile(filepath.Join(repo, "wip.txt"), []byte("keep me\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			before := snapshotCheckout(t, repo, "README.md", "wip.txt")

			for _, style := range []string{"squash", "merge", "rebase"} {
				res := MergeBranch(ctx, wt, protocol.GitMergeRequest{
					Style:         style,
					Push:          withRemote,
					CommitMessage: "Should conflict",
				})
				if res.OK || !res.Conflict || len(res.ConflictFiles) == 0 || res.ConflictFiles[0] != "README.md" {
					t.Fatalf("MergeBranch(%s) conflict = %+v", style, res)
				}
				assertCheckoutUnchanged(t, repo, before)
				assertNoTempWorktrees(t, repo)
			}
		})
	}
}

func TestRefuseAutoCommitOutsideTaskWorktree(t *testing.T) {
	ctx := context.Background()

	t.Run("push_refuses_dirty_main_checkout", func(t *testing.T) {
		repo, _ := initTestRepo(t, true)
		if _, err := runGit(ctx, repo, "checkout", "-b", "feat/manual"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("uncommitted\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := snapshotCheckout(t, repo, "README.md")

		res := PushBranch(ctx, repo, protocol.GitPushRequest{CommitMessage: "Should refuse"})
		if res.OK || !strings.Contains(res.Error, "outside a Konsert task worktree") {
			t.Fatalf("PushBranch on main checkout = %+v, want refusal", res)
		}
		assertCheckoutUnchanged(t, repo, before)
	})

	t.Run("merge_refuses_dirty_non_task_branch", func(t *testing.T) {
		repo, _ := initTestRepo(t, false)
		if _, err := runGit(ctx, repo, "checkout", "-b", "feat/manual"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("uncommitted\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := snapshotCheckout(t, repo, "README.md")

		res := MergeBranch(ctx, repo, protocol.GitMergeRequest{
			Style:         "squash",
			CommitMessage: "Should refuse",
		})
		if res.OK || !strings.Contains(res.Error, "outside a Konsert task worktree") {
			t.Fatalf("MergeBranch on dirty non-task checkout = %+v, want refusal", res)
		}
		assertCheckoutUnchanged(t, repo, before)
	})

	t.Run("merge_refuses_same_branch_in_user_checkout", func(t *testing.T) {
		repo, bare := initTestRepo(t, true)
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("direct edit on main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := snapshotCheckout(t, repo, "README.md")
		bareBefore, _ := runGit(ctx, bare, "rev-parse", "refs/heads/main")

		res := MergeBranch(ctx, repo, protocol.GitMergeRequest{
			Style:         "squash",
			Push:          true,
			CommitMessage: "Should not commit or push",
		})
		if res.OK || res.Merged || res.Pushed || !strings.Contains(res.Error, "not committed or pushed automatically") {
			t.Fatalf("MergeBranch on user main checkout = %+v, want refusal", res)
		}
		assertCheckoutUnchanged(t, repo, before)
		bareAfter, _ := runGit(ctx, bare, "rev-parse", "refs/heads/main")
		if bareAfter != bareBefore {
			t.Fatalf("bare remote changed: %s -> %s", bareBefore, bareAfter)
		}
	})
}

func TestMissingGitIdentityFailsClearly(t *testing.T) {
	ctx := context.Background()
	base, err := os.MkdirTemp("/var/tmp", "konsert-noident-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	// Isolate from the developer's global/system git config.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(base, "empty-gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repo, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repo, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	// Pass -c only for the seed commit so the repo itself has no configured identity.
	if _, err := runGit(ctx, repo, "-c", "user.name=Seed", "-c", "user.email=seed@example.invalid", "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	wt, _, err := CreateWorktree(ctx, repo, "t-noident", "konsert/noident")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Auto-commit during PushBranch fails clearly.
	pRes := PushBranch(ctx, wt, protocol.GitPushRequest{CommitMessage: "Try commit"})
	if pRes.OK || !strings.Contains(pRes.Error, "set git user.name and user.email on") || !strings.Contains(pRes.Error, "to let Konsert commit") {
		t.Fatalf("PushBranch without identity = %+v", pRes)
	}

	// Commit with explicit -c so we can also test MergeBranch's own merge/squash/rebase commits.
	if _, err := runGit(ctx, wt, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, wt, "-c", "user.name=Seed", "-c", "user.email=seed@example.invalid", "commit", "-m", "task work"); err != nil {
		t.Fatal(err)
	}

	for _, style := range []string{"squash", "merge", "rebase"} {
		mRes := MergeBranch(ctx, wt, protocol.GitMergeRequest{
			Style:         style,
			Push:          false,
			CommitMessage: "Try " + style,
		})
		if mRes.OK || !strings.Contains(mRes.Error, "set git user.name and user.email on") || !strings.Contains(mRes.Error, "to let Konsert commit") {
			t.Fatalf("MergeBranch(%s) without identity = %+v", style, mRes)
		}
		assertNoTempWorktrees(t, repo)
	}
}
