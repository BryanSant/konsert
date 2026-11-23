package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/orchestrator"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/google/uuid"
)

// FleetService is the shared application service used by REST API, MCP Server,
// and the Built-in Orchestrator Chat.
type FleetService struct {
	store store.Store
	hub   *Hub
}

func NewFleetService(st store.Store, hub *Hub) *FleetService {
	return &FleetService{store: st, hub: hub}
}

type WorkerStatusView struct {
	ID             string                  `json:"id"`
	Name           string                  `json:"name"`
	HostGroup      string                  `json:"host_group"`
	OS             string                  `json:"os"`
	Arch           string                  `json:"arch"`
	Isolation      string                  `json:"isolation"`
	PermissionMode string                  `json:"permission_mode"`
	Agents         []string                `json:"agents"`
	Tags           map[string]string       `json:"tags"`
	MaxConcurrent  int                     `json:"max_concurrent"`
	Status         string                  `json:"status"`
	ActiveTasks    int                     `json:"active_tasks"`
	LastSeenAt     time.Time               `json:"last_seen_at"`
	Metrics        *protocol.WorkerMetrics `json:"metrics,omitempty"`
}

type FleetStatusResult struct {
	Workers          []WorkerStatusView `json:"workers"`
	ActiveTasksCount int                `json:"active_tasks_count"`
}

func (s *FleetService) FleetStatus(ctx context.Context) (FleetStatusResult, error) {
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		return FleetStatusResult{}, err
	}
	counts := s.hub.ActiveTaskCounts()
	views := make([]WorkerStatusView, 0, len(workers))
	totalActive := 0
	for _, w := range workers {
		ac := counts[w.ID]
		totalActive += ac
		st := w.Status
		if st == "online" && !s.hub.IsWorkerConnected(w.ID) {
			st = "offline"
		}
		views = append(views, WorkerStatusView{
			ID:             w.ID,
			Name:           w.Name,
			HostGroup:      w.HostGroup,
			OS:             w.OS,
			Arch:           w.Arch,
			Isolation:      w.Isolation,
			PermissionMode: string(w.PermissionMode),
			Agents:         w.Agents,
			Tags:           w.Tags,
			MaxConcurrent:  w.MaxConcurrent,
			Status:         st,
			ActiveTasks:    ac,
			LastSeenAt:     w.LastSeenAt,
			Metrics:        s.hub.WorkerMetrics(w.ID),
		})
	}
	return FleetStatusResult{
		Workers:          views,
		ActiveTasksCount: totalActive,
	}, nil
}

type SessionDefaultsResult struct {
	MachineID       string   `json:"machine_id"`
	MachineSource   string   `json:"machine_source"` // "explicit" | "localhost" | "last_used" | "guess"
	Agent           string   `json:"agent"`
	AgentSource     string   `json:"agent_source"` // "explicit" | "default" | "last_used" | "guess"
	Model           string   `json:"model"`
	ModelSource     string   `json:"model_source"` // "explicit" | "default" | "last_used" | "guess"
	AvailableAgents []string `json:"available_agents"`
}

var preferredAgentOrder = []string{"agy", "claude", "codex", "grok", "mock"}

func guessModelForAgent(agent, fallbackModel string) string {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "claude":
		return "claude-sonnet-4-5"
	case "agy":
		return "gemini-2.5-pro"
	case "codex":
		return "o4-mini"
	case "grok":
		return "grok-code-fast-1"
	case "mock":
		return "mock-default"
	default:
		if fallbackModel != "" {
			return fallbackModel
		}
		return "gpt-4o"
	}
}

func isLocalhostWorker(w store.Worker, localHostname string) bool {
	name := strings.ToLower(strings.TrimSpace(w.Name))
	hg := strings.ToLower(strings.TrimSpace(w.HostGroup))
	if name == "localhost" || name == "127.0.0.1" || hg == "localhost" {
		return true
	}
	if w.Tags["local"] == "true" || w.Tags["host"] == "localhost" {
		return true
	}
	if localHostname != "" {
		lh := strings.ToLower(strings.TrimSpace(localHostname))
		shortLH := strings.Split(lh, ".")[0]
		if name == lh || name == shortLH || hg == lh || hg == shortLH {
			return true
		}
	}
	return false
}

