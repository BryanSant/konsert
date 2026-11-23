//go:build unix

package worker

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// commandWakeBackend holds a wake lock by keeping a long-lived helper process (caffeinate,
// systemd-inhibit) running in its own process group. Releasing signals the whole group and
// reaps the helper, so nothing is left behind.
type commandWakeBackend struct {
	method string
	// argv builds the helper command line for reason; argv[0] is resolved with lookPath.
	argv func(reason string) []string
	// lookPath resolves argv[0] (exec.LookPath by default; injectable for tests).
	lookPath func(string) (string, error)
	// startupProbe is how long Acquire waits to catch a helper that exits right away
	// (e.g. systemd-inhibit without a reachable logind).
	startupProbe time.Duration
	// stopTimeout bounds the wait after SIGTERM before the group is SIGKILLed.
	stopTimeout time.Duration
}

func (b *commandWakeBackend) Method() string { return b.method }

func (b *commandWakeBackend) Acquire(reason string) (wakeHolder, error) {
	argv := b.argv(reason)
	if len(argv) == 0 {
		return nil, errors.New("no wake lock command")
	}
	look := b.lookPath
	if look == nil {
		look = exec.LookPath
	}
	path, err := look(argv[0])
	if err != nil {
		return nil, fmt.Errorf("%s not found on PATH: %w", argv[0], err)
	}
	stderr := &cappedBuffer{max: 4096}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = stderr
	// Own process group: Release signals the helper and any child it spawned
	// (systemd-inhibit runs the watchdog shell as its child).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", argv[0], err)
	}
	h := &procWakeHolder{cmd: cmd, name: argv[0], stderr: stderr, done: make(chan struct{}), stopTimeout: b.stopTimeout}
	go h.wait()

	probe := b.startupProbe
	if probe <= 0 {
		probe = 300 * time.Millisecond
	}
	select {
	case <-h.done:
		return nil, h.Err()
	case <-time.After(probe):
	}
	return h, nil
}

type procWakeHolder struct {
	cmd         *exec.Cmd
	name        string
	stderr      *cappedBuffer
	stopTimeout time.Duration

	done    chan struct{}
	waitErr error // set before done is closed

	releaseOnce sync.Once
}

// wait reaps the helper (so it never lingers as a zombie) and closes done.
func (h *procWakeHolder) wait() {
	h.waitErr = h.cmd.Wait()
	close(h.done)
}

func (h *procWakeHolder) Done() <-chan struct{} { return h.done }

func (h *procWakeHolder) Err() error {
	select {
	case <-h.done:
	default:
		return nil
	}
	msg := strings.TrimSpace(h.stderr.String())
	switch {
	case msg != "" && h.waitErr != nil:
		return fmt.Errorf("%s exited (%v): %s", h.name, h.waitErr, msg)
	case msg != "":
		return fmt.Errorf("%s exited: %s", h.name, msg)
	case h.waitErr != nil:
		return fmt.Errorf("%s exited: %w", h.name, h.waitErr)
	default:
		return fmt.Errorf("%s exited", h.name)
	}
}

// Release sends SIGTERM to the helper's process group, waits for it to be reaped, and
// falls back to SIGKILL after stopTimeout.
func (h *procWakeHolder) Release() error {
	var err error
	h.releaseOnce.Do(func() {
		select {
		case <-h.done:
			// Already exited and reaped. Do not signal the group: once reaped, its ID
			// could in principle belong to someone else.
			return
		default:
		}
		// The helper leads its own group, so the group ID is its PID. It cannot be
		// reused before wait() reaps the helper.
		pgid := h.cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		timeout := h.stopTimeout
		if timeout <= 0 {
			timeout = 3 * time.Second
		}
		select {
		case <-h.done:
		case <-time.After(timeout):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			select {
			case <-h.done:
			case <-time.After(timeout):
				err = fmt.Errorf("%s (pid %d) did not exit after SIGKILL", h.name, pgid)
				return
			}
		}
	})
	return err
}

// cappedBuffer keeps the first max bytes written to it (helper stderr for error messages).
type cappedBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - len(c.buf); room > 0 {
		if len(p) > room {
			c.buf = append(c.buf, p[:room]...)
		} else {
			c.buf = append(c.buf, p...)
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}
