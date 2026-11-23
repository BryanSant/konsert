//go:build windows

package chat

import (
	"os/exec"
	"strconv"
	"syscall"
)

const envCaseInsensitive = true

// setProcGroup starts the child in a new process group (no console Ctrl+C propagation).
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killGroup terminates the child and its descendants. Windows has no graceful group
// signal for console-less children, so both stages kill the tree.
func killGroup(cmd *exec.Cmd, hard bool) {
	if cmd.Process == nil {
		return
	}
	args := []string{"/T", "/PID", strconv.Itoa(cmd.Process.Pid)}
	if hard {
		args = append([]string{"/F"}, args...)
	}
	kill := exec.Command("taskkill", args...)
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := kill.Run(); err != nil && hard {
		_ = cmd.Process.Kill()
	}
}