func workerHasAgent(w store.Worker, agent string) bool {
	for _, a := range w.Agents {
		if strings.EqualFold(a, agent) {
			return true
		}
	}
	return false
}

// ResolveSessionDefaults resolves Machine (default localhost), Agent (default -> last used -> guess),
// and Model (default -> last used -> guess) for a new Chat session.
func (s *FleetService) ResolveSessionDefaults(ctx context.Context, reqMachine, reqAgent, reqModel string) (SessionDefaultsResult, error) {
	saved, _ := s.store.GetAllSettings(ctx)
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		return SessionDefaultsResult{}, err
	}
	online := make([]store.Worker, 0, len(workers))
	for _, w := range workers {
		if w.Status == "online" && s.hub.IsWorkerConnected(w.ID) {
			online = append(online, w)
		}
	}
	pool := online
	if len(pool) == 0 {
		pool = workers
	}

	localHost, _ := os.Hostname()
	res := SessionDefaultsResult{AvailableAgents: []string{}}

	// 1. Machine cascade: explicit -> localhost -> last_used -> first available
	var chosenWorker *store.Worker
	if m := strings.TrimSpace(reqMachine); m != "" {
		res.MachineID = m
		res.MachineSource = "explicit"
		for i := range pool {
			if pool[i].ID == m || strings.EqualFold(pool[i].Name, m) {
				chosenWorker = &pool[i]
				res.MachineID = pool[i].ID
				break
			}
		}
	}
	if chosenWorker == nil && res.MachineID == "" {
		for i := range pool {
			if isLocalhostWorker(pool[i], localHost) {
				chosenWorker = &pool[i]
				res.MachineID = pool[i].ID
				res.MachineSource = "localhost"
				break
			}
		}
	}
	if chosenWorker == nil && res.MachineID == "" {
		if last := strings.TrimSpace(saved["last_used_machine"]); last != "" {
			for i := range pool {
				if pool[i].ID == last {
					chosenWorker = &pool[i]
					res.MachineID = pool[i].ID
					res.MachineSource = "last_used"
					break
				}
			}
		}
	}
	if chosenWorker == nil && res.MachineID == "" && len(pool) > 0 {
		chosenWorker = &pool[0]
		res.MachineID = pool[0].ID
		res.MachineSource = "guess"
	}

	// Collect non-shell coding agents available on the resolved machine.
	if chosenWorker != nil {
		for _, a := range chosenWorker.Agents {
			if a != "shell" {
				res.AvailableAgents = append(res.AvailableAgents, a)
			}
		}
	}

	// 2. Agent cascade: explicit -> configured default -> last used -> smart first-run guess
	if a := strings.TrimSpace(reqAgent); a != "" {
		res.Agent = a
		res.AgentSource = "explicit"
	} else if defA := strings.TrimSpace(saved["default_agent"]); defA != "" && (chosenWorker == nil || workerHasAgent(*chosenWorker, defA)) {
		res.Agent = defA
		res.AgentSource = "default"
	} else if lastA := strings.TrimSpace(saved["last_used_agent"]); lastA != "" && lastA != "shell" && (chosenWorker == nil || workerHasAgent(*chosenWorker, lastA)) {
		res.Agent = lastA
		res.AgentSource = "last_used"
	} else {
		for _, pref := range preferredAgentOrder {
			if chosenWorker != nil && workerHasAgent(*chosenWorker, pref) {
				res.Agent = pref
				res.AgentSource = "guess"
				break
			}
		}
		if res.Agent == "" && len(res.AvailableAgents) > 0 {
			res.Agent = res.AvailableAgents[0]
			res.AgentSource = "guess"
		}
		if res.Agent == "" {
			res.Agent = "claude"
			res.AgentSource = "guess"
		}
	}

	// 3. Model cascade: explicit -> configured default -> last used -> smart guess per agent
	if mod := strings.TrimSpace(reqModel); mod != "" {
		res.Model = mod
		res.ModelSource = "explicit"
	} else if defM := strings.TrimSpace(saved["default_model"]); defM != "" {
		res.Model = defM
		res.ModelSource = "default"
	} else if lastM := strings.TrimSpace(saved["last_used_model"]); lastM != "" {
		res.Model = lastM
		res.ModelSource = "last_used"
	} else {
		res.Model = guessModelForAgent(res.Agent, strings.TrimSpace(saved["llm_model"]))
		res.ModelSource = "guess"
	}

	return res, nil
}

