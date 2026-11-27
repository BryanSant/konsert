//go:build linux || darwin

package guard

import (
	"os"
	"runtime"
	"syscall"
)

// rusagePeak is the root process's max RSS from wait4 (bytes on macOS, KiB on Linux).
// It misses descendants, so it is only the fallback when the platform has no
// tree-wide peak (macOS, Linux without a scope).
func rusagePeak(ps *os.ProcessState) uint64 {
	if ps == nil {
		return 0
	}
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil || ru.Maxrss <= 0 {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return uint64(ru.Maxrss)
	}
	return uint64(ru.Maxrss) << 10
}

// killedBySIGKILL reports whether the root died of SIGKILL (what the OOM killer sends).
func killedBySIGKILL(ps *os.ProcessState) bool {
	if ps == nil {
		return false
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}
