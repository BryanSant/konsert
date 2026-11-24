package controller

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/orchestrator"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
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
	SID            string                  `json:"sid"`
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
	Metrics        *protocol.WorkerMetrics `json:"metrics,omitzero"`
	Facts          *protocol.MachineFacts  `json:"facts,omitzero"`
	// Version is the worker's konsertd build version ("" when unknown).
	Version string `json:"version,omitempty"`
	// Launch is how the hub (re)starts this machine's konsertd ("", "local", "ssh");
	// LaunchState/LaunchError report the last start attempt ("starting", "failed").
	Launch      string     `json:"launch,omitempty"`
	SSHTarget   string     `json:"ssh_target,omitempty"`
	LaunchState string     `json:"launch_state,omitempty"`
	LaunchError string     `json:"launch_error,omitempty"`
	LaunchAt    *time.Time `json:"launch_at,omitzero"`
	// WakeLock is the host Keep Awake state (nil until reported or requested).
	WakeLock *WorkerWakeLockView `json:"wake_lock,omitzero"`
	// Heat is the machine's smoothed load score (nil before its first heartbeat).
	Heat *WorkerHeat `json:"heat,omitzero"`
	// Drained machines finish their current work and accept nothing new.
	Drained   bool       `json:"drained"`
	DrainedAt *time.Time `json:"drained_at,omitzero"`
}

// toWorkerView builds the API/WebSocket view of a worker row with its live
// active-task count and latest metrics. Facts are decoded from the cached JSON.
func toWorkerView(w store.Worker, active int, metrics *protocol.WorkerMetrics) WorkerStatusView {
	return WorkerStatusView{
		ID:             w.ID,
		SID:            w.SID,
		Name:           w.Name,
		HostGroup:      w.HostGroup,
		OS:             w.OS,
		Arch:           w.Arch,
		Isolation:      w.Isolation,
		PermissionMode: string(w.PermissionMode),
		Agents:         w.Agents,
		Tags:           w.Tags,
		MaxConcurrent:  w.MaxConcurrent,
		Status:         w.Status,
		ActiveTasks:    active,
		LastSeenAt:     w.LastSeenAt,
		Metrics:        metrics,
		Facts:          decodeWorkerFacts(w.FactsJSON),
		Version:        w.Version,
		Launch:         w.Launch,
		SSHTarget:      w.SSHTarget,
		LaunchState:    w.LaunchState,
		LaunchError:    w.LaunchError,
		LaunchAt:       w.LaunchAt,
		Drained:        w.Drained,
		DrainedAt:      w.DrainedAt,
	}
}

func decodeWorkerFacts(raw string) *protocol.MachineFacts {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var f protocol.MachineFacts
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return nil
	}
	return &f
}

type FleetStatusResult struct {
	Workers          []WorkerStatusView `json:"workers"`
	ActiveTasksCount int                `json:"active_tasks_count"`
	// ControllerVersion is the hub's build version (set by GET /api/fleet).
	ControllerVersion string `json:"controller_version,omitempty"`
	// Pause is the fleet-wide Pause everything state.
	Pause store.FleetPause `json:"pause"`
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
		view := toWorkerView(w, ac, s.hub.WorkerMetrics(w.ID))
		view.Status = st
		view.WakeLock = s.hub.workerWakeLockView(w.ID)
		if st == "online" {
			view.Heat = s.hub.workerHeat(w.ID)
		}
		views = append(views, view)
	}
	return FleetStatusResult{
		Workers:          views,
		ActiveTasksCount: totalActive,
		Pause:            s.hub.fleetPause(ctx),
	}, nil
}

type SessionDefaultsResult struct {
	MachineID       string   `json:"machine_id"`
	MachineSource   string   `json:"machine_source"` // "explicit" | "localhost" | "last_used" | "guess"
	Agent           string   `json:"agent"`
	AgentSource     string   `json:"agent_source"` // "explicit" | "default" | "last_used" | "guess"
	Model           string   `json:"model"`
	ModelSource     string   `json:"model_source"` // "explicit" | "default" | "last_used" | "capabilities" | "" (agent default)
	AvailableAgents []string `json:"available_agents"`
}

var preferredAgentOrder = []string{"agy", "claude", "codex", "grok", "mock"}

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
// and Model (default -> last used -> the agent account's reported default) for a new Chat session.
func (s *FleetService) ResolveSessionDefaults(ctx context.Context, reqMachine, reqAgent, reqModel string) (SessionDefaultsResult, error) {
	reqMachine = resolveRef(ctx, s.store, ids.Machine, reqMachine)
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

	// 3. Model cascade: explicit -> configured default -> last used -> capabilities
	if mod := strings.TrimSpace(reqModel); mod != "" {
		res.Model = mod
		res.ModelSource = "explicit"
	} else if defM := strings.TrimSpace(saved["default_model"]); defM != "" {
		res.Model = defM
		res.ModelSource = "default"
	} else if lastM := strings.TrimSpace(saved["last_used_model"]); lastM != "" {
		res.Model = lastM
		res.ModelSource = "last_used"
	} else if chosenWorker != nil {
		// No guesses: the agent's own reported default model (capabilities), else
		// empty so the agent CLI uses its default.
		if a, ok := s.hub.lookupAccount(ctx, chosenWorker.ID, res.Agent, ""); ok && a.DefaultModel != "" {
			res.Model = a.DefaultModel
			res.ModelSource = "capabilities"
		}
	}

	return res, nil
}

