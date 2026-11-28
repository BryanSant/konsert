package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Guardrails outcomes on the hub (docs/design/guardrails.md). A machine sheds a session
// to keep itself alive; that is not a failure of the work, so the ticket goes back to
// ready without spending a retry, and the machine takes less work for a while.

const (
	// shedEscalateAfter sheds of one ticket raise it to the user instead of requeueing
	// again: it may need more memory than any machine it lands on can give.
	shedEscalateAfter = 3
	// shedThrottleFor is how long a machine stays throttled after shedding work.
	shedThrottleFor = 30 * time.Minute
	// defaultShedCooldown is how long a machine gets no new ticket runs after a shed,
	// so it can recover before work lands on it again. Without it, a machine still
	// short of memory sheds each new run within seconds and burns a ticket's sheds.
	// Each further shed in a streak doubles it, up to maxShedCooldown.
	defaultShedCooldown = 2 * time.Minute
	maxShedCooldown     = time.Hour
	// shedStreakQuiet: a machine that goes this long past its cooldown without
	// shedding again starts its next streak from the base cooldown.
	shedStreakQuiet = time.Hour
)

// shedCooldowns records, per machine, until when it gets no new ticket runs. A machine
// that keeps shedding (memory taken by something outside Syre, say) backs off
// exponentially instead of shedding a ticket every few minutes forever; a run that
// ends any other way on it ends the streak.
type shedCooldowns struct {
	mu sync.Mutex
	m  map[string]*shedStreak
	// period overrides defaultShedCooldown (tests); 0 means the default.
	period time.Duration
}

type shedStreak struct {
	n     int // sheds in the current streak
	until time.Time
}

func (c *shedCooldowns) base() time.Duration {
	if c.period > 0 {
		return c.period
	}
	return defaultShedCooldown
}

// start records a shed on the machine and returns how long it now cools down.
func (c *shedCooldowns) start(workerID string, now time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*shedStreak{}
	}
	st := c.m[workerID]
	if st == nil || now.Sub(st.until) > shedStreakQuiet {
		st = &shedStreak{}
		c.m[workerID] = st
	}
	st.n++
	d := c.base()
	limit := max(maxShedCooldown, d)
	for i := 1; i < st.n && d < limit; i++ {
		d *= 2
	}
	d = min(d, limit)
	st.until = now.Add(d)
	return d
}

// active reports whether the machine is cooling down.
func (c *shedCooldowns) active(workerID string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.m[workerID]
	return st != nil && now.Before(st.until)
}

// until is when the machine's cooldown ends (ok false: none running).
func (c *shedCooldowns) until(workerID string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.m[workerID]
	if st == nil || !time.Now().Before(st.until) {
		return time.Time{}, false
	}
	return st.until, true
}

// latest is the end of the last cooldown still running at now.
func (c *shedCooldowns) latest(now time.Time) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var last time.Time
	for _, st := range c.m {
		if st.until.After(now) && st.until.After(last) {
			last = st.until
		}
	}
	return last, !last.IsZero()
}

// clear ends the machine's shed streak: a run finished on it without a shed.
func (c *shedCooldowns) clear(workerID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, workerID)
}

// restoreShedCooldowns rebuilds the cooldowns a restart forgot by replaying recent
// attempts in the order they ended: a shed (memory_pressure_shed or host_memory_full)
// extends the machine's streak at its end time, a completed run or one sent to review
// ends it, as they do live. Only attempts recent enough to still matter are read: the
// longest cooldown plus the quiet time that ends a streak.
func (h *Hub) restoreShedCooldowns(ctx context.Context) {
	now := time.Now()
	rows, err := h.store.ListAttemptsEndedSince(ctx, now.Add(-(maxShedCooldown + shedStreakQuiet)))
	if err != nil {
		h.logger.Warn("guardrails: restoring shed cooldowns failed", "err", err)
		return
	}
	for _, a := range rows {
		if a.WorkerID == "" || a.EndedAt == nil {
			continue
		}
		switch {
		case a.FailureCategory == protocol.ErrKindMemoryPressureShed || a.FailureCategory == protocol.FailureHostMemoryFull:
			h.shedCooldown.start(a.WorkerID, *a.EndedAt)
		case a.Status == store.AttemptCompleted || a.Status == store.AttemptReview:
			h.shedCooldown.clear(a.WorkerID)
		}
	}
	if until, ok := h.shedCooldown.latest(now); ok {
		time.AfterFunc(time.Until(until)+time.Second, h.kickScheduler)
	}
}

var causeGuardrails = store.Cause{Actor: store.ActorSystem, Source: "guardrails", Confidence: store.ConfidenceCertain}

