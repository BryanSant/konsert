package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

// Event delivery (PRD §5.8a): user actions, holds, requests and vetoes reach
//   - the orchestrator chat as fleet events: when its work is affected and it is idle, a
//     turn starts with a concise system note;
//   - every affected agent chat as a note prepended before its next prompt;
//   - any agent on demand through the recent_events MCP tool.
// Notes are cursor based (chat_sessions.notes_cursor = the last fleet event delivered),
// so nothing is lost across hub restarts and nothing is told twice.

// deliveryDebounce batches events that arrive together (a cancel, its status change and
// the hold it placed) into one orchestrator turn.
var deliveryDebounce = 1500 * time.Millisecond

// maxNoteLines bounds a note.
const maxNoteLines = 12

// eventNotePrefix marks Konsert's notes in prompts.
const eventNotePrefix = "[Konsert]"

type deliveryState struct {
	mu      sync.Mutex
	pending bool
}

// deliverFleetEvent schedules delivery of a newly recorded event. It never blocks: the
// orchestrator check runs on a background goroutine after a short debounce.
func (h *Hub) deliverFleetEvent(e store.FleetEvent) {
	if !eventRelevant(e) {
		return
	}
	h.scheduleDelivery()
}

func (h *Hub) scheduleDelivery() {
	h.delivery.mu.Lock()
	if h.delivery.pending {
		h.delivery.mu.Unlock()
		return
	}
	h.delivery.pending = true
	h.delivery.mu.Unlock()
	started := h.goBackground(func(ctx context.Context) {
		t := time.NewTimer(deliveryDebounce)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.delivery.mu.Lock()
		h.delivery.pending = false
		h.delivery.mu.Unlock()
		h.notifyOrchestrators(ctx)
	})
	if !started {
		h.delivery.mu.Lock()
		h.delivery.pending = false
		h.delivery.mu.Unlock()
	}
}

// eventRelevant reports whether an event is worth telling agents about: a user
// intervention, a hold, a request or a veto. Routine status transitions and commands
// that change nothing are not.
func eventRelevant(e store.FleetEvent) bool {
	switch e.Kind {
	case store.EventKindHold, store.EventKindRequest, store.EventKindVeto:
		return true
	case EventKindOverride:
		// The orchestrator hears the user's decision on its override request and when an
		// approved one expires.
		return e.Action == store.OverrideApproved || e.Action == store.OverrideDenied || e.Action == store.OverrideExpired
	case store.EventKindAction:
		if e.Action == "command" {
			return strings.Contains(e.MetaJSON, `"intervention":true`)
		}
		return isUserCause(e.Cause)
	}
	return false
}

// chatAffected reports whether event e concerns chat cs: for the orchestrator (fleet
// tools) every relevant event; for other agent chats, relevant events on their machine
// or about the chat itself. Events the chat caused itself are never echoed back.
func chatAffected(cs store.ChatSession, e store.FleetEvent) bool {
	if !eventRelevant(e) || e.Cause.ActorID == cs.ID {
		return false
	}
	if e.Kind == EventKindOverride {
		// Overrides are the orchestrator's business only.
		return cs.FleetTools
	}
	if cs.FleetTools {
		// The requesting chat's own request stays with it; the orchestrator hears about
		// everyone else's.
		return true
	}
	if e.ChatID == cs.ID {
		return true
	}
	return e.WorkerID != "" && e.WorkerID == cs.WorkerID
}

// orchestratorWork is what an orchestrator chat is responsible for: the tasks it
// dispatched that are still live, and the machines they run on.
type orchestratorWork struct {
	tasks    map[string]bool
	machines map[string]bool
}

func (h *Hub) orchestratorWorkOf(ctx context.Context, chatID string) orchestratorWork {
	w := orchestratorWork{tasks: map[string]bool{}, machines: map[string]bool{}}
	tasks, err := h.store.ListTasks(ctx, "", "", 500)
	if err != nil {
		return w
	}
	for _, t := range tasks {
		if t.ParentChatID != chatID {
			continue
		}
		w.tasks[t.ID] = true
		if !protocol.IsTerminalTaskStatus(t.Status) && t.WorkerID != "" {
			w.machines[t.WorkerID] = true
		}
	}
	return w
}