type DispatchRequest struct {
	WorkerSelector WorkerSelector `json:"worker_selector,omitzero"`
	SpaceID        string         `json:"space_id,omitempty"`
	WorkspaceID    string         `json:"workspace_id,omitempty"`
	ProjectID      string         `json:"project_id,omitempty"`
	RepoID         string         `json:"repo_id,omitempty"`
	WorktreeID     string         `json:"worktree_id,omitempty"`
	SessionKind    string         `json:"session_kind,omitempty"` // "chat" | "terminal"
	// Title is a short name for the task shown in the live work views (default: the
	// prompt's first line).
	Title          string `json:"title,omitempty" jsonschema:"Short name for the task (max 80 chars), e.g. 'KON-2Q0CH: questions card on mobile'"`
	Agent          string `json:"agent"`
	Model          string `json:"model,omitempty"`
	RepoPath       string `json:"repo_path"`
	Branch         string `json:"branch,omitempty"`
	UseWorktree    *bool  `json:"use_worktree,omitzero"`
	Prompt         string `json:"prompt"`
	Mode           string `json:"mode,omitempty"`
	Origin         string `json:"origin,omitempty"` // "orchestrator" | "direct" | "scratch"
	PermissionMode string `json:"permission_mode,omitempty"`
	OrphanPolicy   string `json:"orphan_policy,omitempty"`
	TimeoutSeconds *int   `json:"timeout_seconds,omitzero"`
	// AccountID selects the worker-local agent account ("" = the agent's default);
	// ApprovalMode and Effort are the agent's own approval mode and effort IDs.
	AccountID    string `json:"account_id,omitempty" jsonschema:"Agent account on the worker (see GET /api/agents/accounts); empty = the agent's default account"`
	ApprovalMode string `json:"approval_mode,omitempty" jsonschema:"The agent's approval mode ID (from its capabilities); empty = the account default"`
	Effort       string `json:"effort,omitempty" jsonschema:"Reasoning effort ID of the model (from capabilities); empty = the model default"`
}

type DispatchResult struct {
	TaskID           string `json:"task_id"`
	TaskSID          string `json:"task_sid"`
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
	// Warnings are non-fatal notes, e.g. the account's usage window is exhausted.
	Warnings []string `json:"warnings,omitempty"`
}

