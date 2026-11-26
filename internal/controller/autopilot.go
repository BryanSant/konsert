package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

const (
	// autopilotGlobalSetting is the controller setting key for the global autopilot switch.
	autopilotGlobalSetting = "autopilot_global"

	AutopilotStatePlaying  = "playing"
	AutopilotStatePaused   = "paused"
	AutopilotStateFinished = "finished"
)

// autopilotRuntimeState tracks transient per-project completion state (finished) in memory
// until Play/Pause is toggled again.
type autopilotRuntimeState struct {
	mu       sync.Mutex
	finished map[string]bool
}

// AutopilotCounts summarizes ticket counts relevant to the autopilot loop.
type AutopilotCounts struct {
	Dispatchable int `json:"dispatchable"`
	Running      int `json:"running"`
	NeedsYou     int `json:"needs_you"`
	Review       int `json:"review"`
}

// AutopilotProjectState is the effective autopilot state and counts for one project.
type AutopilotProjectState struct {
	ProjectID       string `json:"project_id"`
	ProjectSID      string `json:"project_sid"`
	ProjectName     string `json:"project_name"`
	Play            bool   `json:"play"`
	Autopilot       bool   `json:"autopilot"`
	AutopilotPaused bool   `json:"autopilot_paused"`
	Global          bool   `json:"global"`
	FleetPaused     bool   `json:"fleet_paused"`
	// DirectivePaused is an active autopilot.pause directive on this project.
	DirectivePaused bool            `json:"directive_paused,omitempty"`
	State           string          `json:"state"` // "playing" | "paused" | "finished"
	Counts          AutopilotCounts `json:"counts"`
}

// AutopilotState is the fleet-wide autopilot snapshot (global switch + every project).
type AutopilotState struct {
	Global      bool                    `json:"global"`
	FleetPaused bool                    `json:"fleet_paused"`
	State       string                  `json:"state"` // "playing" | "paused" | "finished"
	Counts      AutopilotCounts         `json:"counts"`
	Projects    []AutopilotProjectState `json:"projects"`
}

// AutopilotInput is the reusable input for autopilot control (exposed now via the MCP
// `autopilot` tool and reusable when folded into control/manage_work in A7).
type AutopilotInput struct {
	Project string `json:"project,omitempty" jsonschema:"Project id, sid (p-…) or name; omit for global autopilot"`
	Action  string `json:"action" jsonschema:"Action: play, pause, or status"`
}

// AutopilotBackend is implemented by MCP backends that expose autopilot control.
type AutopilotBackend interface {
	Autopilot(ctx context.Context, in AutopilotInput) (any, error)
}

// autopilotGlobal reports whether the global autopilot switch (autopilot_global) is on.
func (h *Hub) autopilotGlobal(ctx context.Context) bool {
	if h == nil || h.store == nil {
		return false
	}
	saved, err := h.store.GetAllSettings(ctx)
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(saved[autopilotGlobalSetting]), "true")
}

// isProjectAutopilotPlaying evaluates whether a project is allowed to dispatch tickets
// under the autopilot precedence order:
//
//	fleet paused > project paused/directive > global/project play
//
// Specifically:
//  1. If Pause everything (fleet pause) is active, nothing dispatches even when playing.
//  2. If the project is explicitly paused (project.AutopilotPaused) or an
//     autopilot.pause directive is active, it does not dispatch even when
//     autopilot_global is on.
//  3. Otherwise, dispatch is enabled when either autopilot_global or project.Autopilot is on.
func isProjectAutopilotPlaying(proj store.Project, globalOn, fleetPaused, directivePaused bool) bool {
	if fleetPaused || proj.AutopilotPaused || directivePaused {
		return false
	}
	return globalOn || proj.Autopilot
}

func (h *Hub) isProjectAutopilotFinished(projectID string) bool {
	h.autopilot.mu.Lock()
	defer h.autopilot.mu.Unlock()
	return h.autopilot.finished[projectID]
}

func (h *Hub) setProjectAutopilotFinished(projectID string, finished bool) {
	if projectID == "" {
		return
	}
	h.autopilot.mu.Lock()
	defer h.autopilot.mu.Unlock()
	if h.autopilot.finished == nil {
		h.autopilot.finished = map[string]bool{}
	}
	if finished {
		h.autopilot.finished[projectID] = true
	} else {
		delete(h.autopilot.finished, projectID)
	}
}

func (h *Hub) clearAllAutopilotFinished() {
	h.autopilot.mu.Lock()
	clear(h.autopilot.finished)
	h.autopilot.mu.Unlock()
}

