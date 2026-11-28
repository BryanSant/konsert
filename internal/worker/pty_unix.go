//go:build !windows

package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/BryanSant/syre/internal/worker/guard"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type unixPtyProcess struct {
	guard    *guard.Session
	cmd      *exec.Cmd
	ptmx     *os.File
	pgid     int
	waitOnce sync.Once
	exitCode int
	waitErr  error
	closed   sync.Once
}

func startPlatformPty(_ context.Context, opts PtyStartOptions) (PtyProcess, error) {
	bin, args, launch := preparePty(opts)
	cmd := exec.Command(bin, args...)
	cmd.Dir = opts.Dir
	env := os.Environ()
	env = append(env, "TERM=xterm-256color")
	env = append(env, opts.Env...)
	cmd.Env = env

	// Start in its own session/process group via creack/pty (Setsid: true, Setctty: true).
	// Note: creack/pty.StartWithAttrs sets Setsid=true which makes the child the process group leader
	// (pgid == child pid), allowing clean kill(-pgid, sig) on process tree teardown.
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: opts.Cols,
		Rows: opts.Rows,
	})
	if err != nil {
		return nil, fmt.Errorf("start unix pty for %s: %w", opts.Binary, err)
	}

	p := &unixPtyProcess{
		cmd:  cmd,
		ptmx: pollablePtmx(ptmx),
		pgid: cmd.Process.Pid,
	}
	if launch != nil {
		p.guard = launch.Attach(cmd.Process.Pid)
	}
	return p, nil
}

// pollablePtmx returns a non-blocking, runtime-poller-backed copy of the PTY master so a
// concurrent Close unblocks a pending Read. That lets the supervisor finish a task once the
// main process has exited even while a background child still holds the TTY open. If the
// conversion fails the original (blocking) file is returned unchanged.
func pollablePtmx(ptmx *os.File) *os.File {
	fd, err := syscall.Dup(int(ptmx.Fd()))
	if err != nil {
		return ptmx
	}
	syscall.CloseOnExec(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return ptmx
	}
	nf := os.NewFile(uintptr(fd), ptmx.Name())
	if nf == nil {
		_ = syscall.Close(fd)
		return ptmx
	}
	_ = ptmx.Close()
	return nf
}

func (p *unixPtyProcess) Guard() *guard.Session { return p.guard }

func (p *unixPtyProcess) Pid() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *unixPtyProcess) Read(b []byte) (int, error) {
	return p.ptmx.Read(b)
}

func (p *unixPtyProcess) Write(b []byte) (int, error) {
	return p.ptmx.Write(b)
}

func (p *unixPtyProcess) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	// Use SyscallConn rather than pty.Setsize: Setsize calls File.Fd(), which would put the
	// master back into blocking mode and stop Close from unblocking a pending Read.
	sc, err := p.ptmx.SyscallConn()
	if err != nil {
		return err
	}
	var ioErr error
	if err := sc.Control(func(fd uintptr) {
		ioErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	}); err != nil {
		return err
	}
	return ioErr
}

func (p *unixPtyProcess) Interrupt() error {
	if p.pgid <= 0 {
		return nil
	}
	if err := syscall.Kill(-p.pgid, syscall.SIGINT); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (p *unixPtyProcess) Terminate() error {
	return p.TerminateWithGrace(5 * time.Second)
}

// sigtermGrace is how long the process group gets after SIGTERM before SIGKILL.
const sigtermGrace = 3 * time.Second

func (p *unixPtyProcess) TerminateWithGrace(grace time.Duration) error {
	if p.pgid <= 0 {
		return nil
	}
	return terminateWithGrace(grace, p.Interrupt, p.Wait, func() error {
		// SIGTERM before SIGKILL: tools that ignore SIGINT often clean up on SIGTERM.
		if syscall.Kill(-p.pgid, syscall.SIGTERM) == nil {
			deadline := time.Now().Add(sigtermGrace)
			for time.Now().Before(deadline) {
				if syscall.Kill(-p.pgid, 0) != nil {
					return nil
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		err := syscall.Kill(-p.pgid, syscall.SIGKILL)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			if perr := syscall.Kill(p.pgid, syscall.SIGKILL); perr != nil && !errors.Is(perr, syscall.ESRCH) {
				return perr
			}
			return nil
		}
		if errors.Is(err, syscall.ESRCH) {
			_ = syscall.Kill(p.pgid, syscall.SIGKILL)
		}
		return nil
	})
}

// Hangup sends SIGHUP to the shell's process group and to the terminal's foreground
// process group (a running command has its own), as the kernel does when a terminal
// window closes; whatever is left after grace is killed.
func (p *unixPtyProcess) Hangup(grace time.Duration) error {
	if p.pgid <= 0 {
		return nil
	}
	groups := []int{p.pgid}
	if p.ptmx != nil {
		if fg, err := unix.IoctlGetInt(int(p.ptmx.Fd()), unix.TIOCGPGRP); err == nil && fg > 0 && fg != p.pgid {
			groups = append(groups, fg)
		}
	}
	signal := func(sig syscall.Signal) bool {
		alive := false
		for _, g := range groups {
			if syscall.Kill(-g, sig) == nil {
				alive = true
			}
		}
		return alive
	}
	signal(syscall.SIGCONT) // a stopped job can't handle SIGHUP
	if !signal(syscall.SIGHUP) {
		return nil
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !signal(0) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	signal(syscall.SIGKILL)
	return nil
}

func (p *unixPtyProcess) Wait() (int, error) {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		p.guard.Release(p.cmd.ProcessState)
		if err == nil {
			p.exitCode = 0
			return
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			p.exitCode = exitErr.ExitCode()
			p.waitErr = nil
			return
		}
		p.exitCode = -1
		p.waitErr = err
	})
	return p.exitCode, p.waitErr
}

func (p *unixPtyProcess) Close() error {
	var err error
	p.closed.Do(func() {
		if p.ptmx != nil {
			err = p.ptmx.Close()
		}
	})
	return err
}
