package gitutil

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// DefaultCheckoutMaxDepth is the default directory traversal depth for checkout discovery.
// Depth 4 covers layouts like ~/src/<host>/<org>/<repo> without traversing deep trees.
const DefaultCheckoutMaxDepth = 4

// DefaultCheckoutRoots returns the roots syred scans for git checkouts:
// configuredRoots if non-empty, otherwise allowedRoots plus ~/src (deduplicated).
func DefaultCheckoutRoots(allowedRoots, configuredRoots []string) []string {
	var raw []string
	if len(configuredRoots) > 0 {
		raw = append(raw, configuredRoots...)
	} else {
		raw = append(raw, allowedRoots...)
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			raw = append(raw, filepath.Join(home, "src"))
		}
	}
	return dedupRoots(raw)
}

func dedupRoots(raw []string) []string {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		p := ExpandHome(r)
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		key := p
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			key = resolved
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

// ExpandHome expands a leading "~" or "~/" to the user's home directory.
func ExpandHome(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// skipCheckoutDirs are directory basenames never descended into during checkout discovery.
var skipCheckoutDirs = map[string]bool{
	".git":            true,
	".syre-worktrees": true,
	".syre-clones":    true,
	"node_modules":    true,
	"vendor":          true,
	"target":          true,
	"dist":            true,
	"build":           true,
	".cache":          true,
	"__pycache__":     true,
	".venv":           true,
	"venv":            true,
	".Trash":          true,
	"Library":         true,
}

// DiscoverCheckouts walks roots up to maxDepth to find primary git checkouts (directories
// containing a .git directory; linked worktrees with a .git file are skipped) and reads
// their remote URL, branch, HEAD commit, and last fetch timestamp directly from .git files
// without spawning git subprocesses.
func DiscoverCheckouts(ctx context.Context, roots []string, maxDepth int) []protocol.ReportedCheckout {
	if maxDepth <= 0 {
		maxDepth = DefaultCheckoutMaxDepth
	}
	seen := make(map[string]bool)
	var out []protocol.ReportedCheckout

	for _, root := range dedupRoots(roots) {
		if ctx.Err() != nil {
			break
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		walkForCheckouts(ctx, root, 0, maxDepth, seen, &out)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].RemoteURL != out[j].RemoteURL {
			return out[i].RemoteURL < out[j].RemoteURL
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func walkForCheckouts(ctx context.Context, dir string, depth, maxDepth int, seen map[string]bool, out *[]protocol.ReportedCheckout) {
	if ctx.Err() != nil {
		return
	}
	gitDir := filepath.Join(dir, ".git")
	if info, err := os.Lstat(gitDir); err == nil {
		// Only report primary checkouts (.git directory), not linked worktrees (.git file).
		if info.IsDir() {
			canon := dir
			if resolved, rErr := filepath.EvalSymlinks(dir); rErr == nil {
				canon = resolved
			}
			if !seen[canon] {
				seen[canon] = true
				if co, ok := readCheckoutInfo(dir, gitDir); ok {
					*out = append(*out, co)
				}
			}
		}
		// Never descend into a git repository or worktree.
		return
	}

	if depth >= maxDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if skipCheckoutDirs[name] || strings.HasPrefix(name, ".") {
			continue
		}
		walkForCheckouts(ctx, filepath.Join(dir, name), depth+1, maxDepth, seen, out)
	}
}

func readCheckoutInfo(repoDir, gitDir string) (protocol.ReportedCheckout, bool) {
	rawURL := readGitConfigRemoteURL(filepath.Join(gitDir, "config"))
	normURL := NormalizeRemoteURL(rawURL)
	if normURL == "" {
		return protocol.ReportedCheckout{}, false
	}
	branch, head := readGitHead(gitDir)
	var lastFetch *time.Time
	if st, err := os.Stat(filepath.Join(gitDir, "FETCH_HEAD")); err == nil {
		t := st.ModTime().UTC()
		lastFetch = &t
	}
	return protocol.ReportedCheckout{
		Path:      repoDir,
		RemoteURL: normURL,
		Branch:    branch,
		Head:      head,
		LastFetch: lastFetch,
	}, true
}

// readGitConfigRemoteURL parses .git/config for [remote "origin"] url, falling back
// to the first [remote "..."] section with a url if "origin" is absent.
func readGitConfigRemoteURL(configPath string) string {
	f, err := os.Open(configPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	var (
		currentRemote string
		originURL     string
		firstURL      string
	)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSpace(line[1 : len(line)-1])
			currentRemote = ""
			// Match: remote "name"
			if strings.HasPrefix(strings.ToLower(section), "remote ") {
				rest := strings.TrimSpace(section[len("remote "):])
				if len(rest) >= 2 && rest[0] == '"' && rest[len(rest)-1] == '"' {
					currentRemote = rest[1 : len(rest)-1]
				}
			}
			continue
		}
		if currentRemote == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:eq]))
		if key != "url" {
			continue
		}
		val := strings.TrimSpace(line[eq+1:])
		if currentRemote == "origin" {
			originURL = val
			break
		}
		if firstURL == "" {
			firstURL = val
		}
	}
	if originURL != "" {
		return originURL
	}
	return firstURL
}

func readGitHead(gitDir string) (branch, head string) {
	raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", ""
	}
	line := strings.TrimSpace(string(raw))
	if after, ok := strings.CutPrefix(line, "ref:"); ok {
		ref := strings.TrimSpace(after)
		branch = strings.TrimPrefix(ref, "refs/heads/")
		head = resolveGitRef(gitDir, ref)
		return branch, head
	}
	return "", line
}

func resolveGitRef(gitDir, ref string) string {
	// Loose ref.
	refPath := filepath.Join(gitDir, filepath.FromSlash(ref))
	if raw, err := os.ReadFile(refPath); err == nil {
		return strings.TrimSpace(string(raw))
	}
	// Packed refs.
	f, err := os.Open(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == ref {
			return fields[0]
		}
	}
	return ""
}

// PathContains reports whether target is equal to or inside root, handling both Unix
// and Windows path separators without requiring either path to exist on the local OS.
func PathContains(root, target string) bool {
	r := normalizeMatchPath(root)
	t := normalizeMatchPath(target)
	if r == "" || t == "" {
		return false
	}
	// Case-insensitive comparison when both look like Windows drive paths.
	if isWindowsDrivePath(r) && isWindowsDrivePath(t) {
		r = strings.ToLower(r)
		t = strings.ToLower(t)
	}
	if t == r {
		return true
	}
	if r == "/" {
		return strings.HasPrefix(t, "/")
	}
	return strings.HasPrefix(t, r+"/")
}

func normalizeMatchPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return p
}