// affectsWork reports whether an event touches an orchestrator's work (which starts a
// turn rather than waiting for its next prompt).
func (w orchestratorWork) affects(chatID string, e store.FleetEvent) bool {
	if e.Kind == EventKindOverride {
		// A decision on (or the expiry of) its own override request.
		return e.ChatID == chatID
	}
	if e.ChatID == chatID && e.TaskID == "" {
		// The user acted on the orchestrator chat itself (stopped, suspended, vetoed it):
		// starting a turn would defy them. It hears about it before its next prompt.
		return false
	}
	switch {
	case e.ChatID == chatID, e.TaskID != "" && w.tasks[e.TaskID], e.WorkerID != "" && w.machines[e.WorkerID]:
		return true
	case e.Kind == store.EventKindRequest && e.Action == "created":
		return true
	case e.Kind == store.EventKindVeto:
		return true
	case e.Kind == store.EventKindAction && (e.Action == "pause" || e.Action == "unpause"):
		return true
	}
	return false
}

// orchestratorChats lists the agent chats with fleet tools.
func (h *Hub) orchestratorChats(ctx context.Context) []store.ChatSession {
	chats, err := h.store.ListChatSessions(ctx, 500)
	if err != nil {
		return nil
	}
	var out []store.ChatSession
	for _, cs := range chats {
		if cs.Backend == store.ChatBackendAgent && cs.FleetTools {
			out = append(out, cs)
		}
	}
	return out
}

// chatIdle reports whether a chat can take a new turn now.
func chatIdle(status string) bool {
	switch status {
	case "", protocol.ChatStatusIdle, protocol.ChatStatusDone, protocol.ChatStatusFailed, protocol.ChatStatusCancelled:
		return true
	}
	return false
}

// notifyOrchestrators starts a turn in every idle orchestrator chat whose work is
// affected by events it hasn't been told about yet.
func (h *Hub) notifyOrchestrators(ctx context.Context) {
	for _, cs := range h.orchestratorChats(ctx) {
		if !chatIdle(cs.Status) || cs.WorkerID == "" || h.workerConnFor(cs.WorkerID) == nil {
			continue
		}
		events := h.undeliveredEvents(ctx, cs)
		if len(events) == 0 {
			continue
		}
		work := h.orchestratorWorkOf(ctx, cs.ID)
		affected := false
		for _, e := range events {
			if work.affects(cs.ID, e) {
				affected = true
				break
			}
		}
		if !affected {
			continue // told before its next prompt instead
		}
		dctx := withCause(ctx, store.Cause{Actor: store.ActorSystem, Source: "delivery", Confidence: store.ConfidenceCertain})
		turnID := ids.UUID()
		note := h.takeNote(dctx, cs.ID, turnID)
		if note == "" {
			continue
		}
		note += "\nThe user is in charge: don't undo or work around their actions. If this affects work you started, tell the user what is paused or blocked and ask (ask_user) whether to restart, wait or change plans."
		if err := h.sendAgentChat(dctx, cs, chatSendRequest{SessionID: cs.ID, Content: note, TurnID: turnID}); err != nil {
			h.logger.Warn("orchestrator notification failed", "chat_id", cs.ID, "err", err)
			continue
		}
		h.recordFleetEvent(dctx, store.FleetEvent{
			Kind: store.EventKindNotice, Action: "orchestrator_turn", ChatID: cs.ID, WorkerID: cs.WorkerID,
			Summary: fmt.Sprintf("Konsert told the orchestrator (chat %s) about %d event(s) and started a turn", cs.SID, len(events)),
		})
	}
}

// undeliveredEvents returns the relevant events chat cs has not been told about, oldest
// first (at most 50).
func (h *Hub) undeliveredEvents(ctx context.Context, cs store.ChatSession) []store.FleetEvent {
	f := store.FleetEventFilter{AfterID: cs.NotesCursor, Notable: true, Limit: 200}
	if cs.NotesCursor == 0 {
		f.Since = cs.CreatedAt
	}
	evs, err := h.store.ListFleetEvents(ctx, f)
	if err != nil {
		return nil
	}
	var out []store.FleetEvent
	for i := len(evs) - 1; i >= 0; i-- {
		if chatAffected(cs, evs[i]) {
			out = append(out, evs[i])
		}
	}
	if len(out) > 50 {
		out = out[len(out)-50:]
	}
	return out
}

