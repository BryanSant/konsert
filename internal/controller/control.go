package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

// Interventions (PRD §5.8a): suspend/resume, veto, machine drain / suspend all / stop all
// and fleet-wide Pause everything. Every one is attributed (cause model), recorded in
// the fleet timeline and, when the user did it without an agent asking, places a hold.

// Audit actions for interventions.
const (
	AuditSuspend    = "task.suspend"
	AuditResume     = "task.resume"
	AuditVeto       = "veto"
	AuditDrain      = "worker.drain"
	AuditSuspendAll = "worker.suspend_all"
	AuditResumeAll  = "worker.resume_all"
	AuditStopAll    = "worker.stop_all"
	AuditPause      = "fleet.pause"
	AuditUnpause    = "fleet.resume"
)

// fleetPauseSetting persists the Pause everything state (store.FleetPause JSON).
const fleetPauseSetting = "fleet_pause"

// errFleetPaused is returned to automatic dispatch while everything is paused.
var errFleetPaused = errors.New("Konsert is paused: the user paused everything, so no new work is dispatched. " +
	"Do not retry; ask the user (ask_user) whether to wait or resume")

// Veto reasons.
var vetoReasons = map[string]string{
	"wrong_approach": "wrong approach",
	"wrong_machine":  "wrong machine",
	"not_now":        "not now",
	"dangerous":      "dangerous",
	"other":          "other",
}

func capitalize(s string) string {
	for i, r := range s {
		return string(unicode.ToUpper(r)) + s[i+len(string(r)):]
	}
	return s
}

// recordIntervention records an intervention in the fleet timeline.
func (h *Hub) recordIntervention(ctx context.Context, cause store.Cause, action, workerID, taskID, chatID, summary string) store.FleetEvent {
	return h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindAction, Action: action, Cause: cause,
		WorkerID: workerID, TaskID: taskID, ChatID: chatID, Summary: summary,
	})
}

// --- fleet pause ---------------------------------------------------------------------------

// FleetPauseState is the GET/POST /api/fleet/pause response.
type FleetPauseState struct {
	store.FleetPause
	// Affected counts the sessions the call suspended or resumed.
	Affected int `json:"affected,omitzero"`
}

func (h *Hub) fleetPause(ctx context.Context) store.FleetPause {
	saved, err := h.store.GetAllSettings(ctx)
	if err != nil {
		return store.FleetPause{}
	}
	var p store.FleetPause
	if raw := saved[fleetPauseSetting]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	return p
}

// fleetPaused reports whether Pause everything is on.
func (h *Hub) fleetPaused(ctx context.Context) bool { return h.fleetPause(ctx).Paused }

func (h *Hub) saveFleetPause(ctx context.Context, p store.FleetPause) error {
	b, err := json.Marshal(p, protocol.JSONOptions)
	if err != nil {
		return err
	}
	return h.store.SetSettings(context.WithoutCancel(ctx), map[string]string{fleetPauseSetting: string(b)})
}

