//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach runs cmd in its own session, so it outlives `syre open` and its terminal.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
