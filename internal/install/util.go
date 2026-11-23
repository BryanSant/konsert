package install

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func mkdirAll(dir string) error {
	if dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

// CleanPATH drops relative and duplicate entries from a PATH value: a daemon must not
// resolve programs against its working directory ("." or empty entries).
func CleanPATH(path string, sep rune) string {
	seen := map[string]bool{}
	var out []string
	for _, p := range strings.Split(path, string(sep)) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] || !(filepath.IsAbs(p) || isWindowsAbs(p)) {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return strings.Join(out, string(sep))
}

// isWindowsAbs reports a drive-absolute (C:\x) or UNC path, so tests on any GOOS agree.
func isWindowsAbs(p string) bool {
	if strings.HasPrefix(p, `\\`) {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}
