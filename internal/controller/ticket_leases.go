package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

const (
	ticketLeaseTTLSetting        = "ticket_lease_ttl_seconds"
	defaultTicketLeaseTTLSeconds = 600 // 10 minutes
	minTicketLeaseTTLSeconds     = 10
	maxTicketLeaseTTLSeconds     = 86400
)

// ticketLeaseTTL returns the configured ticket lease TTL (Hub override -> controller
// setting -> 10 minutes default).
func (h *Hub) ticketLeaseTTL(ctx context.Context) time.Duration {
	if h.TicketLeaseTTL > 0 {
		return h.TicketLeaseTTL
	}
	if saved, err := h.store.GetAllSettings(ctx); err == nil {
		if raw := strings.TrimSpace(saved[ticketLeaseTTLSetting]); raw != "" {
			if sec, err := strconv.Atoi(raw); err == nil && sec >= minTicketLeaseTTLSeconds && sec <= maxTicketLeaseTTLSeconds {
				return time.Duration(sec) * time.Second
			}
		}
	}
	return store.DefaultTicketLeaseTTL
}

// buildTicketPlaceholderPrompt builds a placeholder task prompt from a ticket's title,
// body, and acceptance criteria. Replaced by the compiled dispatch packet in A3a.
func buildTicketPlaceholderPrompt(tk store.Ticket) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(tk.Title))
	if body := strings.TrimSpace(tk.Body); body != "" {
		b.WriteString("\n\n")
		b.WriteString(body)
	}
	if len(tk.Acceptance) > 0 {
		b.WriteString("\n\nAcceptance criteria:\n")
		for _, item := range tk.Acceptance {
			mark := "[ ]"
			if item.Done {
				mark = "[x]"
			}
			fmt.Fprintf(&b, "- %s %s\n", mark, strings.TrimSpace(item.Text))
		}
	}
	return strings.TrimSpace(b.String())
}

// ticketWorkerSelector maps a ticket's placement hints onto a WorkerSelector.
func (h *Hub) ticketWorkerSelector(ctx context.Context, tk store.Ticket) WorkerSelector {
	sel := WorkerSelector{
		OS:   strings.TrimSpace(tk.Hints.OS),
		Arch: strings.TrimSpace(tk.Hints.Arch),
	}
	if m := strings.TrimSpace(tk.Hints.Machine); m != "" {
		if id, err := h.store.ResolveID(ctx, ids.Machine, m); err == nil {
			sel.WorkerID = id
		} else {
			sel.Name = m
		}
	}
	if tk.Hints.MinRAMGB > 0 {
		sel.MinRAMMB = uint64(tk.Hints.MinRAMGB) * 1024
	}
	if len(tk.Hints.Tools) > 0 {
		sel.RequiredTools = make(map[string]string, len(tk.Hints.Tools))
		for _, tool := range tk.Hints.Tools {
			if t := strings.TrimSpace(tool); t != "" {
				sel.RequiredTools[t] = ""
			}
		}
	}
	return sel
}

func pickWorkerAgent(w store.Worker) string {
	for _, pref := range preferredAgentOrder {
		if slices.Contains(w.Agents, pref) {
			return pref
		}
	}
	if len(w.Agents) > 0 {
		return w.Agents[0]
	}
	return ""
}

