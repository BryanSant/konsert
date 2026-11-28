package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach starts cmd without a console and outside this process group, so it outlives
// `syre open`.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
		HideWindow:    true,
	}
}