func (s *FleetService) DispatchTask(ctx context.Context, req DispatchRequest) (DispatchResult, error) {
	resolveRefs(ctx, s.store, map[ids.Kind][]*string{
		ids.Machine:   {&req.WorkerSelector.WorkerID},
		ids.Space:     {&req.SpaceID},
		ids.Workspace: {&req.WorkspaceID},
		ids.Project:   {&req.ProjectID},
		ids.Repo:      {&req.RepoID},
		ids.Worktree:  {&req.WorktreeID},
	})
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

	taskID := ids.UUID()
	taskSID, err := s.store.NewSID(ctx, ids.Task)
	if err != nil {
		return DispatchResult{}, err
	}
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		return DispatchResult{}, err
	}
	cause := causeFrom(ctx)
	// Pause everything stops automatic dispatching; the user may still start work by hand.
	if isAutomatic(cause) && s.hub.fleetPaused(ctx) {
		return DispatchResult{}, errFleetPaused
	}
	// Restrict scheduling to currently connected online workers that accept new work
	// (not drained) and, for automatic dispatch, are not held.
	onlineWorkers := make([]store.Worker, 0, len(workers))
	pinnedID := req.WorkerSelector.WorkerID
	lifted := map[string]*store.Override{}
	for _, w := range workers {
		if w.Status != "online" || !s.hub.IsWorkerConnected(w.ID) {
			continue
		}
		if w.Drained {
			// A drained machine accepts nothing new: work pinned to it waits in the queue
			// until the user undrains it.
			continue
		}
		if isAutomatic(cause) {
			hold, held, ov := s.hub.holdVerdict(ctx, holdCheck{workerID: w.ID, text: req.Prompt})
			if held {
				if w.ID == pinnedID {
					return DispatchResult{}, holdError(hold, w.Name)
				}
				continue
			}
			lifted[w.ID] = ov
		}
		onlineWorkers = append(onlineWorkers, w)
	}

	counts := s.hub.ActiveTaskCounts()
	// Approved machine_slots overrides raise a machine's slot limit while they last.
	onlineWorkers, slots := s.hub.applySlotOverrides(ctx, onlineWorkers)
	req.AccountID = strings.TrimSpace(req.AccountID)
	selected, ok := SelectWorker(onlineWorkers, counts, req.WorkerSelector, req.Agent, permMode)
	if ok {
		// Never start work on a logged-out account: place it elsewhere if the
		// selector allows, otherwise refuse with the re-login hint.
		if loggedOut := s.hub.checkAccountLoggedIn(ctx, selected.ID, req.Agent, req.AccountID); loggedOut != nil {
			eligible := make([]store.Worker, 0, len(onlineWorkers))
			for _, w := range onlineWorkers {
				if s.hub.checkAccountLoggedIn(ctx, w.ID, req.Agent, req.AccountID) == nil {
					eligible = append(eligible, w)
				}
			}
			if selected, ok = SelectWorker(eligible, counts, req.WorkerSelector, req.Agent, permMode); !ok {
				return DispatchResult{}, loggedOut
			}
		}
	}
	var warnings []string
	if ok {
		if a, found := s.hub.lookupAccount(ctx, selected.ID, req.Agent, req.AccountID); found {
			if u, exhausted := exhaustedWindow(a, time.Now()); exhausted {
				warnings = append(warnings, exhaustedWarning(a, u))
			}
		}
	}

	task := store.Task{
		ID:             taskID,
		SID:            taskSID,
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
		Cause:          &cause,
		CreatedAt:      time.Now().UTC(),
		Title:          oneLine(req.Title, 80),
		TicketID:       detectTicket(req.Title, req.Prompt),
	}
	if o, fromChat := chatOriginFrom(ctx); fromChat {
		task.ParentChatID = o.id
	}
	if ok {
		task.WorkerID = selected.ID
		task.Status = protocol.TaskStatusAssigned
	}

	if err := s.store.CreateTask(ctx, task); err != nil {
		return DispatchResult{}, err
	}
	s.hub.recordTaskTransition(ctx, task, "", cause, "")

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
		SID:            taskSID,
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
		AccountID:      req.AccountID,
		ApprovalMode:   strings.TrimSpace(req.ApprovalMode),
		Effort:         strings.TrimSpace(req.Effort),
	}

	// Persist the selector and full start payload so a queued (or later requeued)
	// task keeps its constraints and launch options across controller restarts.
	pinned := strings.TrimSpace(req.WorkerSelector.WorkerID) != ""
	spec := store.TaskStartSpec{WorkerPinned: pinned}
	if b, mErr := json.Marshal(req.WorkerSelector, protocol.JSONOptions); mErr == nil {
		spec.SelectorJSON = string(b)
	}
	if b, mErr := json.Marshal(startPayload, protocol.JSONOptions); mErr == nil {
		spec.PayloadJSON = string(b)
	}
	if err := s.store.SetTaskStartSpec(ctx, taskID, spec); err != nil {
		return DispatchResult{}, fmt.Errorf("persist task start spec: %w", err)
	}

	if ok {
		if err := s.hub.DispatchToWorker(selected.ID, startPayload); err != nil {
			_, _ = s.store.RequeueTask(ctx, taskID, "", pinned, causeScheduler)
			task.Status = protocol.TaskStatusQueued
			if !pinned {
				task.WorkerID = ""
			}
			s.hub.kickScheduler()
		} else {
			// Measure the overrides that made this placement possible.
			if ov := lifted[selected.ID]; ov != nil {
				s.hub.useOverride(ctx, *ov)
			}
			s.hub.placed(ctx, slots, selected.ID, counts[selected.ID])
		}
	}

	s.hub.broadcastFleetUpdate("task", task)
	s.hub.resolveTaskTicket(task.ID, task.TicketID)

	return DispatchResult{
		TaskID:           task.ID,
		TaskSID:          task.SID,
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
		Warnings:         warnings,
	}, nil
}

type TaskListItem struct {
	TaskID         string `json:"task_id"`
	TaskSID        string `json:"task_sid"`
	WorkerID       string `json:"worker_id,omitempty"`
	WorkerSID      string `json:"worker_sid,omitempty"`
	Agent          string `json:"agent"`
	Status         string `json:"status"`
	Branch         string `json:"branch,omitempty"`
	Workdir        string `json:"workdir"`
	Mode           string `json:"mode"`
	ElapsedSeconds int64  `json:"elapsed_seconds"`
	LastMilestone  string `json:"last_milestone,omitempty"`
	CreatedAt      string `json:"created_at"`
	// LastCause attributes the last status change (who, through what, how sure).
	LastCause *store.Cause `json:"last_cause,omitzero"`
	// SuspendedBy is "user" or "pause" while the task is suspended.
	SuspendedBy string `json:"suspended_by,omitempty"`
	// Holds lists active holds on the task's session or machine ("h-… summary").
	Holds []string `json:"holds,omitempty"`
}

