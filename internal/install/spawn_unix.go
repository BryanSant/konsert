//go:build !windows

package install

import (
	"errors"
	"os/exec"
	"syscall"
)

// spawnDetached starts name in its own session and does not wait for it.
func spawnDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// ScheduleDeleteOnExit is only needed on Windows (Unix unlinks running executables directly).
func ScheduleDeleteOnExit(string, string) error {
	return errors.New("scheduled delete on exit is Windows-only")
}