type DispatchRequest struct {
	WorkerSelector WorkerSelector `json:"worker_selector,omitempty"`
	SpaceID        string         `json:"space_id,omitempty"`
	WorkspaceID    string         `json:"workspace_id,omitempty"`
	ProjectID      string         `json:"project_id,omitempty"`
	RepoID         string         `json:"repo_id,omitempty"`
	WorktreeID     string         `json:"worktree_id,omitempty"`
	SessionKind    string         `json:"session_kind,omitempty"` // "chat" | "terminal"
	Agent          string         `json:"agent"`
	Model          string         `json:"model,omitempty"`
	RepoPath       string         `json:"repo_path"`
	Branch         string         `json:"branch,omitempty"`
	UseWorktree    *bool          `json:"use_worktree,omitempty"`
	Prompt         string         `json:"prompt"`
	Mode           string         `json:"mode,omitempty"`
	Origin         string         `json:"origin,omitempty"` // "orchestrator" | "direct" | "scratch"
	PermissionMode string         `json:"permission_mode,omitempty"`
	OrphanPolicy   string         `json:"orphan_policy,omitempty"`
	TimeoutSeconds *int           `json:"timeout_seconds,omitempty"`
}

type DispatchResult struct {
	TaskID           string `json:"task_id"`
	WorkerID         string `json:"worker_id,omitempty"`
	SpaceID          string `json:"space_id,omitempty"`
	WorkspaceID      string `json:"workspace_id,omitempty"`
	ProjectID        string `json:"project_id,omitempty"`
	RepoID           string `json:"repo_id,omitempty"`
	WorktreeID       string `json:"worktree_id,omitempty"`
	SessionKind      string `json:"session_kind,omitempty"`
	Agent            string `json:"agent,omitempty"`
	Model            string `json:"model,omitempty"`
	Status           string `json:"status"`
	WorkdirEffective string `json:"workdir_effective"`
}

