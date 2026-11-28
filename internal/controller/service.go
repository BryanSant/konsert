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

	"github.com/BryanSant/syre/internal/capacity"
	"github.com/BryanSant/syre/internal/commander"
	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// FleetService is the shared application service used by REST API, MCP Server,
// and the Built-in Commander Chat.
type FleetService struct {
	store store.Store
	hub   *Hub
}

func NewFleetService(st store.Store, hub *Hub) *FleetService {
	return &FleetService{store: st, hub: hub}
}

type WorkerStatusView struct {
	ID   string `json:"id"`
	SID  string `json:"sid"`
	Name string `json:"name"`
	// Labels are the user's operator labels (FleetStatus only; "machine_labels" updates
	// carry changes, so a worker update without them keeps the ones the page has).
	Labels         []string                `json:"labels,omitempty"`
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
	// Version is the worker's syred build version ("" when unknown).
	Version string `json:"version,omitempty"`
	// Launch is how the hub (re)starts this machine's syred ("", "local", "ssh");
	// LaunchState/LaunchError report the last start attempt ("starting", "failed").
	Launch string `json:"launch,omitempty"`
	// Transport is how the machine reaches the hub: "ssh" (the hub attaches over SSH),
	// "local" (the hub's own machine, over syred's socket) or "outbound" (syred
	// dials the hub with its machine credential).
	Transport   string     `json:"transport"`
	SSHTarget   string     `json:"ssh_target,omitempty"`
	LaunchState string     `json:"launch_state,omitempty"`
	LaunchError string     `json:"launch_error,omitempty"`
	LaunchAt    *time.Time `json:"launch_at,omitzero"`
	// Restart is the outage of a hub-started daemon the hub cannot attach to: the host
	// is unreachable, or syred is down and being restarted (nil while connected).
	Restart *WorkerRestartView `json:"restart,omitzero"`
	// WakeLock is the host Keep Awake state (nil until reported or requested).
	WakeLock *WorkerWakeLockView `json:"wake_lock,omitzero"`
	// Heat is the machine's smoothed load score (nil before its first heartbeat).
	Heat *WorkerHeat `json:"heat,omitzero"`
	// Drained machines finish their current work and accept nothing new.
	Drained   bool       `json:"drained"`
	DrainedAt *time.Time `json:"drained_at,omitzero"`
	// DefaultShell is the operator's preferred terminal shell ID for this machine
	// ("" = worker default).
	DefaultShell string `json:"default_shell,omitempty"`
	// AgentsUseGitBash, on Windows machines with Git Bash installed, configures
	// supported agent drivers to use Git Bash for their shell tool (default true).
	AgentsUseGitBash bool `json:"agents_use_git_bash"`
	// Spool is the machine's outbound spool as it last reported it (size, oldest unsent
	// record, replay progress, full); nil before its first report.
	Spool *protocol.SpoolStatus `json:"spool,omitzero"`
	// ReconnectingAfterSleep is set while the hub, just woken from sleep, waits for this
	// machine to re-attach (hub_wake.go).
	ReconnectingAfterSleep bool `json:"reconnecting_after_sleep,omitzero"`
	// Policy is the machine policy the user set (nil: none). Read-only for MCP.
	Policy *protocol.MachinePolicy `json:"policy,omitzero"`
	// PolicyRefusing says why the machine's policy refuses new work right now (control-
	// only, quiet hours, sessions, load, RAM floor); absent when it takes work (SYR-V4RER).
	PolicyRefusing *PolicyRefusal `json:"policy_refusing,omitzero"`
	// Headroom is the machine's estimated room for new work (online machines with a
	// heartbeat; SYR-0AANJ).
	Headroom *capacity.Headroom `json:"headroom,omitzero"`
}