// PauseEverything stops all dispatching and suspends every running managed session
// (tasks and agent chats mid-turn). Resume restores only what the pause suspended.
func (h *Hub) PauseEverything(ctx context.Context) (FleetPauseState, error) {
	cause := causeFrom(ctx)
	cur := h.fleetPause(ctx)
	if cur.Paused {
		return FleetPauseState{FleetPause: cur}, nil
	}
	now := time.Now().UTC()
	p := store.FleetPause{Paused: true, PausedAt: &now, PausedBy: cause.ActorID}
	if p.PausedBy == "" {
		p.PausedBy = describeActor(cause)
	}
	if err := h.saveFleetPause(ctx, p); err != nil {
		return FleetPauseState{}, err
	}
	affected := 0
	if tasks, err := h.store.ListTasks(ctx, "", "", 500); err == nil {
		for _, t := range tasks {
			if suspendable(t) {
				if _, err := h.suspendTask(ctx, t, store.SuspendedByPause); err == nil {
					affected++
				}
			}
		}
	}
	if chats, err := h.store.ListChatSessions(ctx, 500); err == nil {
		for _, cs := range chats {
			if cs.Backend == store.ChatBackendAgent && chatBusy(cs.Status) {
				if _, err := h.suspendChat(ctx, cs, store.SuspendedByPause); err == nil {
					affected++
				}
			}
		}
	}
	h.recordIntervention(ctx, cause, "pause", "", "", "",
		fmt.Sprintf("%s paused everything: dispatching stopped and %d running session(s) suspended", capitalize(describeActor(cause)), affected))
	h.recordAudit(ctx, store.AuditEvent{Action: AuditPause, Message: fmt.Sprintf("Paused everything (%d sessions suspended)", affected)})
	h.broadcastFleetUpdate("pause", p)
	return FleetPauseState{FleetPause: p, Affected: affected}, nil
}

// ResumeEverything turns Pause everything off and resumes what it suspended (sessions
// the user suspended individually stay suspended).
func (h *Hub) ResumeEverything(ctx context.Context) (FleetPauseState, error) {
	cause := causeFrom(ctx)
	if err := h.saveFleetPause(ctx, store.FleetPause{}); err != nil {
		return FleetPauseState{}, err
	}
	affected := 0
	if tasks, err := h.store.ListTasks(ctx, protocol.TaskStatusSuspended, "", 500); err == nil {
		for _, t := range tasks {
			if t.SuspendedBy == store.SuspendedByPause {
				if _, err := h.resumeTask(ctx, t); err == nil {
					affected++
				}
			}
		}
	}
	// A task the pause asked to suspend may still be running if the worker never
	// confirmed; clear its marker so it isn't resumed by surprise later.
	if chats, err := h.store.ListChatSessions(ctx, 500); err == nil {
		for _, cs := range chats {
			if cs.SuspendedBy == store.SuspendedByPause {
				if _, err := h.resumeChat(ctx, cs); err == nil {
					affected++
				}
			}
		}
	}
	h.recordIntervention(ctx, cause, "unpause", "", "", "",
		fmt.Sprintf("%s resumed everything: dispatching restarted and %d session(s) resumed", capitalize(describeActor(cause)), affected))
	h.recordAudit(ctx, store.AuditEvent{Action: AuditUnpause, Message: fmt.Sprintf("Resumed everything (%d sessions resumed)", affected)})
	h.broadcastFleetUpdate("pause", store.FleetPause{})
	h.kickScheduler()
	return FleetPauseState{Affected: affected}, nil
}

// --- tasks ---------------------------------------------------------------------------------

// suspendable reports whether a task has a live, managed process tree to freeze.
func suspendable(t store.Task) bool {
	if t.Origin == protocol.TaskOriginExternal || t.WorkerID == "" {
		return false
	}
	switch t.Status {
	case protocol.TaskStatusRunning, protocol.TaskStatusIdle, protocol.TaskStatusWaitingInput, protocol.TaskStatusAssigned:
		return true
	}
	return false
}

// SuspendTask freezes a task's process tree (user action).
func (h *Hub) SuspendTask(ctx context.Context, taskID string) (ActionResult, error) {
	t, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		return ActionResult{}, err
	}
	return h.suspendTask(ctx, t, store.SuspendedByUser)
}