func (s *FleetService) ListTasks(ctx context.Context, statusFilter, workerID string, limit int) ([]TaskListItem, error) {
	workerID = resolveRef(ctx, s.store, ids.Machine, workerID)
	tasks, err := s.store.ListTasks(ctx, statusFilter, workerID, limit)
	if err != nil {
		return nil, err
	}
	workerSIDs := s.workerSIDs(ctx)
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
			TaskSID:        t.SID,
			WorkerID:       t.WorkerID,
			WorkerSID:      workerSIDs[t.WorkerID],
			Agent:          t.Agent,
			Status:         t.Status,
			Branch:         t.Branch,
			Workdir:        t.Workdir,
			Mode:           t.Mode,
			ElapsedSeconds: elapsed,
			LastMilestone:  lastMilestone,
			CreatedAt:      t.CreatedAt.Format(time.RFC3339),
			LastCause:      t.Cause,
			SuspendedBy:    t.SuspendedBy,
			Holds:          s.hub.holdLabelsFor(ctx, t.WorkerID, t.ID, t.ParentChatID),
		})
	}
	return out, nil
}

// workerSIDs maps every worker's id to its sid.
func (s *FleetService) workerSIDs(ctx context.Context) map[string]string {
	out := map[string]string{}
	if workers, err := s.store.ListWorkers(ctx); err == nil {
		for _, w := range workers {
			out[w.ID] = w.SID
		}
	}
	return out
}

type TaskResult struct {
	TaskID           string               `json:"task_id"`
	TaskSID          string               `json:"task_sid"`
	WorkerID         string               `json:"worker_id,omitempty"`
	Agent            string               `json:"agent"`
	Status           string               `json:"status"`
	ExitCode         *int                 `json:"exit_code,omitzero"`
	DurationSeconds  int64                `json:"duration_seconds"`
	Summary          string               `json:"summary"`
	GitSummary       *protocol.GitSummary `json:"git_summary,omitzero"`
	RecentMilestones []string             `json:"recent_milestones,omitempty"`
	// LastCause attributes the last status change; Holds lists active holds covering it.
	LastCause   *store.Cause `json:"last_cause,omitzero"`
	SuspendedBy string       `json:"suspended_by,omitempty"`
	Holds       []string     `json:"holds,omitempty"`
}

func (s *FleetService) GetTaskResult(ctx context.Context, taskID string, wait bool, timeoutSeconds int) (TaskResult, error) {
	taskID = resolveRef(ctx, s.store, ids.Task, taskID)
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
		TaskSID:          t.SID,
		WorkerID:         t.WorkerID,
		Agent:            t.Agent,
		Status:           t.Status,
		ExitCode:         t.ExitCode,
		DurationSeconds:  dur,
		Summary:          t.Summary,
		GitSummary:       gitSum,
		RecentMilestones: milestones,
		LastCause:        t.Cause,
		SuspendedBy:      t.SuspendedBy,
		Holds:            s.hub.holdLabelsFor(ctx, t.WorkerID, t.ID, t.ParentChatID),
	}, nil
}

type ActionResult struct {
	OK      bool   `json:"ok"`
	TaskID  string `json:"task_id"`
	TaskSID string `json:"task_sid"`
	Status  string `json:"status"`
}

func (s *FleetService) SteerTask(ctx context.Context, taskID, inputText string, appendNewline bool) (ActionResult, error) {
	taskID = resolveRef(ctx, s.store, ids.Task, taskID)
	t, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return ActionResult{}, err
	}
	if protocol.IsTerminalTaskStatus(t.Status) {
		return ActionResult{}, fmt.Errorf("task %s is already %s", t.SID, t.Status)
	}
	if cause := causeFrom(ctx); isAutomatic(cause) {
		if t.Status == protocol.TaskStatusSuspended {
			return ActionResult{}, fmt.Errorf("task %s is suspended by the user; ask the user (ask_user) before steering it", t.SID)
		}
		if hold, held := s.hub.holdBlocking(ctx, holdCheck{workerID: t.WorkerID, taskID: t.ID, chatID: t.ParentChatID, text: inputText}); held {
			return ActionResult{}, holdError(hold, s.hub.workerLabel(ctx, t.WorkerID))
		}
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
	// The steer text itself is never audited: it may carry secrets.
	s.hub.recordAudit(ctx, store.AuditEvent{
		Action:   AuditSteer,
		WorkerID: t.WorkerID,
		TaskID:   taskID,
		Message:  ev.Message,
		MetaJSON: auditMeta(map[string]any{"bytes": len(inputText), "append_newline": appendNewline}),
	})

	return ActionResult{OK: true, TaskID: taskID, TaskSID: t.SID, Status: t.Status}, nil
}

func (s *FleetService) CancelTask(ctx context.Context, taskID string) (ActionResult, error) {
	return s.hub.cancelTask(ctx, resolveRef(ctx, s.store, ids.Task, taskID), "")
}

