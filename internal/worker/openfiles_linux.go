//go:build linux

package worker

import (
	"os"
	"path/filepath"
	"strconv"
)

// processOpenFiles lists the files pid holds open (/proc/<pid>/fd), for matching an
// agent process to the session file it writes.
func processOpenFiles(pid int) []string {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if target, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil && filepath.IsAbs(target) {
			out = append(out, target)
		}
	}
	return out
}