// handleTicketShed records a shed attempt, throttles the machine, and requeues the
// ticket (or, after repeated sheds it caused, hands it to the user). A bystander shed,
// where the run held little of the memory in use, is recorded as host_memory_full and
// never counts toward escalation: it says nothing about what the ticket needs.
func (h *Hub) handleTicketShed(ctx context.Context, tk store.Ticket, cs store.ChatSession, task *store.Task, dig *protocol.RunDigest, cerr *protocol.AgentChatError) {
	category := protocol.ErrKindMemoryPressureShed
	if cerr.Shed != nil && cerr.Shed.Bystander {
		category = protocol.FailureHostMemoryFull
	}
	h.finishTicketAttempt(ctx, tk, cs, task, dig, store.AttemptFailed, category, nil, 0)
	h.throttleAfterShed(ctx, cs.WorkerID, cerr.Shed)
	if cs.WorkerID != "" {
		wait := h.shedCooldown.start(cs.WorkerID, time.Now())
		time.AfterFunc(wait+time.Second, h.kickScheduler)
	}

	sheds := h.ticketShedCount(ctx, tk.ID)
	next := store.TicketStatusReady
	if sheds >= shedEscalateAfter {
		next = store.TicketStatusNeedsYou
	}
	summary := cerr.Message
	if next == store.TicketStatusReady {
		summary += " Requeued."
	} else {
		summary += fmt.Sprintf(" Shed %d times: it may need more memory than these machines have.", sheds)
	}
	if updated, changed, err := h.store.ReleaseTicket(ctx, tk.ID, next); err == nil && changed {
		h.broadcastFleetUpdate("ticket", updated)
		if next == store.TicketStatusNeedsYou {
			h.pulseTicketTransition(ctx, updated, task, dig, protocol.ErrKindMemoryPressureShed)
		}
	}
	if task != nil && !protocol.IsTerminalTaskStatus(task.Status) {
		_, _ = h.setTaskStatus(ctx, task.ID, protocol.TaskStatusFailed, summary, "", nil, causeGuardrails)
	}
	h.logger.Warn("ticket run shed by machine guardrails",
		"ticket_id", tk.ID, "ticket_sid", tk.SID, "worker_id", cs.WorkerID, "sheds", sheds, "category", category, "next_status", next)
	h.closeChat(ctx, cs.ID)
	h.kickScheduler()
}

// ticketShedCount counts the ticket's attempts that ended in a shed it caused
// (bystander sheds are host_memory_full and don't count).
func (h *Hub) ticketShedCount(ctx context.Context, ticketID string) int {
	rows, err := h.store.ListAttempts(ctx, ticketID, "")
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range rows {
		if r.FailureCategory == protocol.ErrKindMemoryPressureShed {
			n++
		}
	}
	return n
}

// throttleAfterShed caps a machine that just shed work at one fewer session than it
// was running, for shedThrottleFor, attributed to the guardrails and visible with undo
// like any directive. Nothing is filed when that would not tighten the machine.
func (h *Hub) throttleAfterShed(ctx context.Context, workerID string, ev *protocol.ShedEvidence) {
	if workerID == "" {
		return
	}
	w, err := h.store.GetWorker(ctx, workerID)
	if err != nil {
		return
	}
	limit := max(w.MaxConcurrent, 1)
	if ev != nil && ev.ActiveSessions > 0 {
		limit = min(limit, ev.ActiveSessions)
	}
	capN := max(limit-1, 1)
	now := h.directiveClock()
	effective := max(w.MaxConcurrent, 1)
	if list, err := h.store.ListDirectives(ctx, store.DirectiveFilter{
		Target: workerID, Kind: store.DirectiveMachineThrottle, ActiveOnly: true, Now: now, Limit: 50,
	}); err == nil {
		for _, d := range list {
			if d.ActiveAt(now) && d.Params.MaxConcurrency != nil {
				effective = min(effective, *d.Params.MaxConcurrency)
			}
		}
	}
	if capN >= effective {
		return
	}
	until := now.Add(shedThrottleFor)
	reason := "guardrails shed work under memory pressure"
	switch {
	case ev == nil || ev.ActiveSessions <= 0:
	case ev.ActiveSessions == 1:
		reason += " with 1 session running"
	default:
		reason += fmt.Sprintf(" with %d sessions running", ev.ActiveSessions)
	}
	if _, err := h.CreateDirective(withCause(ctx, causeGuardrails), DirectiveInput{
		Kind: store.DirectiveMachineThrottle, Target: workerID, Until: &until, Reason: reason,
		Params: store.DirectiveParams{MaxConcurrency: &capN},
	}); err != nil {
		h.logger.Warn("guardrails throttle after shed failed", "worker_id", workerID, "err", err)
	}
}