func (h *Hub) suspendTask(ctx context.Context, t store.Task, by string) (ActionResult, error) {
	if t.Status == protocol.TaskStatusSuspended {
		return ActionResult{OK: true, TaskID: t.ID, TaskSID: t.SID, Status: t.Status}, nil
	}
	if !suspendable(t) {
		return ActionResult{}, fmt.Errorf("task %s is %s: only running managed sessions can be suspended", t.SID, t.Status)
	}
	cause := causeFrom(ctx)
	h.mu.RLock()
	wc := h.workerForTaskLocked(t.ID)
	h.mu.RUnlock()
	if wc == nil {
		return ActionResult{}, fmt.Errorf("the worker running task %s is not connected", t.SID)
	}
	if err := h.store.SetTaskSuspendedBy(ctx, t.ID, by); err != nil {
		return ActionResult{}, err
	}
	h.setPendingCause(t.ID, protocol.TaskStatusSuspended, cause)
	if !h.sendWorkerJSON(wc, protocol.MsgTaskSuspend, protocol.TaskSuspendPayload{TaskID: t.ID}) {
		_ = h.store.SetTaskSuspendedBy(ctx, t.ID, "")
		return ActionResult{}, errors.New("worker send queue full")
	}
	if by == store.SuspendedByUser {
		where := h.workerLabel(ctx, t.WorkerID)
		h.recordIntervention(ctx, cause, "suspend", t.WorkerID, t.ID, t.ParentChatID,
			fmt.Sprintf("%s suspended task %s (%s) on %s", capitalize(describeActor(cause)), t.SID, t.Agent, where))
		h.recordAudit(ctx, store.AuditEvent{Action: AuditSuspend, WorkerID: t.WorkerID, TaskID: t.ID, Message: "Suspended the task's process tree"})
		h.holdAfterIntervention(ctx, cause, holdSpec{scope: store.HoldScopeSession, workerID: t.WorkerID, taskID: t.ID,
			summary: fmt.Sprintf("The user suspended task %s on %s", t.SID, where)})
	}
	return ActionResult{OK: true, TaskID: t.ID, TaskSID: t.SID, Status: protocol.TaskStatusSuspended}, nil
}

// ResumeTask thaws a suspended task (user action) and releases its suspend hold.
func (h *Hub) ResumeTask(ctx context.Context, taskID string) (ActionResult, error) {
	t, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		return ActionResult{}, err
	}
	return h.resumeTask(ctx, t)
}

func (h *Hub) resumeTask(ctx context.Context, t store.Task) (ActionResult, error) {
	if t.Status != protocol.TaskStatusSuspended && t.SuspendedBy == "" {
		return ActionResult{OK: true, TaskID: t.ID, TaskSID: t.SID, Status: t.Status}, nil
	}
	cause := causeFrom(ctx)
	h.mu.RLock()
	wc := h.workerForTaskLocked(t.ID)
	h.mu.RUnlock()
	if wc == nil {
		return ActionResult{}, fmt.Errorf("the worker running task %s is not connected", t.SID)
	}
	h.setPendingCause(t.ID, protocol.TaskStatusRunning, cause)
	if !h.sendWorkerJSON(wc, protocol.MsgTaskResume, protocol.TaskSuspendPayload{TaskID: t.ID}) {
		return ActionResult{}, errors.New("worker send queue full")
	}
	if t.Status != protocol.TaskStatusSuspended {
		// The worker never confirmed the suspension: just forget who asked for it.
		_ = h.store.SetTaskSuspendedBy(ctx, t.ID, "")
	}
	if t.SuspendedBy == store.SuspendedByUser {
		where := h.workerLabel(ctx, t.WorkerID)
		h.recordIntervention(ctx, cause, "resume", t.WorkerID, t.ID, t.ParentChatID,
			fmt.Sprintf("%s resumed task %s (%s) on %s", capitalize(describeActor(cause)), t.SID, t.Agent, where))
		h.recordAudit(ctx, store.AuditEvent{Action: AuditResume, WorkerID: t.WorkerID, TaskID: t.ID, Message: "Resumed the task's process tree"})
		h.releaseSessionHolds(ctx, t.ID, "", "resumed by the user")
	}
	return ActionResult{OK: true, TaskID: t.ID, TaskSID: t.SID, Status: protocol.TaskStatusRunning}, nil
}

