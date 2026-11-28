package worker

import (
	"path/filepath"
	"syscall"
	"time"
)

// ttyLastInput finds the terminal device dev (a process's controlling tty) among
// /dev/ttys* and returns it with its last access time: a terminal's atime moves when its
// user types.
func ttyLastInput(dev uint64) (string, time.Time, bool) {
	paths, _ := filepath.Glob("/dev/ttys[0-9]*")
	for _, p := range paths {
		var st syscall.Stat_t
		if syscall.Stat(p, &st) != nil || uint64(uint32(st.Rdev)) != dev {
			continue
		}
		return p, time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec), true
	}
	return "", time.Time{}, false
}
