package gitutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

func runGitRaw(ctx context.Context, dir string, args ...string) (string, error) {
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
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := runGitRaw(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
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

// InspectWorkingTree returns live branch, uncommitted files (+N/-M), per-file unified diffs,
// and recent commits for a working directory on a worker node.
func InspectWorkingTree(ctx context.Context, dir string) (*protocol.GitInspectResult, error) {
	res := &protocol.GitInspectResult{
		EffectiveDir: dir,
		Files:        []protocol.GitChangedFile{},
		IsWorktree:   strings.Contains(filepath.ToSlash(dir), "/.konsert-worktrees/"),
	}
	if _, ok := IsGitRepo(ctx, dir); !ok {
		return res, nil
	}
	res.IsGitRepo = true

	if br, err := runGit(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		res.Branch = br
	}

	// Collect numstat for unstaged + staged changes.
	type stat struct{ add, del int }
	stats := make(map[string]stat)
	parseNumstat := func(out string) {
		for _, line := range strings.Split(out, "\n") {
			parts := strings.Split(line, "\t")
			if len(parts) < 3 {
				continue
			}
			a, _ := strconv.Atoi(parts[0])
			d, _ := strconv.Atoi(parts[1])
			p := parts[2]
			cur := stats[p]
			cur.add += a
			cur.del += d
			stats[p] = cur
		}
	}
	if out, err := runGit(ctx, dir, "diff", "--numstat"); err == nil && out != "" {
		parseNumstat(out)
	}
	if out, err := runGit(ctx, dir, "diff", "--cached", "--numstat"); err == nil && out != "" {
		parseNumstat(out)
	}

	// Parse porcelain status (preserve leading status column space!)
	statusOut, err := runGitRaw(ctx, dir, "status", "--porcelain=v1", "-uall")
	if err == nil && statusOut != "" {
		lines := strings.Split(statusOut, "\n")
		const maxFiles = 60
		for i, line := range lines {
			if i >= maxFiles || len(line) < 4 {
				break
			}
			stCode := strings.TrimSpace(line[:2])
			filePath := strings.TrimSpace(line[3:])
			if idx := strings.Index(filePath, " -> "); idx >= 0 {
				filePath = filePath[idx+4:]
			}
			filePath = strings.Trim(filePath, `"`)
			if filePath == "" {
				continue
			}

			s := stats[filePath]
			var diffText string
			if stCode == "??" {
				// Preview untracked text file as added lines (capped at 12 KB).
				full := filepath.Join(dir, filePath)
				if raw, rErr := os.ReadFile(full); rErr == nil {
					if len(raw) > 12*1024 {
						raw = raw[:12*1024]
					}
					if !bytes.ContainsRune(raw, 0) {
						fileLines := strings.Split(string(raw), "\n")
						s.add = len(fileLines)
						var b strings.Builder
						b.WriteString(fmt.Sprintf("--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", filePath, len(fileLines)))
						for _, fl := range fileLines {
							b.WriteString("+" + fl + "\n")
						}
						diffText = b.String()
					}
				}
			} else {
				if dOut, dErr := runGit(ctx, dir, "diff", "HEAD", "--", filePath); dErr == nil && dOut != "" {
					diffText = dOut
				} else if dOut, dErr := runGit(ctx, dir, "diff", "--", filePath); dErr == nil {
					diffText = dOut
				}
				if len(diffText) > 16*1024 {
					diffText = diffText[:16*1024] + "\n... (diff truncated)"
				}
			}

			res.Added += s.add
			res.Deleted += s.del
			res.Files = append(res.Files, protocol.GitChangedFile{
				Path:    filePath,
				Status:  stCode,
				Added:   s.add,
				Deleted: s.del,
				Diff:    diffText,
			})
		}
	}

	if commitsOut, err := runGit(ctx, dir, "log", "-n", "8", "--oneline"); err == nil && commitsOut != "" {
		for _, line := range strings.Split(commitsOut, "\n") {
			if t := strings.TrimSpace(line); t != "" {
				res.Commits = append(res.Commits, t)
			}
		}
	}

	return res, nil
}

// CommitChanges stages (if stageAll) and commits changes in dir with message.
func CommitChanges(ctx context.Context, dir, message string, stageAll bool) (*protocol.GitCommitResult, error) {
	if strings.TrimSpace(message) == "" {
		return &protocol.GitCommitResult{OK: false, Error: "commit message is required"}, nil
	}
	if _, ok := IsGitRepo(ctx, dir); !ok {
		return &protocol.GitCommitResult{OK: false, Error: "directory is not a git repository"}, nil
	}
	if stageAll {
		if _, err := runGit(ctx, dir, "add", "-A"); err != nil {
			return &protocol.GitCommitResult{OK: false, Error: err.Error()}, nil
		}
	}
	out, err := runGit(ctx, dir, "commit", "-m", message)
	if err != nil {
		return &protocol.GitCommitResult{OK: false, Error: err.Error()}, nil
	}
	sha, _ := runGit(ctx, dir, "rev-parse", "--short", "HEAD")
	return &protocol.GitCommitResult{
		OK:        true,
		CommitSHA: sha,
		Summary:   out,
	}, nil
}

// CleanupKonsertWorktree safely removes a worktree located under .konsert-worktrees/.
func CleanupKonsertWorktree(ctx context.Context, workdir string) error {
	norm := filepath.ToSlash(filepath.Clean(workdir))
	idx := strings.Index(norm, "/.konsert-worktrees/")
	if idx < 0 {
		return fmt.Errorf("refusing to remove directory outside .konsert-worktrees: %s", workdir)
	}
	repoRoot := filepath.FromSlash(norm[:idx])
	return RemoveWorktree(ctx, repoRoot, workdir)
}
