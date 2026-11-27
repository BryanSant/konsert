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
