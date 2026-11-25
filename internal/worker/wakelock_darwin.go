//go:build darwin

package worker

import (
	"os"
	"strconv"
)

// defaultWakeBackend on macOS runs `caffeinate -i -w <syred pid>`: -i prevents idle
// sleep (the display may still sleep) and -w makes caffeinate exit by itself if syred
// dies without releasing, so a crash never pins the host awake.
func defaultWakeBackend() wakeBackend {
	pid := strconv.Itoa(os.Getpid())
	return &commandWakeBackend{
		method: "caffeinate",
		argv: func(string) []string {
			return []string{"caffeinate", "-i", "-w", pid}
		},
	}
}
