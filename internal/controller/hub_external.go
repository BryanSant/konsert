package controller

import (
	"context"
	"encoding/json/v2"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func (h *Hub) handleExternalSession(ctx context.Context, workerID string, p protocol.ExternalSessionPayload) {
	status := protocol.TaskStatusIdle
	switch strings.ToLower(strings.TrimSpace(p.Status)) {
	case "working", "running":
		status = protocol.TaskStatusRunning
	case "idle":
		status = protocol.TaskStatusIdle
	case "needs_you", "waiting_input":
		status = protocol.TaskStatusWaitingInput
	case "done", "completed":
		status = protocol.TaskStatusCompleted
	case "fail", "failed":
		status = protocol.TaskStatusFailed
	}

	gitJSON := ""
	if len(p.ChangedFiles) > 0 {
		if b, err := json.Marshal(protocol.GitSummary{DirtyFiles: p.ChangedFiles}, protocol.JSONOptions); err == nil {
			gitJSON = string(b)
		}
	}
	projectID := store.DefaultProjectID
	repoID := ""
	if repo, ok := h.repoForPath(ctx, workerID, p.Cwd); ok {
		projectID = repo.ProjectID
		repoID = repo.ID
	}
	t := store.Task{
		ExternalID:     p.ExternalID,
		WorkerID:       workerID,
		ProjectID:      projectID,
		RepoID:         repoID,
		Agent:          p.Agent,
		Model:          p.Model,
		SessionKind:    "chat",
		Workdir:        p.Cwd,
		EffectiveDir:   p.Cwd,
		Branch:         p.Branch,
		Mode:           protocol.TaskModeInteractive,
		Origin:         protocol.TaskOriginExternal,
		PermissionMode: store.PermissionFullAccess,
		Status:         status,
		Summary:        p.Summary,
		GitSummaryJSON: gitJSON,
		// Observed sessions change state on their own: the agent (or its user) drives them.
		Cause: &store.Cause{Actor: store.ActorAgent, Source: "observed:" + p.Source, Confidence: store.ConfidenceLikely},
	}
	taskID, prev, err := h.store.UpsertExternalTask(ctx, t)
	if err != nil {
		h.logger.Warn("record external session", "worker_id", workerID, "external_id", p.ExternalID, "err", err)
		return
	}
	if len(p.ChangedFiles) > 0 {
		ev := store.TaskEvent{
			TaskID:    taskID,
			Kind:      "file_edit",
			Message:   "Modified " + strings.Join(p.ChangedFiles, ", "),
			CreatedAt: time.Now().UTC(),
		}
		if id, err := h.store.AppendEvent(ctx, ev); err == nil {
			ev.ID = id
		}
		h.broadcastFleetUpdate("event", ev)
	}
	if updated, err := h.store.GetTask(ctx, taskID); err == nil {
		if prev != updated.Status {
			h.recordTaskTransition(ctx, updated, prev, *t.Cause, "")
		}
		h.broadcastFleetUpdate("task", updated)
	}
	if a, ok := externalActivity(taskID, workerID, p); ok {
		h.setActivity(a)
	}
}

func metaInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}
