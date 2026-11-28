package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Cause model (PRD §5.8a): every task and chat status transition, intervention and audit
// row records who caused it (actor), through which path, how sure Syre is, and what it
// correlates with. Causes travel in the request context; code paths without a caller
// (worker reports, the reaper, the scheduler) pass an explicit cause.

type causeKey struct{}

// withCause attaches an explicit cause to ctx; it wins over the audit actor.
func withCause(ctx context.Context, c store.Cause) context.Context {
	return context.WithValue(ctx, causeKey{}, c)
}

type chatOriginKey struct{}

// withChatOrigin records that the request came from an agent chat's MCP endpoint, so
// dispatched tasks remember their parent chat and actions are attributed to it.
func withChatOrigin(ctx context.Context, chatID string, commander bool) context.Context {
	return context.WithValue(ctx, chatOriginKey{}, chatOrigin{id: chatID, commander: commander})
}

type chatOrigin struct {
	id        string
	commander bool
}

func chatOriginFrom(ctx context.Context) (chatOrigin, bool) {
	o, ok := ctx.Value(chatOriginKey{}).(chatOrigin)
	return o, ok && o.id != ""
}

// causeFrom derives the cause of an action from ctx: an explicit cause, else the agent
// chat the request came from, else the authenticated audit actor and its path.
func causeFrom(ctx context.Context) store.Cause {
	if c, ok := ctx.Value(causeKey{}).(store.Cause); ok && !c.IsZero() {
		return c
	}
	if o, ok := chatOriginFrom(ctx); ok {
		if o.commander {
			return store.Cause{Actor: store.ActorCommander, Source: "mcp", ActorID: o.id, Confidence: store.ConfidenceCertain}
		}
		return store.Cause{Actor: store.ActorAgent, Source: "mcp", ActorID: o.id, Confidence: store.ConfidenceCertain}
	}
	a, ok := ctx.Value(auditActorKey{}).(auditActor)
	if !ok || a.User == "" {
		return store.Cause{Actor: store.ActorSystem, Confidence: store.ConfidenceCertain}
	}
	switch a.Source {
	case auditSourceREST, auditSourceWS:
		return store.Cause{Actor: store.ActorUser, Source: store.SourceUI, ActorID: a.User, Confidence: store.ConfidenceCertain}
	case auditSourceCLI:
		return store.Cause{Actor: store.ActorUser, Source: store.SourceCLI, ActorID: a.User, Confidence: store.ConfidenceCertain}
	case auditSourceCommander:
		return store.Cause{Actor: store.ActorCommander, Source: "commander", Confidence: store.ConfidenceCertain}
	case auditSourceMCP:
		if id, ok := strings.CutPrefix(a.User, "agent-chat:"); ok {
			return store.Cause{Actor: store.ActorAgent, Source: "mcp", ActorID: id, Confidence: store.ConfidenceCertain}
		}
		// An external MCP client (syre mcp, the /mcp endpoint) is almost always an agent.
		return store.Cause{Actor: store.ActorAgent, Source: "mcp", ActorID: a.User, Confidence: store.ConfidenceLikely}
	}
	return store.Cause{Actor: store.ActorUnknown, Source: a.Source, ActorID: a.User, Confidence: store.ConfidenceUnknown}
}

// Causes of transitions nobody requested through the API.
var (
	causeScheduler = store.Cause{Actor: store.ActorScheduler, Source: "scheduler", Confidence: store.ConfidenceCertain}
	causeReaper    = store.Cause{Actor: store.ActorSystem, Source: "reaper", Confidence: store.ConfidenceCertain}
	causeWorker    = store.Cause{Actor: store.ActorSystem, Source: "worker", Confidence: store.ConfidenceCertain}
)

// isUserCause reports whether c is a user action.
func isUserCause(c store.Cause) bool { return c.Actor == store.ActorUser }

// isAutomatic reports whether an action with this cause is automatic (not the user's
// own): holds refuse automatic actions on their scope.
func isAutomatic(c store.Cause) bool { return c.Actor != store.ActorUser }