func (s *FleetService) DispatchTask(ctx context.Context, req DispatchRequest) (DispatchResult, error) {
	// Resolve taxonomy bindings from Worktree -> Repo -> Project -> Workspace (falling back to Default).
	if wtID := strings.TrimSpace(req.WorktreeID); wtID != "" {
		wt, err := s.store.GetWorktree(ctx, wtID)
		if err != nil {
			return DispatchResult{}, fmt.Errorf("worktree %s not found: %w", wtID, err)
		}
		if req.RepoID == "" {
			req.RepoID = wt.RepoID
		}
		if strings.TrimSpace(req.RepoPath) == "" {
			req.RepoPath = wt.Path
		}
		if strings.TrimSpace(req.Branch) == "" && wt.Branch != "" {
			req.Branch = wt.Branch
		}
	}
	if repoID := strings.TrimSpace(req.RepoID); repoID != "" {
		repo, err := s.store.GetRepo(ctx, repoID)
		if err != nil {
			return DispatchResult{}, fmt.Errorf("repo %s not found: %w", repoID, err)
		}
		if req.ProjectID == "" {
			req.ProjectID = repo.ProjectID
		}
		if req.WorkerSelector.WorkerID == "" && repo.WorkerID != "" {
			req.WorkerSelector.WorkerID = repo.WorkerID
		}
		if strings.TrimSpace(req.RepoPath) == "" {
			req.RepoPath = repo.Path
		}
	}
	if projID := strings.TrimSpace(req.ProjectID); projID != "" {
		proj, err := s.store.GetProject(ctx, projID)
		if err != nil {
			return DispatchResult{}, fmt.Errorf("project %s not found: %w", projID, err)
		}
		if req.WorkspaceID == "" {
			req.WorkspaceID = proj.WorkspaceID
		}
	}
	if strings.TrimSpace(req.WorkspaceID) == "" {
		req.WorkspaceID = store.DefaultWorkspaceID
	}
	if strings.TrimSpace(req.ProjectID) == "" {
		req.ProjectID = store.DefaultProjectID
	}

	// Resolve legacy per-machine Space bindings when space_id is supplied.
	if strings.TrimSpace(req.SpaceID) != "" {
		sp, err := s.store.GetSpace(ctx, req.SpaceID)
		if err != nil {
			return DispatchResult{}, fmt.Errorf("space %s not found: %w", req.SpaceID, err)
		}
		if req.WorkerSelector.WorkerID == "" {
			req.WorkerSelector.WorkerID = sp.WorkerID
		}
		if strings.TrimSpace(req.RepoPath) == "" {
			req.RepoPath = sp.Path
		}
		if strings.TrimSpace(req.Branch) == "" && sp.Branch != "" {
			req.Branch = sp.Branch
		}
		if req.Origin == "" {
			req.Origin = protocol.TaskOriginDirect
		}
	}

	// Normalize unified session kind ("chat" | "terminal") and apply Machine/Agent/Model cascade when requested.
	sessionKind := strings.ToLower(strings.TrimSpace(req.SessionKind))
	if sessionKind == "" {
		if req.Agent == "shell" {
			sessionKind = "terminal"
		} else {
			sessionKind = "chat"
		}
	}
	if sessionKind == "terminal" && strings.TrimSpace(req.Agent) == "" {
		req.Agent = "shell"
	}
	if sessionKind == "chat" && (strings.TrimSpace(req.Agent) == "" || (req.Origin == protocol.TaskOriginDirect && strings.TrimSpace(req.Model) == "")) {
		resolved, rErr := s.ResolveSessionDefaults(ctx, req.WorkerSelector.WorkerID, req.Agent, req.Model)
		if rErr == nil {
			if req.WorkerSelector.WorkerID == "" && req.Origin == protocol.TaskOriginDirect && resolved.MachineID != "" {
				req.WorkerSelector.WorkerID = resolved.MachineID
			}
			if strings.TrimSpace(req.Agent) == "" {
				req.Agent = resolved.Agent
			}
			if strings.TrimSpace(req.Model) == "" && req.Origin == protocol.TaskOriginDirect {
				req.Model = resolved.Model
			}
		}
	}
	if strings.TrimSpace(req.Agent) == "" {
		return DispatchResult{}, fmt.Errorf("agent is required")
	}

	origin := req.Origin
	if origin == "" {
		origin = protocol.TaskOriginOrchestrator
	}
	mode := req.Mode
	if mode == "" {
		if req.Agent == "shell" || sessionKind == "terminal" || origin == protocol.TaskOriginDirect || origin == protocol.TaskOriginScratch {
			mode = protocol.TaskModeInteractive
		} else {
			mode = protocol.TaskModeOneshot
		}
	}
	if strings.TrimSpace(req.RepoPath) == "" {
		if origin == protocol.TaskOriginDirect || origin == protocol.TaskOriginScratch || req.Agent == "shell" || sessionKind == "terminal" {
			req.RepoPath = "~"
		} else {
			return DispatchResult{}, fmt.Errorf("repo_path is required")
		}
	}
	if strings.TrimSpace(req.Prompt) == "" && req.Agent != "shell" && mode != protocol.TaskModeInteractive {
		return DispatchResult{}, fmt.Errorf("prompt is required")
	}

	permMode := store.PermissionMode(req.PermissionMode)
	if permMode == "" {
		permMode = store.PermissionFullAccess
	}
	orphanPolicy := req.OrphanPolicy
	if orphanPolicy == "" {
		orphanPolicy = protocol.OrphanPolicyFail
	}
	useWorktree := true
	if req.UseWorktree != nil {
		useWorktree = *req.UseWorktree
	}
	timeoutSecs := 3600
	if req.TimeoutSeconds != nil {
		timeoutSecs = *req.TimeoutSeconds
	}

	taskID := uuid.NewString()
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		return DispatchResult{}, err
	}
	// Restrict scheduling to currently connected online workers.
	onlineWorkers := make([]store.Worker, 0, len(workers))
	for _, w := range workers {
		if w.Status == "online" && s.hub.IsWorkerConnected(w.ID) {
			onlineWorkers = append(onlineWorkers, w)
		}
	}

	counts := s.hub.ActiveTaskCounts()
	selected, ok := SelectWorker(onlineWorkers, counts, req.WorkerSelector, req.Agent, permMode)

	task := store.Task{
		ID:             taskID,
		SpaceID:        req.SpaceID,
		WorkspaceID:    req.WorkspaceID,
		ProjectID:      req.ProjectID,
		RepoID:         req.RepoID,
		WorktreeID:     req.WorktreeID,
		Agent:          req.Agent,
		Model:          strings.TrimSpace(req.Model),
		SessionKind:    sessionKind,
		Workdir:        req.RepoPath,
		EffectiveDir:   req.RepoPath,
		Branch:         req.Branch,
		Mode:           mode,
		Origin:         origin,
		PermissionMode: permMode,
		OrphanPolicy:   orphanPolicy,
		Prompt:         req.Prompt,
		Status:         protocol.TaskStatusQueued,
		CreatedAt:      time.Now().UTC(),
	}
	if ok {
		task.WorkerID = selected.ID
		task.Status = protocol.TaskStatusAssigned
	}

	if err := s.store.CreateTask(ctx, task); err != nil {
		return DispatchResult{}, err
	}

	// Persist last-used Machine/Agent/Model for future chat sessions.
	if sessionKind == "chat" && origin != protocol.TaskOriginScratch {
		lastUpdates := map[string]string{}
		if task.WorkerID != "" {
			lastUpdates["last_used_machine"] = task.WorkerID
		}
		if task.Agent != "" && task.Agent != "shell" {
			lastUpdates["last_used_agent"] = task.Agent
		}
		if task.Model != "" {
			lastUpdates["last_used_model"] = task.Model
		}
		if len(lastUpdates) > 0 {
			_ = s.store.SetSettings(ctx, lastUpdates)
		}
	}

	startPayload := protocol.TaskStartPayload{
		TaskID:         taskID,
		Agent:          req.Agent,
		Model:          task.Model,
		SessionKind:    sessionKind,
		Workdir:        req.RepoPath,
		Branch:         req.Branch,
		UseWorktree:    useWorktree,
		Prompt:         req.Prompt,
		Mode:           mode,
		PermissionMode: string(permMode),
		TimeoutSeconds: timeoutSecs,
	}

	if ok {
		if err := s.hub.DispatchToWorker(selected.ID, startPayload); err != nil {
			_ = s.store.UpdateTaskStatus(ctx, taskID, protocol.TaskStatusQueued, "", "", nil)
			task.Status = protocol.TaskStatusQueued
			hQueue := s.hub
			hQueue.QueuePendingStart(startPayload, req.WorkerSelector)
		}
	} else {
		s.hub.QueuePendingStart(startPayload, req.WorkerSelector)
	}

	s.hub.broadcastFleetUpdate("task", task)

	return DispatchResult{
		TaskID:           task.ID,
		WorkerID:         task.WorkerID,
		SpaceID:          task.SpaceID,
		WorkspaceID:      task.WorkspaceID,
		ProjectID:        task.ProjectID,
		RepoID:           task.RepoID,
		WorktreeID:       task.WorktreeID,
		SessionKind:      task.SessionKind,
		Agent:            task.Agent,
		Model:            task.Model,
		Status:           task.Status,
		WorkdirEffective: task.EffectiveDir,
	}, nil
}

