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

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type unixPtyProcess struct {
	cmd      *exec.Cmd
	ptmx     *os.File
	pgid     int
	waitOnce sync.Once
	exitCode int
	waitErr  error
	closed   sync.Once
}

func startPlatformPty(_ context.Context, opts PtyStartOptions) (PtyProcess, error) {
	cmd := exec.Command(opts.Binary, opts.Args...)
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

	return &unixPtyProcess{
		cmd:  cmd,
		ptmx: pollablePtmx(ptmx),
		pgid: cmd.Process.Pid,
	}, nil
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
	if p.pgid <= 0 {
		return nil
	}
	// Escalation: SIGINT -> SIGTERM -> SIGKILL on the process group.
	_ = syscall.Kill(-p.pgid, syscall.SIGINT)

	done := make(chan struct{})
	go func() {
		_, _ = p.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(2 * time.Second):
	}

	_ = syscall.Kill(-p.pgid, syscall.SIGTERM)
	select {
	case <-done:
		return nil
	case <-time.After(3 * time.Second):
	}

	if err := syscall.Kill(-p.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	<-done
	return nil
}

func (p *unixPtyProcess) Wait() (int, error) {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
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
