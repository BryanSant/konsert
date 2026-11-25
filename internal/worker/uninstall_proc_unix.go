//go:build !windows

package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// otherKonsertdRunning reports whether another konsertd daemon process (`konsertd run`)
// is currently running on this host.
func otherKonsertdRunning(binPath string) (bool, error) {
	out, err := exec.Command("ps", "-eo", "pid=,args=").Output()
	if err != nil {
		return false, err
	}
	self := os.Getpid()
	parent := os.Getppid()
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pidStr, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
		if err != nil || pid <= 0 || pid == self || pid == parent {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			continue
		}
		cmd := fields[0]
		if filepath.Base(cmd) != "konsertd" && (binPath == "" || !samePath(cmd, binPath)) {
			continue
		}
		if slices.Contains(fields[1:], "run") {
			return true, nil
		}
	}
	return false, nil
}