type TaskListItem struct {
	TaskID         string `json:"task_id"`
	WorkerID       string `json:"worker_id,omitempty"`
	Agent          string `json:"agent"`
	Status         string `json:"status"`
	Branch         string `json:"branch,omitempty"`
	Workdir        string `json:"workdir"`
	Mode           string `json:"mode"`
	ElapsedSeconds int64  `json:"elapsed_seconds"`
	LastMilestone  string `json:"last_milestone,omitempty"`
	CreatedAt      string `json:"created_at"`
}

func (s *FleetService) ListTasks(ctx context.Context, statusFilter, workerID string, limit int) ([]TaskListItem, error) {
	tasks, err := s.store.ListTasks(ctx, statusFilter, workerID, limit)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]TaskListItem, 0, len(tasks))
	for _, t := range tasks {
		var elapsed int64
		start := t.CreatedAt
		if t.StartedAt != nil {
			start = *t.StartedAt
		}
		if t.CompletedAt != nil {
			elapsed = int64(t.CompletedAt.Sub(start).Seconds())
		} else {
			elapsed = int64(now.Sub(start).Seconds())
		}
		if elapsed < 0 {
			elapsed = 0
		}

		lastMilestone := ""
		if evs, err := s.store.ListEvents(ctx, t.ID, 0, 50); err == nil && len(evs) > 0 {
			lastMilestone = evs[len(evs)-1].Message
		}

		out = append(out, TaskListItem{
			TaskID:         t.ID,
			WorkerID:       t.WorkerID,
			Agent:          t.Agent,
			Status:         t.Status,
			Branch:         t.Branch,
			Workdir:        t.Workdir,
			Mode:           t.Mode,
			ElapsedSeconds: elapsed,
			LastMilestone:  lastMilestone,
			CreatedAt:      t.CreatedAt.Format(time.RFC3339),
		})
	}
	return out, nil
}