// VetoTask cancels a task with a reason the orchestrator must remember.
func (h *Hub) VetoTask(ctx context.Context, taskID, reason, note string) (ActionResult, error) {
	t, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		return ActionResult{}, err
	}
	label, err := vetoLabel(reason)
	if err != nil {
		return ActionResult{}, err
	}
	res, err := h.cancelTask(ctx, t.ID, "veto: "+label)
	if err != nil {
		return res, err
	}
	what := fmt.Sprintf("task %s (%s on %s: %q)", t.SID, t.Agent, h.workerLabel(ctx, t.WorkerID), excerpt(t.Prompt, 160))
	h.recordVeto(ctx, reason, label, note, what, t.WorkerID, t.ID, t.ParentChatID, "project:"+t.ProjectID)
	return res, nil
}

func vetoLabel(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "other"
	}
	label, ok := vetoReasons[reason]
	if !ok {
		return "", fmt.Errorf("unknown veto reason %q (want wrong_approach, wrong_machine, not_now, dangerous or other)", reason)
	}
	return label, nil
}

// recordVeto stores a veto as orchestrator memory at the work's scope (project for now)
// and records it in the fleet timeline, which delivers it to the orchestrator.
func (h *Hub) recordVeto(ctx context.Context, reason, label, note, what, workerID, taskID, chatID, scope string) {
	cause := causeFrom(ctx)
	text := fmt.Sprintf("The user vetoed %s — reason: %s", what, label)
	if n := strings.TrimSpace(note); n != "" {
		text += ` ("` + n + `")`
	}
	text += ". Don't retry the same approach without asking the user."
	key := "veto:" + time.Now().UTC().Format("20060102T150405.000")
	if err := h.store.UpsertMemory(context.WithoutCancel(ctx), store.MemoryEntry{
		Scope: scope, Key: key, Summary: text,
		MetaJSON: auditMeta(map[string]any{"reason": reason, "note": note, "task_id": taskID, "chat_id": chatID, "worker_id": workerID}),
	}); err != nil {
		h.logger.Warn("store veto memory failed", "err", err)
	}
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindVeto, Action: reason, Cause: cause,
		WorkerID: workerID, TaskID: taskID, ChatID: chatID, Summary: text,
		MetaJSON: auditMeta(map[string]any{"reason": reason, "note": note, "memory_scope": scope, "memory_key": key}),
	})
	h.recordAudit(ctx, store.AuditEvent{Action: AuditVeto, WorkerID: workerID, TaskID: taskID, Message: text,
		MetaJSON: auditMeta(map[string]any{"reason": reason, "chat_id": chatID})})
}

func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// --- chats ---------------------------------------------------------------------------------

func chatBusy(status string) bool {
	switch status {
	case protocol.ChatStatusWorking, protocol.ChatStatusNeedsYou, protocol.ChatStatusStarting:
		return true
	}
	return false
}

// SuspendChat freezes an agent chat's engine process tree (user action).
func (h *Hub) SuspendChat(ctx context.Context, chatID string) (store.ChatSession, error) {
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil {
		return cs, err
	}
	return h.suspendChat(ctx, cs, store.SuspendedByUser)
}

