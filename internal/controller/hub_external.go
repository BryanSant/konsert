package controller

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

func (h *Hub) handleExternalSession(ctx context.Context, workerID string, p protocol.ExternalSessionPayload) {
	wsID, projID, repoID, wtID := h.matchTaxonomyByPath(ctx, workerID, p.Cwd)
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

	taskID := "ext-" + workerID + "-" + p.ExternalID
	gitJSON := ""
	if len(p.ChangedFiles) > 0 {
		if b, err := json.Marshal(protocol.GitSummary{DirtyFiles: p.ChangedFiles}); err == nil {
			gitJSON = string(b)
		}
	}
	t := store.Task{
		ID:             taskID,
		WorkerID:       workerID,
		WorkspaceID:    wsID,
		ProjectID:      projID,
		RepoID:         repoID,
		WorktreeID:     wtID,
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
	}
	if err := h.store.UpsertExternalTask(ctx, t); err != nil {
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
		h.broadcastFleetUpdate("task", updated)
	}
}

func (h *Hub) matchTaxonomyByPath(ctx context.Context, workerID, cwd string) (wsID, projID, repoID, wtID string) {
	wsID = store.DefaultWorkspaceID
	projID = store.DefaultProjectID
	cleanCwd := strings.TrimRight(strings.TrimSpace(cwd), "/\\")
	if cleanCwd == "" {
		return
	}
	tax, err := h.store.GetTaxonomy(ctx)
	if err != nil {
		return
	}
	projByID := make(map[string]store.Project, len(tax.Projects))
	for _, pr := range tax.Projects {
		projByID[pr.ID] = pr
	}
	repoByID := make(map[string]store.Repo, len(tax.Repos))
	for _, rp := range tax.Repos {
		repoByID[rp.ID] = rp
	}

	bestLen := -1
	// Prefer longest matching Worktree on this worker.
	for _, wt := range tax.Worktrees {
		rp, ok := repoByID[wt.RepoID]
		if !ok || rp.WorkerID != workerID {
			continue
		}
		p := strings.TrimRight(wt.Path, "/\\")
		if p != "" && (cleanCwd == p || strings.HasPrefix(cleanCwd, p+"/")) && len(p) > bestLen {
			bestLen = len(p)
			wtID = wt.ID
			repoID = rp.ID
			projID = rp.ProjectID
			if pr, ok := projByID[rp.ProjectID]; ok {
				wsID = pr.WorkspaceID
			}
		}
	}
	// Fallback to longest matching Repo on this worker.
	for _, rp := range tax.Repos {
		if rp.WorkerID != workerID {
			continue
		}
		p := strings.TrimRight(rp.Path, "/\\")
		if p != "" && (cleanCwd == p || strings.HasPrefix(cleanCwd, p+"/")) && len(p) > bestLen {
			bestLen = len(p)
			wtID = ""
			repoID = rp.ID
			projID = rp.ProjectID
			if pr, ok := projByID[rp.ProjectID]; ok {
				wsID = pr.WorkspaceID
			}
		}
	}
	return
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
