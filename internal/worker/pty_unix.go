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
		ptmx: ptmx,
		pgid: cmd.Process.Pid,
	}, nil
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
	return pty.Setsize(p.ptmx, &pty.Winsize{
		Cols: cols,
		Rows: rows,
	})
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
