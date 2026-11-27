package controller

import (
	"context"
	"fmt"
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
)

var causeGuardrails = store.Cause{Actor: store.ActorSystem, Source: "guardrails", Confidence: store.ConfidenceCertain}

// handleTicketShed records a shed attempt, throttles the machine, and requeues the
// ticket (or, after repeated sheds, hands it to the user).
func (h *Hub) handleTicketShed(ctx context.Context, tk store.Ticket, cs store.ChatSession, task *store.Task, dig *protocol.RunDigest, cerr *protocol.AgentChatError) {
	h.finishTicketAttempt(ctx, tk, cs, task, dig, store.AttemptFailed, protocol.ErrKindMemoryPressureShed, nil, 0)
	h.throttleAfterShed(ctx, cs.WorkerID, cerr.Shed)

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
		"ticket_id", tk.ID, "ticket_sid", tk.SID, "worker_id", cs.WorkerID, "sheds", sheds, "next_status", next)
	h.closeChat(ctx, cs.ID)
	h.kickScheduler()
}

// ticketShedCount counts the ticket's attempts that ended in a shed.
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
