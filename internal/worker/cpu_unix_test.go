//go:build !windows

package worker

import (
	"syscall"
	"time"
)

// processCPU is this process's user + system CPU time so far.
func processCPU() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
