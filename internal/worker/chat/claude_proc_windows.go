//go:build windows

package chat

import (
	"os/exec"
	"syscall"
)

// ccSetProcGroup gives the child its own process group so console signals aimed at
// syred do not reach it.
func ccSetProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// ccSignalGroup terminates the process (Windows has no SIGTERM for console children).
func ccSignalGroup(cmd *exec.Cmd, _ bool) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
