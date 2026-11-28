package worker

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BryanSant/syre/internal/protocol"
)

// homeFolderNames are the folders people keep code in, checked under home (SYR-W3SVZ).
var homeFolderNames = []string{
	"src", "code", "Code", "dev", "Dev", "git", "repos", "Repos", "projects", "Projects",
	"workspace", "Workspace", "workspaces", "sandbox", "Developer", "source/repos", "work", "go/src",
}

// surveyHome reports which of the common code folders exist under home, plus the checkout
// roots that lie under home, each with the number of git repos directly in it. It never
// lists home itself. Paths are home-relative with "/" between parts.
func surveyHome(home string, checkoutRoots []string) []protocol.HomeFolder {
	if home == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []protocol.HomeFolder
	add := func(rel string) {
		rel = strings.Trim(filepath.ToSlash(rel), "/")
		key := strings.ToLower(rel)
		if rel == "" || rel == "." || strings.HasPrefix(rel, "..") || seen[key] {
			return
		}
		dir := filepath.Join(home, filepath.FromSlash(rel))
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return
		}
		// Case-insensitive file systems answer for "Code" and "code" alike: keep the
		// spelling the folder really has.
		if real := realCase(home, rel); real != "" {
			rel = real
		}
		seen[key] = true
		out = append(out, protocol.HomeFolder{Path: rel, Repos: countRepos(dir)})
	}
	for _, name := range homeFolderNames {
		add(name)
	}
	for _, root := range checkoutRoots {
		if rel, err := filepath.Rel(home, root); err == nil && filepath.IsAbs(root) {
			add(rel)
		}
	}
	return out
}

// realCase returns rel as the directory entries spell it, or "" when it can't tell.
func realCase(home, rel string) string {
	parts := strings.Split(rel, "/")
	dir := home
	for i, p := range parts {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return ""
		}
		found := ""
		for _, e := range entries {
			if e.Name() == p {
				found = p
				break
			}
			if found == "" && strings.EqualFold(e.Name(), p) {
				found = e.Name()
			}
		}
		if found == "" {
			return ""
		}
		parts[i] = found
		dir = filepath.Join(dir, found)
	}
	return strings.Join(parts, "/")
}

// countRepos counts the folders directly in dir that hold a .git (directory or file).
func countRepos(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), ".git")); err == nil {
			n++
		}
	}
	return n
}
