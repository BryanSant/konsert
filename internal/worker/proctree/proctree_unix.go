//go:build !windows

package proctree

import (
	"errors"
	"syscall"
)

func suspend(pid int) error { return signalGroup(pid, syscall.SIGSTOP) }

func resume(pid int) error { return signalGroup(pid, syscall.SIGCONT) }

// signalGroup signals pid's process group (pid leads it), falling back to the process
// itself when it does not lead a group.
func signalGroup(pid int, sig syscall.Signal) error {
	err := syscall.Kill(-pid, sig)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		if perr := syscall.Kill(pid, sig); perr != nil {
			if errors.Is(perr, syscall.ESRCH) {
				return ErrNoProcess
			}
			return perr
		}
		return nil
	}
	return err
}