// cancelTask cancels a task with the cause in ctx. note (a veto reason) is added to the
// audit message. A cancel by the user that no agent asked for places a hold on the
// session so the orchestrator doesn't simply restart it.
func (h *Hub) cancelTask(ctx context.Context, taskID, note string) (ActionResult, error) {
	t, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		return ActionResult{}, err
	}
	if protocol.IsTerminalTaskStatus(t.Status) {
		return ActionResult{OK: true, TaskID: taskID, TaskSID: t.SID, Status: t.Status}, nil
	}
	cause := causeFrom(ctx)
	if isAutomatic(cause) {
		if hold, held := h.holdBlocking(ctx, holdCheck{taskID: t.ID, chatID: t.ParentChatID, sessionOnly: true}); held {
			return ActionResult{}, holdError(hold, h.workerLabel(ctx, t.WorkerID))
		}
	}
	audit := func(msg string) {
		if note != "" {
			msg += " (" + note + ")"
		}
		h.recordAudit(ctx, store.AuditEvent{
			Action:   AuditCancel,
			WorkerID: t.WorkerID,
			TaskID:   taskID,
			Message:  msg,
			MetaJSON: auditMeta(map[string]any{"prior_status": t.Status}),
		})
	}
	h.recordIntervention(ctx, cause, "cancel", t.WorkerID, t.ID, t.ParentChatID,
		fmt.Sprintf("%s canceled task %s (%s) on %s", capitalize(describeActor(cause)), t.SID, t.Agent, h.workerLabel(ctx, t.WorkerID)))
	hold := func() {
		h.holdAfterIntervention(ctx, cause, holdSpec{scope: store.HoldScopeSession, workerID: t.WorkerID, taskID: t.ID,
			summary: fmt.Sprintf("The user canceled task %s on %s", t.SID, h.workerLabel(ctx, t.WorkerID))})
	}
	if t.Status == protocol.TaskStatusQueued {
		audit("Canceled while queued")
		_, _ = h.setTaskStatus(ctx, taskID, protocol.TaskStatusCanceled, "canceled while queued", "", nil, cause)
		hold()
		return ActionResult{OK: true, TaskID: taskID, TaskSID: t.SID, Status: protocol.TaskStatusCanceled}, nil
	}

	h.setPendingCause(taskID, protocol.TaskStatusCanceled, cause)
	if !h.SendCancel(taskID) {
		audit("Canceled (worker unreachable)")
		_, _ = h.setTaskStatus(ctx, taskID, protocol.TaskStatusCanceled, "canceled (worker unreachable)", "", nil, cause)
		hold()
		return ActionResult{OK: true, TaskID: taskID, TaskSID: t.SID, Status: protocol.TaskStatusCanceled}, nil
	}

	ev := store.TaskEvent{
		TaskID:    taskID,
		Kind:      "control",
		Message:   "Cancellation requested",
		CreatedAt: time.Now().UTC(),
	}
	if id, err := h.store.AppendEvent(ctx, ev); err == nil {
		ev.ID = id
	}
	h.broadcastFleetUpdate("event", ev)
	audit("Cancellation requested")
	hold()
	return ActionResult{OK: true, TaskID: taskID, TaskSID: t.SID, Status: protocol.TaskStatusCanceled}, nil
}

// --- orchestrator.ContextProvider & daemon-first RPC helpers ---

