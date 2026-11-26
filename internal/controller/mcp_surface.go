package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- Inputs & Outputs for the consolidated Commander + /mcp surface ---

type FleetStatusInput struct {
	Detail  string `json:"detail,omitempty"`
	Machine string `json:"machine,omitempty"`
}

type FleetSummaryResult struct {
	Online      int      `json:"online"`
	Total       int      `json:"total"`
	ActiveTasks int      `json:"active_tasks"`
	Paused      bool     `json:"paused,omitzero"`
	Autopilot   string   `json:"autopilot,omitempty"`
	Groups      []string `json:"groups"`
	Directives  []string `json:"directives,omitempty"`
	Holds       []string `json:"holds,omitempty"`
}

type MachineDetailRow struct {
	WorkerID      string                 `json:"worker_id"`
	WorkerSID     string                 `json:"worker_sid"`
	WorkerName    string                 `json:"worker_name"`
	Status        string                 `json:"status"`
	OS            string                 `json:"os"`
	Arch          string                 `json:"arch"`
	Agents        []string               `json:"agents,omitempty"`
	ActiveTasks   int                    `json:"active_tasks"`
	MaxConcurrent int                    `json:"max_concurrent"`
	Drained       bool                   `json:"drained,omitzero"`
	Facts         *protocol.MachineFacts `json:"facts,omitzero"`
}

type BacklogInput struct {
	Project string `json:"project,omitempty"`
	Parent  string `json:"parent,omitempty"`
	Status  string `json:"status,omitempty"`
	Limit   int    `json:"limit,omitzero"`
}

type BacklogProjectRow struct {
	SID       string   `json:"sid"`
	Name      string   `json:"name"`
	Autopilot bool     `json:"autopilot,omitzero"`
	Repos     []string `json:"repos,omitempty"`
}

type BacklogTicketRow struct {
	SID      string   `json:"sid"`
	ID       string   `json:"id"`
	Project  string   `json:"project,omitempty"`
	Parent   string   `json:"parent,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	Title    string   `json:"title"`
	Status   string   `json:"status"`
	Priority string   `json:"priority,omitempty"`
	Lease    string   `json:"lease,omitempty"`
	Blockers []string `json:"blockers,omitempty"`
}

type BacklogTaskRow struct {
	SID       string `json:"sid"`
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Status    string `json:"status"`
	Machine   string `json:"machine,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Milestone string `json:"milestone,omitempty"`
}

type BacklogResult struct {
	Projects []BacklogProjectRow `json:"projects,omitempty"`
	Tickets  []BacklogTicketRow  `json:"tickets"`
	Tasks    []BacklogTaskRow    `json:"tasks,omitempty"`
}

type ManageWorkInput struct {
	Action string          `json:"action"`
	Items  []WorkItemInput `json:"items"`
}

type WorkItemInput struct {
	ID            string             `json:"id,omitempty"`
	Project       string             `json:"project,omitempty"`
	Parent        string             `json:"parent,omitempty"`
	Kind          string             `json:"kind,omitempty"`
	Title         string             `json:"title,omitempty"`
	Body          string             `json:"body,omitempty"`
	Status        string             `json:"status,omitempty"`
	Priority      string             `json:"priority,omitempty"`
	Acceptance    []string           `json:"acceptance,omitempty"`
	Labels        []string           `json:"labels,omitempty"`
	BlockedBy     []string           `json:"blocked_by,omitempty"`
	Repos         []string           `json:"repos,omitempty"`
	VerifyCmd     string             `json:"verify_cmd,omitempty"`
	Hints         *store.TicketHints `json:"hints,omitempty"`
	Note          string             `json:"note,omitempty"`
	Agent         string             `json:"agent,omitempty"`
	Model         string             `json:"model,omitempty"`
	Machine       string             `json:"machine,omitempty"`
	Repo          string             `json:"repo,omitempty"`
	Prompt        string             `json:"prompt,omitempty"`
	Mode          string             `json:"mode,omitempty"`
	RemoteURL     string             `json:"remote_url,omitempty"`
	DefaultBranch string             `json:"default_branch,omitempty"`
	DeliveryMode  string             `json:"delivery_mode,omitempty"`
}

type ManageWorkResult struct {
	Action  string `json:"action"`
	Results []any  `json:"results"`
}

