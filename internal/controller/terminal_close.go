package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Closing a terminal (SYR-V8AHB). A terminal the user opened ends when they close it,
// as in every terminal app: syred hangs up its shell and foreground job, then kills
// what is left (TaskCancelPayload.Hangup). That is not an intervention in someone
// else's work, so unlike a cancel it places no hold and tells no agent. Agents'
// sessions are never closed this way: closing their view leaves them running.

// AuditTaskClose records a terminal the user closed.
const AuditTaskClose = "task.close"

// errNotTerminal refuses closing an agent session.
var errNotTerminal = errors.New("only a terminal is closed this way; stop an agent session instead")

func isTerminalTask(t store.Task) bool {
	return t.Agent == "shell" || t.SessionKind == "terminal"
}

// CloseTerminal ends a user's terminal.
func (s *FleetService) CloseTerminal(ctx context.Context, taskID string) (ActionResult, error) {
	h := s.hub
	t, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return ActionResult{}, err
	}
	if !isTerminalTask(t) {
		return ActionResult{}, errNotTerminal
	}
	if protocol.IsTerminalTaskStatus(t.Status) {
		return ActionResult{OK: true, TaskID: t.ID, TaskSID: t.SID, Status: t.Status}, nil
	}
	cause := causeFrom(ctx)
	h.recordAudit(ctx, store.AuditEvent{Action: AuditTaskClose, WorkerID: t.WorkerID, TaskID: t.ID,
		Message: fmt.Sprintf("Closed terminal %s on %s", t.SID, h.workerLabel(ctx, t.WorkerID))})
	if t.Status == protocol.TaskStatusQueued {
		_, _ = h.setTaskStatus(ctx, t.ID, protocol.TaskStatusCanceled, "closed", "", nil, cause)
		return ActionResult{OK: true, TaskID: t.ID, TaskSID: t.SID, Status: protocol.TaskStatusCanceled}, nil
	}
	h.setPendingCause(t.ID, protocol.TaskStatusCanceled, cause)
	h.mu.RLock()
	wc := h.workerForTaskLocked(t.ID)
	h.mu.RUnlock()
	if wc == nil || !h.sendWorkerJSON(wc, protocol.MsgTaskCancel, protocol.TaskCancelPayload{TaskID: t.ID, Hangup: true}) {
		_, _ = h.setTaskStatus(ctx, t.ID, protocol.TaskStatusCanceled, "closed (machine unreachable)", "", nil, cause)
		return ActionResult{OK: true, TaskID: t.ID, TaskSID: t.SID, Status: protocol.TaskStatusCanceled}, nil
	}
	ev := store.TaskEvent{TaskID: t.ID, Kind: "control", Message: "Terminal closed", CreatedAt: time.Now().UTC()}
	if id, err := s.store.AppendEvent(ctx, ev); err == nil {
		ev.ID = id
	}
	h.broadcastFleetUpdate("event", ev)
	return ActionResult{OK: true, TaskID: t.ID, TaskSID: t.SID, Status: protocol.TaskStatusCanceled}, nil
}

// handleCloseTerminal serves POST /api/tasks/{id}/close.
func (s *Server) handleCloseTerminal(w http.ResponseWriter, r *http.Request) {
	res, err := s.service.CloseTerminal(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, res)
	}
}