// BuildCompactContext produces a token-efficient (<~400 token) live snapshot of fleet facts,
// taxonomy, active sessions, and persisted SQLite memory for the Orchestrator system prompt.
func (s *FleetService) BuildCompactContext(ctx context.Context, sessionID string) string {
	var b strings.Builder
	fs, err := s.FleetStatus(ctx)
	if err == nil && len(fs.Workers) > 0 {
		b.WriteString("Machines:\n")
		for _, w := range fs.Workers {
			line := fmt.Sprintf("- %s (id=%s, %s, %s/%s, active=%d/%d, agents=[%s]",
				w.Name, cmp.Or(w.SID, w.ID), w.Status, w.OS, w.Arch, w.ActiveTasks, w.MaxConcurrent, strings.Join(w.Agents, ","))
			if w.Facts != nil {
				line += fmt.Sprintf(", virt=%s, %dc/%dGB RAM, %dGB free",
					w.Facts.Virtualization, w.Facts.CPUCores, w.Facts.RAMTotalMB/1024, w.Facts.DiskFreeGB)
				if len(w.Facts.Tools) > 0 {
					var tools []string
					for k, v := range w.Facts.Tools {
						tools = append(tools, k+":"+v)
					}
					sort.Strings(tools)
					if len(tools) > 8 {
						tools = tools[:8]
					}
					line += ", tools=[" + strings.Join(tools, ",") + "]"
				}
				if len(w.Facts.AuthReady) > 0 {
					var ready []string
					for k, ok := range w.Facts.AuthReady {
						if ok {
							ready = append(ready, k)
						}
					}
					sort.Strings(ready)
					if len(ready) > 0 {
						line += ", auth_ready=[" + strings.Join(ready, ",") + "]"
					}
				}
			}
			line += ")\n"
			b.WriteString(line)
		}
	}

	if tax, err := s.store.GetTaxonomy(ctx); err == nil && len(tax.Projects) > 0 {
		var pNames []string
		for _, p := range tax.Projects {
			pNames = append(pNames, p.Name)
		}
		b.WriteString(fmt.Sprintf("Taxonomy: %d spaces, %d projects (%s), %d repos, %d worktrees\n",
			len(tax.Workspaces), len(tax.Projects), strings.Join(pNames, ", "), len(tax.Repos), len(tax.Worktrees)))
	}

	if fleetMem, err := s.store.ListMemory(ctx, "fleet", 6); err == nil && len(fleetMem) > 0 {
		b.WriteString("Fleet Memory:\n")
		for _, m := range fleetMem {
			b.WriteString(fmt.Sprintf("- %s: %s\n", m.Key, m.Summary))
		}
	}
	if sessionID != "" {
		if sessMem, err := s.store.ListMemory(ctx, "session:"+sessionID, 6); err == nil && len(sessMem) > 0 {
			for _, m := range sessMem {
				if m.Key == "thread_checkpoint" {
					continue
				}
				b.WriteString(fmt.Sprintf("- [session] %s: %s\n", m.Key, m.Summary))
			}
		}
	}
	return b.String()
}

// ScanTempWorktrees deterministically scans one or all online machines via konsertd RPC (zero agent LLM calls).
func (s *FleetService) ScanTempWorktrees(ctx context.Context, workerID, rootPath, query string) (map[string]any, error) {
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		return nil, err
	}
	var targets []store.Worker
	for _, w := range workers {
		if w.Status != "online" || !s.hub.IsWorkerConnected(w.ID) {
			continue
		}
		if workerID != "" && w.ID != workerID && w.SID != workerID && !strings.EqualFold(w.Name, workerID) {
			continue
		}
		targets = append(targets, w)
	}
	if len(targets) == 0 {
		return map[string]any{"scanned_workers": 0, "results": []any{}}, nil
	}

	type workerScan struct {
		WorkerID   string                       `json:"worker_id"`
		WorkerName string                       `json:"worker_name"`
		RootPath   string                       `json:"root_path,omitempty"`
		Matches    []protocol.WorktreeScanMatch `json:"matches"`
		NoOp       bool                         `json:"no_op"`
		Error      string                       `json:"error,omitempty"`
	}
	results := make([]workerScan, len(targets))
	var wg sync.WaitGroup
	for i, w := range targets {
		wg.Add(1)
		go func(idx int, wk store.Worker) {
			defer wg.Done()
			rpcCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			env, rErr := s.hub.RequestWorker(rpcCtx, wk.ID, protocol.MsgWorktreeScan, protocol.WorktreeScanRequest{
				RootPath: rootPath,
				Query:    query,
			})
			if rErr != nil {
				results[idx] = workerScan{WorkerID: wk.ID, WorkerName: wk.Name, Matches: []protocol.WorktreeScanMatch{}, Error: rErr.Error()}
				return
			}
			res, dErr := protocol.DecodePayload[protocol.WorktreeScanResult](env)
			if dErr != nil {
				results[idx] = workerScan{WorkerID: wk.ID, WorkerName: wk.Name, Matches: []protocol.WorktreeScanMatch{}, Error: dErr.Error()}
				return
			}
			results[idx] = workerScan{
				WorkerID:   wk.ID,
				WorkerName: wk.Name,
				RootPath:   res.RootPath,
				Matches:    res.Matches,
				NoOp:       len(res.Matches) == 0 && res.Error == "",
				Error:      res.Error,
			}
		}(i, w)
	}
	wg.Wait()

	return map[string]any{
		"scanned_workers": len(targets),
		"results":         results,
	}, nil
}

// --- orchestrator.ToolExecutor implementation ---

