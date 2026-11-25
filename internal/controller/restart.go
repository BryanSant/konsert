package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/sshutil"
	"github.com/BryanSant/syre/internal/store"
)

// Restarting hub-started daemons that died (SYR-JAX1E). The attach loop (transport.go)
// reports every attach to the restart tracker. A machine whose host answers but whose
// syred is not running (the forced command finds no socket, or the local socket is
// gone) is restarted with the same start path that added it (startWorker: the SSH
// bootstrap or `syred install`, both idempotent: they never start a second daemon)
// once it has failed restartPolicy.failures attaches in a row or for
// restartPolicy.window, then with backoff until it attaches again. A host ssh cannot
// reach is only re-dialed: restarting cannot help it.

// AuditWorkerRestart records an automatic restart of a machine's syred.
const AuditWorkerRestart = "worker.restart"

// restartPolicy says when a down daemon is restarted.
type restartPolicy struct {
	// failures (in a row) or window (failing that long) make a down daemon due.
	failures int
	window   time.Duration
	// backoff[k-1] is the wait after restart k; the last entry repeats.
	backoff []time.Duration
}

var defaultRestartPolicy = restartPolicy{
	failures: 3,
	window:   time.Minute,
	backoff:  []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute},
}

// after is the wait before the restart that follows restart number attempt.
func (p restartPolicy) after(attempt int) time.Duration {
	return p.backoff[min(max(attempt, 1), len(p.backoff))-1]
}

// restartState is one machine's current outage.
type restartState struct {
	failure  sshutil.AttachFailure
	failures int       // attaches failed in a row with this kind of failure
	since    time.Time // the first of them
	lastErr  string
	attempt  int       // restarts so far in this outage
	nextAt   time.Time // earliest next restart (zero: none yet)
}

// restartTracker keeps the outage of every hub-dialed machine that cannot be attached.
type restartTracker struct {
	mu     sync.Mutex
	now    func() time.Time
	policy restartPolicy
	// enableAt holds restarts back until the startup grace has passed (running daemons
	// attach, or reconnect, first).
	enableAt time.Time
	m        map[string]*restartState
}

func newRestartTracker() *restartTracker {
	return &restartTracker{now: time.Now, policy: defaultRestartPolicy, m: map[string]*restartState{}}
}

// holdUntil keeps restarts back until t.
func (t *restartTracker) holdUntil(at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.enableAt = at
}

// holdAtLeast keeps restarts back until at least t (a hub wake: daemons re-attach first).
func (t *restartTracker) holdAtLeast(at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if at.After(t.enableAt) {
		t.enableAt = at
	}
}

// attached ends id's outage; it reports whether there was one (its view changes).
func (t *restartTracker) attached(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.m[id]
	delete(t.m, id)
	return ok
}

// forget drops id (the machine was removed or no longer dialed by the hub).
func (t *restartTracker) forget(id string) { t.attached(id) }

// failed records a failed attach of id. due says the daemon should be restarted now;
// changed says the machine's view changed (its state, or it became visible).
func (t *restartTracker) failed(id string, f sshutil.AttachFailure, msg string) (due, changed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	st := t.m[id]
	if st == nil {
		st = &restartState{}
		t.m[id] = st
	}
	before := st.visible(t.policy)
	if st.failure != f || st.failures == 0 {
		st.failure, st.failures, st.since = f, 0, now
	}
	st.failures++
	if msg != st.lastErr {
		st.lastErr = msg
		changed = true
	}
	if f == sshutil.AttachDaemonDown && !now.Before(t.enableAt) && !now.Before(st.nextAt) &&
		(st.failures >= t.policy.failures || now.Sub(st.since) >= t.policy.window) {
		due = true
	}
	return due, changed || before != st.visible(t.policy)
}

// restarted records a restart of id that began now and returns its attempt number
// and when the next one may run.
func (t *restartTracker) restarted(id string) (int, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.m[id]
	if st == nil {
		st = &restartState{failure: sshutil.AttachDaemonDown, since: t.now()}
		t.m[id] = st
	}
	st.attempt++
	st.nextAt = t.now().Add(t.policy.after(st.attempt))
	return st.attempt, st.nextAt
}

