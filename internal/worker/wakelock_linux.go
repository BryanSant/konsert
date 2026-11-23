//go:build linux

package worker

import (
	"os"
	"strconv"
)

// defaultWakeBackend on Linux holds a logind idle+sleep inhibitor with systemd-inhibit.
// Instead of `sleep infinity`, the inhibited child is a small shell loop that exits once
// konsertd is gone, so a crashed konsertd never leaves the host pinned awake.
func defaultWakeBackend() wakeBackend {
	pid := strconv.Itoa(os.Getpid())
	return &commandWakeBackend{
		method: "systemd-inhibit",
		argv: func(reason string) []string {
			return []string{
				"systemd-inhibit",
				"--what=idle:sleep",
				"--who=konsert",
				"--why=" + reason,
				"--mode=block",
				"/bin/sh", "-c", `while kill -0 "$0" 2>/dev/null; do sleep 15; done`, pid,
			}
		},
	}
}