// scheduleDispatchableTickets runs at the end of each scheduler pass, after queued tasks.
// Only tickets belonging to projects with autopilot=true are dispatched.
func (h *Hub) scheduleDispatchableTickets(ctx context.Context, workers []store.Worker, counts map[string]int, slots slotPlan) {
	projects, err := h.store.ListProjects(ctx)
	if err != nil {
		return
	}
	autopilotProjects := make(map[string]store.Project, len(projects))
	for _, p := range projects {
		if p.Autopilot {
			autopilotProjects[p.ID] = p
		}
	}
	if len(autopilotProjects) == 0 {
		return
	}

	tickets, err := h.store.ListDispatchableTickets(ctx, "", 200)
	if err != nil || len(tickets) == 0 {
		return
	}

	saved, _ := h.store.GetAllSettings(ctx)
	defaultAgent := strings.TrimSpace(saved["default_agent"])
	defaultModel := strings.TrimSpace(saved["default_model"])
	permMode := store.PermissionMode(strings.TrimSpace(saved["default_permission_mode"]))
	if permMode == "" {
		permMode = store.PermissionFullAccess
	}
	leaseTTL := h.ticketLeaseTTL(ctx)

	for _, tk := range tickets {
		if ctx.Err() != nil {
			return
		}
		if _, ok := autopilotProjects[tk.ProjectID]; !ok {
			continue
		}
		bindings, err := h.store.ListTicketRepoBindings(ctx, tk.ID)
		if err != nil || len(bindings) == 0 {
			continue
		}
		firstBinding := bindings[0]
		prompt := buildTicketPlaceholderPrompt(tk)
		sel := h.ticketWorkerSelector(ctx, tk)

		agent := strings.TrimSpace(tk.Hints.Agent)
		if agent == "" {
			agent = defaultAgent
		}
		model := strings.TrimSpace(tk.Hints.Model)
		if model == "" {
			model = defaultModel
		}

		// Filter workers through holds (machine holds and resource holds matching prompt).
		eligible := make([]store.Worker, 0, len(workers))
		lifted := map[string]*store.Override{}
		for _, w := range workers {
			if _, held, ov := h.holdVerdict(ctx, holdCheck{workerID: w.ID, text: prompt}); !held {
				eligible = append(eligible, w)
				lifted[w.ID] = ov
			}
		}
		w, ok := SelectWorker(eligible, counts, sel, agent, permMode)
		if !ok {
			continue
		}
		if agent == "" {
			agent = pickWorkerAgent(w)
			if agent == "" {
				continue
			}
		}
		if h.checkAccountLoggedIn(ctx, w.ID, agent, "") != nil {
			continue
		}

		taskID := ids.UUID()
		taskSID, err := h.store.NewSID(ctx, ids.Task)
		if err != nil {
			continue
		}

		now := time.Now().UTC()
		claimedTk, claimed, err := h.store.ClaimTicket(ctx, tk.ID, w.ID, taskID, now, leaseTTL)
		if err != nil || !claimed {
			continue
		}

		// The daemon's default workdir until A1c provisions per-ticket worktrees from
		// reported checkouts (SYR-YDTCV); a repo name is not a path.
		workdir := ""
		branch := firstBinding.TargetBranch
		if branch == "" {
			branch = firstBinding.BaseBranch
		}
		orphanPolicy := protocol.OrphanPolicyRequeue
		if tk.OrphanPolicy == store.TicketOrphanPolicyFail {
			orphanPolicy = protocol.OrphanPolicyFail
		}

		payload := protocol.TaskStartPayload{
			TaskID:         taskID,
			SID:            taskSID,
			Agent:          agent,
			Model:          model,
			SessionKind:    "chat",
			Workdir:        workdir,
			Branch:         branch,
			UseWorktree:    branch != "",
			Prompt:         prompt,
			Mode:           protocol.TaskModeOneshot,
			PermissionMode: string(permMode),
			TimeoutSeconds: 3600,
			UseGitBash:     w.AgentsUseGitBash,
		}
		task := store.Task{
			ID:             taskID,
			SID:            taskSID,
			WorkerID:       w.ID,
			ProjectID:      tk.ProjectID,
			RepoID:         firstBinding.RepoID,
			Agent:          agent,
			Model:          model,
			SessionKind:    "chat",
			Workdir:        workdir,
			Branch:         branch,
			Mode:           protocol.TaskModeOneshot,
			Origin:         protocol.TaskOriginCommander,
			PermissionMode: permMode,
			OrphanPolicy:   orphanPolicy,
			Prompt:         prompt,
			Status:         protocol.TaskStatusAssigned,
			Title:          tk.Title,
			TicketID:       tk.ID,
			TicketTitle:    tk.Title,
			Cause:          &causeScheduler,
			CreatedAt:      now,
		}
		if err := h.store.CreateTask(ctx, task); err != nil {
			h.logger.Warn("create task for claimed ticket failed; releasing lease", "ticket_id", tk.ID, "err", err)
			_, _, _ = h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusReady)
			continue
		}
		selJSON, _ := json.Marshal(sel)
		payJSON, _ := json.Marshal(payload)
		_ = h.store.SetTaskStartSpec(ctx, taskID, store.TaskStartSpec{
			SelectorJSON: string(selJSON),
			PayloadJSON:  string(payJSON),
			WorkerPinned: sel.WorkerID != "",
		})

		if err := h.DispatchToWorker(w.ID, payload); err != nil {
			h.logger.Warn("dispatch of ticket task failed; releasing ticket back to ready",
				"ticket_id", tk.ID, "task_id", taskID, "worker_id", w.ID, "err", err)
			_, _, _ = h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusReady)
			_, _ = h.store.UpdateTaskStatus(ctx, taskID, protocol.TaskStatusCanceled, "dispatch failed", "", nil, causeScheduler)
			continue
		}

		if ov := lifted[w.ID]; ov != nil {
			h.useOverride(ctx, *ov)
		}
		h.placed(ctx, slots, w.ID, counts[w.ID])
		counts[w.ID]++
		if updatedTask, err := h.store.GetTask(ctx, taskID); err == nil {
			h.recordTaskTransition(ctx, updatedTask, "", causeScheduler, "placed ticket "+claimedTk.SID+" on "+w.Name)
			h.broadcastFleetUpdate("task", updatedTask)
		}
		h.broadcastFleetUpdate("ticket", claimedTk)
	}
}

