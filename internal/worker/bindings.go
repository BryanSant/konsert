package worker

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/gitutil"
	"github.com/BryanSant/syre/internal/protocol"
)

// provisionedBindings is the result of provisioning per-repo worktrees under
// <workdirRoot>/<ticketDirName>/<repoName>.
type provisionedBindings struct {
	workdir          string
	worktrees        []protocol.TaskWorktree
	worktreeBaseSHAs map[string]string
	baseSHA          string
	created          []gitutil.WorktreeInfo
}

// undo removes any worktrees newly created by provisionBindings (e.g. when launch fails).
func (p *provisionedBindings) undo(logger *slog.Logger, ownerID string) {
	if p == nil || (len(p.created) == 0 && p.workdir == "") {
		return
	}
	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ccancel()
	for _, info := range p.created {
		if rmErr := gitutil.RemoveTaskWorktree(cctx, info); rmErr != nil && logger != nil {
			logger.Warn("failed to remove worktree after launch failure", "owner_id", ownerID, "path", info.Path, "err", rmErr)
		}
	}
	if len(p.created) > 0 && p.workdir != "" {
		_ = os.Remove(p.workdir)
	}
}

// resolveWorkdirRoot picks the base directory under which per-ticket workdirs
// (<root>/<ticket sid>/<repo name>) and clones (<root>/.syre-clones/<repo name>) live.
func resolveWorkdirRoot(requested string, allowedRoots []string) (string, error) {
	if strings.TrimSpace(requested) != "" {
		return gitutil.ValidateWorkdir(requested, allowedRoots)
	}
	if len(allowedRoots) > 0 {
		root := gitutil.ExpandHome(allowedRoots[0])
		if err := os.MkdirAll(root, 0o755); err != nil {
			return "", err
		}
		return gitutil.ValidateWorkdir(root, allowedRoots)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return os.TempDir(), nil
	}
	root := filepath.Join(home, "src")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return gitutil.ValidateWorkdir(root, allowedRoots)
}

// provisionBindings creates (or, when reuseExisting is true, reuses) per-repo worktrees
// under <root>/<ticketDirName>/<repoName> for every binding. On error it cleans up any
// worktrees created earlier in the same call.
func provisionBindings(
	ctx context.Context,
	allowedRoots []string,
	requestedRoot string,
	ticketDirName string,
	bindings []protocol.TaskRepoBinding,
	reuseExisting bool,
	logger *slog.Logger,
) (*provisionedBindings, error) {
	workdirRoot, err := resolveWorkdirRoot(requestedRoot, allowedRoots)
	if err != nil {
		return nil, fmt.Errorf("workdir root resolution failed: %w", err)
	}
	ticketDirName = strings.TrimSpace(ticketDirName)
	if err := gitutil.ValidatePathSegment(ticketDirName); err != nil {
		return nil, fmt.Errorf("invalid ticket directory name %q: %w", ticketDirName, err)
	}
	ticketWorkdir := filepath.Join(workdirRoot, ticketDirName)
	if err := os.MkdirAll(ticketWorkdir, 0o755); err != nil {
		return nil, fmt.Errorf("create ticket workdir failed: %w", err)
	}

	res := &provisionedBindings{
		workdir:          ticketWorkdir,
		worktrees:        make([]protocol.TaskWorktree, 0, len(bindings)),
		worktreeBaseSHAs: make(map[string]string, len(bindings)),
	}

	for _, b := range bindings {
		repoName := strings.TrimSpace(b.RepoName)
		if err := gitutil.ValidatePathSegment(repoName); err != nil {
			res.undo(logger, ticketDirName)
			return nil, fmt.Errorf("invalid binding repo name %q: %w", b.RepoName, err)
		}
		checkoutPath := strings.TrimSpace(b.CheckoutPath)
		var repoRoot string
		if checkoutPath != "" {
			validCheckout, vErr := gitutil.ValidateWorkdir(checkoutPath, allowedRoots)
			if vErr != nil {
				res.undo(logger, ticketDirName)
				return nil, fmt.Errorf("checkout validation failed for %s: %w", repoName, vErr)
			}
			top, isGit := gitutil.IsGitRepo(ctx, validCheckout)
			if !isGit {
				res.undo(logger, ticketDirName)
				return nil, fmt.Errorf("checkout %s for repo %s is not a git repository", validCheckout, repoName)
			}
			repoRoot = top
		} else {
			if strings.TrimSpace(b.RemoteURL) == "" {
				res.undo(logger, ticketDirName)
				return nil, fmt.Errorf("binding %s has neither checkout_path nor remote_url", repoName)
			}
			cloneDir := filepath.Join(workdirRoot, ".syre-clones", repoName)
			if cloneErr := gitutil.CloneRepo(ctx, b.RemoteURL, cloneDir); cloneErr != nil {
				res.undo(logger, ticketDirName)
				return nil, fmt.Errorf("git clone failed for %s: %w", repoName, cloneErr)
			}
			top, isGit := gitutil.IsGitRepo(ctx, cloneDir)
			if !isGit {
				res.undo(logger, ticketDirName)
				return nil, fmt.Errorf("cloned directory %s for repo %s is not a git repository", cloneDir, repoName)
			}
			repoRoot = top
		}

		targetBranch := strings.TrimSpace(b.TargetBranch)
		if targetBranch == "" {
			targetBranch = "syre/" + ticketDirName
		}
		wtPath := filepath.Join(ticketWorkdir, repoName)

		var info gitutil.WorktreeInfo
		if reuseExisting {
			if top, isGit := gitutil.IsGitRepo(ctx, wtPath); isGit {
				baseSHA, _ := gitutil.CurrentSHA(ctx, repoRoot)
				if baseSHA == "" {
					baseSHA, _ = gitutil.CurrentSHA(ctx, top)
				}
				info = gitutil.WorktreeInfo{
					Path:     top,
					BaseSHA:  baseSHA,
					RepoRoot: repoRoot,
					Branch:   targetBranch,
				}
			}
		}
		if info.Path == "" {
			var wtErr error
			info, wtErr = gitutil.CreateWorktreeAt(ctx, repoRoot, wtPath, b.BaseBranch, targetBranch)
			if wtErr != nil {
				res.undo(logger, ticketDirName)
				return nil, fmt.Errorf("git worktree setup failed for %s: %w", repoName, wtErr)
			}
			res.created = append(res.created, info)
		}

		res.worktrees = append(res.worktrees, protocol.TaskWorktree{
			RepoName:     repoName,
			CheckoutPath: repoRoot,
			WorktreePath: info.Path,
			Branch:       targetBranch,
		})
		res.worktreeBaseSHAs[repoName] = info.BaseSHA
		if res.baseSHA == "" {
			res.baseSHA = info.BaseSHA
		}
	}

	return res, nil
}

// ticketDirNameFor returns the ticket directory segment for binding provisioning.
func ticketDirNameFor(ticketSID, fallback string) string {
	return cmp.Or(strings.TrimSpace(ticketSID), strings.TrimSpace(fallback))
}
