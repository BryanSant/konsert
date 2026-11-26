package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/gitutil"
	"github.com/BryanSant/syre/internal/protocol"
)

// ArtifactHost is the retained output the daemon can read without leaving the machine.
type ArtifactHost struct {
	AllowedRoots []string
	// Stderr returns the chat engine's retained stderr tail. ok is false when this
	// daemon has no tail for chatID.
	Stderr func(chatID string) (text string, ok bool)
	// Log returns the PTY ring buffer or on-disk scrollback for a task.
	Log func(taskID string) (text string, ok bool)
}

// FetchArtifact reads one edge artifact and caps it (lines and bytes). The byte
// cap is applied here, on the daemon, before anything is sent to the hub.
func FetchArtifact(ctx context.Context, host ArtifactHost, req protocol.ArtifactFetchRequest) protocol.ArtifactFetchResult {
	kind := strings.TrimSpace(req.Kind)
	switch kind {
	case protocol.InspectKindDiff, protocol.InspectKindDiffStat, protocol.InspectKindFile:
		return fetchWorktree(ctx, host, req, kind)
	case protocol.InspectKindStderr:
		return pageRetained(kind, req.ChatID, req.MaxLines, req.Offset, host.Stderr, "no retained stderr for this session")
	case protocol.InspectKindLog:
		return pageRetained(kind, req.TaskID, req.MaxLines, req.Offset, host.Log, "no retained log for this session")
	default:
		return protocol.ArtifactFetchResult{Kind: kind, Error: fmt.Sprintf("unknown artifact kind %q", kind)}
	}
}

func pageRetained(kind, id string, maxLines, offset int, read func(string) (string, bool), missing string) protocol.ArtifactFetchResult {
	if read == nil || strings.TrimSpace(id) == "" {
		return protocol.ArtifactFetchResult{Kind: kind, Error: missing}
	}
	text, ok := read(id)
	if !ok {
		return protocol.ArtifactFetchResult{Kind: kind, Error: missing}
	}
	return protocol.PageText(text, maxLines, offset).ToArtifact(kind)
}

func fetchWorktree(ctx context.Context, host ArtifactHost, req protocol.ArtifactFetchRequest, kind string) protocol.ArtifactFetchResult {
	dir, err := gitutil.ValidateWorkdir(req.Workdir, host.AllowedRoots)
	if err != nil {
		if errors.Is(err, gitutil.ErrWorkdirInvalid) {
			return protocol.ArtifactFetchResult{Kind: kind, Error: "worktree was cleaned up"}
		}
		return protocol.ArtifactFetchResult{Kind: kind, Error: err.Error()}
	}
	rel, err := cleanRelPath(req.Path)
	if err != nil {
		return protocol.ArtifactFetchResult{Kind: kind, Error: err.Error()}
	}
	switch kind {
	case protocol.InspectKindFile:
		if rel == "" {
			return protocol.ArtifactFetchResult{Kind: kind, Error: "path is required for kind file"}
		}
		f, oErr := gitutil.OpenInside(dir, rel)
		if oErr != nil {
			return protocol.ArtifactFetchResult{Kind: kind, Error: oErr.Error()}
		}
		defer f.Close()
		return protocol.PageReader(f, req.MaxLines, req.Offset).ToArtifact(kind)
	case protocol.InspectKindDiffStat:
		page, gErr := gitDiffPage(ctx, dir, req.MaxLines, req.Offset, true, rel)
		if gErr != nil {
			return protocol.ArtifactFetchResult{Kind: kind, Error: gErr.Error()}
		}
		return page.ToArtifact(kind)
	default:
		page, gErr := gitDiffPage(ctx, dir, req.MaxLines, req.Offset, false, rel)
		if gErr != nil {
			return protocol.ArtifactFetchResult{Kind: kind, Error: gErr.Error()}
		}
		return page.ToArtifact(kind)
	}
}

func gitDiffPage(ctx context.Context, dir string, maxLines, offset int, stat bool, rel string) (protocol.TextPage, error) {
	args := []string{"--literal-pathspecs", "diff"}
	if stat {
		args = append(args, "--stat")
	}
	args = append(args, "HEAD")
	if rel != "" {
		args = append(args, "--", rel)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return protocol.TextPage{}, err
	}
	if err := cmd.Start(); err != nil {
		return protocol.TextPage{}, err
	}
	page := protocol.PageReader(stdout, maxLines, offset)
	_ = stdout.Close()
	waitErr := cmd.Wait()
	if waitErr != nil && page.Text == "" && !page.Truncated {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return protocol.TextPage{}, fmt.Errorf("%s", msg)
	}
	return page, nil
}

// cleanRelPath rejects absolute paths and paths that leave the worktree.
// An empty path means the whole worktree.
func cleanRelPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	if filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\n\r") {
		return "", fmt.Errorf("path must be a relative file path")
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the worktree")
	}
	return clean, nil
}