type ControlInput struct {
	Scope  string `json:"scope"`
	Target string `json:"target,omitempty"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	GraceS int    `json:"grace_s,omitzero"`
}

type ReplyInput struct {
	Target  string `json:"target"`
	Message string `json:"message"`
	Resume  bool   `json:"resume,omitzero"`
}

type MemoryInput struct {
	Action  string `json:"action"`
	Scope   string `json:"scope,omitempty"`
	Key     string `json:"key,omitempty"`
	Summary string `json:"summary,omitempty"`
	Limit   int    `json:"limit,omitzero"`
}

// SurfaceBackend is the consolidated Commander + /mcp tool backend.
type SurfaceBackend interface {
	FleetStatusView(ctx context.Context, in FleetStatusInput) (any, error)
	Backlog(ctx context.Context, in BacklogInput) (BacklogResult, error)
	ManageWork(ctx context.Context, in ManageWorkInput) (ManageWorkResult, error)
	Control(ctx context.Context, in ControlInput, confirm confirmFunc) (any, error)
	Reply(ctx context.Context, in ReplyInput, confirm confirmFunc) (any, error)
	Memory(ctx context.Context, in MemoryInput) (any, error)
}

// --- Flat, compact JSON Schemas (shared by MCP and API-key Commander) ---

var (
	fleetStatusSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"detail":  map[string]any{"type": "string", "enum": []any{"summary", "machines"}},
			"machine": map[string]any{"type": "string", "description": "Machine id, sid or name"},
		},
	}

	backlogSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{"type": "string"},
			"parent":  map[string]any{"type": "string"},
			"status":  map[string]any{"type": "string"},
			"limit":   map[string]any{"type": "integer"},
		},
	}

	manageWorkSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []any{"create", "update", "cancel", "requeue", "dispatch", "create_project", "add_repo"},
			},
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":             map[string]any{"type": "string"},
						"project":        map[string]any{"type": "string"},
						"parent":         map[string]any{"type": "string"},
						"kind":           map[string]any{"type": "string"},
						"title":          map[string]any{"type": "string"},
						"body":           map[string]any{"type": "string"},
						"status":         map[string]any{"type": "string"},
						"priority":       map[string]any{"type": "string"},
						"acceptance":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"labels":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"blocked_by":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"repos":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"verify_cmd":     map[string]any{"type": "string"},
						"hints":          map[string]any{"type": "object"},
						"note":           map[string]any{"type": "string"},
						"agent":          map[string]any{"type": "string"},
						"model":          map[string]any{"type": "string"},
						"machine":        map[string]any{"type": "string"},
						"repo":           map[string]any{"type": "string"},
						"prompt":         map[string]any{"type": "string"},
						"mode":           map[string]any{"type": "string", "enum": []any{"oneshot", "interactive"}},
						"remote_url":     map[string]any{"type": "string"},
						"default_branch": map[string]any{"type": "string"},
						"delivery_mode":  map[string]any{"type": "string"},
					},
				},
			},
		},
		"required": []any{"action", "items"},
	}

	controlSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scope":   map[string]any{"type": "string", "enum": []any{"ticket", "session", "machine", "project", "fleet"}},
			"target":  map[string]any{"type": "string"},
			"action":  map[string]any{"type": "string", "enum": []any{"pause", "resume", "stop", "drain", "undrain", "suspend", "veto", "autopilot_play", "autopilot_pause"}},
			"reason":  map[string]any{"type": "string"},
			"grace_s": map[string]any{"type": "integer"},
		},
		"required": []any{"scope", "action"},
	}

	replySchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target":  map[string]any{"type": "string", "description": "Task, chat, ticket, or delegated question id/sid"},
			"message": map[string]any{"type": "string"},
			"resume":  map[string]any{"type": "boolean"},
		},
		"required": []any{"target", "message"},
	}

	inspectSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target":    map[string]any{"type": "string"},
			"kind":      map[string]any{"type": "string", "enum": []any{"digest", "diff_stat", "diff", "stderr", "log", "transcript", "file", "result", "events", "audit"}},
			"repo":      map[string]any{"type": "string"},
			"path":      map[string]any{"type": "string"},
			"max_lines": map[string]any{"type": "integer"},
			"offset":    map[string]any{"type": "integer"},
		},
		"required": []any{"kind"},
	}

	memorySchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":  map[string]any{"type": "string", "enum": []any{"save", "recall"}},
			"scope":   map[string]any{"type": "string"},
			"key":     map[string]any{"type": "string"},
			"summary": map[string]any{"type": "string"},
			"limit":   map[string]any{"type": "integer"},
		},
		"required": []any{"action"},
	}

	setPolicySchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target":      map[string]any{"type": "string"},
			"directive":   map[string]any{"type": "string", "enum": []any{"machine.pause", "machine.throttle", "machine.drain", "provider.disable", "autopilot.pause", "end"}},
			"until":       map[string]any{"type": "string", "description": "RFC3339 expiry"},
			"id":          map[string]any{"type": "string"},
			"reason":      map[string]any{"type": "string"},
			"constraints": map[string]any{"type": "object"},
		},
		"required": []any{"target", "directive"},
	}
)

const (
	fleetStatusDesc = "Fleet summary grouped by capability (default) with directives and holds, or per-machine facts when detail=machines or machine is set."
	backlogDesc     = "Compact table of projects/repos, tickets (sid, title, status, lease, blockers), and ad-hoc tasks."
	manageWorkDesc  = "Batch create/update/cancel/requeue tickets (incl. blocked_by, repos, hints), dispatch ad-hoc tasks, or create projects/repos."
	controlDesc     = "Pause, resume, stop, drain, undrain, suspend, veto, or toggle autopilot at ticket, session, machine, project, or fleet scope. Resume/undrain require user confirmation."
	replyDesc       = "Steer a running task/session or answer a delegated question (only via answer_question rules)."
	memoryDesc      = "Save or recall concise fleet/project/session memory entries."
	setPolicyDesc   = "Set a tightening policy directive (machine.pause, machine.throttle, machine.drain, provider.disable, autopilot.pause). Loosening (directive=end or raising caps) requires request_override."
)

// --- FleetService implementation of SurfaceBackend ---

func workerHasGPU(w WorkerStatusView) bool {
	if g := strings.ToLower(strings.TrimSpace(w.Tags["gpu"])); g != "" && g != "false" && g != "0" && g != "no" {
		return true
	}
	if w.Facts != nil {
		for _, k := range []string{"nvidia-smi", "rocm-smi", "cuda"} {
			if strings.TrimSpace(w.Facts.Tools[k]) != "" {
				return true
			}
		}
	}
	return false
}

func workerCapabilityKey(w WorkerStatusView) string {
	osArch := strings.TrimSpace(w.OS)
	if w.Arch != "" {
		osArch += "/" + strings.TrimSpace(w.Arch)
	}
	if osArch == "" {
		osArch = "unknown"
	}
	parts := []string{osArch}
	if w.Facts != nil && w.Facts.RAMTotalMB > 0 {
		gb := (w.Facts.RAMTotalMB + 512) / 1024
		if gb > 0 {
			parts = append(parts, fmt.Sprintf("%dGB", gb))
		}
	}
	if workerHasGPU(w) {
		parts = append(parts, "GPU")
	}
	state := "idle"
	switch {
	case w.Status != "online":
		state = "offline"
	case w.Drained:
		state = "drained"
	case w.ActiveTasks > 0:
		state = "busy"
	}
	parts = append(parts, state)
	return strings.Join(parts, " ")
}

func formatDirectiveLine(d store.Directive) string {
	s := fmt.Sprintf("%s %s %s", d.SID, d.Kind, d.Target)
	if d.Params.MaxConcurrency != nil {
		s += fmt.Sprintf(" max=%d", *d.Params.MaxConcurrency)
	}
	if d.Params.Fallback != "" {
		s += " fallback=" + d.Params.Fallback
	}
	if d.Until != nil {
		s += " until " + d.Until.UTC().Format("15:04Z")
	}
	return s
}

func (s *FleetService) FleetStatusView(ctx context.Context, in FleetStatusInput) (any, error) {
	fs, err := s.FleetStatus(ctx)
	if err != nil {
		return nil, err
	}
	detail := strings.ToLower(strings.TrimSpace(in.Detail))
	machine := strings.TrimSpace(in.Machine)
	if detail == "" {
		if machine != "" {
			detail = "machines"
		} else {
			detail = "summary"
		}
	}
	switch detail {
	case "machines":
		out := make([]MachineDetailRow, 0, len(fs.Workers))
		for _, w := range fs.Workers {
			if machine != "" && w.ID != machine && w.SID != machine && !strings.EqualFold(w.Name, machine) {
				continue
			}
			out = append(out, MachineDetailRow{
				WorkerID:      w.ID,
				WorkerSID:     w.SID,
				WorkerName:    w.Name,
				Status:        w.Status,
				OS:            w.OS,
				Arch:          w.Arch,
				Agents:        w.Agents,
				ActiveTasks:   w.ActiveTasks,
				MaxConcurrent: w.MaxConcurrent,
				Drained:       w.Drained,
				Facts:         w.Facts,
			})
		}
		return map[string]any{"machines": out}, nil
	case "summary":
		counts := map[string]int{}
		online := 0
		for _, w := range fs.Workers {
			if w.Status == "online" {
				online++
			}
			counts[workerCapabilityKey(w)]++
		}
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		groups := make([]string, 0, len(keys))
		for _, k := range keys {
			groups = append(groups, fmt.Sprintf("%dx %s", counts[k], k))
		}

		res := FleetSummaryResult{
			Online:      online,
			Total:       len(fs.Workers),
			ActiveTasks: fs.ActiveTasksCount,
			Paused:      fs.Pause.Paused,
			Groups:      groups,
		}
		if s.hub != nil {
			res.Autopilot = s.hub.AutopilotState(ctx).State
			now := s.hub.directiveClock()
			if dirs, err := s.store.ListDirectives(ctx, store.DirectiveFilter{ActiveOnly: true, Now: now, Limit: 20}); err == nil {
				for _, d := range dirs {
					if d.ActiveAt(now) {
						res.Directives = append(res.Directives, formatDirectiveLine(d))
					}
				}
			}
			if holds, err := s.store.ListHolds(ctx, store.HoldFilter{ActiveOnly: true, Limit: 20}); err == nil {
				for _, hd := range holds {
					if hd.Active() {
						res.Holds = append(res.Holds, fmt.Sprintf("%s %s: %s", hd.SID, s.hub.holdScopeLabel(ctx, hd), oneLine(hd.Summary, 60)))
					}
				}
			}
			// Include exhausted usage windows as usage holds.
			if accts, err := s.hub.agentAccountViews(ctx, ""); err == nil {
				for _, a := range accts {
					if u, exhausted := exhaustedWindow(a.AgentAccount, now); exhausted {
						res.Holds = append(res.Holds, fmt.Sprintf("usage:%s/%s (%s) until %s", a.Agent, a.ID, u.Window, u.ResetsAt))
					}
				}
			}
		}
		return res, nil
	default:
		return nil, fmt.Errorf("invalid detail %q (want summary or machines)", in.Detail)
	}
}

func (s *FleetService) Backlog(ctx context.Context, in BacklogInput) (BacklogResult, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var projID string
	if ref := strings.TrimSpace(in.Project); ref != "" {
		if p, err := s.hub.resolveProject(ctx, ref); err == nil {
			projID = p.ID
		} else {
			projID = resolveRef(ctx, s.store, ids.Project, ref)
		}
	}
	parentID := resolveRef(ctx, s.store, ids.Ticket, strings.TrimSpace(in.Parent))

	projects, _ := s.store.ListProjects(ctx)
	projNames := map[string]string{}
	reposByProj := map[string][]string{}
	if repos, err := s.store.ListRepos(ctx, projID); err == nil {
		for _, r := range repos {
			reposByProj[r.ProjectID] = append(reposByProj[r.ProjectID], r.SID+":"+r.Name)
		}
	}
	out := BacklogResult{Tickets: []BacklogTicketRow{}}
	for _, p := range projects {
		projNames[p.ID] = p.Name
		if projID != "" && p.ID != projID {
			continue
		}
		out.Projects = append(out.Projects, BacklogProjectRow{
			SID:       p.SID,
			Name:      p.Name,
			Autopilot: p.Autopilot,
			Repos:     reposByProj[p.ID],
		})
	}

	tickets, err := s.store.ListTickets(ctx, store.TicketFilter{
		ProjectID: projID,
		ParentID:  parentID,
		Status:    strings.TrimSpace(in.Status),
		Limit:     limit,
	})
	if err != nil {
		return BacklogResult{}, err
	}
	edges, _ := s.store.ListTicketEdgesByProject(ctx, projID)
	ticketSIDByID := map[string]string{}
	if allTickets, err := s.store.ListTickets(ctx, store.TicketFilter{ProjectID: projID, Limit: 500}); err == nil {
		for _, tk := range allTickets {
			ticketSIDByID[tk.ID] = tk.SID
		}
	}
	blockersByTicket := map[string][]string{}
	for _, e := range edges {
		bRef := ticketSIDByID[e.BlockedByID]
		if bRef == "" {
			bRef = e.BlockedByID
		}
		blockersByTicket[e.TicketID] = append(blockersByTicket[e.TicketID], bRef)
	}
	workerNames := map[string]string{}
	if workers, err := s.store.ListWorkers(ctx); err == nil {
		for _, w := range workers {
			workerNames[w.ID] = w.Name
		}
	}

	for _, tk := range tickets {
		row := BacklogTicketRow{
			SID:      tk.SID,
			ID:       tk.ID,
			Project:  projNames[tk.ProjectID],
			Parent:   ticketSIDByID[tk.ParentID],
			Kind:     tk.Kind,
			Title:    tk.Title,
			Status:   tk.Status,
			Priority: tk.Priority,
			Blockers: blockersByTicket[tk.ID],
		}
		if tk.Lease != nil {
			m := workerNames[tk.Lease.WorkerID]
			if m == "" {
				m = tk.Lease.WorkerID
			}
			agent := tk.Hints.Agent
			if tk.Lease.TaskID != "" {
				if t, err := s.store.GetTask(ctx, tk.Lease.TaskID); err == nil && t.Agent != "" {
					agent = t.Agent
				}
			}
			if agent != "" {
				row.Lease = m + "/" + agent
			} else {
				row.Lease = m
			}
		}
		out.Tickets = append(out.Tickets, row)
	}

	// Include ad-hoc tasks (no ticket_id) when not filtering by a specific parent ticket.
	if parentID == "" {
		tasks, err := s.ListTasks(ctx, strings.TrimSpace(in.Status), "", limit)
		if err == nil {
			for _, t := range tasks {
				raw, gErr := s.store.GetTask(ctx, t.TaskID)
				if gErr == nil {
					if raw.TicketID != "" {
						continue
					}
					if projID != "" && raw.ProjectID != projID {
						continue
					}
				}
				m := workerNames[t.WorkerID]
				if m == "" {
					m = t.WorkerSID
				}
				title := ""
				if gErr == nil {
					title = raw.Title
					if title == "" {
						title = oneLine(raw.Prompt, 60)
					}
				}
				out.Tasks = append(out.Tasks, BacklogTaskRow{
					SID:       t.TaskSID,
					ID:        t.TaskID,
					Title:     title,
					Status:    t.Status,
					Machine:   m,
					Agent:     t.Agent,
					Milestone: oneLine(t.LastMilestone, 80),
				})
			}
		}
	}
	return out, nil
}

func (s *FleetService) resolveRepoInProject(ctx context.Context, projectID, ref string) (store.Repo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return store.Repo{}, store.ErrNotFound
	}
	if id, err := s.store.ResolveID(ctx, ids.Repo, ref); err == nil {
		return s.store.GetRepo(ctx, id)
	}
	if r, err := s.store.GetRepo(ctx, ref); err == nil {
		return r, nil
	}
	repos, err := s.store.ListRepos(ctx, projectID)
	if err != nil {
		return store.Repo{}, err
	}
	for _, r := range repos {
		if strings.EqualFold(r.Name, ref) {
			return r, nil
		}
	}
	return store.Repo{}, fmt.Errorf("%w: repo %q", store.ErrNotFound, ref)
}

func (s *FleetService) ManageWork(ctx context.Context, in ManageWorkInput) (ManageWorkResult, error) {
	action := strings.ToLower(strings.TrimSpace(in.Action))
	if len(in.Items) == 0 {
		return ManageWorkResult{}, errors.New("items must be non-empty")
	}
	out := ManageWorkResult{Action: action, Results: make([]any, 0, len(in.Items))}
	// Track tickets created earlier in the same batch by title or SID so blocked_by can reference them.
	batchByRef := map[string]string{}

	resolveBlocker := func(ref string) string {
		ref = strings.TrimSpace(ref)
		if id, ok := batchByRef[ref]; ok {
			return id
		}
		return resolveRef(ctx, s.store, ids.Ticket, ref)
	}

	resolveProjID := func(ref string) (string, error) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return store.DefaultProjectID, nil
		}
		p, err := s.hub.resolveProject(ctx, ref)
		if err != nil {
			return "", err
		}
		return p.ID, nil
	}

	switch action {
	case "create":
		for _, item := range in.Items {
			projID, err := resolveProjID(item.Project)
			if err != nil {
				return out, err
			}
			parentID := ""
			if strings.TrimSpace(item.Parent) != "" {
				parentID = resolveBlocker(item.Parent)
			}
			var acc []store.AcceptanceItem
			for _, line := range item.Acceptance {
				if line = strings.TrimSpace(line); line != "" {
					acc = append(acc, store.AcceptanceItem{Text: line})
				}
			}
			var hints store.TicketHints
			if item.Hints != nil {
				hints = *item.Hints
			}
			tk, err := s.store.CreateTicket(ctx, store.Ticket{
				ProjectID:  projID,
				ParentID:   parentID,
				Kind:       item.Kind,
				Title:      item.Title,
				Body:       item.Body,
				Acceptance: acc,
				Labels:     item.Labels,
				Priority:   item.Priority,
				Status:     item.Status,
				Hints:      hints,
				VerifyCmd:  strings.TrimSpace(item.VerifyCmd),
			})
			if err != nil {
				return out, err
			}
			batchByRef[tk.SID] = tk.ID
			batchByRef[tk.Title] = tk.ID
			for _, b := range item.BlockedBy {
				if err := s.store.AddTicketEdge(ctx, tk.ID, resolveBlocker(b)); err != nil {
					return out, err
				}
			}
			for _, rRef := range item.Repos {
				repo, err := s.resolveRepoInProject(ctx, projID, rRef)
				if err != nil {
					return out, err
				}
				if err := s.store.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repo.ID}); err != nil {
					return out, err
				}
			}
			s.hub.broadcastFleetUpdate("ticket", tk)
			s.hub.pulseTicketDecompositionCheck(ctx, tk)
			if tk.Status == store.TicketStatusReady {
				s.hub.kickScheduler()
			}
			out.Results = append(out.Results, map[string]any{"id": tk.ID, "sid": tk.SID, "title": tk.Title, "status": tk.Status})
		}

	case "update":
		for _, item := range in.Items {
			id := resolveRef(ctx, s.store, ids.Ticket, strings.TrimSpace(item.ID))
			if id == "" {
				return out, errors.New("id is required for update")
			}
			tk, err := s.store.GetTicket(ctx, id)
			if err != nil {
				return out, err
			}
			prevStatus := tk.Status
			if item.Parent != "" {
				tk.ParentID = resolveBlocker(item.Parent)
			}
			if item.Kind != "" {
				tk.Kind = item.Kind
			}
			if item.Title != "" {
				tk.Title = item.Title
			}
			if item.Body != "" {
				tk.Body = item.Body
			}
			if item.Status != "" {
				tk.Status = item.Status
			}
			if item.Priority != "" {
				tk.Priority = item.Priority
			}
			if item.VerifyCmd != "" {
				tk.VerifyCmd = strings.TrimSpace(item.VerifyCmd)
			}
			if item.Hints != nil {
				tk.Hints = *item.Hints
			}
			if len(item.Labels) > 0 {
				tk.Labels = item.Labels
			}
			if len(item.Acceptance) > 0 {
				acc := make([]store.AcceptanceItem, 0, len(item.Acceptance))
				for _, line := range item.Acceptance {
					if line = strings.TrimSpace(line); line != "" {
						acc = append(acc, store.AcceptanceItem{Text: line})
					}
				}
				tk.Acceptance = acc
			}
			updated, err := s.store.UpdateTicket(ctx, tk)
			if err != nil {
				return out, err
			}
			for _, b := range item.BlockedBy {
				if err := s.store.AddTicketEdge(ctx, updated.ID, resolveBlocker(b)); err != nil {
					return out, err
				}
			}
			for _, rRef := range item.Repos {
				repo, err := s.resolveRepoInProject(ctx, updated.ProjectID, rRef)
				if err != nil {
					return out, err
				}
				if err := s.store.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: updated.ID, RepoID: repo.ID}); err != nil {
					return out, err
				}
			}
			s.hub.broadcastFleetUpdate("ticket", updated)
			if updated.Status != prevStatus {
				switch updated.Status {
				case store.TicketStatusReview, store.TicketStatusFailed, store.TicketStatusNeedsYou:
					s.hub.pulseTicketTransition(ctx, updated, nil, nil, "")
				case store.TicketStatusDone, store.TicketStatusCanceled:
					s.hub.closeTicketChats(ctx, updated.ID)
					s.hub.checkProjectBoardIdle(ctx, updated.ProjectID)
				}
			}
			s.hub.pulseTicketDecompositionCheck(ctx, updated)
			if updated.Status == store.TicketStatusReady || updated.Status == store.TicketStatusDone || updated.Status == store.TicketStatusReview {
				s.hub.kickScheduler()
			}
			out.Results = append(out.Results, map[string]any{"id": updated.ID, "sid": updated.SID, "title": updated.Title, "status": updated.Status})
		}

	case "cancel":
		for _, item := range in.Items {
			ref := strings.TrimSpace(item.ID)
			if ref == "" {
				return out, errors.New("id is required for cancel")
			}
			tkID := resolveRef(ctx, s.store, ids.Ticket, ref)
			if _, err := s.store.GetTicket(ctx, tkID); err == nil {
				updated, err := s.hub.CancelTicket(ctx, tkID, item.Note)
				if err != nil {
					return out, err
				}
				out.Results = append(out.Results, map[string]any{"id": updated.ID, "sid": updated.SID, "status": updated.Status})
				continue
			}
			res, err := s.CancelTask(ctx, ref)
			if err != nil {
				return out, err
			}
			out.Results = append(out.Results, res)
		}

	case "requeue":
		for _, item := range in.Items {
			ref := strings.TrimSpace(item.ID)
			if ref == "" {
				return out, errors.New("id is required for requeue")
			}
			tkID := resolveRef(ctx, s.store, ids.Ticket, ref)
			if _, err := s.store.GetTicket(ctx, tkID); err == nil {
				updated, err := s.hub.RequeueTicket(ctx, tkID, item.Note)
				if err != nil {
					return out, err
				}
				out.Results = append(out.Results, map[string]any{"id": updated.ID, "sid": updated.SID, "status": updated.Status})
				continue
			}
			taskID := resolveRef(ctx, s.store, ids.Task, ref)
			t, err := s.store.GetTask(ctx, taskID)
			if err != nil {
				return out, err
			}
			cause := causeFrom(ctx)
			if _, err := s.store.RequeueTask(ctx, t.ID, "", false, cause); err != nil {
				return out, err
			}
			s.hub.kickScheduler()
			out.Results = append(out.Results, ActionResult{OK: true, TaskID: t.ID, TaskSID: t.SID, Status: protocol.TaskStatusQueued})
		}

	case "dispatch":
		for _, item := range in.Items {
			projID, err := resolveProjID(item.Project)
			if err != nil {
				return out, err
			}
			req := DispatchRequest{
				ProjectID: projID,
				Title:     item.Title,
				Agent:     item.Agent,
				Model:     item.Model,
				Prompt:    item.Prompt,
				Mode:      item.Mode,
			}
			if m := strings.TrimSpace(item.Machine); m != "" {
				resolved := s.hub.resolveMachine(ctx, m)
				if _, err := s.store.GetWorker(ctx, resolved); err == nil {
					req.WorkerSelector.WorkerID = resolved
				} else {
					req.WorkerSelector.Name = m
				}
			}
			if r := strings.TrimSpace(item.Repo); r != "" {
				if strings.HasPrefix(r, "/") || strings.HasPrefix(r, "~") || strings.HasPrefix(r, ".") || strings.ContainsAny(r, "/\\") {
					req.RepoPath = r
				} else if repo, rErr := s.resolveRepoInProject(ctx, projID, r); rErr == nil {
					req.RepoID = repo.ID
					req.RepoPath = "~"
				} else {
					req.RepoPath = r
				}
			} else {
				req.RepoPath = "~"
			}
			res, err := s.DispatchTask(ctx, req)
			if err != nil {
				return out, err
			}
			out.Results = append(out.Results, res)
		}

	case "create_project":
		for _, item := range in.Items {
			name := strings.TrimSpace(item.Title)
			if name == "" {
				name = strings.TrimSpace(item.Project)
			}
			if name == "" {
				return out, errors.New("title (project name) is required for create_project")
			}
			deliveryMode := strings.TrimSpace(item.DeliveryMode)
			if !store.ValidDeliveryMode(deliveryMode) {
				return out, fmt.Errorf("invalid delivery_mode %q", deliveryMode)
			}
			sid, err := s.store.NewSID(ctx, ids.Project)
			if err != nil {
				return out, err
			}
			proj := store.Project{
				ID:           ids.UUID(),
				SID:          sid,
				Name:         name,
				VerifyCmd:    strings.TrimSpace(item.VerifyCmd),
				DeliveryMode: deliveryMode,
				CreatedAt:    time.Now().UTC(),
			}
			if err := s.store.CreateProject(ctx, proj); err != nil {
				return out, err
			}
			s.hub.broadcastFleetUpdate("project", proj)
			s.hub.broadcastAutopilotUpdate(ctx)
			out.Results = append(out.Results, proj)
		}

	case "add_repo":
		for _, item := range in.Items {
			projID, err := resolveProjID(item.Project)
			if err != nil {
				return out, err
			}
			remoteURL := strings.TrimSpace(item.RemoteURL)
			if remoteURL == "" {
				return out, errors.New("remote_url is required for add_repo")
			}
			name := strings.TrimSpace(item.Title)
			if name == "" {
				name = strings.TrimSpace(item.Repo)
			}
			if name == "" {
				name = deriveRepoName(remoteURL)
			}
			sid, err := s.store.NewSID(ctx, ids.Repo)
			if err != nil {
				return out, err
			}
			repo := store.Repo{
				ID:            ids.UUID(),
				SID:           sid,
				ProjectID:     projID,
				Name:          name,
				RemoteURL:     remoteURL,
				DefaultBranch: strings.TrimSpace(item.DefaultBranch),
				VerifyCmd:     strings.TrimSpace(item.VerifyCmd),
				DeliveryMode:  strings.TrimSpace(item.DeliveryMode),
				CreatedAt:     time.Now().UTC(),
			}
			if err := s.store.CreateRepo(ctx, repo); err != nil {
				return out, err
			}
			s.hub.broadcastFleetUpdate("repo", repo)
			out.Results = append(out.Results, repo)
		}

	default:
		return out, fmt.Errorf("invalid manage_work action %q", in.Action)
	}
	return out, nil
}

func splitVetoReason(raw string) (reason, note string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "other", ""
	}
	if _, ok := vetoReasons[raw]; ok {
		return raw, ""
	}
	if head, tail, ok := strings.Cut(raw, ":"); ok {
		head = strings.TrimSpace(head)
		if _, known := vetoReasons[head]; known {
			return head, strings.TrimSpace(tail)
		}
	}
	return "other", raw
}

func (s *FleetService) Control(ctx context.Context, in ControlInput, confirm confirmFunc) (any, error) {
	scope := strings.ToLower(strings.TrimSpace(in.Scope))
	action := strings.ToLower(strings.TrimSpace(in.Action))
	target := strings.TrimSpace(in.Target)

	needConfirm := func(question string) error {
		if confirm == nil {
			return errNeedsUI
		}
		return confirm(ctx, question)
	}

	switch scope {
	case "fleet":
		switch action {
		case "pause":
			return s.hub.PauseEverything(ctx)
		case "resume":
			if err := needConfirm("Resume everything (restart dispatching and resume what Pause everything suspended)?"); err != nil {
				return nil, err
			}
			return s.hub.ResumeEverything(ctx)
		case "autopilot_play":
			return s.hub.Autopilot(ctx, AutopilotInput{Action: "play"})
		case "autopilot_pause":
			return s.hub.Autopilot(ctx, AutopilotInput{Action: "pause"})
		default:
			return nil, fmt.Errorf("unsupported action %q for scope fleet", in.Action)
		}

	case "project":
		switch action {
		case "autopilot_play":
			return s.hub.Autopilot(ctx, AutopilotInput{Project: target, Action: "play"})
		case "autopilot_pause", "pause":
			return s.hub.Autopilot(ctx, AutopilotInput{Project: target, Action: "pause"})
		default:
			return nil, fmt.Errorf("unsupported action %q for scope project", in.Action)
		}

	case "machine":
		if target == "" {
			return nil, errors.New("target (machine id, sid or name) is required for scope machine")
		}
		workerID := s.hub.resolveMachine(ctx, target)
		switch action {
		case "drain":
			return s.hub.DrainWorker(ctx, workerID, true)
		case "undrain":
			if err := needConfirm("Undrain " + target + " so it accepts new work again?"); err != nil {
				return nil, err
			}
			return s.hub.DrainWorker(ctx, workerID, false)
		case "suspend", "pause":
			return s.hub.SuspendAll(ctx, workerID)
		case "resume":
			if err := needConfirm("Resume suspended sessions on " + target + "?"); err != nil {
				return nil, err
			}
			return s.hub.ResumeAll(ctx, workerID)
		case "stop":
			return s.hub.StopAll(ctx, workerID)
		default:
			return nil, fmt.Errorf("unsupported action %q for scope machine", in.Action)
		}

	case "session":
		if target == "" {
			return nil, errors.New("target (task or chat id/sid) is required for scope session")
		}
		taskID := resolveRef(ctx, s.store, ids.Task, target)
		if t, err := s.store.GetTask(ctx, taskID); err == nil {
			switch action {
			case "suspend", "pause":
				return s.hub.suspendTask(ctx, t, store.SuspendedByUser)
			case "resume":
				if err := needConfirm("Resume suspended session " + target + "?"); err != nil {
					return nil, err
				}
				return s.hub.resumeTask(ctx, t)
			case "stop":
				return s.CancelTask(ctx, t.ID)
			case "veto":
				reason, note := splitVetoReason(in.Reason)
				return s.hub.VetoTask(ctx, t.ID, reason, note)
			default:
				return nil, fmt.Errorf("unsupported action %q for scope session", in.Action)
			}
		}
		chatID := resolveRef(ctx, s.store, ids.Chat, target)
		cs, err := s.store.GetChatSession(ctx, chatID)
		if err != nil {
			return nil, fmt.Errorf("session %q not found", target)
		}
		switch action {
		case "suspend", "pause":
			updated, err := s.hub.suspendChat(ctx, cs, store.SuspendedByUser)
			return chatActionResult(updated), err
		case "resume":
			if err := needConfirm("Resume suspended session " + target + "?"); err != nil {
				return nil, err
			}
			updated, err := s.hub.resumeChat(ctx, cs)
			return chatActionResult(updated), err
		case "stop":
			updated, err := s.hub.StopChatTurn(ctx, cs.ID)
			return chatActionResult(updated), err
		case "veto":
			reason, note := splitVetoReason(in.Reason)
			updated, err := s.hub.VetoChat(ctx, cs.ID, reason, note)
			return chatActionResult(updated), err
		default:
			return nil, fmt.Errorf("unsupported action %q for scope session", in.Action)
		}

	case "ticket":
		if target == "" {
			return nil, errors.New("target (ticket id or sid) is required for scope ticket")
		}
		tkID := resolveRef(ctx, s.store, ids.Ticket, target)
		tk, err := s.store.GetTicket(ctx, tkID)
		if err != nil {
			return nil, err
		}
		switch action {
		case "stop":
			return s.hub.CancelTicket(ctx, tk.ID, in.Reason)
		case "pause", "suspend":
			if tk.Lease == nil || tk.Lease.TaskID == "" {
				return nil, fmt.Errorf("ticket %s has no running session to suspend", tk.SID)
			}
			return s.hub.SuspendTask(ctx, tk.Lease.TaskID)
		case "resume":
			if tk.Lease == nil || tk.Lease.TaskID == "" {
				return nil, fmt.Errorf("ticket %s has no suspended session to resume", tk.SID)
			}
			if err := needConfirm("Resume suspended ticket " + tk.SID + "?"); err != nil {
				return nil, err
			}
			return s.hub.ResumeTask(ctx, tk.Lease.TaskID)
		case "veto":
			reason, note := splitVetoReason(in.Reason)
			if tk.Lease != nil && tk.Lease.TaskID != "" {
				if _, err := s.hub.VetoTask(ctx, tk.Lease.TaskID, reason, note); err != nil {
					return nil, err
				}
			}
			return s.hub.CancelTicket(ctx, tk.ID, in.Reason)
		default:
			return nil, fmt.Errorf("unsupported action %q for scope ticket", in.Action)
		}

	default:
		return nil, fmt.Errorf("invalid scope %q (want ticket, session, machine, project, or fleet)", in.Scope)
	}
}

func (s *FleetService) Reply(ctx context.Context, in ReplyInput, confirm confirmFunc) (any, error) {
	target := strings.TrimSpace(in.Target)
	msg := in.Message
	if target == "" {
		return nil, errors.New("target is required")
	}
	if strings.TrimSpace(msg) == "" {
		return nil, errors.New("message is required")
	}

	// 1. Question target: enforce answer_question delegation rules.
	qID := resolveRef(ctx, s.store, ids.Question, target)
	if q, err := s.store.GetQuestion(ctx, qID); err == nil {
		return s.replyToQuestion(ctx, q, msg)
	}

	// 2. Task target (direct or via ticket).
	taskID := resolveRef(ctx, s.store, ids.Task, target)
	if t, err := s.store.GetTask(ctx, taskID); err == nil {
		return s.replyToTask(ctx, t, msg, in.Resume, confirm)
	}
	tkID := resolveRef(ctx, s.store, ids.Ticket, target)
	if tk, err := s.store.GetTicket(ctx, tkID); err == nil {
		if tk.Lease != nil && tk.Lease.TaskID != "" {
			if t, tErr := s.store.GetTask(ctx, tk.Lease.TaskID); tErr == nil {
				return s.replyToTask(ctx, t, msg, in.Resume, confirm)
			}
		}
		return nil, fmt.Errorf("ticket %s has no active task to reply to", tk.SID)
	}

	// 3. Chat target: check for pending question first, then resume if requested.
	chatID := resolveRef(ctx, s.store, ids.Chat, target)
	if cs, err := s.store.GetChatSession(ctx, chatID); err == nil {
		if qs, qErr := s.store.ListQuestions(ctx, store.QuestionFilter{Statuses: []string{store.QuestionOpen, store.QuestionDelegated}, Limit: 50}); qErr == nil {
			for _, q := range qs {
				if q.ChatID == cs.ID {
					return s.replyToQuestion(ctx, q, msg)
				}
			}
		}
		if cs.Status == protocol.ChatStatusSuspended && in.Resume {
			if confirm == nil {
				return nil, errNeedsUI
			}
			if err := confirm(ctx, "Resume suspended chat "+cs.SID+"?"); err != nil {
				return nil, err
			}
			if _, err := s.hub.resumeChat(ctx, cs); err != nil {
				return nil, err
			}
		}
		if err := s.hub.sendAgentChat(ctx, cs, chatSendRequest{SessionID: cs.ID, Content: msg}); err != nil {
			return nil, err
		}
		return chatActionResult(cs), nil
	}

	return nil, fmt.Errorf("target %q not found", target)
}

func (s *FleetService) replyToQuestion(ctx context.Context, q store.Question, msg string) (any, error) {
	o, ok := chatOriginFrom(ctx)
	if !ok || !o.commander || o.id == "" {
		return nil, fmt.Errorf("question %s can only be answered by the user (or by the Commander chat when delegated via 'Let the commander decide')", q.SID)
	}
	in := answerQuestionInput{
		Question:  q.ID,
		Rationale: "Decided via reply: " + oneLine(msg, 160),
	}
	trimmed := strings.TrimSpace(msg)
	matchedOpt := false
	for i, opt := range q.Options {
		if strings.EqualFold(opt.Label, trimmed) || trimmed == strconv.Itoa(i+1) || strings.EqualFold(opt.ID, trimmed) {
			in.Option = trimmed
			matchedOpt = true
			break
		}
	}
	if !matchedOpt {
		in.Text = trimmed
	}
	out, err := s.hub.decideDelegated(ctx, o.id, in)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "question": q.SID, "message": out}, nil
}

func (s *FleetService) replyToTask(ctx context.Context, t store.Task, msg string, resume bool, confirm confirmFunc) (ActionResult, error) {
	if t.Status == protocol.TaskStatusSuspended && resume {
		if confirm == nil {
			return ActionResult{}, errNeedsUI
		}
		if err := confirm(ctx, "Resume suspended task "+t.SID+"?"); err != nil {
			return ActionResult{}, err
		}
		if _, err := s.hub.resumeTask(ctx, t); err != nil {
			return ActionResult{}, err
		}
	}
	return s.SteerTask(ctx, t.ID, msg, true)
}

func (s *FleetService) Memory(ctx context.Context, in MemoryInput) (any, error) {
	switch strings.ToLower(strings.TrimSpace(in.Action)) {
	case "save":
		key := strings.TrimSpace(in.Key)
		summary := strings.TrimSpace(in.Summary)
		if key == "" || summary == "" {
			return nil, errors.New("key and summary are required for memory save")
		}
		scope := strings.TrimSpace(in.Scope)
		if scope == "" {
			scope = "fleet"
		}
		if err := s.SaveMemory(ctx, scope, key, summary); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "scope": scope, "key": key}, nil
	case "recall":
		entries, err := s.RecallMemory(ctx, strings.TrimSpace(in.Scope), in.Limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"entries": entries}, nil
	default:
		return nil, fmt.Errorf("invalid memory action %q (want save or recall)", in.Action)
	}
}

// --- serverControlPlane audit-actor wrappers ---

func (b serverControlPlane) FleetStatusView(ctx context.Context, in FleetStatusInput) (any, error) {
	return b.FleetService.FleetStatusView(ctx, in)
}

func (b serverControlPlane) Backlog(ctx context.Context, in BacklogInput) (BacklogResult, error) {
	return b.FleetService.Backlog(ctx, in)
}

func (b serverControlPlane) ManageWork(ctx context.Context, in ManageWorkInput) (ManageWorkResult, error) {
	return b.FleetService.ManageWork(withDefaultAuditActor(ctx, auditSourceMCP, auditSourceMCP), in)
}

func (b serverControlPlane) Control(ctx context.Context, in ControlInput, confirm confirmFunc) (any, error) {
	return b.FleetService.Control(withDefaultAuditActor(ctx, auditSourceMCP, auditSourceMCP), in, confirm)
}

func (b serverControlPlane) Reply(ctx context.Context, in ReplyInput, confirm confirmFunc) (any, error) {
	return b.FleetService.Reply(withDefaultAuditActor(ctx, auditSourceMCP, auditSourceMCP), in, confirm)
}

func (b serverControlPlane) Memory(ctx context.Context, in MemoryInput) (any, error) {
	return b.FleetService.Memory(ctx, in)
}

// --- RemoteFleetClient implementation of SurfaceBackend + InspectBackend ---

func (c *RemoteFleetClient) FleetStatusView(ctx context.Context, in FleetStatusInput) (any, error) {
	fs, err := c.FleetStatus(ctx)
	if err != nil {
		return nil, err
	}
	detail := strings.ToLower(strings.TrimSpace(in.Detail))
	machine := strings.TrimSpace(in.Machine)
	if detail == "" {
		if machine != "" {
			detail = "machines"
		} else {
			detail = "summary"
		}
	}
	if detail == "machines" {
		out := make([]MachineDetailRow, 0, len(fs.Workers))
		for _, w := range fs.Workers {
			if machine != "" && w.ID != machine && w.SID != machine && !strings.EqualFold(w.Name, machine) {
				continue
			}
			out = append(out, MachineDetailRow{
				WorkerID: w.ID, WorkerSID: w.SID, WorkerName: w.Name, Status: w.Status,
				OS: w.OS, Arch: w.Arch, Agents: w.Agents, ActiveTasks: w.ActiveTasks,
				MaxConcurrent: w.MaxConcurrent, Drained: w.Drained, Facts: w.Facts,
			})
		}
		return map[string]any{"machines": out}, nil
	}
	counts := map[string]int{}
	online := 0
	for _, w := range fs.Workers {
		if w.Status == "online" {
			online++
		}
		counts[workerCapabilityKey(w)]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	groups := make([]string, 0, len(keys))
	for _, k := range keys {
		groups = append(groups, fmt.Sprintf("%dx %s", counts[k], k))
	}
	return FleetSummaryResult{
		Online:      online,
		Total:       len(fs.Workers),
		ActiveTasks: fs.ActiveTasksCount,
		Paused:      fs.Pause.Paused,
		Groups:      groups,
	}, nil
}

func (c *RemoteFleetClient) Backlog(ctx context.Context, in BacklogInput) (BacklogResult, error) {
	q := url.Values{}
	if in.Project != "" {
		q.Set("project_id", in.Project)
	}
	if in.Parent != "" {
		q.Set("parent_id", in.Parent)
	}
	if in.Status != "" {
		q.Set("status", in.Status)
	}
	if in.Limit > 0 {
		q.Set("limit", strconv.Itoa(in.Limit))
	}
	var resp struct {
		Tickets []store.Ticket     `json:"tickets"`
		Edges   []store.TicketEdge `json:"edges"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/tickets?"+q.Encode(), nil, &resp); err != nil {
		return BacklogResult{}, err
	}
	blockers := map[string][]string{}
	for _, e := range resp.Edges {
		blockers[e.TicketID] = append(blockers[e.TicketID], e.BlockedByID)
	}
	out := BacklogResult{Tickets: make([]BacklogTicketRow, 0, len(resp.Tickets))}
	for _, tk := range resp.Tickets {
		out.Tickets = append(out.Tickets, BacklogTicketRow{
			SID: tk.SID, ID: tk.ID, Kind: tk.Kind, Title: tk.Title,
			Status: tk.Status, Priority: tk.Priority, Blockers: blockers[tk.ID],
		})
	}
	if tasks, err := c.ListTasks(ctx, in.Status, "", in.Limit); err == nil {
		for _, t := range tasks {
			out.Tasks = append(out.Tasks, BacklogTaskRow{
				SID: t.TaskSID, ID: t.TaskID, Status: t.Status,
				Machine: t.WorkerSID, Agent: t.Agent, Milestone: oneLine(t.LastMilestone, 80),
			})
		}
	}
	return out, nil
}

