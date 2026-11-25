package gitutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BryanSant/syre/internal/protocol"
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

// ErrInvalidWorktreeName is returned when a task ID cannot be used as a worktree directory.
var ErrInvalidWorktreeName = errors.New("invalid worktree name")

// WorktreePath returns <repoRoot>/.syre-worktrees/<taskID>, refusing any taskID that is
// not a single plain path component or whose result would escape .syre-worktrees.
func WorktreePath(repoRoot, taskID string) (string, error) {
	if taskID == "" || taskID == "." || taskID == ".." || strings.ContainsAny(taskID, `/\:`) || filepath.Base(taskID) != taskID {
		return "", fmt.Errorf("%w: %q", ErrInvalidWorktreeName, taskID)
	}
	wtParent := filepath.Join(repoRoot, ".syre-worktrees")
	p := filepath.Join(wtParent, taskID)
	if rel, err := filepath.Rel(wtParent, p); err != nil || rel != taskID {
		return "", fmt.Errorf("%w: %q escapes %s", ErrInvalidWorktreeName, taskID, wtParent)
	}
	return p, nil
}

// WorktreeInfo describes a worktree created by CreateWorktreeInfo.
type WorktreeInfo struct {
	Path          string
	BaseSHA       string
	RepoRoot      string
	Branch        string
	CreatedBranch bool // the branch did not exist before and was created for this worktree
}

// CreateWorktree creates an isolated git worktree at <repoRoot>/.syre-worktrees/<taskID>
// checked out to branch (creating or resetting the branch from HEAD if needed).
func CreateWorktree(ctx context.Context, repoRoot, taskID, branch string) (worktreePath string, baseSHA string, err error) {
	info, err := CreateWorktreeInfo(ctx, repoRoot, taskID, branch)
	if err != nil {
		return "", "", err
	}
	return info.Path, info.BaseSHA, nil
}

// CreateWorktreeInfo is CreateWorktree but also reports whether the branch was created, so
// a caller can undo the worktree (RemoveTaskWorktree) if the task then fails to launch.
func CreateWorktreeInfo(ctx context.Context, repoRoot, taskID, branch string) (WorktreeInfo, error) {
	worktreePath, err := WorktreePath(repoRoot, taskID)
	if err != nil {
		return WorktreeInfo{}, err
	}
	if _, statErr := os.Lstat(worktreePath); statErr == nil {
		return WorktreeInfo{}, fmt.Errorf("worktree path already exists: %s", worktreePath)
	}
	baseSHA, _ := CurrentSHA(ctx, repoRoot)
	info := WorktreeInfo{Path: worktreePath, BaseSHA: baseSHA, RepoRoot: repoRoot, Branch: branch}

	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		return WorktreeInfo{}, fmt.Errorf("create worktree parent: %w", err)
	}

	// Check if branch already exists locally.
	_, branchExistsErr := runGit(ctx, repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if branchExistsErr == nil {
		// Try checking out existing branch directly.
		if _, err := runGit(ctx, repoRoot, "worktree", "add", worktreePath, branch); err != nil {
			// Fallback: branch might already be checked out in the main worktree; create detached or task branch.
			if _, err2 := runGit(ctx, repoRoot, "worktree", "add", "--detach", worktreePath, branch); err2 != nil {
				return WorktreeInfo{}, fmt.Errorf("create worktree for existing branch %s: %w", branch, err)
			}
		}
	} else {
		if _, err := runGit(ctx, repoRoot, "worktree", "add", "-b", branch, worktreePath); err != nil {
			return WorktreeInfo{}, fmt.Errorf("create worktree with new branch %s: %w", branch, err)
		}
		info.CreatedBranch = true
	}

	return info, nil
}

