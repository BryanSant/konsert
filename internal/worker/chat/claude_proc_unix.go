//go:build !windows

package chat

import (
	"os/exec"
	"syscall"
)

// ccSetProcGroup starts the command in its own process group so the whole tree
// (tool subprocesses included) can be signalled at once.
func ccSetProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// ccSignalGroup sends SIGTERM (or SIGKILL when hard) to the command's process group.
func ccSignalGroup(cmd *exec.Cmd, hard bool) {
	if cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		_ = cmd.Process.Signal(sig)
	}
}