func (h *Hub) projectAutopilotCounts(ctx context.Context, projectID string) AutopilotCounts {
	var c AutopilotCounts
	if disp, err := h.store.ListDispatchableTickets(ctx, projectID, 1000); err == nil {
		c.Dispatchable = len(disp)
	}
	if tickets, err := h.store.ListTickets(ctx, store.TicketFilter{ProjectID: projectID, Limit: 1000}); err == nil {
		for _, tk := range tickets {
			if store.IsContainerKind(tk.Kind) {
				continue
			}
			switch tk.Status {
			case store.TicketStatusRunning:
				c.Running++
			case store.TicketStatusNeedsYou:
				c.NeedsYou++
			case store.TicketStatusReview:
				c.Review++
			}
		}
	}
	return c
}

func (h *Hub) buildProjectAutopilotState(ctx context.Context, proj store.Project, globalOn, fleetPaused bool) AutopilotProjectState {
	directivePaused := h.projectDirectivePaused(ctx, proj.ID)
	playing := isProjectAutopilotPlaying(proj, globalOn, fleetPaused, directivePaused)
	state := AutopilotStatePaused
	switch {
	case fleetPaused:
		state = AutopilotStatePaused
	case playing:
		state = AutopilotStatePlaying
	case h.isProjectAutopilotFinished(proj.ID):
		state = AutopilotStateFinished
	default:
		state = AutopilotStatePaused
	}
	return AutopilotProjectState{
		ProjectID:       proj.ID,
		ProjectSID:      proj.SID,
		ProjectName:     proj.Name,
		Play:            playing,
		Autopilot:       proj.Autopilot,
		AutopilotPaused: proj.AutopilotPaused,
		Global:          globalOn,
		FleetPaused:     fleetPaused,
		DirectivePaused: directivePaused,
		State:           state,
		Counts:          h.projectAutopilotCounts(ctx, proj.ID),
	}
}

// ProjectAutopilotState returns the effective autopilot state and ticket counts for projectID.
func (h *Hub) ProjectAutopilotState(ctx context.Context, projectID string) (AutopilotProjectState, error) {
	proj, err := h.store.GetProject(ctx, projectID)
	if err != nil {
		return AutopilotProjectState{}, err
	}
	return h.buildProjectAutopilotState(ctx, proj, h.autopilotGlobal(ctx), h.fleetPaused(ctx)), nil
}

// AutopilotState returns the fleet-wide autopilot state (global switch + all projects).
func (h *Hub) AutopilotState(ctx context.Context) AutopilotState {
	globalOn := h.autopilotGlobal(ctx)
	fleetPaused := h.fleetPaused(ctx)
	projects, _ := h.store.ListProjects(ctx)
	out := AutopilotState{
		Global:      globalOn,
		FleetPaused: fleetPaused,
		State:       AutopilotStatePaused,
		Projects:    make([]AutopilotProjectState, 0, len(projects)),
	}
	anyPlaying := false
	anyFinished := false
	for _, proj := range projects {
		ps := h.buildProjectAutopilotState(ctx, proj, globalOn, fleetPaused)
		out.Projects = append(out.Projects, ps)
		out.Counts.Dispatchable += ps.Counts.Dispatchable
		out.Counts.Running += ps.Counts.Running
		out.Counts.NeedsYou += ps.Counts.NeedsYou
		out.Counts.Review += ps.Counts.Review
		if ps.Play {
			anyPlaying = true
		}
		if ps.State == AutopilotStateFinished {
			anyFinished = true
		}
	}
	switch {
	case fleetPaused:
		out.State = AutopilotStatePaused
	case globalOn || anyPlaying:
		out.State = AutopilotStatePlaying
	case anyFinished:
		out.State = AutopilotStateFinished
	default:
		out.State = AutopilotStatePaused
	}
	return out
}

// broadcastAutopilotUpdate pushes the current AutopilotState to connected browsers.
func (h *Hub) broadcastAutopilotUpdate(ctx context.Context) {
	if h == nil || h.store == nil {
		return
	}
	h.mu.RLock()
	hasClients := len(h.clients) > 0
	h.mu.RUnlock()
	if !hasClients {
		return
	}
	h.broadcastFleetUpdate("autopilot", h.AutopilotState(context.WithoutCancel(ctx)))
}

