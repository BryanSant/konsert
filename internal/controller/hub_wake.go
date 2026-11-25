package controller

import (
	"context"
	"sync"
	"time"
)

// Hub wake grace (KON-VZR9F, docs/design/spool.md). When the hub's machine sleeps (or
// its process is stopped), every link drops and every timer fires at once on wake: the
// stale-machine reaper would mark every machine offline and fail its tasks, and lost-task
// checks would fire against connections that are already dead, before any daemon had a
// chance to re-attach and replay what it spooled. A clock jump (wall or monotonic) past
// WakeAfter counts as a wake: for WakeGrace, or until a machine re-attaches, whichever
// comes first, those hub-side actions that assume the hub was watching wait. Real-time
// deadlines (override expiry, veto escalation) are not affected. Fleet shows the
// machines that have not re-attached yet as "reconnecting after sleep".

const (
	defaultWakeGrace      = 2 * time.Minute
	defaultWakeWatchEvery = 5 * time.Second
	defaultWakeAfter      = 30 * time.Second
)

// wakeGuard detects hub wakes and tracks the grace that follows.
type wakeGuard struct {
	mu      sync.Mutex
	last    time.Time       // last observation (may carry a monotonic reading)
	until   time.Time       // grace end (zero: no grace)
	waiting map[string]bool // machines linked at the wake that have not re-attached
	changed chan struct{}   // closed and replaced on every change
}

func (g *wakeGuard) changedLocked() chan struct{} {
	if g.changed == nil {
		g.changed = make(chan struct{})
	}
	return g.changed
}

func (g *wakeGuard) notifyLocked() {
	if g.changed != nil {
		close(g.changed)
	}
	g.changed = make(chan struct{})
}

// observe looks at the clock (now, which may carry a monotonic reading) and reports
// whether the hub just woke: the previous observation is more than after ago. linked
// returns the machines whose links were up when the hub went to sleep.
func (g *wakeGuard) observe(now time.Time, after, grace time.Duration, linked func() []string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	prev := g.last
	g.last = now
	if prev.IsZero() {
		return false
	}
	gap := max(now.Sub(prev), now.Round(0).Sub(prev.Round(0)))
	if gap <= after {
		return false
	}
	g.until = now.Round(0).Add(grace)
	g.waiting = map[string]bool{}
	if linked != nil {
		for _, id := range linked() {
			g.waiting[id] = true
		}
	}
	g.notifyLocked()
	return true
}

// active reports whether the hub is in a wake grace at now.
func (g *wakeGuard) active(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.until.IsZero() && now.Round(0).Before(g.until)
}

// machineInGrace reports whether workerID was linked when the hub slept and has not
// re-attached since, while the wake grace runs at now.
func (g *wakeGuard) machineInGrace(workerID string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.until.IsZero() && now.Round(0).Before(g.until) && g.waiting[workerID]
}

// attach records that workerID re-attached (its grace ends).
func (g *wakeGuard) attach(workerID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.waiting[workerID] {
		return
	}
	delete(g.waiting, workerID)
	g.notifyLocked()
}

// wait blocks while workerID is in a wake grace (until it re-attaches or the grace ends)
// or ctx ends. It reports whether it waited.
func (g *wakeGuard) wait(ctx context.Context, workerID string) bool {
	waited := false
	for {
		now := time.Now()
		g.mu.Lock()
		in := !g.until.IsZero() && now.Round(0).Before(g.until) && g.waiting[workerID]
		ch := g.changedLocked()
		left := g.until.Sub(now.Round(0))
		g.mu.Unlock()
		if !in {
			return waited
		}
		waited = true
		t := time.NewTimer(left)
		select {
		case <-ctx.Done():
			t.Stop()
			return waited
		case <-ch:
		case <-t.C:
		}
		t.Stop()
	}
}

func (h *Hub) wakeGrace() time.Duration {
	if h.WakeGrace > 0 {
		return h.WakeGrace
	}
	return defaultWakeGrace
}

func (h *Hub) wakeAfter() time.Duration {
	if h.WakeAfter > 0 {
		return h.WakeAfter
	}
	return defaultWakeAfter
}

// checkWake looks at the clock and starts a wake grace when the hub just woke. Every
// path that acts on silence calls it first: on wake, timers fire in any order.
func (h *Hub) checkWake(now time.Time) bool {
	if !h.sleepWake.observe(now, h.wakeAfter(), h.wakeGrace(), h.linkedWorkers) {
		return false
	}
	h.logger.Warn("hub woke from sleep: waiting for machines to re-attach before treating anything as lost", "grace", h.wakeGrace())
	if h.restarts != nil {
		// Running daemons re-attach on their own: no restart before the grace is over.
		h.restarts.holdAtLeast(now.Round(0).Add(h.wakeGrace()))
	}
	h.broadcastAllWorkers()
	h.goBackground(func(ctx context.Context) {
		t := time.NewTimer(h.wakeGrace())
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
			h.broadcastAllWorkers() // "reconnecting after sleep" ends
		}
	})
	return true
}

// inWakeGrace reports whether workerID waits out a wake grace (checking the clock first).
func (h *Hub) inWakeGrace(workerID string) bool {
	now := time.Now()
	h.checkWake(now)
	return h.sleepWake.machineInGrace(workerID, now)
}

// wakeLoop watches the clock for wakes until the hub stops.
func (h *Hub) wakeLoop(ctx context.Context) {
	every := h.WakeWatchEvery
	if every <= 0 {
		every = defaultWakeWatchEvery
	}
	h.checkWake(time.Now())
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.checkWake(time.Now())
		}
	}
}

// broadcastAllWorkers re-sends every machine's fleet view (wake grace changes).
func (h *Hub) broadcastAllWorkers() {
	if h.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	defer cancel()
	workers, err := h.store.ListWorkers(ctx)
	if err != nil {
		return
	}
	counts := h.ActiveTaskCounts()
	for _, w := range workers {
		v := toWorkerView(w, counts[w.ID], h.WorkerMetrics(w.ID))
		if v.Status == "online" && !h.IsWorkerConnected(w.ID) {
			v.Status = "offline"
		}
		h.broadcastFleetUpdate("worker", v)
	}
}

// linkedWorkers lists the machines with a registered connection.
func (h *Hub) linkedWorkers() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.workers))
	for id := range h.workers {
		out = append(out, id)
	}
	return out
}