// describeActor renders a cause as a short phrase: "the user (UI)", "the commander".
func describeActor(c store.Cause) string {
	switch c.Actor {
	case store.ActorUser:
		switch c.Source {
		case store.SourceUI:
			return "the user via the Syre UI"
		case store.SourceCLI:
			return "the user via the syre CLI"
		case store.SourceTerminal:
			return "the user via a Syre terminal"
		case store.SourceSSH:
			return "the user over SSH"
		case store.SourceCommander:
			return "the user via Commander"
		}
		return "the user"
	case store.ActorCommander:
		return "the commander"
	case store.ActorAgent:
		if c.ActorID != "" {
			return "agent " + c.ActorID
		}
		return "an agent"
	case store.ActorScheduler:
		return "the scheduler"
	case store.ActorSystem:
		return "Syre"
	}
	return "an unknown actor"
}

// --- recording -------------------------------------------------------------------------

// recordFleetEvent persists e, pushes it to browsers as fleet.update "fleet_event" and
// hands notable events to delivery (commander notifications and agent notes).
func (h *Hub) recordFleetEvent(ctx context.Context, e store.FleetEvent) store.FleetEvent {
	if h == nil || h.store == nil {
		return e
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.Cause.IsZero() {
		e.Cause = causeFrom(ctx)
	}
	id, err := h.store.AppendFleetEvent(context.WithoutCancel(ctx), e)
	if err != nil {
		h.logger.Warn("record fleet event failed", "kind", e.Kind, "err", err)
		return e
	}
	e.ID = id
	h.broadcastFleetUpdate("fleet_event", e)
	h.deliverFleetEvent(e)
	return e
}

// setTaskStatus updates a task's status with its cause, records the transition in the
// fleet timeline when the status changed, broadcasts the task and wakes waiters when it
// became terminal. It returns the updated task.
func (h *Hub) setTaskStatus(ctx context.Context, id, status, summary, gitJSON string, exitCode *int, cause store.Cause) (store.Task, error) {
	ctx = context.WithoutCancel(ctx)
	if cause.IsZero() {
		cause = causeFrom(ctx)
	}
	prev, err := h.store.UpdateTaskStatus(ctx, id, status, summary, gitJSON, exitCode, cause)
	if err != nil {
		return store.Task{}, err
	}
	t, err := h.store.GetTask(ctx, id)
	if err != nil {
		return store.Task{}, err
	}
	if prev != status {
		h.recordTaskTransition(ctx, t, prev, cause, summary)
	}
	h.broadcastFleetUpdate("task", t)
	if protocol.IsTerminalTaskStatus(t.Status) {
		h.notifyTaskDone(t)
	}
	return t, nil
}

// recordTaskTransition records a task status change in the fleet timeline.
func (h *Hub) recordTaskTransition(ctx context.Context, t store.Task, from string, cause store.Cause, detail string) {
	label := t.SID
	if label == "" {
		label = t.ID
	}
	msg := fmt.Sprintf("Task %s (%s) %s → %s by %s", label, t.Agent, statusLabel(from), statusLabel(t.Status), describeActor(cause))
	if d := strings.TrimSpace(detail); d != "" && len(d) < 200 {
		msg += ": " + d
	}
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindStatus, Action: "task", Cause: cause,
		WorkerID: t.WorkerID, TaskID: t.ID, ChatID: t.ParentChatID,
		FromStatus: from, ToStatus: t.Status, Summary: msg,
	})
	h.pushTaskTransition(t, from)
}

// recordChatTransition records a chat status change in the fleet timeline.
func (h *Hub) recordChatTransition(ctx context.Context, cs store.ChatSession, from string, cause store.Cause) {
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindStatus, Action: "chat", Cause: cause,
		WorkerID: cs.WorkerID, ChatID: cs.ID, FromStatus: from, ToStatus: cs.Status,
		Summary: fmt.Sprintf("Chat %s (%s) %s → %s by %s", cs.SID, cs.Title, statusLabel(from), statusLabel(cs.Status), describeActor(cause)),
	})
	h.pushChatTransition(cs, from)
}