// renewWorkerTicketLeases extends leases for every ticket leased to workerID whose
// leased task is currently active in the worker's heartbeat.
func (h *Hub) renewWorkerTicketLeases(ctx context.Context, workerID string, activeTaskIDs []string, now time.Time) {
	if workerID == "" || len(activeTaskIDs) == 0 {
		return
	}
	leased, err := h.store.ListLeasedTicketsByWorker(ctx, workerID)
	if err != nil || len(leased) == 0 {
		return
	}
	active := make(map[string]bool, len(activeTaskIDs))
	for _, id := range activeTaskIDs {
		active[id] = true
	}
	var ticketIDs []string
	for _, tk := range leased {
		if tk.Lease != nil && (tk.Lease.TaskID == "" || active[tk.Lease.TaskID]) {
			ticketIDs = append(ticketIDs, tk.ID)
		}
	}
	if len(ticketIDs) == 0 {
		return
	}
	if _, err := h.store.RenewLeases(ctx, workerID, ticketIDs, now, h.ticketLeaseTTL(ctx)); err != nil {
		h.logger.Warn("renew ticket leases failed", "worker_id", workerID, "err", err)
	}
}

// syncTicketFromTaskStatus maps task status transitions onto the ticket leased to that task:
//   - completed     -> ticket review (lease released; kicks scheduler for optimistic chaining)
//   - failed        -> ticket failed (lease released)
//   - canceled      -> ticket canceled (lease released)
//   - waiting_input -> ticket needs_you (lease kept)
//   - running       -> ticket running (when coming back from needs_you; lease kept)
func (h *Hub) syncTicketFromTaskStatus(ctx context.Context, t store.Task, prevStatus string) {
	if t.TicketID == "" || prevStatus == t.Status {
		return
	}
	tk, err := h.store.GetTicket(ctx, t.TicketID)
	if err != nil || tk.Lease == nil || tk.Lease.TaskID != t.ID {
		return
	}

	switch t.Status {
	case protocol.TaskStatusCompleted:
		if updated, changed, err := h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusReview); err == nil && changed {
			h.broadcastFleetUpdate("ticket", updated)
			h.kickScheduler()
		}
	case protocol.TaskStatusFailed:
		if updated, changed, err := h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusFailed); err == nil && changed {
			h.broadcastFleetUpdate("ticket", updated)
		}
	case protocol.TaskStatusCanceled:
		if updated, changed, err := h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusCanceled); err == nil && changed {
			h.broadcastFleetUpdate("ticket", updated)
		}
	case protocol.TaskStatusWaitingInput:
		if tk.Status == store.TicketStatusRunning {
			tk.Status = store.TicketStatusNeedsYou
			if updated, err := h.store.UpdateTicket(ctx, tk); err == nil {
				h.broadcastFleetUpdate("ticket", updated)
			}
		}
	case protocol.TaskStatusRunning:
		if tk.Status == store.TicketStatusNeedsYou {
			tk.Status = store.TicketStatusRunning
			if updated, err := h.store.UpdateTicket(ctx, tk); err == nil {
				h.broadcastFleetUpdate("ticket", updated)
			}
		}
	}
}

// isTaskLeasedTicket reports whether t is currently the active leased task of a Syre ticket.
func (h *Hub) isTaskLeasedTicket(ctx context.Context, t store.Task) bool {
	if t.TicketID == "" {
		return false
	}
	tk, err := h.store.GetTicket(ctx, t.TicketID)
	return err == nil && tk.Lease != nil && tk.Lease.TaskID == t.ID
}

