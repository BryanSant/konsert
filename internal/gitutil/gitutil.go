package gitutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BryanSant/konsert/internal/protocol"
)

var (
	ErrWorkdirNotAllowed = errors.New("workdir is outside allowed roots")
	ErrWorkdirInvalid    = errors.New("workdir does not exist or is not a directory")
)

// ValidateWorkdir verifies that workdir exists and is a directory.
// If allowedRoots is non-empty (opt-in sandboxing), it verifies using symlink-resolved paths
// that workdir is contained within at least one allowed root.
func ValidateWorkdir(workdir string, allowedRoots []string) (string, error) {
	workdir = strings.TrimSpace(workdir)
	if workdir == "" {
		return "", fmt.Errorf("%w: empty path", ErrWorkdirInvalid)
	}
	if workdir == "~" || strings.HasPrefix(workdir, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if workdir == "~" {
				workdir = home
			} else {
				workdir = filepath.Join(home, workdir[2:])
			}
		}
	}
	absDir, err := filepath.Abs(workdir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrWorkdirInvalid, err)
	}
	info, err := os.Stat(absDir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrWorkdirInvalid, absDir)
	}
	resolved, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrWorkdirInvalid, err)
	}

	if len(allowedRoots) == 0 {
		return resolved, nil
	}

	for _, root := range allowedRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		resolvedRoot, err := filepath.EvalSymlinks(absRoot)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(resolvedRoot, resolved)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
			return resolved, nil
		}
	}

	return "", fmt.Errorf("%w: %s", ErrWorkdirNotAllowed, resolved)
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// IsGitRepo checks if dir is inside a git repository and returns the repository top-level root.
func IsGitRepo(ctx context.Context, dir string) (string, bool) {
	out, err := runGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || out == "" {
		return "", false
	}
	return out, true
}

// CurrentSHA returns HEAD's full commit hash, or "" if the repository has no commits yet.
func CurrentSHA(ctx context.Context, dir string) (string, error) {
	out, err := runGit(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", nil
	}
	return out, nil
}

// CreateWorktree creates an isolated git worktree at <repoRoot>/.konsert-worktrees/<taskID>
// checked out to branch (creating or resetting the branch from HEAD if needed).
func CreateWorktree(ctx context.Context, repoRoot, taskID, branch string) (worktreePath string, baseSHA string, err error) {
	baseSHA, _ = CurrentSHA(ctx, repoRoot)

	wtParent := filepath.Join(repoRoot, ".konsert-worktrees")
	if err := os.MkdirAll(wtParent, 0o755); err != nil {
		return "", "", fmt.Errorf("create worktree parent: %w", err)
	}
	worktreePath = filepath.Join(wtParent, taskID)

	// Check if branch already exists locally.
	_, branchExistsErr := runGit(ctx, repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if branchExistsErr == nil {
		// Try checking out existing branch directly.
		if _, err := runGit(ctx, repoRoot, "worktree", "add", worktreePath, branch); err != nil {
			// Fallback: branch might already be checked out in the main worktree; create detached or task branch.
			if _, err2 := runGit(ctx, repoRoot, "worktree", "add", "--detach", worktreePath, branch); err2 != nil {
				return "", "", fmt.Errorf("create worktree for existing branch %s: %w", branch, err)
			}
		}
	} else {
		if _, err := runGit(ctx, repoRoot, "worktree", "add", "-b", branch, worktreePath); err != nil {
			return "", "", fmt.Errorf("create worktree with new branch %s: %w", branch, err)
		}
	}

	return worktreePath, baseSHA, nil
}

// RemoveWorktree removes a previously created git worktree.
func RemoveWorktree(ctx context.Context, repoRoot, worktreePath string) error {
	_, err := runGit(ctx, repoRoot, "worktree", "remove", "--force", worktreePath)
	return err
}

// CaptureSummary inspects git status, diff --stat, and commits since baseSHA.
func CaptureSummary(ctx context.Context, dir, baseSHA string) (*protocol.GitSummary, error) {
	if _, ok := IsGitRepo(ctx, dir); !ok {
		return nil, nil
	}

	summary := &protocol.GitSummary{
		BaseSHA: baseSHA,
	}

	headSHA, _ := CurrentSHA(ctx, dir)
	summary.HeadSHA = headSHA

	if statusOut, err := runGit(ctx, dir, "status", "--short"); err == nil && statusOut != "" {
		for _, line := range strings.Split(statusOut, "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				summary.DirtyFiles = append(summary.DirtyFiles, trimmed)
			}
		}
	}

	if baseSHA != "" {
		if diffStat, err := runGit(ctx, dir, "diff", "--stat", baseSHA); err == nil {
			summary.DiffStat = diffStat
		}
		if commitsOut, err := runGit(ctx, dir, "log", "--oneline", baseSHA+"..HEAD"); err == nil && commitsOut != "" {
			for _, line := range strings.Split(commitsOut, "\n") {
				if trimmed := strings.TrimSpace(line); trimmed != "" {
					summary.Commits = append(summary.Commits, trimmed)
				}
			}
		}
	} else {
		if diffStat, err := runGit(ctx, dir, "diff", "--stat"); err == nil {
			summary.DiffStat = diffStat
		}
	}

	return summary, nil
}