func statusLabel(s string) string {
	if s == "" {
		return "new"
	}
	return s
}

// taskCauseForWorkerStatus attributes a status a worker reported for t: the pending cause
// of an intervention in flight (cancel, suspend, resume), else the agent for an agent
// task that ended by itself, the user for a terminal they exited, or the worker.
func (h *Hub) taskCauseForWorkerStatus(t store.Task, status string, exitCode *int) store.Cause {
	if c, ok := h.takePendingCause(t.ID, status); ok {
		return c
	}
	switch {
	case status == protocol.TaskStatusCompleted && t.Agent == "shell":
		return store.Cause{Actor: store.ActorUser, Source: store.SourceTerminal, Confidence: store.ConfidenceLikely}
	case status == protocol.TaskStatusCompleted:
		return store.Cause{Actor: store.ActorAgent, Source: "worker", ActorID: t.ID, Confidence: store.ConfidenceHigh}
	case status == protocol.TaskStatusFailed && exitCode != nil && *exitCode > 0:
		return store.Cause{Actor: store.ActorAgent, Source: "worker", ActorID: t.ID, Confidence: store.ConfidenceLikely}
	case status == protocol.TaskStatusWaitingInput:
		return store.Cause{Actor: store.ActorAgent, Source: "worker", ActorID: t.ID, Confidence: store.ConfidenceHigh}
	}
	return causeWorker
}

// pendingCause remembers the cause of an intervention sent to a worker until the worker
// reports the resulting status.
type pendingCause struct {
	cause  store.Cause
	status string // the status the intervention leads to
	at     time.Time
}

const pendingCauseTTL = 10 * time.Minute

func (h *Hub) setPendingCause(taskID, status string, c store.Cause) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pendingCauses == nil {
		h.pendingCauses = map[string]pendingCause{}
	}
	h.pendingCauses[taskID] = pendingCause{cause: c, status: status, at: time.Now()}
}

func (h *Hub) takePendingCause(taskID, status string) (store.Cause, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pc, ok := h.pendingCauses[taskID]
	if !ok {
		return store.Cause{}, false
	}
	if time.Since(pc.at) > pendingCauseTTL {
		delete(h.pendingCauses, taskID)
		return store.Cause{}, false
	}
	match := pc.status == status ||
		// A cancel ends the task as canceled or failed; a resume returns it to any live status.
		(pc.status == protocol.TaskStatusCanceled && protocol.IsTerminalTaskStatus(status)) ||
		(pc.status == protocol.TaskStatusRunning && !protocol.IsTerminalTaskStatus(status) && status != protocol.TaskStatusSuspended)
	if !match {
		return store.Cause{}, false
	}
	delete(h.pendingCauses, taskID)
	return pc.cause, true
}

// chatEventCause attributes the chat status change an event makes.
func (h *Hub) chatEventCause(ctx context.Context, cs store.ChatSession, ev protocol.AgentChatEvent, o emitOpts) store.Cause {
	if o.cause != nil {
		return *o.cause
	}
	if c, ok := h.takePendingCause("chat:"+cs.ID, cs.Status); ok {
		return c
	}
	if o.fromWorker == nil {
		// Hub-generated events (the user's message, ask_user questions, refusals) carry
		// the caller's cause.
		return causeFrom(ctx)
	}
	agent := store.Cause{Actor: store.ActorAgent, Source: "worker", ActorID: cs.ID, Confidence: store.ConfidenceHigh}
	switch ev.Kind {
	case protocol.EventTurnEnd, protocol.EventElicitation, protocol.EventElicitationOK, protocol.EventSessionReady:
		return agent
	case protocol.EventStatus:
		if ev.Status == protocol.ChatStatusWorking || ev.Status == protocol.ChatStatusNeedsYou {
			return agent
		}
	}
	return causeWorker
}