func (c *RemoteFleetClient) ManageWork(ctx context.Context, in ManageWorkInput) (ManageWorkResult, error) {
	action := strings.ToLower(strings.TrimSpace(in.Action))
	out := ManageWorkResult{Action: action, Results: make([]any, 0, len(in.Items))}
	for _, item := range in.Items {
		switch action {
		case "create":
			var tk store.Ticket
			if err := c.doJSON(ctx, http.MethodPost, "/api/tickets", map[string]any{
				"project_id": item.Project, "parent_id": item.Parent, "kind": item.Kind,
				"title": item.Title, "body": item.Body, "priority": item.Priority,
				"status": item.Status, "labels": item.Labels, "verify_cmd": item.VerifyCmd,
			}, &tk); err != nil {
				return out, err
			}
			out.Results = append(out.Results, tk)
		case "update":
			var tk store.Ticket
			if err := c.doJSON(ctx, http.MethodPatch, "/api/tickets/"+url.PathEscape(item.ID), item, &tk); err != nil {
				return out, err
			}
			out.Results = append(out.Results, tk)
		case "cancel":
			var res any
			if err := c.doJSON(ctx, http.MethodPost, "/api/tickets/"+url.PathEscape(item.ID)+"/cancel", map[string]string{"note": item.Note}, &res); err != nil {
				ar, tErr := c.CancelTask(ctx, item.ID)
				if tErr != nil {
					return out, err
				}
				res = ar
			}
			out.Results = append(out.Results, res)
		case "requeue":
			var res any
			if err := c.doJSON(ctx, http.MethodPost, "/api/tickets/"+url.PathEscape(item.ID)+"/requeue", map[string]string{"note": item.Note}, &res); err != nil {
				return out, err
			}
			out.Results = append(out.Results, res)
		case "dispatch":
			res, err := c.DispatchTask(ctx, DispatchRequest{
				ProjectID:      item.Project,
				Title:          item.Title,
				Agent:          item.Agent,
				Model:          item.Model,
				RepoPath:       firstNonEmpty(item.Repo, "~"),
				Prompt:         item.Prompt,
				Mode:           item.Mode,
				WorkerSelector: WorkerSelector{WorkerID: item.Machine},
			})
			if err != nil {
				return out, err
			}
			out.Results = append(out.Results, res)
		case "create_project":
			var p store.Project
			if err := c.doJSON(ctx, http.MethodPost, "/api/projects", map[string]any{
				"name": firstNonEmpty(item.Title, item.Project), "verify_cmd": item.VerifyCmd, "delivery_mode": item.DeliveryMode,
			}, &p); err != nil {
				return out, err
			}
			out.Results = append(out.Results, p)
		case "add_repo":
			var r store.Repo
			proj := firstNonEmpty(item.Project, store.DefaultProjectID)
			if err := c.doJSON(ctx, http.MethodPost, "/api/projects/"+url.PathEscape(proj)+"/repos", map[string]any{
				"name": firstNonEmpty(item.Title, item.Repo), "remote_url": item.RemoteURL,
				"default_branch": item.DefaultBranch, "verify_cmd": item.VerifyCmd,
			}, &r); err != nil {
				return out, err
			}
			out.Results = append(out.Results, r)
		default:
			return out, fmt.Errorf("invalid manage_work action %q", in.Action)
		}
	}
	return out, nil
}

