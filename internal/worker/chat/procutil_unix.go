//go:build !windows

package chat

import (
	"os/exec"
	"syscall"
)

const envCaseInsensitive = false

// setProcGroup makes the child lead a new process group so the whole tree can be signalled.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup signals the child's process group: SIGTERM, or SIGKILL when hard.
func killGroup(cmd *exec.Cmd, hard bool) {
	if cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, sig); err != nil && !hard {
		_ = cmd.Process.Signal(sig)
	}
}