func (h *Hub) suspendChat(ctx context.Context, cs store.ChatSession, by string) (store.ChatSession, error) {
	if cs.Backend != store.ChatBackendAgent {
		return cs, errors.New("only agent chats can be suspended")
	}
	if cs.Status == protocol.ChatStatusSuspended {
		return cs, nil
	}
	cause := causeFrom(ctx)
	if wc := h.workerConnFor(cs.WorkerID); wc != nil {
		h.sendWorkerJSON(wc, protocol.MsgAgentChatSuspend, protocol.AgentChatSuspend{ChatID: cs.ID})
	}
	prev := cs.Status
	updated, _ := h.emitChatEvent(ctx, cs.ID, protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusSuspended,
		Text: "Suspended by " + describeActor(cause)}, emitOpts{
		cause: &cause,
		update: func(s *store.ChatSession) {
			s.SuspendedBy, s.SuspendedPrev = by, prev
		},
	})
	_ = updated
	if by == store.SuspendedByUser {
		h.recordIntervention(ctx, cause, "suspend", cs.WorkerID, "", cs.ID,
			fmt.Sprintf("%s suspended chat %s (%s) on %s", capitalize(describeActor(cause)), cs.SID, cs.Title, h.workerLabel(ctx, cs.WorkerID)))
		h.recordAudit(ctx, store.AuditEvent{Action: AuditSuspend, WorkerID: cs.WorkerID, Message: "Suspended chat " + cs.SID,
			MetaJSON: auditMeta(map[string]any{"chat_id": cs.ID})})
		h.holdAfterIntervention(ctx, cause, holdSpec{scope: store.HoldScopeSession, workerID: cs.WorkerID, chatID: cs.ID,
			summary: fmt.Sprintf("The user suspended chat %s (%s)", cs.SID, cs.Title)})
	}
	return h.store.GetChatSession(ctx, cs.ID)
}

// ResumeChat thaws a suspended chat (user action) and releases its suspend hold.
func (h *Hub) ResumeChat(ctx context.Context, chatID string) (store.ChatSession, error) {
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil {
		return cs, err
	}
	return h.resumeChat(ctx, cs)
}

func (h *Hub) resumeChat(ctx context.Context, cs store.ChatSession) (store.ChatSession, error) {
	if cs.Status != protocol.ChatStatusSuspended && cs.SuspendedBy == "" {
		return cs, nil
	}
	cause := causeFrom(ctx)
	by := cs.SuspendedBy
	prev := cs.SuspendedPrev
	if prev == "" || prev == protocol.ChatStatusSuspended {
		prev = protocol.ChatStatusIdle
	}
	if wc := h.workerConnFor(cs.WorkerID); wc != nil {
		h.sendWorkerJSON(wc, protocol.MsgAgentChatResume, protocol.AgentChatSuspend{ChatID: cs.ID})
	}
	h.emitChatEvent(ctx, cs.ID, protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: prev, Text: "Resumed by " + describeActor(cause)}, emitOpts{
		cause: &cause,
		update: func(s *store.ChatSession) {
			s.SuspendedBy, s.SuspendedPrev = "", ""
		},
	})
	if by == store.SuspendedByUser {
		h.recordIntervention(ctx, cause, "resume", cs.WorkerID, "", cs.ID,
			fmt.Sprintf("%s resumed chat %s (%s)", capitalize(describeActor(cause)), cs.SID, cs.Title))
		h.recordAudit(ctx, store.AuditEvent{Action: AuditResume, WorkerID: cs.WorkerID, Message: "Resumed chat " + cs.SID,
			MetaJSON: auditMeta(map[string]any{"chat_id": cs.ID})})
		h.releaseSessionHolds(ctx, "", cs.ID, "resumed by the user")
	}
	return h.store.GetChatSession(ctx, cs.ID)
}

// StopChatTurn interrupts a chat's running turn (user action).
func (h *Hub) StopChatTurn(ctx context.Context, chatID string) (store.ChatSession, error) {
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil {
		return cs, err
	}
	if err := h.controlChat(ctx, chatControlRequest{SessionID: cs.ID, Cancel: true}); err != nil {
		return cs, err
	}
	return h.store.GetChatSession(ctx, cs.ID)
}