func (c *RemoteFleetClient) Control(ctx context.Context, in ControlInput, confirm confirmFunc) (any, error) {
	scope := strings.ToLower(strings.TrimSpace(in.Scope))
	action := strings.ToLower(strings.TrimSpace(in.Action))
	if action == "resume" || action == "undrain" {
		if confirm == nil {
			return nil, errNeedsUI
		}
		if err := confirm(ctx, fmt.Sprintf("%s %s %s?", action, scope, in.Target)); err != nil {
			return nil, err
		}
	}
	switch scope {
	case "fleet":
		switch action {
		case "pause":
			return c.PauseEverything(ctx)
		case "resume":
			return c.ResumeEverything(ctx)
		case "autopilot_play":
			return c.Autopilot(ctx, AutopilotInput{Action: "play"})
		case "autopilot_pause":
			return c.Autopilot(ctx, AutopilotInput{Action: "pause"})
		}
	case "project":
		switch action {
		case "autopilot_play":
			return c.Autopilot(ctx, AutopilotInput{Project: in.Target, Action: "play"})
		case "autopilot_pause", "pause":
			return c.Autopilot(ctx, AutopilotInput{Project: in.Target, Action: "pause"})
		}
	case "machine":
		switch action {
		case "drain":
			t := true
			return c.DrainMachine(ctx, DrainInput{Machine: in.Target, Drained: &t})
		case "undrain":
			f := false
			return c.DrainMachine(ctx, DrainInput{Machine: in.Target, Drained: &f})
		}
	case "session":
		switch action {
		case "suspend", "pause":
			return c.SuspendSession(ctx, SessionRefInput{TaskID: in.Target})
		case "resume":
			return c.ResumeSession(ctx, SessionRefInput{TaskID: in.Target})
		case "stop":
			return c.CancelTask(ctx, in.Target)
		case "veto":
			reason, note := splitVetoReason(in.Reason)
			return c.VetoTask(ctx, VetoInput{TaskID: in.Target, Reason: reason, Note: note})
		}
	case "ticket":
		if action == "stop" {
			var res any
			return res, c.doJSON(ctx, http.MethodPost, "/api/tickets/"+url.PathEscape(in.Target)+"/cancel", map[string]string{"note": in.Reason}, &res)
		}
	}
	return nil, fmt.Errorf("unsupported control scope=%q action=%q", in.Scope, in.Action)
}

