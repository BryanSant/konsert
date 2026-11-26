package gitutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// beforeDirectPushHook is an optional test hook invoked immediately before each
// direct-merge push attempt so tests can deterministically simulate a concurrent
// non-fast-forward push race without sleeps.
var beforeDirectPushHook func(attempt int)

// RepoGitInfo inspects the git checkout at dir and returns its branch, default branch,
// primary remote URL, and dirty state.
func RepoGitInfo(ctx context.Context, dir string) protocol.GitInfoResult {
	res := protocol.GitInfoResult{}
	if _, ok := IsGitRepo(ctx, dir); !ok {
		res.Error = "directory is not a git repository"
		return res
	}
	res.OK = true
	res.IsGitRepo = true

	if br, err := runGit(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD"); err == nil && br != "" {
		res.Branch = br
	} else if br, err := runGit(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && br != "" && br != "HEAD" {
		res.Branch = br
	}

	remoteName, remoteURL := primaryRemote(ctx, dir)
	res.RemoteName = remoteName
	res.RemoteURL = remoteURL
	res.HasRemote = remoteURL != ""

	res.DefaultBranch = detectDefaultBranch(ctx, dir, remoteName)

	if st, err := runGit(ctx, dir, "status", "--porcelain"); err == nil && st != "" {
		res.Dirty = true
	}
	return res
}

// primaryRemote returns "origin" if configured, else the first remote's name and URL.
func primaryRemote(ctx context.Context, dir string) (name, url string) {
	if u, err := runGit(ctx, dir, "remote", "get-url", "origin"); err == nil && u != "" {
		return "origin", u
	}
	remotes, err := runGit(ctx, dir, "remote")
	if err != nil || remotes == "" {
		return "", ""
	}
	for line := range strings.SplitSeq(remotes, "\n") {
		r := strings.TrimSpace(line)
		if r == "" {
			continue
		}
		if u, err := runGit(ctx, dir, "remote", "get-url", r); err == nil && u != "" {
			return r, u
		}
	}
	return "", ""
}

// detectDefaultBranch determines the repository's default branch.
func detectDefaultBranch(ctx context.Context, dir, remoteName string) string {
	if remoteName != "" {
		if ref, err := runGit(ctx, dir, "symbolic-ref", "--short", "-q", "refs/remotes/"+remoteName+"/HEAD"); err == nil && ref != "" {
			return strings.TrimPrefix(ref, remoteName+"/")
		}
	}
	for _, cand := range []string{"main", "master"} {
		if _, err := runGit(ctx, dir, "show-ref", "--verify", "--quiet", "refs/heads/"+cand); err == nil {
			return cand
		}
	}
	if root := mainRepoRoot(ctx, dir); root != "" && root != dir {
		if br, err := runGit(ctx, root, "symbolic-ref", "--short", "-q", "HEAD"); err == nil && br != "" {
			return br
		}
	}
	if br, err := runGit(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD"); err == nil && br != "" {
		return br
	}
	return "main"
}

// mainRepoRoot returns the main repository checkout directory for dir (which may be a
// linked worktree).
func mainRepoRoot(ctx context.Context, dir string) string {
	common, err := runGit(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil || common == "" {
		root, _ := IsGitRepo(ctx, dir)
		return root
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	common = filepath.Clean(common)
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	root, _ := IsGitRepo(ctx, dir)
	return root
}

// IsSyreTaskWorktree reports whether dir is inside a Syre-managed linked git
// worktree at <repoRoot>/.syre-worktrees/<taskID>.
func IsSyreTaskWorktree(ctx context.Context, dir string) bool {
	top, ok := IsGitRepo(ctx, dir)
	if !ok || top == "" {
		return false
	}
	root := mainRepoRoot(ctx, dir)
	if root == "" {
		return false
	}
	topResolved, err := filepath.EvalSymlinks(top)
	if err != nil {
		topResolved = filepath.Clean(top)
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		rootResolved = filepath.Clean(root)
	}
	if topResolved == rootResolved {
		return false
	}
	wtParent := filepath.Join(rootResolved, ".syre-worktrees")
	if filepath.Dir(topResolved) != wtParent {
		return false
	}
	fi, err := os.Stat(filepath.Join(topResolved, ".git"))
	return err == nil && !fi.IsDir()
}

// requireGitIdentity ensures git user.name and user.email are configured for dir.
// Syre never invents a fallback identity.
func requireGitIdentity(ctx context.Context, dir string) error {
	name, _ := runGit(ctx, dir, "config", "user.name")
	email, _ := runGit(ctx, dir, "config", "user.email")
	if strings.TrimSpace(name) == "" || strings.TrimSpace(email) == "" {
		host, err := os.Hostname()
		if err != nil || strings.TrimSpace(host) == "" {
			host = "this machine"
		}
		return fmt.Errorf("git identity not configured: set git user.name and user.email on %s to let Syre commit", host)
	}
	return nil
}

// commitIfDirty stages and commits any uncommitted changes in dir when msg is non-empty.
// It refuses to auto-commit outside a Syre-managed task worktree.
func commitIfDirty(ctx context.Context, dir, msg string) error {
	if strings.TrimSpace(msg) == "" {
		return nil
	}
	st, err := runGit(ctx, dir, "status", "--porcelain")
	if err != nil || st == "" {
		return nil
	}
	if !IsSyreTaskWorktree(ctx, dir) {
		return fmt.Errorf("refusing to auto-commit uncommitted changes outside a Syre task worktree (%s); commit your changes manually", dir)
	}
	if err := requireGitIdentity(ctx, dir); err != nil {
		return err
	}
	if _, err := runGit(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	_, err = runGitAs(ctx, dir, "commit", "-m", msg)
	return err
}

// PushBranch pushes the checkout's branch to Remote (default "origin") or RemoteURL.
// If req.CommitMessage is non-empty and the working tree is dirty, it stages and commits
// the changes first (only inside a Syre-managed task worktree).
func PushBranch(ctx context.Context, dir string, req protocol.GitPushRequest) protocol.GitPushResult {
	res := protocol.GitPushResult{}
	if _, ok := IsGitRepo(ctx, dir); !ok {
		res.Error = "directory is not a git repository"
		return res
	}
	if err := commitIfDirty(ctx, dir, req.CommitMessage); err != nil {
		res.Error = fmt.Sprintf("commit uncommitted changes: %v", err)
		return res
	}
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		var err error
		branch, err = runGit(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
		if err != nil || branch == "" {
			res.Error = "checkout is not on a branch (detached HEAD)"
			return res
		}
	}
	res.Branch = branch
	if sha, err := CurrentSHA(ctx, dir); err == nil {
		res.HeadSHA = sha
	}

	target := strings.TrimSpace(req.RemoteURL)
	remote := strings.TrimSpace(req.Remote)
	if remote == "" {
		remote, res.RemoteURL = primaryRemote(ctx, dir)
	} else {
		res.RemoteURL, _ = runGit(ctx, dir, "remote", "get-url", remote)
	}

	// When retargeting a stacked branch onto base after its blockers merged, rebase
	// commits after StackBaseSHA onto RebaseOnto before pushing with --force-with-lease.
	rebased := false
	rebaseOnto := strings.TrimSpace(req.RebaseOnto)
	stackBase := strings.TrimSpace(req.StackBaseSHA)
	if rebaseOnto != "" && stackBase != "" {
		if remote != "" {
			_, _ = runGit(ctx, dir, "fetch", remote, rebaseOnto)
		}
		var ontoSHA string
		for _, cand := range []string{
			"refs/remotes/" + remote + "/" + rebaseOnto,
			"refs/heads/" + rebaseOnto,
			rebaseOnto,
		} {
			if remote == "" && strings.HasPrefix(cand, "refs/remotes/") {
				continue
			}
			if s, err := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", cand+"^{commit}"); err == nil && s != "" {
				ontoSHA = s
				break
			}
		}
		if ontoSHA != "" && stackBase != ontoSHA {
			if _, ancErr := runGit(ctx, dir, "merge-base", "--is-ancestor", stackBase, "HEAD"); ancErr == nil {
				if err := requireGitIdentity(ctx, dir); err != nil {
					res.Error = err.Error()
					return res
				}
				if _, err := runGitAs(ctx, dir, "rebase", "--onto", ontoSHA, stackBase); err != nil {
					_, files := conflictFiles(ctx, dir)
					_, _ = runGit(ctx, dir, "rebase", "--abort")
					if len(files) > 0 {
						res.Error = fmt.Sprintf("rebase conflict retargeting %s onto %s (%s)", branch, rebaseOnto, strings.Join(files, ", "))
					} else {
						res.Error = fmt.Sprintf("rebase %s onto %s: %v", branch, rebaseOnto, err)
					}
					return res
				}
				rebased = true
				if sha, err := CurrentSHA(ctx, dir); err == nil {
					res.HeadSHA = sha
				}
			}
		}
	}

	if target != "" {
		res.RemoteURL = target
		pushArgs := []string{"push"}
		if rebased {
			pushArgs = append(pushArgs, "--force-with-lease")
		}
		pushArgs = append(pushArgs, target, "HEAD:refs/heads/"+branch)
		if _, err := runGit(ctx, dir, pushArgs...); err != nil {
			res.Error = err.Error()
			return res
		}
		res.OK = true
		return res
	}

	if remote == "" {
		res.Error = "no git remote configured"
		return res
	}
	pushArgs := []string{"push", "-u"}
	if rebased {
		pushArgs = append(pushArgs, "--force-with-lease")
	}
	pushArgs = append(pushArgs, remote, branch)
	if _, err := runGit(ctx, dir, pushArgs...); err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	return res
}

// MergeBranch merges SourceBranch into TargetBranch using Style ("squash", "merge", or
// "rebase") and optionally pushes TargetBranch to Remote.
//
// Every merge runs inside an ephemeral detached worktree that is always removed afterwards.
// The user's primary checkout is never checked out, modified, or auto-committed.
func MergeBranch(ctx context.Context, dir string, req protocol.GitMergeRequest) protocol.GitMergeResult {
	res := protocol.GitMergeResult{}
	if _, ok := IsGitRepo(ctx, dir); !ok {
		res.Error = "directory is not a git repository"
		return res
	}
	style := strings.TrimSpace(req.Style)
	if style == "" {
		style = "squash"
	}
	if style != "squash" && style != "merge" && style != "rebase" {
		res.Error = fmt.Sprintf("unsupported merge style %q", style)
		return res
	}

	source := strings.TrimSpace(req.SourceBranch)
	if source == "" {
		var err error
		source, err = runGit(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
		if err != nil || source == "" {
			res.Error = "checkout is not on a branch (detached HEAD)"
			return res
		}
	}

	remote := strings.TrimSpace(req.Remote)
	var remoteURL string
	if remote == "" {
		remote, remoteURL = primaryRemote(ctx, dir)
	} else {
		remoteURL, _ = runGit(ctx, dir, "remote", "get-url", remote)
	}
	hasRemote := remote != "" && remoteURL != ""

	target := strings.TrimSpace(req.TargetBranch)
	if target == "" {
		target = detectDefaultBranch(ctx, dir, remote)
	}
	res.TargetBranch = target

	// Never auto-commit or push when a task ran directly in the user's own checkout on the
	// target branch: leave it untouched and tell the user to commit their changes manually.
	if source == target {
		if !IsSyreTaskWorktree(ctx, dir) {
			res.Error = fmt.Sprintf("changes are in your checkout (%s) on %s and were not committed or pushed automatically; commit them manually", dir, target)
			return res
		}
		res.Error = fmt.Sprintf("source branch and target branch are both %s; nothing to merge", target)
		return res
	}

	msg := strings.TrimSpace(req.CommitMessage)
	if msg == "" {
		msg = "Merge task branch"
	}
	if err := commitIfDirty(ctx, dir, msg); err != nil {
		res.Error = fmt.Sprintf("commit uncommitted changes: %v", err)
		return res
	}

	root := mainRepoRoot(ctx, dir)
	if root == "" {
		root = dir
	}

	sourceSHA, err := runGit(ctx, root, "rev-parse", "--verify", "refs/heads/"+source)
	if err != nil || sourceSHA == "" {
		sourceSHA, err = runGit(ctx, dir, "rev-parse", "--verify", source)
		if err != nil || sourceSHA == "" {
			res.Error = fmt.Sprintf("resolve source branch %s: %v", source, err)
			return res
		}
	}

	stackBaseSHA := strings.TrimSpace(req.StackBaseSHA)
	if req.Push && hasRemote {
		return mergeWithRemote(ctx, root, source, sourceSHA, target, remote, style, msg, stackBaseSHA)
	}
	sid := resolveMergeSID(req.TaskSID, dir, source)
	return mergeLocalOnly(ctx, root, source, sourceSHA, target, style, msg, sid, stackBaseSHA)
}

func mergeWithRemote(ctx context.Context, root, source, sourceSHA, target, remote, style, msg, stackBaseSHA string) protocol.GitMergeResult {
	res := protocol.GitMergeResult{TargetBranch: target}

	const maxAttempts = 2
	for attempt := range maxAttempts {
		if _, err := runGit(ctx, root, "fetch", remote, target); err != nil {
			res.Error = fmt.Sprintf("fetch %s/%s: %v", remote, target, err)
			return res
		}
		baseSHA, err := runGit(ctx, root, "rev-parse", "--verify", "refs/remotes/"+remote+"/"+target)
		if err != nil || baseSHA == "" {
			baseSHA, err = runGit(ctx, root, "rev-parse", "--verify", "FETCH_HEAD")
			if err != nil || baseSHA == "" {
				res.Error = fmt.Sprintf("resolve %s/%s: %v", remote, target, err)
				return res
			}
		}

		newSHA, mergeRes, pushErr := runMergeInTempWorktree(ctx, root, source, sourceSHA, target, baseSHA, style, msg, stackBaseSHA, func(tmpWT string) error {
			if beforeDirectPushHook != nil {
				beforeDirectPushHook(attempt)
			}
			_, err := runGit(ctx, tmpWT, "push", remote, "HEAD:refs/heads/"+target)
			return err
		})
		if mergeRes != nil {
			return *mergeRes
		}
		if pushErr != nil {
			if attempt+1 < maxAttempts && isNonFastForwardError(pushErr) {
				continue
			}
			res.Error = fmt.Sprintf("push %s to %s/%s failed: %v", source, remote, target, pushErr)
			return res
		}

		// Keep remote-tracking ref up to date.
		_, _ = runGit(ctx, root, "update-ref", "refs/remotes/"+remote+"/"+target, newSHA)

		res.MergeSHA = newSHA
		res.Merged = true
		res.Pushed = true

		wtPath, _ := findBranchWorktree(ctx, root, target)
		if wtPath != "" {
			// Local target is checked out in a worktree (e.g. the user's primary checkout).
			// Never touch it.
			oldLocal, _ := runGit(ctx, root, "rev-parse", "--verify", "refs/heads/"+target)
			if oldLocal != newSHA {
				res.LocalBehind = true
				res.Message = fmt.Sprintf("pushed to %s/%s; your local %s is behind — pull when ready", remote, target, target)
			}
		} else {
			updateLocalRefIfNotCheckedOut(ctx, root, target, newSHA)
		}
		res.OK = true
		return res
	}

	res.Error = fmt.Sprintf("push to %s/%s failed after retry", remote, target)
	return res
}

func mergeLocalOnly(ctx context.Context, root, source, sourceSHA, target, style, msg, sid, stackBaseSHA string) protocol.GitMergeResult {
	res := protocol.GitMergeResult{TargetBranch: target}

	oldTargetSHA, err := runGit(ctx, root, "rev-parse", "--verify", "refs/heads/"+target)
	if err != nil || oldTargetSHA == "" {
		res.Error = fmt.Sprintf("resolve local target branch %s: %v", target, err)
		return res
	}

	newSHA, mergeRes, _ := runMergeInTempWorktree(ctx, root, source, sourceSHA, target, oldTargetSHA, style, msg, stackBaseSHA, nil)
	if mergeRes != nil {
		return *mergeRes
	}

	wtPath, err := findBranchWorktree(ctx, root, target)
	if err != nil {
		res.Error = fmt.Sprintf("inspect worktrees: %v", err)
		return res
	}

	if wtPath == "" {
		// Target branch is not checked out anywhere: safely advance refs/heads/<target>
		// guarded by the old SHA.
		if _, err := runGit(ctx, root, "update-ref", "refs/heads/"+target, newSHA, oldTargetSHA); err != nil {
			res.Error = fmt.Sprintf("update %s: %v", target, err)
			return res
		}
		res.OK = true
		res.Merged = true
		res.MergeSHA = newSHA
		return res
	}

	// Target branch is checked out in wtPath. Check whether wtPath is clean.
	clean, err := isCheckoutClean(ctx, wtPath)
	if err != nil {
		res.Error = fmt.Sprintf("inspect %s checkout: %v", target, err)
		return res
	}
	if clean {
		// Clean checkout: fast-forward it in place.
		if _, err := runGit(ctx, wtPath, "merge", "--ff-only", newSHA); err != nil {
			res.Error = fmt.Sprintf("fast-forward %s: %v", target, err)
			return res
		}
		res.OK = true
		res.Merged = true
		res.MergeSHA = newSHA
		return res
	}

	// Dirty checkout: do NOT touch wtPath. Save the merge on syre/merged/<sid>.
	mergedBranch := "syre/merged/" + sid
	if _, err := runGit(ctx, root, "update-ref", "refs/heads/"+mergedBranch, newSHA); err != nil {
		res.Error = fmt.Sprintf("create %s: %v", mergedBranch, err)
		return res
	}
	res.OK = true
	res.Merged = true
	res.MergeSHA = newSHA
	res.MergedBranch = mergedBranch
	res.Message = fmt.Sprintf("%s is checked out with uncommitted changes; the merge is ready on branch %s — fast-forward %s when you're ready", target, mergedBranch, target)
	return res
}

// runMergeInTempWorktree creates an ephemeral detached worktree, performs the requested
// merge style (squash/merge/rebase), runs optional afterMerge(tmpWT) (such as git push),
// and unconditionally removes the temp worktree before returning.
func runMergeInTempWorktree(
	ctx context.Context,
	root, source, sourceSHA, target, baseSHA, style, msg, stackBaseSHA string,
	afterMerge func(tmpWT string) error,
) (newSHA string, earlyRes *protocol.GitMergeResult, afterErr error) {
	tmpParent, err := os.MkdirTemp(tempBaseDir(), "syre-merge-*")
	if err != nil {
		r := protocol.GitMergeResult{TargetBranch: target, Error: fmt.Sprintf("create temp merge dir: %v", err)}
		return "", &r, nil
	}
	tmpWT := filepath.Join(tmpParent, "wt")
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = runGit(cleanCtx, root, "worktree", "remove", "--force", tmpWT)
		_, _ = runGit(cleanCtx, root, "worktree", "prune")
		_ = os.RemoveAll(tmpParent)
	}()

	// When delivering a stacked branch whose blockers have already merged into target,
	// replay only the commits after stackBaseSHA onto baseSHA first so squash-merged
	// blocker commits do not conflict or duplicate.
	retarget := false
	if stackBaseSHA != "" && stackBaseSHA != baseSHA {
		if _, ancErr := runGit(ctx, root, "merge-base", "--is-ancestor", stackBaseSHA, sourceSHA); ancErr == nil {
			retarget = true
		}
	}

	startPoint := baseSHA
	if style == "rebase" || retarget {
		startPoint = sourceSHA
	}
	if _, err := runGit(ctx, root, "worktree", "add", "--detach", tmpWT, startPoint); err != nil {
		r := protocol.GitMergeResult{TargetBranch: target, Error: fmt.Sprintf("create temp merge worktree: %v", err)}
		return "", &r, nil
	}

	retargeted := false
	if retarget {
		if err := requireGitIdentity(ctx, tmpWT); err != nil {
			r := protocol.GitMergeResult{TargetBranch: target, Error: err.Error()}
			return "", &r, nil
		}
		if _, err := runGitAs(ctx, tmpWT, "rebase", "--onto", baseSHA, stackBaseSHA); err != nil {
			res := protocol.GitMergeResult{TargetBranch: target}
			res.Conflict, res.ConflictFiles = conflictFiles(ctx, tmpWT)
			_, _ = runGit(ctx, tmpWT, "rebase", "--abort")
			if res.Conflict {
				res.Error = fmt.Sprintf("rebase conflict retargeting %s onto %s (%s)", source, target, strings.Join(res.ConflictFiles, ", "))
			} else {
				res.Error = err.Error()
			}
			return "", &res, nil
		}
		rebasedSHA, err := CurrentSHA(ctx, tmpWT)
		if err != nil || rebasedSHA == "" {
			r := protocol.GitMergeResult{TargetBranch: target, Error: "could not determine rebased commit SHA"}
			return "", &r, nil
		}
		sourceSHA = rebasedSHA
		retargeted = true
		if style != "rebase" {
			if _, err := runGit(ctx, tmpWT, "checkout", "--detach", baseSHA); err != nil {
				r := protocol.GitMergeResult{TargetBranch: target, Error: fmt.Sprintf("checkout base after retarget: %v", err)}
				return "", &r, nil
			}
		}
	}

	switch style {
	case "squash":
		if _, err := runGit(ctx, tmpWT, "merge", "--squash", sourceSHA); err != nil {
			res := protocol.GitMergeResult{TargetBranch: target}
			res.Conflict, res.ConflictFiles = conflictFiles(ctx, tmpWT)
			_, _ = runGit(ctx, tmpWT, "reset", "--merge")
			if res.Conflict {
				res.Error = fmt.Sprintf("squash-merge conflict merging %s into %s (%s)", source, target, strings.Join(res.ConflictFiles, ", "))
			} else {
				res.Error = err.Error()
			}
			return "", &res, nil
		}
		if st, _ := runGit(ctx, tmpWT, "diff", "--cached", "--name-only"); st != "" {
			if err := commitStaged(ctx, tmpWT, msg); err != nil {
				r := protocol.GitMergeResult{TargetBranch: target, Error: fmt.Sprintf("commit squash merge: %v", err)}
				return "", &r, nil
			}
		}

	case "merge":
		if err := requireGitIdentity(ctx, tmpWT); err != nil {
			r := protocol.GitMergeResult{TargetBranch: target, Error: err.Error()}
			return "", &r, nil
		}
		if _, err := runGitAs(ctx, tmpWT, "merge", "--no-ff", "-m", msg, sourceSHA); err != nil {
			res := protocol.GitMergeResult{TargetBranch: target}
			res.Conflict, res.ConflictFiles = conflictFiles(ctx, tmpWT)
			_, _ = runGit(ctx, tmpWT, "merge", "--abort")
			if res.Conflict {
				res.Error = fmt.Sprintf("merge conflict merging %s into %s (%s)", source, target, strings.Join(res.ConflictFiles, ", "))
			} else {
				res.Error = err.Error()
			}
			return "", &res, nil
		}

	case "rebase":
		if !retargeted {
			if err := requireGitIdentity(ctx, tmpWT); err != nil {
				r := protocol.GitMergeResult{TargetBranch: target, Error: err.Error()}
				return "", &r, nil
			}
			if _, err := runGitAs(ctx, tmpWT, "rebase", baseSHA); err != nil {
				res := protocol.GitMergeResult{TargetBranch: target}
				res.Conflict, res.ConflictFiles = conflictFiles(ctx, tmpWT)
				_, _ = runGit(ctx, tmpWT, "rebase", "--abort")
				if res.Conflict {
					res.Error = fmt.Sprintf("rebase conflict rebasing %s onto %s (%s)", source, target, strings.Join(res.ConflictFiles, ", "))
				} else {
					res.Error = err.Error()
				}
				return "", &res, nil
			}
		}
	}

	newSHA, err = CurrentSHA(ctx, tmpWT)
	if err != nil || newSHA == "" {
		r := protocol.GitMergeResult{TargetBranch: target, Error: "could not determine merge commit SHA"}
		return "", &r, nil
	}

	if afterMerge != nil {
		if err := afterMerge(tmpWT); err != nil {
			return newSHA, nil, err
		}
	}
	return newSHA, nil, nil
}

func updateLocalRefIfNotCheckedOut(ctx context.Context, root, target, newSHA string) {
	oldLocal, err := runGit(ctx, root, "rev-parse", "--verify", "refs/heads/"+target)
	if err != nil || oldLocal == "" {
		_, _ = runGit(ctx, root, "update-ref", "refs/heads/"+target, newSHA, strings.Repeat("0", 40))
		return
	}
	if oldLocal == newSHA {
		return
	}
	// Only advance if oldLocal is an ancestor of newSHA (never discard unpushed local commits).
	if _, err := runGit(ctx, root, "merge-base", "--is-ancestor", oldLocal, newSHA); err == nil {
		_, _ = runGit(ctx, root, "update-ref", "refs/heads/"+target, newSHA, oldLocal)
	}
}

// findBranchWorktree returns the worktree path where refs/heads/<branch> is currently
// checked out, or "" if it is not checked out in any worktree.
func findBranchWorktree(ctx context.Context, dir, branch string) (string, error) {
	out, err := runGitRaw(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	wantRef := "refs/heads/" + branch
	var curPath string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			curPath = ""
			continue
		}
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			curPath = p
		} else if br, ok := strings.CutPrefix(line, "branch "); ok {
			if br == wantRef && curPath != "" {
				return curPath, nil
			}
		}
	}
	return "", nil
}

// isCheckoutClean reports whether dir has no uncommitted changes, ignoring Syre's
// own untracked .syre-worktrees/ directory.
func isCheckoutClean(ctx context.Context, dir string) (bool, error) {
	st, err := runGitRaw(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(st, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if len(line) >= 3 && line[:2] == "??" {
			p := strings.TrimSpace(line[3:])
			if p == ".syre-worktrees" || p == ".syre-worktrees/" || strings.HasPrefix(p, ".syre-worktrees/") {
				continue
			}
		}
		return false, nil
	}
	return true, nil
}

func isNonFastForwardError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "fetch first") ||
		strings.Contains(msg, "[rejected]")
}