// RemoveTaskWorktree undoes CreateWorktreeInfo: it force-removes the worktree and, if the
// branch was created for it, deletes the branch as well.
func RemoveTaskWorktree(ctx context.Context, info WorktreeInfo) error {
	if info.Path == "" || info.RepoRoot == "" {
		return nil
	}
	err := RemoveWorktree(ctx, info.RepoRoot, info.Path)
	if info.CreatedBranch && info.Branch != "" {
		if _, bErr := runGit(ctx, info.RepoRoot, "branch", "-D", "--", info.Branch); bErr != nil && err == nil {
			err = bErr
		}
	}
	return err
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
		IsWorktree:   strings.Contains(filepath.ToSlash(dir), "/.syre-worktrees/"),
	}
	if _, ok := IsGitRepo(ctx, dir); !ok {
		return res, nil
	}
	res.IsGitRepo = true

	if br, err := runGit(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		res.Branch = br
	}

	// Collect numstat for unstaged + staged changes. -z keeps paths verbatim (no C-quoting)
	// and reports renames as separate old/new fields instead of "a => b".
	stats := make(map[string]numstat)
	if out, err := runGitRaw(ctx, dir, "--literal-pathspecs", "diff", "--numstat", "-z"); err == nil {
		addNumstats(stats, ParseNumstatZ(out))
	}
	if out, err := runGitRaw(ctx, dir, "--literal-pathspecs", "diff", "--cached", "--numstat", "-z"); err == nil {
		addNumstats(stats, ParseNumstatZ(out))
	}

	rootResolved, rootErr := filepath.EvalSymlinks(dir)
	if rootErr != nil {
		rootResolved = dir
	}

	statusOut, err := runGitRaw(ctx, dir, "status", "--porcelain=v1", "-z", "-uall")
	if err == nil {
		const maxFiles = 60
		for _, entry := range ParsePorcelainZ(statusOut) {
			if len(res.Files) >= maxFiles {
				break
			}
			stCode := strings.TrimSpace(entry.Code)
			filePath := entry.Path

			s := stats[filePath]
			var diffText string
			if stCode == "??" {
				// Preview untracked regular text files as added lines (capped at 12 KB).
				// Symlinks and anything else that is not a regular file inside the repo are
				// never read, so a repo symlink to ~/.ssh/id_ed25519 cannot leak its target.
				if raw, ok := readUntrackedPreview(rootResolved, dir, filePath, 12*1024); ok && !bytes.ContainsRune(raw, 0) {
					fileLines := strings.Split(string(raw), "\n")
					s.add = len(fileLines)
					var b strings.Builder
					b.WriteString(fmt.Sprintf("--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", filePath, len(fileLines)))
					for _, fl := range fileLines {
						b.WriteString("+" + fl + "\n")
					}
					diffText = b.String()
				}
			} else {
				if dOut, dErr := runGit(ctx, dir, "--literal-pathspecs", "diff", "HEAD", "--", filePath); dErr == nil && dOut != "" {
					diffText = dOut
				} else if dOut, dErr := runGit(ctx, dir, "--literal-pathspecs", "diff", "--", filePath); dErr == nil {
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

// PorcelainEntry is one record of `git status --porcelain=v1 -z`.
type PorcelainEntry struct {
	Code     string // two-character XY status
	Path     string // current path (the destination for renames/copies)
	OrigPath string // source path for renames/copies
}

// ParsePorcelainZ parses NUL-separated `git status --porcelain=v1 -z` output. Paths are
// verbatim (never C-quoted) and a rename/copy record is followed by its original path.
func ParsePorcelainZ(out string) []PorcelainEntry {
	fields := strings.Split(out, "\x00")
	var entries []PorcelainEntry
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 || f[2] != ' ' {
			// Empty trailing field or malformed record: skip it, never abort the rest.
			continue
		}
		e := PorcelainEntry{Code: f[:2], Path: f[3:]}
		if e.Code[0] == 'R' || e.Code[0] == 'C' || e.Code[1] == 'R' || e.Code[1] == 'C' {
			if i+1 < len(fields) {
				e.OrigPath = fields[i+1]
				i++
			}
		}
		entries = append(entries, e)
	}
	return entries
}

// NumstatEntry is one record of `git diff --numstat -z`.
type NumstatEntry struct {
	Added, Deleted int
	Path           string // destination path for renames
	OrigPath       string // source path for renames
	Binary         bool
}

// ParseNumstatZ parses `git diff --numstat -z` output. Ordinary records are
// "A\tD\tpath\0"; renames are "A\tD\t\0old\0new\0".
func ParseNumstatZ(out string) []NumstatEntry {
	fields := strings.Split(out, "\x00")
	var entries []NumstatEntry
	for i := 0; i < len(fields); i++ {
		parts := strings.SplitN(fields[i], "\t", 3)
		if len(parts) != 3 {
			continue
		}
		e := NumstatEntry{}
		if parts[0] == "-" && parts[1] == "-" {
			e.Binary = true
		} else {
			e.Added, _ = strconv.Atoi(parts[0])
			e.Deleted, _ = strconv.Atoi(parts[1])
		}
		if parts[2] == "" {
			if i+2 >= len(fields) {
				break
			}
			e.OrigPath, e.Path = fields[i+1], fields[i+2]
			i += 2
		} else {
			e.Path = parts[2]
		}
		entries = append(entries, e)
	}
	return entries
}

type numstat struct{ add, del int }

func addNumstats(stats map[string]numstat, entries []NumstatEntry) {
	for _, e := range entries {
		cur := stats[e.Path]
		cur.add += e.Added
		cur.del += e.Deleted
		stats[e.Path] = cur
	}
}

// readUntrackedPreview returns up to limit bytes of an untracked file, but only when the
// path is a regular file (Lstat, so symlinks are never followed) whose parent directory
// resolves inside the repository root.
func readUntrackedPreview(rootResolved, dir, rel string, limit int64) ([]byte, bool) {
	full := filepath.Join(dir, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(full))
	if err != nil {
		return nil, false
	}
	if r, err := filepath.Rel(rootResolved, parent); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
		return nil, false
	}
	f, err := os.OpenFile(full, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	// Re-check the opened file is the same regular file (guards against a swap after Lstat).
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || !os.SameFile(st, info) {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, false
	}
	return raw, true
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

// CleanupSyreWorktree safely removes a worktree located under .syre-worktrees/.
func CleanupSyreWorktree(ctx context.Context, workdir string) error {
	norm := filepath.ToSlash(filepath.Clean(workdir))
	idx := strings.Index(norm, "/.syre-worktrees/")
	if idx < 0 {
		return fmt.Errorf("refusing to remove directory outside .syre-worktrees: %s", workdir)
	}
	repoRoot := filepath.FromSlash(norm[:idx])
	return RemoveWorktree(ctx, repoRoot, workdir)
}