// VetoChat stops a chat's turn with a reason: remembered for the orchestrator, told to
// the chat itself on its next turn, and the chat is held.
func (h *Hub) VetoChat(ctx context.Context, chatID, reason, note string) (store.ChatSession, error) {
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil {
		return cs, err
	}
	label, err := vetoLabel(reason)
	if err != nil {
		return cs, err
	}
	if cs.Status == protocol.ChatStatusSuspended {
		if cs, err = h.resumeChat(ctx, cs); err != nil {
			return cs, err
		}
	}
	if chatBusy(cs.Status) {
		if err := h.controlChat(ctx, chatControlRequest{SessionID: cs.ID, Cancel: true}); err != nil {
			return cs, err
		}
	}
	cause := causeFrom(ctx)
	_, projID, _, _ := h.matchTaxonomyByPath(ctx, cs.WorkerID, cs.Cwd)
	what := fmt.Sprintf("chat %s (%s, %s on %s)", cs.SID, cs.Title, cs.Agent, h.workerLabel(ctx, cs.WorkerID))
	h.recordVeto(ctx, reason, label, note, what, cs.WorkerID, "", cs.ID, "project:"+projID)
	h.holdAfterIntervention(ctx, cause, holdSpec{scope: store.HoldScopeSession, workerID: cs.WorkerID, chatID: cs.ID,
		summary: fmt.Sprintf("The user vetoed chat %s: %s", cs.SID, label)})
	return h.store.GetChatSession(ctx, cs.ID)
}

// --- machines ------------------------------------------------------------------------------

// WorkerControlResult is the response of machine-scope interventions.
type WorkerControlResult struct {
	OK       bool   `json:"ok"`
	WorkerID string `json:"worker_id"`
	Drained  bool   `json:"drained"`
	Affected int    `json:"affected"`
}

// DrainWorker sets or clears a machine's drain flag: drained machines finish their
// current work and receive nothing new.
func (h *Hub) DrainWorker(ctx context.Context, workerID string, drained bool) (WorkerControlResult, error) {
	w, err := h.store.GetWorker(ctx, workerID)
	if err != nil {
		return WorkerControlResult{}, err
	}
	cause := causeFrom(ctx)
	if err := h.store.SetWorkerDrained(ctx, w.ID, drained, time.Now().UTC()); err != nil {
		return WorkerControlResult{}, err
	}
	w, _ = h.store.GetWorker(ctx, w.ID)
	action, verb := "drain", "drained"
	if !drained {
		action, verb = "undrain", "undrained"
	}
	msg := fmt.Sprintf("%s %s %s", capitalize(describeActor(cause)), verb, w.Name)
	if drained {
		msg += ": it finishes its current work and accepts nothing new"
	}
	h.recordIntervention(ctx, cause, action, w.ID, "", "", msg)
	h.recordAudit(ctx, store.AuditEvent{Action: AuditDrain, WorkerID: w.ID, Message: msg, MetaJSON: auditMeta(map[string]any{"drained": drained})})
	h.broadcastWorker(ctx, w.ID)
	if !drained {
		h.kickScheduler()
	}
	return WorkerControlResult{OK: true, WorkerID: w.ID, Drained: drained}, nil
}

// broadcastWorker pushes a worker's current view to browsers.
func (h *Hub) broadcastWorker(ctx context.Context, workerID string) {
	w, err := h.store.GetWorker(ctx, workerID)
	if err != nil {
		return
	}
	active := 0
	if wc := h.workerConnFor(workerID); wc != nil {
		active = wc.activeCount()
	}
	h.broadcastFleetUpdate("worker", toWorkerView(w, active, h.WorkerMetrics(workerID)))
}

// machineSessions lists a machine's live managed tasks and agent chats.
func (h *Hub) machineSessions(ctx context.Context, workerID string) ([]store.Task, []store.ChatSession) {
	var tasks []store.Task
	if ts, err := h.store.ListTasks(ctx, "", workerID, 500); err == nil {
		for _, t := range ts {
			if t.Origin != protocol.TaskOriginExternal && !protocol.IsTerminalTaskStatus(t.Status) && t.Status != protocol.TaskStatusQueued {
				tasks = append(tasks, t)
			}
		}
	}
	chats, _ := h.store.ListChatSessionsByWorker(ctx, workerID)
	return tasks, chats
}