// toWorkerView builds the API/WebSocket view of a worker row with its live
// active-task count and latest metrics. Facts are decoded from the cached JSON.
func toWorkerView(w store.Worker, active int, metrics *protocol.WorkerMetrics) WorkerStatusView {
	facts := decodeWorkerFacts(w.FactsJSON)
	return WorkerStatusView{
		Policy:           workerPolicy(w),
		PolicyRefusing:   policyRefusal(workerPolicy(w), active, metrics, facts, time.Now()),
		ID:               w.ID,
		SID:              w.SID,
		Name:             w.Name,
		HostGroup:        w.HostGroup,
		OS:               w.OS,
		Arch:             w.Arch,
		Isolation:        w.Isolation,
		PermissionMode:   string(w.PermissionMode),
		Agents:           w.Agents,
		Tags:             w.Tags,
		MaxConcurrent:    w.MaxConcurrent,
		Status:           w.Status,
		ActiveTasks:      active,
		LastSeenAt:       w.LastSeenAt,
		Metrics:          metrics,
		Facts:            facts,
		Version:          w.Version,
		Launch:           w.Launch,
		Transport:        transportFor(w.Launch),
		SSHTarget:        w.SSHTarget,
		LaunchState:      w.LaunchState,
		LaunchError:      w.LaunchError,
		LaunchAt:         w.LaunchAt,
		Drained:          w.Drained,
		DrainedAt:        w.DrainedAt,
		DefaultShell:     w.DefaultShell,
		AgentsUseGitBash: w.AgentsUseGitBash,
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
	labels, _ := s.store.ListMachineLabels(ctx)
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
		view.Labels = labels[w.ID]
		view.Status = st
		view.WakeLock = s.hub.workerWakeLockView(w.ID)
		s.hub.decorateSpool(&view)
		view.Restart = s.hub.workerRestartView(w.ID)
		if st == "online" {
			view.Heat = s.hub.workerHeat(w.ID)
			view.Headroom = s.hub.workerHeadroom(ctx, w)
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
	ProjectID      string         `json:"project_id,omitempty"`
	RepoID         string         `json:"repo_id,omitempty"`
	TicketID       string         `json:"ticket_id,omitempty"`
	SessionKind    string         `json:"session_kind,omitempty"` // "chat" | "terminal"
	// Title is a short name for the task shown in the live work views (default: the
	// prompt's first line).
	Title       string `json:"title,omitempty" jsonschema:"Short name for the task (max 80 chars), e.g. 'SYR-2Q0CH: questions card on mobile'"`
	Agent       string `json:"agent"`
	Model       string `json:"model,omitempty"`
	RepoPath    string `json:"repo_path"`
	Branch      string `json:"branch,omitempty"`
	UseWorktree *bool  `json:"use_worktree,omitzero"`
	// CreateWorkdir creates a missing home-relative RepoPath on the machine the work
	// lands on (the fleet workspace folder; SYR-W3SVZ).
	CreateWorkdir  bool   `json:"create_workdir,omitzero"`
	Prompt         string `json:"prompt"`
	Mode           string `json:"mode,omitempty"`
	Origin         string `json:"origin,omitempty"` // "commander" | "direct" | "scratch"
	PermissionMode string `json:"permission_mode,omitempty"`
	OrphanPolicy   string `json:"orphan_policy,omitempty"`
	TimeoutSeconds *int   `json:"timeout_seconds,omitzero"`
	// AccountID selects the worker-local agent account ("" = the agent's default);
	// ApprovalMode and Effort are the agent's own approval mode and effort IDs.
	AccountID    string `json:"account_id,omitempty" jsonschema:"Agent account on the worker (see GET /api/agents/accounts); empty = the agent's default account"`
	ApprovalMode string `json:"approval_mode,omitempty" jsonschema:"The agent's approval mode ID (from its capabilities); empty = the account default"`
	Effort       string `json:"effort,omitempty" jsonschema:"Reasoning effort ID of the model (from capabilities); empty = the model default"`
	// Shell selects the terminal shell ID (e.g. "git-bash", "pwsh", "powershell",
	// "zsh", "bash", "fish") for terminal sessions; empty = machine default.
	Shell string `json:"shell,omitempty" jsonschema:"Terminal shell ID (git-bash, pwsh, powershell, zsh, bash, fish); empty = machine default"`
	// Kind, WorkKey and Requirements (SYR-2BE8M): the kind of work, appended to the
	// derived work key; the dispatcher's own work key instead; hard requirements on the
	// machine, remembered for the work key and inherited by later work of it.
	Kind         string            `json:"kind,omitempty" jsonschema:"Kind of work (build, test, ml-train, review, …); part of the work key"`
	WorkKey      string            `json:"work_key,omitempty" jsonschema:"Work key override; default project/epic-or-ticket/repo[/kind]"`
	Requirements *WorkRequirements `json:"requirements,omitzero" jsonschema:"Hard requirements on the machine (os, arch, min_ram_gb, min_disk_free_gb, tools, auth, virtualization); remembered for the work key"`
	// DryRun explains where the work would go without dispatching it (SYR-QY2TG);
	// Seed stands in for the task id in the rotation of work with no profile yet.
	DryRun bool   `json:"dry_run,omitzero"`
	Seed   string `json:"seed,omitempty"`
}

type DispatchResult struct {
	TaskID           string `json:"task_id"`
	TaskSID          string `json:"task_sid"`
	WorkerID         string `json:"worker_id,omitempty"`
	ProjectID        string `json:"project_id,omitempty"`
	RepoID           string `json:"repo_id,omitempty"`
	TicketID         string `json:"ticket_id,omitempty"`
	SessionKind      string `json:"session_kind,omitempty"`
	Agent            string `json:"agent,omitempty"`
	Model            string `json:"model,omitempty"`
	Status           string `json:"status"`
	WorkdirEffective string `json:"workdir_effective"`
	// Warnings are non-fatal notes, e.g. the account's usage window is exhausted.
	Warnings []string `json:"warnings,omitempty"`
	// Placement answers a dry run (nothing was dispatched).
	Placement *PlacementExplanation `json:"placement,omitzero"`
}

func (s *FleetService) DispatchTask(ctx context.Context, req DispatchRequest) (DispatchResult, error) {
	if req.DryRun {
		ctx = withDryRun(ctx)
	}
	resolveRefs(ctx, s.store, map[ids.Kind][]*string{
		ids.Machine: {&req.WorkerSelector.WorkerID},
		ids.Project: {&req.ProjectID},
		ids.Repo:    {&req.RepoID},
		ids.Ticket:  {&req.TicketID},
	})
	if ticketID := strings.TrimSpace(req.TicketID); ticketID != "" {
		if tk, err := s.store.GetTicket(ctx, ticketID); err == nil {
			if req.ProjectID == "" {
				req.ProjectID = tk.ProjectID
			}
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
	}
	if projID := strings.TrimSpace(req.ProjectID); projID != "" {
		if _, err := s.store.GetProject(ctx, projID); err != nil {
			return DispatchResult{}, fmt.Errorf("project %s not found: %w", projID, err)
		}
	}
	if strings.TrimSpace(req.ProjectID) == "" {
		req.ProjectID = store.DefaultProjectID
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
		origin = protocol.TaskOriginCommander
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
	var earlier []Rejection // machines ruled out before scoring, for the placement log
	for _, w := range workers {
		if w.Status != "online" || !s.hub.IsWorkerConnected(w.ID) {
			earlier = append(earlier, Rejection{WorkerID: w.ID, WorkerName: w.Name, Reason: "offline"})
			continue
		}
		if w.Drained {
			// A drained machine accepts nothing new: work pinned to it waits in the queue
			// until the user undrains it.
			earlier = append(earlier, Rejection{WorkerID: w.ID, WorkerName: w.Name, Reason: "drained"})
			continue
		}
		if isAutomatic(cause) {
			hold, held, ov := s.hub.holdVerdict(ctx, holdCheck{workerID: w.ID, text: req.Prompt})
			if held {
				if w.ID == pinnedID {
					return DispatchResult{}, holdError(hold, w.Name)
				}
				earlier = append(earlier, Rejection{WorkerID: w.ID, WorkerName: w.Name, Reason: "held"})
				continue
			}
			lifted[w.ID] = ov
		}
		onlineWorkers = append(onlineWorkers, w)
	}

	// A Commander chat sends work only to its machines (SYR-FJA2T), and a ticket's work
	// only where its Run on allows (SYR-0M4QQ); from a chat about a ticket, both hold. A
	// machine named outside them is refused, and unpinned work is placed inside them.
	{
		var dropped []Rejection
		var err error
		onlineWorkers, dropped, err = applyScopes(workers, onlineWorkers, req.WorkerSelector,
			s.hub.chatScope(ctx), s.hub.ticketScope(ctx, cmpOr(strings.TrimSpace(req.TicketID), detectTicket(req.Title, req.Prompt))))
		if err != nil {
			return DispatchResult{}, err
		}
		earlier = append(earlier, dropped...)
	}

	counts := s.hub.ActiveTaskCounts()
	// Approved machine_slots overrides raise a machine's slot limit while they last.
	// Directives then tighten: pause and drain drop the machine, throttle caps it.
	onlineWorkers, slots := s.hub.applySlotOverrides(ctx, onlineWorkers)
	beforeDirectives := onlineWorkers
	onlineWorkers = s.hub.applyMachineDirectives(ctx, onlineWorkers)
	earlier = append(earlier, droppedBy(beforeDirectives, onlineWorkers, "a directive (pause, drain or throttle)")...)
	// The machine policy refuses work it rules out; work pinned to such a machine
	// says why instead of waiting.
	if pinnedID != "" {
		for _, w := range onlineWorkers {
			if w.ID == pinnedID {
				if v, _ := s.hub.policyViolation(ctx, w, taskWork(req.TicketID), counts[w.ID], "dispatch"); v != nil {
					return DispatchResult{}, &errPolicyRefused{machine: w.Name, v: v}
				}
			}
		}
	}
	var lifts liftUses
	beforePolicy := onlineWorkers
	onlineWorkers, lifts = s.hub.applyMachinePolicy(ctx, onlineWorkers, counts, taskWork(req.TicketID), "dispatch")
	earlier = append(earlier, droppedBy(beforePolicy, onlineWorkers, "its machine policy")...)
	req.AccountID = strings.TrimSpace(req.AccountID)
	requestedAgent, requestedAccount := req.Agent, req.AccountID
	var placement providerDecision
	if req.Agent != "shell" && sessionKind != "terminal" {
		var idx providerIndex
		placement, idx = s.hub.applyProviderPlacement(ctx, req.Agent, req.AccountID, false, onlineWorkers)
		req.Agent = placement.Agent
		req.AccountID = placement.AccountID
		beforeProvider := onlineWorkers
		onlineWorkers = filterProviderWorkers(onlineWorkers, req.Agent, idx)
		earlier = append(earlier, droppedBy(beforeProvider, onlineWorkers, "the provider policy for "+req.Agent)...)
	}
	kind, kindOK := normalizeWorkKind(req.Kind)
	if !kindOK {
		return DispatchResult{}, fmt.Errorf("kind %q: use letters, digits, - or _ (build, test, ml-train, review, …)", req.Kind)
	}
	ticketID := strings.TrimSpace(req.TicketID)
	if ticketID == "" {
		ticketID = detectTicket(req.Title, req.Prompt)
	}
	workKey := s.hub.taskWorkKey(ctx, "", store.Task{ProjectID: req.ProjectID, RepoID: req.RepoID, Workdir: req.RepoPath, TicketID: ticketID},
		kind, req.WorkKey)
	reqs, inherited := s.hub.workRequirements(ctx, workKey, req.Requirements, describeActor(cause))
	reqs.apply(&req.WorkerSelector)
	peak := s.hub.expectedPeak(ctx, workKey, "")
	scope := s.hub.placementScopeOf(ctx, req.ProjectID, req.RepoID, req.RepoPath, req.Branch, workKey)
	popts := s.hub.placementOpts(ctx, scope, peak, cmpOr(req.Seed, taskID), nil)
	if sessionKind != "terminal" {
		popts = s.hub.withAccounts(ctx, popts, req.Agent, req.AccountID, isAutomatic(cause))
	}
	selected, pout, ok := SelectWorkerScored(onlineWorkers, counts, req.WorkerSelector, req.Agent, permMode, popts)
	// No account asked for: the one the scorer chose there (most quota left).
	if ok && req.AccountID == "" {
		req.AccountID = chosenAccount(pout, selected.ID)
	}
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
			if selected, pout, ok = SelectWorkerScored(eligible, counts, req.WorkerSelector, req.Agent, permMode, popts); !ok {
				return DispatchResult{}, loggedOut
			}
			if requestedAccount == "" {
				req.AccountID = chosenAccount(pout, selected.ID)
			}
		}
	}
	var warnings []string
	if placement.Substitution != "" {
		warnings = append(warnings, placement.Substitution)
	}
	if ok && req.Agent == "" && sessionKind != "terminal" {
		idx, _ := s.hub.loadProviderIndex(ctx)
		req.Agent = pickAllowedAgent(selected, idx)
		if req.Agent == "" {
			ok = false
			selected = store.Worker{}
		}
	}
	if !ok {
		// Leave the preference on the queued task. A later pass substitutes.
		req.Agent = requestedAgent
		req.AccountID = requestedAccount
		placement = providerDecision{}
		warnings = nil
	}
	if ok {
		if a, found := s.hub.lookupAccount(ctx, selected.ID, req.Agent, req.AccountID); found {
			if u, exhausted := exhaustedWindow(a, time.Now()); exhausted {
				warnings = append(warnings, exhaustedWarning(a, u))
			}
		}
	}

	if inherited {
		warnings = append(warnings, "requirements inherited from work key "+workKey)
	}
	pout.Rejected = append(earlier, pout.Rejected...)
	if req.DryRun {
		ex := &PlacementExplanation{WorkKey: workKey, Detail: PlacementDetail{PlacementOutcome: pout, PeakMB: peak, Known: peak > 0}}
		if ok {
			ex.WorkerID, ex.WorkerName = selected.ID, selected.Name
		}
		return DispatchResult{Status: "dry_run", WorkerID: ex.WorkerID, Agent: req.Agent, Warnings: warnings, Placement: ex}, nil
	}
	task := store.Task{
		ID:             taskID,
		SID:            taskSID,
		ProjectID:      req.ProjectID,
		RepoID:         req.RepoID,
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
		TicketID:       ticketID,
		WorkKey:        workKey,
		WorkKind:       kind,
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
	s.hub.logPlacement(ctx, taskSubject(task.ID), workKey, deciderOf(cause), task.WorkerID, pout, placementWorkInfo{peak: peak})
	if placement.Substitution != "" {
		workerID := ""
		if ok {
			workerID = selected.ID
		}
		s.hub.recordAttemptSubstitution(ctx, task.ID, task.TicketID, workerID, req.Agent, req.AccountID, placement.Substitution, 0)
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

	shell := strings.TrimSpace(req.Shell)
	if shell != "" && !protocol.ValidTerminalShellID(shell) {
		return DispatchResult{}, fmt.Errorf("unknown terminal shell %q", shell)
	}
	if shell == "" && ok && (req.Agent == "shell" || sessionKind == "terminal") {
		shell = selected.DefaultShell
	}
	useGitBash := false
	if ok && req.Agent != "shell" && sessionKind != "terminal" {
		useGitBash = selected.AgentsUseGitBash
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
		CreateWorkdir:  req.CreateWorkdir,
		Prompt:         req.Prompt,
		Mode:           mode,
		PermissionMode: string(permMode),
		TimeoutSeconds: timeoutSecs,
		AccountID:      req.AccountID,
		ApprovalMode:   strings.TrimSpace(req.ApprovalMode),
		Effort:         strings.TrimSpace(req.Effort),
		Shell:          shell,
		UseGitBash:     useGitBash,
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
			s.hub.liftUsed(ctx, lifts, selected.ID)
		}
	}

	s.hub.broadcastFleetUpdate("task", task)
	s.hub.resolveTaskTicket(task.ID, task.TicketID)

	return DispatchResult{
		TaskID:           task.ID,
		TaskSID:          task.SID,
		WorkerID:         task.WorkerID,
		ProjectID:        task.ProjectID,
		RepoID:           task.RepoID,
		TicketID:         task.TicketID,
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
	Digest           *protocol.RunDigest  `json:"digest,omitzero"`
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

	var runDigest *protocol.RunDigest
	summary := t.Summary
	if t.DigestJSON != "" {
		var d protocol.RunDigest
		if err := json.Unmarshal([]byte(t.DigestJSON), &d, protocol.JSONOptions); err == nil {
			runDigest = &d
			if text := d.Text(); text != "" {
				summary = text
			}
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
		Summary:          summary,
		Digest:           runDigest,
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
	return s.hub.cancelTaskWithGrace(ctx, resolveRef(ctx, s.store, ids.Task, taskID), "", protocol.DefaultStopGraceS)
}

func (s *FleetService) CancelTaskWithGrace(ctx context.Context, taskID string, graceS int) (ActionResult, error) {
	return s.hub.cancelTaskWithGrace(ctx, resolveRef(ctx, s.store, ids.Task, taskID), "", graceS)
}

// cancelTask cancels a task with the cause in ctx using the default grace period.
func (h *Hub) cancelTask(ctx context.Context, taskID, note string) (ActionResult, error) {
	return h.cancelTaskWithGrace(ctx, taskID, note, protocol.DefaultStopGraceS)
}

// cancelTaskWithGrace cancels a task with the cause in ctx and a normalized grace period.
// note (a veto reason) is added to the audit message. A cancel by the user that no agent
// asked for places a hold on the session so the commander doesn't simply restart it.
func (h *Hub) cancelTaskWithGrace(ctx context.Context, taskID, note string, graceS int) (ActionResult, error) {
	graceS = protocol.NormalizeStopGraceS(graceS)
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
			MetaJSON: auditMeta(map[string]any{"prior_status": t.Status, "grace_s": graceS}),
		})
	}
	defer h.beginDeliveryBatch()()
	h.recordIntervention(ctx, cause, "cancel", t.WorkerID, t.ID, t.ParentChatID,
		fmt.Sprintf("%s canceled task %s (%s) on %s (grace %ds)", capitalize(describeActor(cause)), t.SID, t.Agent, h.workerLabel(ctx, t.WorkerID), graceS))
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
	if !h.SendCancelWithGrace(taskID, graceS) {
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

// --- commander.ContextProvider & daemon-first RPC helpers ---

// BuildCompactContext produces a token-efficient (<~400 token) live snapshot of fleet facts,
// taxonomy, active sessions, and persisted SQLite memory for the Commander system prompt.
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

	if projects, err := s.store.ListProjects(ctx); err == nil && len(projects) > 0 {
		var pNames []string
		for _, p := range projects {
			pNames = append(pNames, p.Name)
		}
		repos, _ := s.store.ListRepos(ctx, "")
		b.WriteString(fmt.Sprintf("Projects: %d (%s), %d repos\n",
			len(projects), strings.Join(pNames, ", "), len(repos)))
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

// ScanTempWorktrees deterministically scans one or all online machines via syred RPC (zero agent LLM calls).
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

// --- commander.ToolExecutor implementation ---

func (s *FleetService) ToolDefinitions() []commander.ToolDef {
	return []commander.ToolDef{
		{Name: "fleet_status", Description: fleetStatusDesc, Parameters: fleetStatusSchema},
		{Name: "backlog", Description: backlogDesc, Parameters: backlogSchema},
		{Name: "manage_work", Description: manageWorkDesc, Parameters: manageWorkSchema},
		{Name: "control", Description: controlDesc, Parameters: controlSchema},
		{Name: "set_policy", Description: setPolicyDesc, Parameters: setPolicySchema},
		{Name: "reply", Description: replyDesc, Parameters: replySchema},
		{Name: "inspect", Description: inspectToolDescription, Parameters: inspectSchema},
		{Name: "memory", Description: memoryDesc, Parameters: memorySchema},
	}
}

func (s *FleetService) ExecuteTool(ctx context.Context, name string, args jsontext.Value) (any, error) {
	// Tool calls made by the Commander LLM are audited as "commander".
	ctx = withDefaultAuditActor(ctx, auditSourceCommander, auditSourceCommander)
	switch name {
	case "fleet_status":
		var in FleetStatusInput
		if len(args) > 0 {
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, fmt.Errorf("invalid fleet_status args: %w", err)
			}
		}
		return s.FleetStatusView(ctx, in)

	case "backlog":
		var in BacklogInput
		if len(args) > 0 {
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, fmt.Errorf("invalid backlog args: %w", err)
			}
		}
		return s.Backlog(ctx, in)

	case "manage_work":
		var in ManageWorkInput
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid manage_work args: %w", err)
		}
		return s.ManageWork(ctx, in)

	case "control":
		var in ControlInput
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid control args: %w", err)
		}
		return s.Control(ctx, in, nil)

	case "set_policy":
		var in SetPolicyInput
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid set_policy args: %w", err)
		}
		return s.SetPolicy(ctx, in)

	case "reply":
		var in ReplyInput
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid reply args: %w", err)
		}
		return s.Reply(ctx, in, nil)

	case "inspect":
		var req InspectRequest
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, fmt.Errorf("invalid inspect args: %w", err)
		}
		return s.Inspect(ctx, req)

	case "memory":
		var in MemoryInput
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid memory args: %w", err)
		}
		return s.Memory(ctx, in)

	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}