type TaskResult struct {
	TaskID           string               `json:"task_id"`
	WorkerID         string               `json:"worker_id,omitempty"`
	Agent            string               `json:"agent"`
	Status           string               `json:"status"`
	ExitCode         *int                 `json:"exit_code,omitempty"`
	DurationSeconds  int64                `json:"duration_seconds"`
	Summary          string               `json:"summary"`
	GitSummary       *protocol.GitSummary `json:"git_summary,omitempty"`
	RecentMilestones []string             `json:"recent_milestones,omitempty"`
}

func (s *FleetService) GetTaskResult(ctx context.Context, taskID string, wait bool, timeoutSeconds int) (TaskResult, error) {
	t, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return TaskResult{}, err
	}

	if wait && !protocol.IsTerminalTaskStatus(t.Status) {
		if timeoutSeconds <= 0 {
			timeoutSeconds = 300
		}
		waitCh, cancelWait := s.hub.WaitForTask(taskID)
		defer cancelWait()

		// Re-check store after registering waiter to avoid missing a racey transition.
		if latest, err := s.store.GetTask(ctx, taskID); err == nil && protocol.IsTerminalTaskStatus(latest.Status) {
			t = latest
		} else {
			timer := time.NewTimer(time.Duration(timeoutSeconds) * time.Second)
			defer timer.Stop()
			select {
			case doneTask := <-waitCh:
				t = doneTask
			case <-timer.C:
				if latest, err := s.store.GetTask(ctx, taskID); err == nil {
					t = latest
				}
			case <-ctx.Done():
				return TaskResult{}, ctx.Err()
			}
		}
	}

	var dur int64
	start := t.CreatedAt
	if t.StartedAt != nil {
		start = *t.StartedAt
	}
	end := time.Now().UTC()
	if t.CompletedAt != nil {
		end = *t.CompletedAt
	}
	dur = int64(end.Sub(start).Seconds())
	if dur < 0 {
		dur = 0
	}

	var gitSum *protocol.GitSummary
	if t.GitSummaryJSON != "" {
		var gs protocol.GitSummary
		if err := json.Unmarshal([]byte(t.GitSummaryJSON), &gs); err == nil {
			gitSum = &gs
		}
	}

	var milestones []string
	if evs, err := s.store.ListEvents(ctx, t.ID, 0, 100); err == nil {
		startIdx := 0
		if len(evs) > 10 {
			startIdx = len(evs) - 10
		}
		for _, e := range evs[startIdx:] {
			milestones = append(milestones, fmt.Sprintf("[%s] %s", e.Kind, e.Message))
		}
	}

	return TaskResult{
		TaskID:           t.ID,
		WorkerID:         t.WorkerID,
		Agent:            t.Agent,
		Status:           t.Status,
		ExitCode:         t.ExitCode,
		DurationSeconds:  dur,
		Summary:          t.Summary,
		GitSummary:       gitSum,
		RecentMilestones: milestones,
	}, nil
}

