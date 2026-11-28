package worker

import (
	"context"
	"time"

	"github.com/BryanSant/syre/internal/worker/guard"
)

// PtyStartOptions defines parameters for starting a cross-platform PTY process.
type PtyStartOptions struct {
	Binary string
	Args   []string
	Dir    string
	Env    []string
	Cols   uint16
	Rows   uint16
	// Guard makes the process a managed session for the machine's guardrails (agent
	// tasks). Binary and Args here are ignored; the PTY's own are used. Interactive
	// shells start without one.
	Guard *guard.LaunchSpec
}

// PtyProcess abstracts a pseudo-terminal child process across Unix (PTY) and Windows (ConPTY).
type PtyProcess interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows uint16) error
	Interrupt() error
	Terminate() error
	TerminateWithGrace(grace time.Duration) error
	// Hangup ends a terminal as closing its window would: hang up the shell and its
	// foreground job, wait up to grace, then kill what is left.
	Hangup(grace time.Duration) error
	Wait() (int, error)
	Close() error
	// Pid returns the OS process ID of the PTY's main child process.
	Pid() int
	// Guard is the guardrails session (nil when unguarded).
	Guard() *guard.Session
}

// preparePty returns the launch for opts: guard-prepared when opts.Guard is set.
func preparePty(opts PtyStartOptions) (binary string, args []string, launch *guard.Launch) {
	if opts.Guard == nil {
		return opts.Binary, opts.Args, nil
	}
	spec := *opts.Guard
	spec.Binary, spec.Args = opts.Binary, opts.Args
	l := guard.Default().Prepare(spec)
	return l.Binary, l.Args, l
}

// StartPty spawns a command inside a platform-native PTY/ConPTY.
func StartPty(ctx context.Context, opts PtyStartOptions) (PtyProcess, error) {
	if opts.Cols == 0 {
		opts.Cols = 120
	}
	if opts.Rows == 0 {
		opts.Rows = 36
	}
	return startPlatformPty(ctx, opts)
}

// terminateWithGrace runs the cross-platform graceful-stop sequence: send a graceful
// interrupt, wait up to grace for wait() to return, and if the process has not exited,
// invoke kill() and wait for termination.
func terminateWithGrace(grace time.Duration, interrupt func() error, wait func() (int, error), kill func() error) error {
	_ = interrupt()

	done := make(chan struct{})
	go func() {
		_, _ = wait()
		close(done)
	}()

	if grace > 0 {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-done:
			return nil
		case <-timer.C:
		}
	} else {
		select {
		case <-done:
			return nil
		default:
		}
	}

	err := kill()
	<-done
	return err
}