func resolveMergeSID(taskSID, dir, source string) string {
	sid := strings.TrimSpace(taskSID)
	if sid == "" {
		sid = filepath.Base(filepath.Clean(dir))
	}
	if sid == "" || sid == "." || sid == string(filepath.Separator) {
		sid = source
	}
	sid = strings.ReplaceAll(sid, "/", "-")
	sid = strings.ReplaceAll(sid, "\\", "-")
	sid = strings.ReplaceAll(sid, " ", "-")
	return sid
}

func tempBaseDir() string {
	// /var/tmp only on Unix: on Windows "/var/tmp" resolves to a drive-relative path
	// (C:\var\tmp may even exist) that git cannot use.
	if runtime.GOOS == "windows" {
		return os.TempDir()
	}
	if fi, err := os.Stat("/var/tmp"); err == nil && fi.IsDir() {
		return "/var/tmp"
	}
	return os.TempDir()
}

func commitStaged(ctx context.Context, dir, msg string) error {
	if err := requireGitIdentity(ctx, dir); err != nil {
		return err
	}
	_, err := runGitAs(ctx, dir, "commit", "-m", msg)
	return err
}

func conflictFiles(ctx context.Context, dir string) (bool, []string) {
	out, err := runGit(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil || out == "" {
		return false, nil
	}
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		if f := strings.TrimSpace(line); f != "" {
			files = append(files, f)
		}
	}
	return len(files) > 0, files
}
