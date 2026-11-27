//go:build windows

package worker

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/BryanSant/syre/internal/worker/guard"
	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

type windowsPtyProcess struct {
	cpty     *conpty.ConPty
	guard    *guard.Session
	job      windows.Handle // own job when unguarded
	waitOnce sync.Once
	exitCode int
	waitErr  error
	closed   sync.Once
}

func startPlatformPty(_ context.Context, opts PtyStartOptions) (PtyProcess, error) {
	bin, args, launch := preparePty(opts)
	cmdLine := buildWindowsCommandLine(bin, args)
	env := os.Environ()
	env = append(env, "TERM=xterm-256color")
	env = append(env, opts.Env...)

	cptyOpts := []conpty.ConPtyOption{
		conpty.ConPtyDimensions(int(opts.Cols), int(opts.Rows)),
		conpty.ConPtyEnv(env),
	}
	if opts.Dir != "" {
		cptyOpts = append(cptyOpts, conpty.ConPtyWorkDir(opts.Dir))
	}

	c, err := conpty.Start(cmdLine, cptyOpts...)
	if err != nil {
		return nil, fmt.Errorf("start windows conpty for %s: %w", opts.Binary, err)
	}

	p := &windowsPtyProcess{cpty: c}
	if launch != nil {
		// The guard's per-session Job Object (KILL_ON_JOB_CLOSE, CPU weight).
		p.guard = launch.Attach(c.Pid())
	}
	if p.guard.Job() == 0 {
		// Best-effort own Job Object with KILL_ON_JOB_CLOSE so child trees never leak.
		p.job = attachProcessToJobObject(uint32(c.Pid()))
	}
	return p, nil
}

func buildWindowsCommandLine(binary string, args []string) string {
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, syscall.EscapeArg(binary))
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}
	return strings.Join(parts, " ")
}

func attachProcessToJobObject(pid uint32) windows.Handle {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	defer windows.CloseHandle(proc)

	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	return job
}

func (p *windowsPtyProcess) Guard() *guard.Session { return p.guard }

func (p *windowsPtyProcess) Pid() int {
	return p.cpty.Pid()
}

func (p *windowsPtyProcess) Read(b []byte) (int, error) {
	return p.cpty.Read(b)
}

func (p *windowsPtyProcess) Write(b []byte) (int, error) {
	return p.cpty.Write(b)
}

func (p *windowsPtyProcess) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	return p.cpty.Resize(int(cols), int(rows))
}

func (p *windowsPtyProcess) Interrupt() error {
	if pid := p.Pid(); pid > 0 {
		_ = windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
	}
	_, err := p.cpty.Write([]byte{0x03}) // Ctrl+C
	return err
}

func (p *windowsPtyProcess) Terminate() error {
	return p.TerminateWithGrace(5 * time.Second)
}

func (p *windowsPtyProcess) TerminateWithGrace(grace time.Duration) error {
	return terminateWithGrace(grace, p.Interrupt, p.Wait, p.kill)
}

func (p *windowsPtyProcess) kill() error {
	if !p.guard.Kill() && p.job != 0 {
		_ = windows.TerminateJobObject(p.job, 1)
	}
	return p.Close()
}

func (p *windowsPtyProcess) Wait() (int, error) {
	p.waitOnce.Do(func() {
		code, err := p.cpty.Wait(context.Background())
		p.exitCode = int(code)
		p.waitErr = err
	})
	return p.exitCode, p.waitErr
}

func (p *windowsPtyProcess) Close() error {
	var err error
	p.closed.Do(func() {
		err = p.cpty.Close()
		p.guard.Release(nil) // closes the guard's job: KILL_ON_JOB_CLOSE
		if p.job != 0 {
			_ = windows.CloseHandle(p.job)
			p.job = 0
		}
	})
	return err
}