// SuspendAll suspends every running managed session on a machine and holds the machine.
func (h *Hub) SuspendAll(ctx context.Context, workerID string) (WorkerControlResult, error) {
	w, err := h.store.GetWorker(ctx, workerID)
	if err != nil {
		return WorkerControlResult{}, err
	}
	cause := causeFrom(ctx)
	tasks, chats := h.machineSessions(ctx, w.ID)
	n := 0
	for _, t := range tasks {
		if suspendable(t) && t.Status != protocol.TaskStatusSuspended {
			if _, err := h.suspendTask(ctx, t, store.SuspendedByUser); err == nil {
				n++
			}
		}
	}
	for _, cs := range chats {
		if chatBusy(cs.Status) {
			if _, err := h.suspendChat(ctx, cs, store.SuspendedByUser); err == nil {
				n++
			}
		}
	}
	msg := fmt.Sprintf("%s suspended all %d running session(s) on %s", capitalize(describeActor(cause)), n, w.Name)
	h.recordIntervention(ctx, cause, "suspend_all", w.ID, "", "", msg)
	h.recordAudit(ctx, store.AuditEvent{Action: AuditSuspendAll, WorkerID: w.ID, Message: msg})
	h.holdAfterIntervention(ctx, cause, holdSpec{scope: store.HoldScopeMachine, workerID: w.ID,
		summary: fmt.Sprintf("The user suspended everything on %s", w.Name)})
	return WorkerControlResult{OK: true, WorkerID: w.ID, Drained: w.Drained, Affected: n}, nil
}

// ResumeAll resumes the sessions the user suspended on a machine and releases the
// machine's hold.
func (h *Hub) ResumeAll(ctx context.Context, workerID string) (WorkerControlResult, error) {
	w, err := h.store.GetWorker(ctx, workerID)
	if err != nil {
		return WorkerControlResult{}, err
	}
	cause := causeFrom(ctx)
	tasks, chats := h.machineSessions(ctx, w.ID)
	n := 0
	for _, t := range tasks {
		if t.Status == protocol.TaskStatusSuspended && t.SuspendedBy == store.SuspendedByUser {
			if _, err := h.resumeTask(ctx, t); err == nil {
				n++
			}
		}
	}
	for _, cs := range chats {
		if cs.Status == protocol.ChatStatusSuspended && cs.SuspendedBy == store.SuspendedByUser {
			if _, err := h.resumeChat(ctx, cs); err == nil {
				n++
			}
		}
	}
	if holds, err := h.store.ListHolds(ctx, store.HoldFilter{ActiveOnly: true, WorkerID: w.ID}); err == nil {
		for _, hd := range holds {
			if hd.ScopeKind == store.HoldScopeMachine && hd.Resource == nil {
				_, _ = h.releaseHold(ctx, hd.ID, "resumed everything on the machine")
			}
		}
	}
	msg := fmt.Sprintf("%s resumed %d session(s) on %s", capitalize(describeActor(cause)), n, w.Name)
	h.recordIntervention(ctx, cause, "resume_all", w.ID, "", "", msg)
	h.recordAudit(ctx, store.AuditEvent{Action: AuditResumeAll, WorkerID: w.ID, Message: msg})
	return WorkerControlResult{OK: true, WorkerID: w.ID, Drained: w.Drained, Affected: n}, nil
}