// expireTicketLeases expires leases past their deadline and applies each ticket's orphan_policy.
func (h *Hub) expireTicketLeases(ctx context.Context, now time.Time) {
	expired, err := h.store.ExpireLeases(ctx, now)
	if err != nil || len(expired) == 0 {
		return
	}
	requeued := false
	for _, ex := range expired {
		h.logger.Warn("ticket lease expired",
			"ticket_id", ex.Ticket.ID,
			"ticket_sid", ex.Ticket.SID,
			"worker_id", ex.WorkerID,
			"task_id", ex.TaskID,
			"orphan_policy", ex.Ticket.OrphanPolicy,
			"next_status", ex.Ticket.Status,
		)
		if ex.TaskID != "" {
			h.forgetTask(ex.TaskID)
			code := -1
			_, _ = h.setTaskStatus(ctx, ex.TaskID, protocol.TaskStatusFailed, "ticket lease expired", "", &code, causeReaper)
		}
		h.broadcastFleetUpdate("ticket", ex.Ticket)
		if ex.Ticket.Status == store.TicketStatusReady {
			requeued = true
		}
	}
	if requeued {
		h.kickScheduler()
	}
}

// RequeueTicket breaks any active lease on ticketID, moves it back to ready, cancels its
// running task (if any), and records an attributed audit + fleet timeline entry.
func (h *Hub) RequeueTicket(ctx context.Context, ticketID, note string) (store.Ticket, error) {
	return h.breakTicketLease(ctx, ticketID, store.TicketStatusReady, AuditTicketRequeue, "requeue", note)
}

// CancelTicket breaks any active lease on ticketID, moves it to canceled, cancels its
// running task (if any), and records an attributed audit + fleet timeline entry.
func (h *Hub) CancelTicket(ctx context.Context, ticketID, note string) (store.Ticket, error) {
	return h.breakTicketLease(ctx, ticketID, store.TicketStatusCanceled, AuditTicketCancel, "cancel", note)
}

func (h *Hub) breakTicketLease(ctx context.Context, ticketID, nextStatus, auditAction, verb, note string) (store.Ticket, error) {
	tk, err := h.store.GetTicket(ctx, ticketID)
	if err != nil {
		return store.Ticket{}, err
	}
	cause := causeFrom(ctx)
	priorStatus := tk.Status
	var priorWorkerID, priorTaskID string
	if tk.Lease != nil {
		priorWorkerID = tk.Lease.WorkerID
		priorTaskID = tk.Lease.TaskID
	}

	updated, _, err := h.store.ReleaseTicket(ctx, tk.ID, nextStatus)
	if err != nil {
		return store.Ticket{}, err
	}

	// Cancel the previously leased task without placing a session hold (otherwise
	// RequeueTicket would immediately block the ticket from re-dispatching).
	if priorTaskID != "" {
		if t, err := h.store.GetTask(ctx, priorTaskID); err == nil && !protocol.IsTerminalTaskStatus(t.Status) {
			h.setPendingCause(priorTaskID, protocol.TaskStatusCanceled, cause)
			if !h.SendCancel(priorTaskID) {
				_, _ = h.setTaskStatus(ctx, priorTaskID, protocol.TaskStatusCanceled, "ticket "+verb+"d", "", nil, cause)
			}
		}
	}

	msg := fmt.Sprintf("%s %sd ticket %s (%s)", capitalize(describeActor(cause)), verb, tk.SID, tk.Title)
	if n := strings.TrimSpace(note); n != "" {
		msg += ": " + n
	}
	h.recordAudit(ctx, store.AuditEvent{
		Action:   auditAction,
		WorkerID: priorWorkerID,
		TaskID:   priorTaskID,
		Message:  msg,
		MetaJSON: auditMeta(map[string]any{
			"ticket_id":    tk.ID,
			"ticket_sid":   tk.SID,
			"prior_status": priorStatus,
			"next_status":  updated.Status,
			"note":         strings.TrimSpace(note),
		}),
	})
	h.recordIntervention(ctx, cause, auditAction, priorWorkerID, priorTaskID, "", msg)
	h.broadcastFleetUpdate("ticket", updated)
	if nextStatus == store.TicketStatusReady {
		h.kickScheduler()
	}
	return updated, nil
}
