//go:build !windows

package shells

import "syscall"

type signalKiller struct{}

// OSKiller signals processes with SIGTERM / SIGKILL.
func OSKiller() Killer { return signalKiller{} }

func (signalKiller) Terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func (signalKiller) Kill(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