// StopAll cancels every managed task and stops every running chat turn on a machine,
// and holds the machine so nothing restarts there automatically.
func (h *Hub) StopAll(ctx context.Context, workerID string) (WorkerControlResult, error) {
	w, err := h.store.GetWorker(ctx, workerID)
	if err != nil {
		return WorkerControlResult{}, err
	}
	cause := causeFrom(ctx)
	tasks, chats := h.machineSessions(ctx, w.ID)
	n := 0
	for _, t := range tasks {
		if _, err := h.cancelTask(ctx, t.ID, "stop all"); err == nil {
			n++
		}
	}
	for _, cs := range chats {
		if cs.Status == protocol.ChatStatusSuspended {
			cs, _ = h.resumeChat(ctx, cs)
		}
		if chatBusy(cs.Status) {
			if err := h.controlChat(ctx, chatControlRequest{SessionID: cs.ID, Cancel: true}); err == nil {
				n++
			}
		}
	}
	msg := fmt.Sprintf("%s stopped all %d session(s) on %s", capitalize(describeActor(cause)), n, w.Name)
	h.recordIntervention(ctx, cause, "stop_all", w.ID, "", "", msg)
	h.recordAudit(ctx, store.AuditEvent{Action: AuditStopAll, WorkerID: w.ID, Message: msg})
	h.holdAfterIntervention(ctx, cause, holdSpec{scope: store.HoldScopeMachine, workerID: w.ID,
		summary: fmt.Sprintf("The user stopped everything on %s", w.Name)})
	return WorkerControlResult{OK: true, WorkerID: w.ID, Drained: w.Drained, Affected: n}, nil
}

// --- REST ------------------------------------------------------------------------------------

func writeControlError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
}

type vetoRequest struct {
	Reason string `json:"reason"`
	Note   string `json:"note"`
}

func (s *Server) handleSuspendTask(w http.ResponseWriter, r *http.Request) {
	res, err := s.hub.SuspendTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleResumeTask(w http.ResponseWriter, r *http.Request) {
	res, err := s.hub.ResumeTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleVetoTask(w http.ResponseWriter, r *http.Request) {
	var body vetoRequest
	if requestHasBody(r) && !decodeJSON(w, r, &body) {
		return
	}
	res, err := s.hub.VetoTask(r.Context(), r.PathValue("id"), body.Reason, body.Note)
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func chatActionResult(cs store.ChatSession) map[string]any {
	return map[string]any{"ok": true, "chat_id": cs.ID, "status": cs.Status}
}

func (s *Server) handleSuspendChat(w http.ResponseWriter, r *http.Request) {
	cs, err := s.hub.SuspendChat(r.Context(), r.PathValue("id"))
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chatActionResult(cs))
}

func (s *Server) handleResumeChat(w http.ResponseWriter, r *http.Request) {
	cs, err := s.hub.ResumeChat(r.Context(), r.PathValue("id"))
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chatActionResult(cs))
}

func (s *Server) handleStopChat(w http.ResponseWriter, r *http.Request) {
	cs, err := s.hub.StopChatTurn(r.Context(), r.PathValue("id"))
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chatActionResult(cs))
}

func (s *Server) handleVetoChat(w http.ResponseWriter, r *http.Request) {
	var body vetoRequest
	if requestHasBody(r) && !decodeJSON(w, r, &body) {
		return
	}
	cs, err := s.hub.VetoChat(r.Context(), r.PathValue("id"), body.Reason, body.Note)
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chatActionResult(cs))
}

func (s *Server) handleDrainWorker(w http.ResponseWriter, r *http.Request) {
	body := struct {
		Drained *bool `json:"drained"`
	}{}
	if requestHasBody(r) && !decodeJSON(w, r, &body) {
		return
	}
	drained := true
	if body.Drained != nil {
		drained = *body.Drained
	}
	res, err := s.hub.DrainWorker(r.Context(), r.PathValue("id"), drained)
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) workerControl(fn func(*Hub, context.Context, string) (WorkerControlResult, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := fn(s.hub, r.Context(), r.PathValue("id"))
		if err != nil {
			writeControlError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

func (s *Server) handleGetFleetPause(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, FleetPauseState{FleetPause: s.hub.fleetPause(r.Context())})
}

func (s *Server) handlePauseFleet(w http.ResponseWriter, r *http.Request) {
	res, err := s.hub.PauseEverything(r.Context())
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleResumeFleet(w http.ResponseWriter, r *http.Request) {
	res, err := s.hub.ResumeEverything(r.Context())
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