type ActionResult struct {
	OK     bool   `json:"ok"`
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

func (s *FleetService) SteerTask(ctx context.Context, taskID, inputText string, appendNewline bool) (ActionResult, error) {
	t, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return ActionResult{}, err
	}
	if protocol.IsTerminalTaskStatus(t.Status) {
		return ActionResult{}, fmt.Errorf("task %s is already %s", taskID, t.Status)
	}
	if err := s.hub.SendSteer(taskID, inputText, appendNewline); err != nil {
		return ActionResult{}, err
	}

	// Audit event
	ev := store.TaskEvent{
		TaskID:    taskID,
		Kind:      "control",
		Message:   fmt.Sprintf("Steer input sent (%d bytes)", len(inputText)),
		CreatedAt: time.Now().UTC(),
	}
	if id, err := s.store.AppendEvent(ctx, ev); err == nil {
		ev.ID = id
	}
	s.hub.broadcastFleetUpdate("event", ev)

	return ActionResult{OK: true, TaskID: taskID, Status: t.Status}, nil
}

func (s *FleetService) CancelTask(ctx context.Context, taskID string) (ActionResult, error) {
	t, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return ActionResult{}, err
	}
	if protocol.IsTerminalTaskStatus(t.Status) {
		return ActionResult{OK: true, TaskID: taskID, Status: t.Status}, nil
	}
	if t.Status == protocol.TaskStatusQueued {
		_ = s.store.UpdateTaskStatus(ctx, taskID, protocol.TaskStatusCanceled, "canceled while queued", "", nil)
		if updated, err := s.store.GetTask(ctx, taskID); err == nil {
			s.hub.notifyTaskDone(updated)
			s.hub.broadcastFleetUpdate("task", updated)
		}
		return ActionResult{OK: true, TaskID: taskID, Status: protocol.TaskStatusCanceled}, nil
	}

	sent := s.hub.SendCancel(taskID)
	if !sent {
		_ = s.store.UpdateTaskStatus(ctx, taskID, protocol.TaskStatusCanceled, "canceled (worker unreachable)", "", nil)
		if updated, err := s.store.GetTask(ctx, taskID); err == nil {
			s.hub.notifyTaskDone(updated)
			s.hub.broadcastFleetUpdate("task", updated)
		}
		return ActionResult{OK: true, TaskID: taskID, Status: protocol.TaskStatusCanceled}, nil
	}

	ev := store.TaskEvent{
		TaskID:    taskID,
		Kind:      "control",
		Message:   "Cancellation requested",
		CreatedAt: time.Now().UTC(),
	}
	if id, err := s.store.AppendEvent(ctx, ev); err == nil {
		ev.ID = id
	}
	s.hub.broadcastFleetUpdate("event", ev)

	return ActionResult{OK: true, TaskID: taskID, Status: protocol.TaskStatusCanceled}, nil
}

// --- orchestrator.ToolExecutor implementation ---