// SetProjectAutopilot turns autopilot Play on or off for one project, recording an
// attributed audit + fleet event and waking the scheduler when turned on.
func (h *Hub) SetProjectAutopilot(ctx context.Context, projectID string, play bool) (AutopilotProjectState, error) {
	proj, err := h.store.GetProject(ctx, projectID)
	if err != nil {
		return AutopilotProjectState{}, err
	}
	proj.Autopilot = play
	proj.AutopilotPaused = !play
	if err := h.store.UpdateProject(ctx, proj); err != nil {
		return AutopilotProjectState{}, err
	}
	h.setProjectAutopilotFinished(proj.ID, false)
	if play {
		h.markProjectActive(proj.ID)
	}

	cause := causeFrom(ctx)
	auditAction := AuditAutopilotPause
	eventAction := "autopilot_pause"
	verb := "paused"
	if play {
		auditAction = AuditAutopilotPlay
		eventAction = "autopilot_play"
		verb = "started"
	}
	msg := fmt.Sprintf("%s %s autopilot for project %s (%s)", capitalize(describeActor(cause)), verb, proj.Name, proj.SID)
	meta := auditMeta(map[string]any{
		"scope":        "project",
		"project_id":   proj.ID,
		"project_sid":  proj.SID,
		"project_name": proj.Name,
		"play":         play,
	})
	h.recordAudit(ctx, store.AuditEvent{
		Action:   auditAction,
		Message:  msg,
		MetaJSON: meta,
	})
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind:     store.EventKindAction,
		Action:   eventAction,
		Cause:    cause,
		Summary:  msg,
		MetaJSON: meta,
	})
	h.broadcastFleetUpdate("project", proj)
	h.broadcastAutopilotUpdate(ctx)
	if play && !h.fleetPaused(ctx) {
		h.kickScheduler()
	}
	return h.buildProjectAutopilotState(ctx, proj, h.autopilotGlobal(ctx), h.fleetPaused(ctx)), nil
}

// SetGlobalAutopilot turns the global autopilot switch (autopilot_global) on or off,
// recording an attributed audit + fleet event and waking the scheduler when turned on.
func (h *Hub) SetGlobalAutopilot(ctx context.Context, play bool) (AutopilotState, error) {
	if err := h.store.SetSettings(ctx, map[string]string{
		autopilotGlobalSetting: strconv.FormatBool(play),
	}); err != nil {
		return AutopilotState{}, err
	}
	if play {
		h.clearAllAutopilotFinished()
		if projects, err := h.store.ListProjects(ctx); err == nil {
			for _, p := range projects {
				if !p.AutopilotPaused {
					h.markProjectActive(p.ID)
				}
			}
		}
	}

	cause := causeFrom(ctx)
	auditAction := AuditAutopilotPause
	eventAction := "autopilot_global_pause"
	verb := "paused"
	if play {
		auditAction = AuditAutopilotPlay
		eventAction = "autopilot_global_play"
		verb = "started"
	}
	msg := fmt.Sprintf("%s %s global autopilot", capitalize(describeActor(cause)), verb)
	meta := auditMeta(map[string]any{
		"scope": "global",
		"play":  play,
	})
	h.recordAudit(ctx, store.AuditEvent{
		Action:   auditAction,
		Message:  msg,
		MetaJSON: meta,
	})
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind:     store.EventKindAction,
		Action:   eventAction,
		Cause:    cause,
		Summary:  msg,
		MetaJSON: meta,
	})
	state := h.AutopilotState(ctx)
	h.broadcastFleetUpdate("autopilot", state)
	if play && !h.fleetPaused(ctx) {
		h.kickScheduler()
	}
	return state, nil
}

// finishProjectAutopilot turns Play off automatically for proj when no dispatchable,
// running, or needs_you tickets remain, and records an audit + fleet event.
func (h *Hub) finishProjectAutopilot(ctx context.Context, proj store.Project) {
	wctx := context.WithoutCancel(ctx)
	globalOn := h.autopilotGlobal(wctx)
	proj.Autopilot = false
	if globalOn {
		// Global autopilot is on, so explicitly pause this project so Play actually turns off for it.
		proj.AutopilotPaused = true
	}
	if err := h.store.UpdateProject(wctx, proj); err != nil {
		h.logger.Warn("auto-disable autopilot on idle failed", "project_id", proj.ID, "err", err)
	}
	h.setProjectAutopilotFinished(proj.ID, true)
	h.broadcastFleetUpdate("project", proj)
	h.broadcastAutopilotUpdate(wctx)

	sysCtx := withCause(
		withAuditActor(wctx, "system", "autopilot"),
		store.Cause{Actor: store.ActorSystem, Source: "autopilot", Confidence: store.ConfidenceCertain},
	)
	msg := fmt.Sprintf("Autopilot finished for project %s (%s)", proj.Name, proj.SID)
	meta := auditMeta(map[string]any{
		"project_id":   proj.ID,
		"project_sid":  proj.SID,
		"project_name": proj.Name,
	})
	h.recordAudit(sysCtx, store.AuditEvent{
		Action:   AuditAutopilotFinish,
		Message:  msg,
		MetaJSON: meta,
	})
	h.recordFleetEvent(sysCtx, store.FleetEvent{
		Kind:     store.EventKindNotice,
		Action:   "autopilot_finished",
		Summary:  msg,
		MetaJSON: meta,
	})
}