// takeNote builds the note for chat chatID's turn turnID from the events it hasn't been
// told about and advances its cursor (so each event is told once). Vetoes in the note are
// marked delivered in that turn (its normal end acknowledges them). "" when there is
// nothing to tell.
func (h *Hub) takeNote(ctx context.Context, chatID, turnID string) string {
	ctx = context.WithoutCancel(ctx)
	rt := h.chatRT(chatID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil {
		return ""
	}
	events := h.undeliveredEvents(ctx, cs)
	maxID, _ := h.store.MaxFleetEventID(ctx)
	if maxID > cs.NotesCursor {
		cs.NotesCursor = maxID
		if err := h.store.UpdateChatSession(ctx, cs); err != nil {
			h.logger.Warn("advance notes cursor failed", "chat_id", chatID, "err", err)
		}
	}
	if len(events) == 0 {
		return ""
	}
	h.markVetoesDelivered(ctx, events[max(0, len(events)-maxNoteLines):], chatID, turnID)
	return h.formatNote(ctx, events)
}

// formatNote renders events as a concise note for an agent.
func (h *Hub) formatNote(ctx context.Context, events []store.FleetEvent) string {
	var b strings.Builder
	b.WriteString(eventNotePrefix + " Since your last turn:\n")
	start := 0
	if len(events) > maxNoteLines {
		start = len(events) - maxNoteLines
		fmt.Fprintf(&b, "- (%d earlier events omitted; call recent_events for all)\n", start)
	}
	for _, e := range events[start:] {
		fmt.Fprintf(&b, "- %s %s\n", e.CreatedAt.Local().Format("15:04"), h.eventLine(ctx, e))
	}
	b.WriteString("These were deliberate actions by the user (or Konsert on their behalf), not failures: don't investigate them; if something you depend on is affected, stop and ask the user before acting.")
	return b.String()
}

// eventLine renders one event for agents, with attribution and correlation.
func (h *Hub) eventLine(_ context.Context, e store.FleetEvent) string {
	line := e.Summary
	var tags []string
	if e.Cause.Confidence != "" && e.Cause.Confidence != store.ConfidenceCertain {
		tags = append(tags, "confidence "+e.Cause.Confidence)
	}
	if line == "" {
		line = e.Kind + " " + e.Action
	}
	if len(tags) > 0 {
		line += " (" + strings.Join(tags, ", ") + ")"
	}
	return line
}

// noteForPrompt returns the note to prepend to a chat's turn turnID (and advances its
// cursor), or "" when nothing happened.
func (h *Hub) noteForPrompt(ctx context.Context, chatID, turnID string) string {
	return h.takeNote(ctx, chatID, turnID)
}

// initNotesCursor starts a new chat's note cursor at the newest event: a new chat is
// not told about the past (recent_events has it).
func (h *Hub) initNotesCursor(ctx context.Context, cs *store.ChatSession) {
	if id, err := h.store.MaxFleetEventID(ctx); err == nil {
		cs.NotesCursor = id
	}
}

// --- recent_events ----------------------------------------------------------------------

// RecentEventsInput is the recent_events MCP tool input.
type RecentEventsInput struct {
	Machine      string `json:"machine,omitempty" jsonschema:"Only events on this machine (id, sid m-… or name)"`
	TaskID       string `json:"task_id,omitempty" jsonschema:"Only events about this task (id or sid t-…)"`
	SinceMinutes int    `json:"since_minutes,omitzero" jsonschema:"How far back to look (default 120)"`
	Limit        int    `json:"limit,omitzero" jsonschema:"Maximum events (default 25, max 200)"`
	All          bool   `json:"all,omitzero" jsonschema:"Include routine status changes (default: only user actions, holds, requests, vetoes and failures)"`
}

// RecentEventsResult is the recent_events MCP tool output.
type RecentEventsResult struct {
	Events []RecentEvent `json:"events"`
	// ActiveHolds lists holds in effect on the selected scope.
	ActiveHolds []string `json:"active_holds,omitempty"`
	// OpenRequests lists requests still waiting for the user.
	OpenRequests []string `json:"open_requests,omitempty"`
	Paused       bool     `json:"paused,omitzero"`
}

// RecentEvent is one event as agents see it.
type RecentEvent struct {
	ID         int64  `json:"id"`
	At         string `json:"at"`
	Kind       string `json:"kind"`
	Action     string `json:"action,omitempty"`
	Actor      string `json:"actor"`
	Source     string `json:"source,omitempty"`
	Confidence string `json:"confidence"`
	Machine    string `json:"machine,omitempty"`
	Task       string `json:"task,omitempty"`
	Chat       string `json:"chat,omitempty"`
	Summary    string `json:"summary"`
	RequestID  string `json:"request_id,omitempty"`
	CommandID  string `json:"command_id,omitempty"`
	HoldID     string `json:"hold_id,omitempty"`
}

// RecentEvents answers recent_events. forChat, when set, restricts the default view to
// events that concern that chat (its machine, itself, or everything for the orchestrator).
func (h *Hub) RecentEvents(ctx context.Context, in RecentEventsInput, forChat *store.ChatSession) (RecentEventsResult, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 25
	}
	limit = min(limit, 200)
	since := in.SinceMinutes
	if since <= 0 {
		since = 120
	}
	f := store.FleetEventFilter{
		Notable: !in.All,
		Since:   time.Now().UTC().Add(-time.Duration(since) * time.Minute),
		Limit:   500,
	}
	if m := strings.TrimSpace(in.Machine); m != "" {
		f.WorkerID = h.resolveMachine(ctx, m)
	}
	if t := strings.TrimSpace(in.TaskID); t != "" {
		f.TaskID = resolveRef(ctx, h.store, ids.Task, t)
	}
	evs, err := h.store.ListFleetEvents(ctx, f)
	if err != nil {
		return RecentEventsResult{}, err
	}
	names := map[string]string{}
	if ws, err := h.store.ListWorkers(ctx); err == nil {
		for _, w := range ws {
			names[w.ID] = w.Name
		}
	}
	out := RecentEventsResult{Events: []RecentEvent{}, Paused: h.fleetPaused(ctx)}
	for _, e := range evs {
		if forChat != nil && f.WorkerID == "" && f.TaskID == "" && !in.All {
			if !forChat.FleetTools && e.ChatID != forChat.ID && e.WorkerID != forChat.WorkerID {
				continue
			}
		}
		out.Events = append(out.Events, RecentEvent{
			ID: e.ID, At: e.CreatedAt.Local().Format(time.RFC3339), Kind: e.Kind, Action: e.Action,
			Actor: e.Cause.Actor, Source: e.Cause.Source, Confidence: e.Cause.Confidence,
			Machine: names[e.WorkerID], Task: e.TaskID, Chat: e.ChatID, Summary: e.Summary,
			RequestID: e.Cause.RequestID, CommandID: e.Cause.CommandID, HoldID: e.Cause.HoldID,
		})
		if len(out.Events) >= limit {
			break
		}
	}
	scopeWorker := f.WorkerID
	if scopeWorker == "" && forChat != nil && !forChat.FleetTools {
		scopeWorker = forChat.WorkerID
	}
	if holds, err := h.store.ListHolds(ctx, store.HoldFilter{ActiveOnly: true, WorkerID: scopeWorker}); err == nil {
		for _, hd := range holds {
			out.ActiveHolds = append(out.ActiveHolds, fmt.Sprintf("%s on %s since %s: %s", hd.SID, h.holdScopeLabel(ctx, hd), hd.CreatedAt.Local().Format("15:04"), hd.Summary))
		}
	}
	if reqs, err := h.store.ListUserRequests(ctx, store.UserRequestFilter{Status: store.RequestOpen, WorkerID: scopeWorker}); err == nil {
		for _, r := range reqs {
			out.OpenRequests = append(out.OpenRequests, fmt.Sprintf("%s on %s: %s", r.SID, names[r.WorkerID], r.Description))
		}
	}
	return out, nil
}

// resolveMachine resolves a machine reference: id, sid or name.
func (h *Hub) resolveMachine(ctx context.Context, ref string) string {
	if id := resolveRef(ctx, h.store, ids.Machine, ref); id != ref {
		return id
	}
	if ws, err := h.store.ListWorkers(ctx); err == nil {
		for _, w := range ws {
			if w.ID == ref || strings.EqualFold(w.Name, ref) || strings.EqualFold(w.HostGroup, ref) {
				return w.ID
			}
		}
	}
	return ref
}