func (s *FleetService) ToolDefinitions() []orchestrator.ToolDef {
	return []orchestrator.ToolDef{
		{
			Name:        "fleet_status",
			Description: "List all registered Konsert worker nodes, their online status, available coding agents, cached machine facts, isolation tier, permission mode, and active task counts.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "inspect_machine_facts",
			Description: "Deterministic zero-token lookup of cached Machine Facts (OS/patch, CPU/RAM/swap/disk, /tmp tmpfs, WSL/container/VM, installed tool versions like postgres 18, and safe auth_ready flags like gcloud).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"worker_id": map[string]any{"type": "string", "description": "Optional worker ID or name; omit to return facts for all machines"},
				},
			},
		},
		{
			Name:        "inspect_git_status",
			Description: "Deterministic daemon RPC to inspect git branch, dirty files, and diff stats on a remote machine without invoking any coding agent.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"worker_id": map[string]any{"type": "string"},
					"workdir":   map[string]any{"type": "string"},
				},
				"required": []string{"worker_id", "workdir"},
			},
		},
		{
			Name:        "list_remote_dir",
			Description: "Deterministic daemon RPC to list folders/git repos on a remote machine without invoking any coding agent.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"worker_id": map[string]any{"type": "string"},
					"path":      map[string]any{"type": "string"},
					"prefix":    map[string]any{"type": "string"},
				},
				"required": []string{"worker_id", "path"},
			},
		},
		{
			Name:        "scan_temp_worktrees",
			Description: "Deterministic daemon RPC across one or all online machines to find .konsert-worktrees or temp worktrees matching a query (short-circuits no-op hosts at zero agent token cost).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"worker_id": map[string]any{"type": "string", "description": "Optional worker ID or name; omit to scan all online machines"},
					"root_path": map[string]any{"type": "string", "description": "Optional root path to scan on target machine(s)"},
					"query":     map[string]any{"type": "string", "description": "Optional substring filter such as an epic or branch name"},
				},
			},
		},
		{
			Name:        "cleanup_worktree",
			Description: "Deterministic daemon RPC to cleanly remove a Konsert-managed git worktree on a remote machine without invoking any coding agent.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"worker_id": map[string]any{"type": "string"},
					"workdir":   map[string]any{"type": "string"},
				},
				"required": []string{"worker_id", "workdir"},
			},
		},
		{
			Name:        "save_memory",
			Description: "Persist a concise fleet, project, or session note in SQLite so the Orchestrator remembers decisions and active bindings across context resets.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"scope":   map[string]any{"type": "string", "description": "Memory scope (default 'fleet', or 'project:<id>')"},
					"key":     map[string]any{"type": "string", "description": "Short unique memory key"},
					"summary": map[string]any{"type": "string", "description": "Concise summary to persist"},
				},
				"required": []string{"key", "summary"},
			},
		},
		{
			Name:        "recall_memory",
			Description: "Query persisted Orchestrator fleet/project/session memory entries from SQLite.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"scope": map[string]any{"type": "string", "description": "Optional scope filter (e.g. 'fleet')"},
					"limit": map[string]any{"type": "integer"},
				},
			},
		},
		{
			Name:        "dispatch_task",
			Description: "Delegate non-deterministic coding/engineering work to a coding agent on a matching worker node in the fleet.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"worker_selector": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"worker_id":        map[string]any{"type": "string"},
							"name":             map[string]any{"type": "string", "description": "Named host match (e.g. 'foo1')"},
							"host_group":       map[string]any{"type": "string"},
							"os":               map[string]any{"type": "string", "description": "OS constraint (darwin/macos, linux, windows)"},
							"virtualization":   map[string]any{"type": "string", "description": "bare_metal, wsl, container, mac_container, vm"},
							"required_tools":   map[string]any{"type": "object", "description": "Map of required tool -> minimum version (e.g. {\"postgres\":\"18\"})"},
							"required_auth":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tools that must be logged in/ready (e.g. [\"gcloud\"])"},
							"min_ram_mb":       map[string]any{"type": "integer"},
							"min_disk_free_gb": map[string]any{"type": "integer"},
							"tags":             map[string]any{"type": "object"},
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
			Name:        "fan_out_fleet",
			Description: "Execute a parallel operation across all matching online machines (e.g. cleaning up temp worktrees for an epic across the fleet), deterministically short-circuiting no-op hosts at zero agent token cost.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind":      map[string]any{"type": "string", "enum": []string{"worktree_cleanup", "agent_sweep"}},
					"query":     map[string]any{"type": "string", "description": "Substring filter (e.g. 'epic-123')"},
					"root_path": map[string]any{"type": "string"},
					"apply":     map[string]any{"type": "boolean", "description": "For worktree_cleanup: delete removable matches when true (default dry-run)"},
					"precheck":  map[string]any{"type": "boolean", "description": "For agent_sweep: skip machines with 0 deterministic matches"},
					"agent":     map[string]any{"type": "string"},
					"prompt":    map[string]any{"type": "string"},
				},
				"required": []string{"kind"},
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
		{
			Name:        "set_narration",
			Description: setNarrationDescription,
			Parameters:  setNarrationToolDef,
		},
		{
			Name:        "recent_events",
			Description: recentEventsDescription,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"machine":       map[string]any{"type": "string", "description": "Only events on this machine (id, sid or name)"},
					"task_id":       map[string]any{"type": "string"},
					"since_minutes": map[string]any{"type": "integer", "description": "How far back (default 120)"},
					"limit":         map[string]any{"type": "integer"},
					"all":           map[string]any{"type": "boolean", "description": "Include routine status changes"},
				},
			},
		},
	}
}