// visible: the outage is worth showing (it lasted a few attaches, or a restart ran).
func (st *restartState) visible(p restartPolicy) bool {
	if st.attempt > 0 {
		return true
	}
	return st.failure != sshutil.AttachFailureOther && st.failures >= p.failures
}

// WorkerRestartView is the outage of a machine the hub starts and cannot attach to.
type WorkerRestartView struct {
	// State is "unreachable" (ssh cannot reach the host: the hub keeps re-dialing) or
	// "down" (the host answers, syred is not running: the hub restarts it).
	State string `json:"state"`
	// Attempt counts the restarts in this outage; NextAt is the earliest next one.
	Attempt int        `json:"attempt,omitempty"`
	NextAt  *time.Time `json:"next_at,omitzero"`
	// Error is the last attach error.
	Error string    `json:"error,omitempty"`
	Since time.Time `json:"since"`
}

// view is id's outage, or nil when there is none worth showing.
func (t *restartTracker) view(id string) *WorkerRestartView {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.m[id]
	if st == nil || !st.visible(t.policy) {
		return nil
	}
	v := &WorkerRestartView{State: "down", Attempt: st.attempt, Error: st.lastErr, Since: st.since.UTC()}
	if st.failure == sshutil.AttachUnreachable {
		v.State = "unreachable"
	}
	if !st.nextAt.IsZero() {
		n := st.nextAt.UTC()
		v.NextAt = &n
	}
	return v
}

// workerRestartView is the machine view's restart state (nil while it is connected).
func (h *Hub) workerRestartView(workerID string) *WorkerRestartView {
	if h.IsWorkerConnected(workerID) {
		return nil
	}
	return h.restarts.view(workerID)
}

// errDaemonSocket marks a failed dial of the hub machine's own syred socket: the
// daemon is not running (the host is this one, so it is reachable).
type errDaemonSocket struct{ err error }

func (e errDaemonSocket) Error() string { return sshutil.DaemonDownMarker + ": " + e.err.Error() }
func (e errDaemonSocket) Unwrap() error { return e.err }

// classifyAttach says what a failed attach over transport means.
func classifyAttach(transport string, err error) sshutil.AttachFailure {
	if err == nil {
		return sshutil.AttachFailureOther
	}
	if transport == TransportLocal {
		if errors.As(err, new(errDaemonSocket)) {
			return sshutil.AttachDaemonDown
		}
		return sshutil.AttachFailureOther
	}
	return sshutil.ClassifyAttachFailure(err.Error())
}

// onAttached is the attach loop's report of an established link.
func (s *Server) onAttached(ctx context.Context, workerID string) {
	if s.hub.restarts.attached(workerID) {
		s.hub.broadcastWorkerView(ctx, workerID)
	}
}

// onAttachFailed is the attach loop's report of a failed attach to w. A daemon that is
// down on a reachable host is restarted when the policy says it is due.
func (s *Server) onAttachFailed(ctx context.Context, w store.Worker, transport string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	due, changed := s.hub.restarts.failed(w.ID, classifyAttach(transport, err), msg)
	if due && !s.hub.IsWorkerConnected(w.ID) {
		switch serr := s.startWorker(ctx, w); {
		case errors.Is(serr, errAlreadyStarting):
			// A start (by hand, or the last restart) is still running: a later failure
			// tries again.
		case serr != nil:
			s.logger.Warn("restarting syred failed", "worker_id", w.ID, "err", serr)
		default:
			attempt, next := s.hub.restarts.restarted(w.ID)
			s.logger.Warn("syred on "+w.Name+" is down; restarting it", "worker_id", w.ID, "launch", w.Launch,
				"attempt", attempt, "next_in", time.Until(next).Round(time.Second), "err", msg)
			s.hub.recordAudit(ctx, store.AuditEvent{
				Action: AuditWorkerRestart, WorkerID: w.ID, WorkerSID: w.SID,
				Message: fmt.Sprintf("Restarted syred on %s (attempt %d): %s", w.Name, attempt, msg),
			})
			changed = true
		}
	}
	if changed {
		s.hub.broadcastWorkerView(ctx, w.ID)
	}
}
