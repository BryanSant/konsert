package install

import (
	"context"
	"encoding/csv"
	"errors"
	"strings"
	"time"
)

// Starting an installed instance now and waiting for it (`konsertd status --start`). The
// SSH bootstrap uses it after `konsertd install` on Windows: a logon task registered from
// an SSH session may not start on /Run right away (the previous version still exiting,
// Task Scheduler still queuing it), and the task's own state lags behind the daemon. So
// the daemon itself decides: running means the task runs or the daemon answers on its
// local socket.

// ErrNoStart means the instance's mechanism has no "start now" here.
var ErrNoStart = errors.New("starting on demand is not supported by this mechanism")

// starter is a backend that can start an installed instance on demand.
type starter interface {
	start(ctx context.Context, in *Installer, instance string) error
}

func (s schtasksBackend) start(ctx context.Context, in *Installer, instance string) error {
	_, err := in.Runner.Run(ctx, "", "schtasks", "/Run", "/TN", TaskName(instance))
	return err
}

// WaitOptions tune StartAndWait.
type WaitOptions struct {
	// Wait is how long to wait for the daemon (default 20 s).
	Wait time.Duration
	// Poll is the interval between checks (default 500 ms).
	Poll time.Duration
	// Alive reports whether the daemon answers (its local socket); nil = rely on the
	// mechanism's state only.
	Alive func(ctx context.Context) bool
	// Sleep waits between checks (tests replace it); nil = a timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

// StartAndWait starts the installed instance if it is not running and waits until it
// runs (the mechanism says so or opts.Alive answers) or opts.Wait passes. It returns the
// last status; Running is true when either signal said so.
func (in *Installer) StartAndWait(ctx context.Context, instance string, opts WaitOptions) (Status, error) {
	b, err := defaultBackend()
	if err != nil {
		return Status{}, err
	}
	return in.startAndWait(ctx, b, instance, opts)
}

func (in *Installer) startAndWait(ctx context.Context, b backend, instance string, opts WaitOptions) (Status, error) {
	if opts.Wait <= 0 {
		opts.Wait = 20 * time.Second
	}
	if opts.Poll <= 0 {
		opts.Poll = 500 * time.Millisecond
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	check := func() (Status, error) {
		st, err := b.status(ctx, in, instance)
		if err != nil {
			return st, err
		}
		if !st.Running && opts.Alive != nil && opts.Alive(ctx) {
			st.Running = true
			if st.Detail == "" || !strings.EqualFold(st.Detail, "Running") {
				st.Detail = strings.TrimSpace(st.Detail + " (daemon answering)")
			}
		}
		return st, nil
	}
	st, err := check()
	if err != nil || st.Running || !st.Installed {
		return st, err
	}
	sb, ok := b.(starter)
	if !ok {
		return st, nil
	}
	if err := sb.start(ctx, in, instance); err != nil {
		return st, err
	}
	retried := false
	for waited := time.Duration(0); waited < opts.Wait; waited += opts.Poll {
		if err := opts.Sleep(ctx, opts.Poll); err != nil {
			return st, err
		}
		if st, err = check(); err != nil || st.Running {
			return st, err
		}
		// Halfway, ask once more: a /Run that raced the old instance's exit is ignored.
		if !retried && waited >= opts.Wait/2 {
			retried = true
			_ = sb.start(ctx, in, instance)
		}
	}
	return st, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// parseSchtasksQuery reads `schtasks /Query /TN name /FO CSV /NH`: whether the task is
// listed and its state ("Ready", "Running", "Queued", "Disabled", or the localized
// word). The output may carry a byte order mark, blank lines and CRLF.
func parseSchtasksQuery(out, name string) (found bool, state string) {
	out = strings.TrimPrefix(strings.TrimSpace(out), "\uFEFF")
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	recs, _ := r.ReadAll()
	for _, rec := range recs {
		if len(rec) >= 3 && strings.EqualFold(strings.TrimSpace(rec[0]), name) {
			found, state = true, strings.TrimSpace(rec[2])
			if strings.EqualFold(state, "Running") {
				return true, state // one running instance row wins over others
			}
		}
	}
	return found, state
}

// schtasksStateRunning reports whether a task state means it runs now.
func schtasksStateRunning(state string) bool { return strings.EqualFold(state, "Running") }
