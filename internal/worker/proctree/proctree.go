// Package proctree freezes and thaws a managed session's whole process tree, for the
// user's Suspend/Resume (PRD §5.8a): on Unix SIGSTOP/SIGCONT to the session's process
// group (every managed PTY task and chat engine leads its own group); on Windows every
// thread of the session's processes (the root and all its descendants) is suspended
// with NtSuspendProcess and resumed with NtResumeProcess.
package proctree

import "errors"

// ErrNoProcess means the process (group) no longer exists.
var ErrNoProcess = errors.New("process not found")

// Suspend freezes the process tree rooted at pid (a process group leader on Unix).
func Suspend(pid int) error {
	if pid <= 0 {
		return ErrNoProcess
	}
	return suspend(pid)
}

// Resume thaws the process tree rooted at pid.
func Resume(pid int) error {
	if pid <= 0 {
		return ErrNoProcess
	}
	return resume(pid)
}
