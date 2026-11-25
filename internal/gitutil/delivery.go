package gitutil

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BryanSant/konsert/internal/protocol"
)

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

// commitIfDirty stages and commits any uncommitted changes in dir when msg is non-empty.
func commitIfDirty(ctx context.Context, dir, msg string) error {
	if strings.TrimSpace(msg) == "" {
		return nil
	}
	st, err := runGit(ctx, dir, "status", "--porcelain")
	if err != nil || st == "" {
		return nil
	}
	if _, err := runGit(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	// Ensure we have a fallback author identity only if git has none configured,
	// so automated commits in fresh test repos don't fail while real checkouts keep
	// the user's configured identity.
	if name, _ := runGit(ctx, dir, "config", "user.name"); name == "" {
		if _, err := runGit(ctx, dir, "-c", "user.name=Konsert", "-c", "user.email=konsert@users.noreply.github.com", "commit", "-m", msg); err != nil {
			return err
		}
		return nil
	}
	_, err = runGit(ctx, dir, "commit", "-m", msg)
	return err
}

// PushBranch pushes the checkout's branch to Remote (default "origin") or RemoteURL.
// If req.CommitMessage is non-empty and the working tree is dirty, it stages and commits
// the changes first.
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
	if target != "" {
		res.RemoteURL = target
		if _, err := runGit(ctx, dir, "push", target, "HEAD:refs/heads/"+branch); err != nil {
			res.Error = err.Error()
			return res
		}
		res.OK = true
		return res
	}

	remote := strings.TrimSpace(req.Remote)
	if remote == "" {
		remote, res.RemoteURL = primaryRemote(ctx, dir)
	} else {
		res.RemoteURL, _ = runGit(ctx, dir, "remote", "get-url", remote)
	}
	if remote == "" {
		res.Error = "no git remote configured"
		return res
	}
	if _, err := runGit(ctx, dir, "push", "-u", remote, branch); err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	return res
}

// MergeBranch merges SourceBranch into TargetBranch using Style ("squash", "merge", or
// "rebase") and optionally pushes TargetBranch to Remote. Runs entirely with plain git on
// the machine that owns the checkout.
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

	msg := strings.TrimSpace(req.CommitMessage)
	if msg == "" {
		msg = "Merge task branch"
	}
	if err := commitIfDirty(ctx, dir, msg); err != nil {
		res.Error = fmt.Sprintf("commit uncommitted changes: %v", err)
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

	root := mainRepoRoot(ctx, dir)
	if root == "" {
		root = dir
	}

	// Same branch: the task ran directly on target (no separate task branch).
	// Uncommitted changes were committed above; push if requested and remote exists.
	if source == target {
		sha, _ := CurrentSHA(ctx, dir)
		res.MergeSHA = sha
		res.Merged = true
		if req.Push && hasRemote {
			if _, err := runGit(ctx, dir, "push", remote, target); err != nil {
				res.Error = fmt.Sprintf("push %s: %v", target, err)
				return res
			}
			res.Pushed = true
		}
		res.OK = true
		return res
	}

	switch style {
	case "squash":
		if _, err := runGit(ctx, root, "checkout", target); err != nil {
			res.Error = fmt.Sprintf("checkout %s: %v", target, err)
			return res
		}
		if _, err := runGit(ctx, root, "merge", "--squash", source); err != nil {
			res.Conflict, res.ConflictFiles = conflictFiles(ctx, root)
			_, _ = runGit(ctx, root, "reset", "--merge")
			if res.Conflict {
				res.Error = fmt.Sprintf("squash-merge conflict merging %s into %s (%s)", source, target, strings.Join(res.ConflictFiles, ", "))
			} else {
				res.Error = err.Error()
			}
			return res
		}
		// Only commit if there are staged changes (avoid empty commit error if already merged).
		if st, _ := runGit(ctx, root, "diff", "--cached", "--name-only"); st != "" {
			if err := commitStaged(ctx, root, msg); err != nil {
				res.Error = fmt.Sprintf("commit squash merge: %v", err)
				return res
			}
		}

	case "merge":
		if _, err := runGit(ctx, root, "checkout", target); err != nil {
			res.Error = fmt.Sprintf("checkout %s: %v", target, err)
			return res
		}
		var err error
		if name, _ := runGit(ctx, root, "config", "user.name"); name == "" {
			_, err = runGit(ctx, root, "-c", "user.name=Konsert", "-c", "user.email=konsert@users.noreply.github.com", "merge", "--no-ff", "-m", msg, source)
		} else {
			_, err = runGit(ctx, root, "merge", "--no-ff", "-m", msg, source)
		}
		if err != nil {
			res.Conflict, res.ConflictFiles = conflictFiles(ctx, root)
			_, _ = runGit(ctx, root, "merge", "--abort")
			if res.Conflict {
				res.Error = fmt.Sprintf("merge conflict merging %s into %s (%s)", source, target, strings.Join(res.ConflictFiles, ", "))
			} else {
				res.Error = err.Error()
			}
			return res
		}

	case "rebase":
		// Rebase source onto target inside dir (if dir is on source) or root.
		rebaseDir := dir
		if curBr, _ := runGit(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD"); curBr != source {
			rebaseDir = root
			if _, err := runGit(ctx, rebaseDir, "checkout", source); err != nil {
				res.Error = fmt.Sprintf("checkout %s: %v", source, err)
				return res
			}
		}
		if _, err := runGit(ctx, rebaseDir, "rebase", target); err != nil {
			res.Conflict, res.ConflictFiles = conflictFiles(ctx, rebaseDir)
			_, _ = runGit(ctx, rebaseDir, "rebase", "--abort")
			if res.Conflict {
				res.Error = fmt.Sprintf("rebase conflict rebasing %s onto %s (%s)", source, target, strings.Join(res.ConflictFiles, ", "))
			} else {
				res.Error = err.Error()
			}
			return res
		}
		if _, err := runGit(ctx, root, "checkout", target); err != nil {
			res.Error = fmt.Sprintf("checkout %s: %v", target, err)
			return res
		}
		if _, err := runGit(ctx, root, "merge", "--ff-only", source); err != nil {
			res.Error = fmt.Sprintf("fast-forward %s to %s: %v", target, source, err)
			return res
		}
	}

	res.MergeSHA, _ = CurrentSHA(ctx, root)
	res.Merged = true

	if req.Push && hasRemote {
		if _, err := runGit(ctx, root, "push", remote, target); err != nil {
			res.Error = fmt.Sprintf("merged %s into %s locally, but push failed: %v", source, target, err)
			return res
		}
		res.Pushed = true
	}
	res.OK = true
	return res
}

func commitStaged(ctx context.Context, dir, msg string) error {
	if name, _ := runGit(ctx, dir, "config", "user.name"); name == "" {
		_, err := runGit(ctx, dir, "-c", "user.name=Konsert", "-c", "user.email=konsert@users.noreply.github.com", "commit", "-m", msg)
		return err
	}
	_, err := runGit(ctx, dir, "commit", "-m", msg)
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
