//go:build !windows

package worker

import (
	"os"
	"strconv"
	"syscall"
)

func scratchDirName() string {
	return "syre-attachments-" + strconv.Itoa(os.Getuid())
}

func ownedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