// resolveProject resolves a project reference by UUID, SID (p-…), or case-insensitive name.
func (h *Hub) resolveProject(ctx context.Context, ref string) (store.Project, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return store.Project{}, store.ErrNotFound
	}
	if id, err := h.store.ResolveID(ctx, ids.Project, ref); err == nil {
		return h.store.GetProject(ctx, id)
	}
	if p, err := h.store.GetProject(ctx, ref); err == nil {
		return p, nil
	}
	projects, err := h.store.ListProjects(ctx)
	if err != nil {
		return store.Project{}, err
	}
	for _, p := range projects {
		if strings.EqualFold(p.Name, ref) {
			return p, nil
		}
	}
	return store.Project{}, fmt.Errorf("%w: project %q", store.ErrNotFound, ref)
}

// Autopilot is the reusable handler for play/pause/status across global or per-project
// autopilot. Used by the MCP `autopilot` tool and ready to fold into control/manage_work.
func (h *Hub) Autopilot(ctx context.Context, in AutopilotInput) (any, error) {
	action := strings.ToLower(strings.TrimSpace(in.Action))
	switch action {
	case "play", "pause", "status":
	default:
		return nil, fmt.Errorf("invalid autopilot action %q (want play, pause, or status)", in.Action)
	}

	projRef := strings.TrimSpace(in.Project)
	if projRef == "" {
		switch action {
		case "status":
			return h.AutopilotState(ctx), nil
		case "play":
			return h.SetGlobalAutopilot(ctx, true)
		case "pause":
			return h.SetGlobalAutopilot(ctx, false)
		}
	}

	proj, err := h.resolveProject(ctx, projRef)
	if err != nil {
		return nil, err
	}
	switch action {
	case "status":
		return h.ProjectAutopilotState(ctx, proj.ID)
	case "play":
		return h.SetProjectAutopilot(ctx, proj.ID, true)
	case "pause":
		return h.SetProjectAutopilot(ctx, proj.ID, false)
	}
	return nil, nil
}

// --- FleetService & RemoteFleetClient & serverControlPlane ---

func (s *FleetService) Autopilot(ctx context.Context, in AutopilotInput) (any, error) {
	return s.hub.Autopilot(ctx, in)
}

func (b serverControlPlane) Autopilot(ctx context.Context, in AutopilotInput) (any, error) {
	return b.FleetService.Autopilot(withDefaultAuditActor(ctx, auditSourceMCP, auditSourceMCP), in)
}

func (c *RemoteFleetClient) Autopilot(ctx context.Context, in AutopilotInput) (any, error) {
	action := strings.ToLower(strings.TrimSpace(in.Action))
	switch action {
	case "play", "pause", "status":
	default:
		return nil, fmt.Errorf("invalid autopilot action %q (want play, pause, or status)", in.Action)
	}
	projRef := strings.TrimSpace(in.Project)
	if projRef != "" {
		var out AutopilotProjectState
		path := "/api/projects/" + url.PathEscape(projRef) + "/autopilot"
		if action == "status" {
			return out, c.doJSON(ctx, http.MethodGet, path, nil, &out)
		}
		return out, c.doJSON(ctx, http.MethodPost, path, map[string]bool{"play": action == "play"}, &out)
	}
	if action != "status" {
		if err := c.doJSON(ctx, http.MethodPut, "/api/settings", map[string]any{"autopilot_global": action == "play"}, nil); err != nil {
			return nil, err
		}
	}
	var out AutopilotState
	return out, c.doJSON(ctx, http.MethodGet, "/api/autopilot", nil, &out)
}

// --- REST handlers ---

func (s *Server) handleGetAutopilot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.AutopilotState(r.Context()))
}

func (s *Server) handleGetProjectAutopilot(w http.ResponseWriter, r *http.Request) {
	res, err := s.hub.ProjectAutopilotState(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSetProjectAutopilot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Play *bool `json:"play"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Play == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "play is required"})
		return
	}
	res, err := s.hub.SetProjectAutopilot(r.Context(), r.PathValue("id"), *body.Play)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