func (s *FleetService) ToolDefinitions() []orchestrator.ToolDef {
	return []orchestrator.ToolDef{
		{
			Name:        "fleet_status",
			Description: "List all registered Konsert worker nodes, their online status, available coding agents, isolation tier, permission mode, and active task counts.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "dispatch_task",
			Description: "Dispatch a coding agent task to a matching worker node in the fleet.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"worker_selector": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"worker_id":  map[string]any{"type": "string"},
							"host_group": map[string]any{"type": "string"},
							"os":         map[string]any{"type": "string"},
							"tags":       map[string]any{"type": "object"},
						},
					},
					"agent":           map[string]any{"type": "string", "description": "Agent driver name (e.g. claude, agy, grok, mock)"},
					"repo_path":       map[string]any{"type": "string", "description": "Working directory / git repository path on the worker"},
					"branch":          map[string]any{"type": "string", "description": "Optional git branch name (creates isolated worktree by default)"},
					"use_worktree":    map[string]any{"type": "boolean", "description": "Use isolated git worktree when branch is specified (default true)"},
					"prompt":          map[string]any{"type": "string", "description": "Instructions for the coding agent"},
					"mode":            map[string]any{"type": "string", "enum": []string{"oneshot", "interactive"}},
					"permission_mode": map[string]any{"type": "string", "enum": []string{"full_access", "sandboxed"}},
					"timeout_seconds": map[string]any{"type": "integer"},
				},
				"required": []string{"agent", "repo_path", "prompt"},
			},
		},
		{
			Name:        "list_tasks",
			Description: "List recent and active fleet tasks with concise status and last milestone.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status_filter": map[string]any{"type": "string"},
					"worker_id":     map[string]any{"type": "string"},
					"limit":         map[string]any{"type": "integer"},
				},
			},
		},
		{
			Name:        "get_task_result",
			Description: "Retrieve concise structured result for a task (exit code, summary, git diff stats, milestones). Set wait=true to block until completion.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id":         map[string]any{"type": "string"},
					"wait":            map[string]any{"type": "boolean"},
					"timeout_seconds": map[string]any{"type": "integer"},
				},
				"required": []string{"task_id"},
			},
		},
		{
			Name:        "steer_task",
			Description: "Send follow-up steering text or prompt response to a running or waiting_input task.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id":        map[string]any{"type": "string"},
					"input_text":     map[string]any{"type": "string"},
					"append_newline": map[string]any{"type": "boolean"},
				},
				"required": []string{"task_id", "input_text"},
			},
		},
		{
			Name:        "cancel_task",
			Description: "Cancel a queued or running task and terminate its remote process tree.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "string"},
				},
				"required": []string{"task_id"},
			},
		},
	}
}

func (s *FleetService) ExecuteTool(ctx context.Context, name string, args json.RawMessage) (any, error) {
	switch name {
	case "fleet_status":
		return s.FleetStatus(ctx)

	case "dispatch_task":
		var req DispatchRequest
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, fmt.Errorf("invalid dispatch_task args: %w", err)
		}
		return s.DispatchTask(ctx, req)

	case "list_tasks":
		var req struct {
			StatusFilter string `json:"status_filter"`
			WorkerID     string `json:"worker_id"`
			Limit        int    `json:"limit"`
		}
		_ = json.Unmarshal(args, &req)
		return s.ListTasks(ctx, req.StatusFilter, req.WorkerID, req.Limit)

	case "get_task_result":
		var req struct {
			TaskID         string `json:"task_id"`
			Wait           bool   `json:"wait"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, fmt.Errorf("invalid get_task_result args: %w", err)
		}
		return s.GetTaskResult(ctx, req.TaskID, req.Wait, req.TimeoutSeconds)

	case "steer_task":
		var req struct {
			TaskID        string `json:"task_id"`
			InputText     string `json:"input_text"`
			AppendNewline *bool  `json:"append_newline"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, fmt.Errorf("invalid steer_task args: %w", err)
		}
		appendNL := true
		if req.AppendNewline != nil {
			appendNL = *req.AppendNewline
		}
		return s.SteerTask(ctx, req.TaskID, req.InputText, appendNL)

	case "cancel_task":
		var req struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, fmt.Errorf("invalid cancel_task args: %w", err)
		}
		return s.CancelTask(ctx, req.TaskID)

	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}