func (c *RemoteFleetClient) Reply(ctx context.Context, in ReplyInput, _ confirmFunc) (any, error) {
	return c.SteerTask(ctx, in.Target, in.Message, true)
}

func (c *RemoteFleetClient) Memory(ctx context.Context, in MemoryInput) (any, error) {
	switch strings.ToLower(strings.TrimSpace(in.Action)) {
	case "save":
		scope := strings.TrimSpace(in.Scope)
		if scope == "" {
			scope = "fleet"
		}
		if err := c.SaveMemory(ctx, scope, in.Key, in.Summary); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "scope": scope, "key": in.Key}, nil
	case "recall":
		entries, err := c.RecallMemory(ctx, in.Scope, in.Limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"entries": entries}, nil
	default:
		return nil, fmt.Errorf("invalid memory action %q", in.Action)
	}
}

func (c *RemoteFleetClient) Inspect(ctx context.Context, req InspectRequest) (InspectResult, error) {
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	switch kind {
	case protocol.InspectKindEvents:
		ev, err := c.RecentEvents(ctx, RecentEventsInput{Machine: req.Target, Limit: req.MaxLines})
		if err != nil {
			return InspectResult{}, err
		}
		b, err := json.Marshal(ev, protocol.JSONOptions)
		if err != nil {
			return InspectResult{}, err
		}
		res := InspectResult{Target: req.Target, Kind: kind}
		applyPage(&res, protocol.PageText(string(b), req.MaxLines, req.Offset))
		return res, nil
	case protocol.InspectKindAudit:
		au, err := c.ListAudit(ctx, ListAuditInput{TaskID: req.Target, Limit: req.MaxLines})
		if err != nil {
			return InspectResult{}, err
		}
		b, err := json.Marshal(au, protocol.JSONOptions)
		if err != nil {
			return InspectResult{}, err
		}
		res := InspectResult{Target: req.Target, Kind: kind}
		applyPage(&res, protocol.PageText(string(b), req.MaxLines, req.Offset))
		return res, nil
	case protocol.InspectKindResult:
		tr, err := c.GetTaskResult(ctx, req.Target, false, 0)
		if err != nil {
			return InspectResult{}, err
		}
		b, err := json.Marshal(tr, protocol.JSONOptions)
		if err != nil {
			return InspectResult{}, err
		}
		res := InspectResult{Target: req.Target, Kind: kind, TaskID: tr.TaskID, TaskSID: tr.TaskSID}
		applyPage(&res, protocol.PageText(string(b), req.MaxLines, req.Offset))
		return res, nil
	default:
		q := url.Values{}
		q.Set("kind", kind)
		if req.Repo != "" {
			q.Set("repo", req.Repo)
		}
		if req.Path != "" {
			q.Set("path", req.Path)
		}
		if req.MaxLines > 0 {
			q.Set("max_lines", strconv.Itoa(req.MaxLines))
		}
		if req.Offset > 0 {
			q.Set("offset", strconv.Itoa(req.Offset))
		}
		var res InspectResult
		err := c.doJSON(ctx, http.MethodGet, "/api/tickets/"+url.PathEscape(req.Target)+"/inspect?"+q.Encode(), nil, &res)
		return res, err
	}
}

// --- MCP Registration ---

func registerSurfaceTools(srv *mcp.Server, b SurfaceBackend, confirm confirmFunc) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_status",
		Description: fleetStatusDesc,
		InputSchema: fleetStatusSchema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in FleetStatusInput) (*mcp.CallToolResult, any, error) {
		res, err := b.FleetStatusView(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "backlog",
		Description: backlogDesc,
		InputSchema: backlogSchema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in BacklogInput) (*mcp.CallToolResult, any, error) {
		res, err := b.Backlog(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "manage_work",
		Description: manageWorkDesc,
		InputSchema: manageWorkSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ManageWorkInput) (*mcp.CallToolResult, any, error) {
		res, err := b.ManageWork(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "control",
		Description: controlDesc,
		InputSchema: controlSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ControlInput) (*mcp.CallToolResult, any, error) {
		res, err := b.Control(ctx, in, confirm)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "reply",
		Description: replyDesc,
		InputSchema: replySchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ReplyInput) (*mcp.CallToolResult, any, error) {
		res, err := b.Reply(ctx, in, confirm)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory",
		Description: memoryDesc,
		InputSchema: memorySchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in MemoryInput) (*mcp.CallToolResult, any, error) {
		res, err := b.Memory(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})
}