func (s *FleetService) ExecuteTool(ctx context.Context, name string, args jsontext.Value) (any, error) {
	// Tool calls made by the Orchestrator LLM are audited as "orchestrator".
	ctx = withDefaultAuditActor(ctx, auditSourceOrchestrator, auditSourceOrchestrator)
	switch name {
	case "fleet_status":
		return s.FleetStatus(ctx)

	case "inspect_machine_facts":
		var req struct {
			WorkerID string `json:"worker_id"`
		}
		_ = json.Unmarshal(args, &req)
		fs, err := s.FleetStatus(ctx)
		if err != nil {
			return nil, err
		}
		type factRow struct {
			WorkerID   string                 `json:"worker_id"`
			WorkerSID  string                 `json:"worker_sid"`
			WorkerName string                 `json:"worker_name"`
			Status     string                 `json:"status"`
			Facts      *protocol.MachineFacts `json:"facts,omitzero"`
		}
		var out []factRow
		for _, w := range fs.Workers {
			if req.WorkerID != "" && w.ID != req.WorkerID && w.SID != req.WorkerID && !strings.EqualFold(w.Name, req.WorkerID) {
				continue
			}
			out = append(out, factRow{WorkerID: w.ID, WorkerSID: w.SID, WorkerName: w.Name, Status: w.Status, Facts: w.Facts})
		}
		return map[string]any{"machines": out}, nil

	case "inspect_git_status":
		var req struct {
			WorkerID string `json:"worker_id"`
			Workdir  string `json:"workdir"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, fmt.Errorf("invalid inspect_git_status args: %w", err)
		}
		rpcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		env, err := s.hub.RequestWorker(rpcCtx, req.WorkerID, protocol.MsgGitInspect, protocol.GitInspectRequest{Workdir: req.Workdir})
		if err != nil {
			return nil, err
		}
		return protocol.DecodePayload[protocol.GitInspectResult](env)

	case "list_remote_dir":
		var req struct {
			WorkerID string `json:"worker_id"`
			Path     string `json:"path"`
			Prefix   string `json:"prefix"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, fmt.Errorf("invalid list_remote_dir args: %w", err)
		}
		rpcCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		env, err := s.hub.RequestWorker(rpcCtx, req.WorkerID, protocol.MsgFSList, protocol.FSListRequest{
			Path:     req.Path,
			Prefix:   req.Prefix,
			DirsOnly: true,
		})
		if err != nil {
			return nil, err
		}
		return protocol.DecodePayload[protocol.FSListResult](env)

	case "scan_temp_worktrees":
		var req struct {
			WorkerID string `json:"worker_id"`
			RootPath string `json:"root_path"`
			Query    string `json:"query"`
		}
		_ = json.Unmarshal(args, &req)
		return s.ScanTempWorktrees(ctx, req.WorkerID, req.RootPath, req.Query)

	case "cleanup_worktree":
		var req struct {
			WorkerID string `json:"worker_id"`
			Workdir  string `json:"workdir"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, fmt.Errorf("invalid cleanup_worktree args: %w", err)
		}
		rpcCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		env, err := s.hub.RequestWorker(rpcCtx, req.WorkerID, protocol.MsgGitWorktreeCleanup, protocol.GitWorktreeCleanupRequest{Workdir: req.Workdir})
		if err != nil {
			return nil, err
		}
		return protocol.DecodePayload[protocol.GitWorktreeCleanupResult](env)

	case "save_memory":
		var req struct {
			Scope   string `json:"scope"`
			Key     string `json:"key"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(args, &req); err != nil || strings.TrimSpace(req.Key) == "" || strings.TrimSpace(req.Summary) == "" {
			return nil, fmt.Errorf("key and summary are required")
		}
		if strings.TrimSpace(req.Scope) == "" {
			req.Scope = "fleet"
		}
		entry := store.MemoryEntry{
			Scope:   strings.TrimSpace(req.Scope),
			Key:     strings.TrimSpace(req.Key),
			Summary: strings.TrimSpace(req.Summary),
		}
		if err := s.store.UpsertMemory(ctx, entry); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "scope": entry.Scope, "key": entry.Key}, nil

	case "recall_memory":
		var req struct {
			Scope string `json:"scope"`
			Limit int    `json:"limit"`
		}
		_ = json.Unmarshal(args, &req)
		items, err := s.store.ListMemory(ctx, req.Scope, req.Limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"entries": items}, nil

	case "fan_out_fleet":
		var req FanOutRequest
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, fmt.Errorf("invalid fan_out_fleet args: %w", err)
		}
		return s.FanOutFleet(ctx, req)

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

	case "recent_events":
		var req RecentEventsInput
		_ = json.Unmarshal(args, &req)
		return s.RecentEvents(ctx, req)

	case "set_narration":
		in, err := decodeSetNarration(args)
		if err != nil {
			return nil, err
		}
		return s.SetNarration(ctx, in)

	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}
