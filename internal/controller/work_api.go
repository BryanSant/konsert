package controller

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func deriveRepoName(remoteURL string) string {
	trimmed := strings.TrimSpace(remoteURL)
	trimmed = strings.TrimRight(trimmed, "/\\")
	trimmed = strings.TrimSuffix(trimmed, ".git")
	if i := strings.LastIndexAny(trimmed, "/:\\"); i >= 0 && i+1 < len(trimmed) {
		return trimmed[i+1:]
	}
	return trimmed
}

func writeWorkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, store.ErrCycle), errors.Is(err, store.ErrDuplicate):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, store.ErrInvalidWork), errors.Is(err, store.ErrProtectedDefault):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func (s *Server) handleSessionDefaults(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, err := s.service.ResolveSessionDefaults(r.Context(), q.Get("worker_id"), q.Get("agent"), q.Get("model"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- Projects ---

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListProjects(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": list})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name         string               `json:"name"`
		VerifyCmd    string               `json:"verify_cmd"`
		Autonomy     store.AutonomyPolicy `json:"autonomy"`
		Autopilot    bool                 `json:"autopilot"`
		DeliveryMode string               `json:"delivery_mode"`
		MergeStyle   string               `json:"merge_style"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	body.DeliveryMode = strings.TrimSpace(body.DeliveryMode)
	body.MergeStyle = strings.TrimSpace(body.MergeStyle)
	if !store.ValidDeliveryMode(body.DeliveryMode) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid delivery_mode"})
		return
	}
	if !store.ValidMergeStyle(body.MergeStyle) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid merge_style"})
		return
	}
	projectSID, err := s.store.NewSID(r.Context(), ids.Project)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	proj := store.Project{
		ID:           ids.UUID(),
		SID:          projectSID,
		Name:         name,
		IsDefault:    false,
		VerifyCmd:    strings.TrimSpace(body.VerifyCmd),
		Autonomy:     body.Autonomy,
		Autopilot:    body.Autopilot,
		DeliveryMode: body.DeliveryMode,
		MergeStyle:   body.MergeStyle,
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.store.CreateProject(r.Context(), proj); err != nil {
		writeWorkError(w, err)
		return
	}
	s.hub.broadcastFleetUpdate("project", proj)
	writeJSON(w, http.StatusCreated, proj)
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	proj, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	var body struct {
		Name         *string               `json:"name"`
		VerifyCmd    *string               `json:"verify_cmd"`
		Autonomy     *store.AutonomyPolicy `json:"autonomy"`
		Autopilot    *bool                 `json:"autopilot"`
		DeliveryMode *string               `json:"delivery_mode"`
		MergeStyle   *string               `json:"merge_style"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name != nil {
		proj.Name = strings.TrimSpace(*body.Name)
		if proj.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name cannot be empty"})
			return
		}
	}
	if body.VerifyCmd != nil {
		proj.VerifyCmd = strings.TrimSpace(*body.VerifyCmd)
	}
	if body.Autonomy != nil {
		proj.Autonomy = *body.Autonomy
	}
	autopilotTurnedOn := false
	if body.Autopilot != nil {
		if *body.Autopilot && !proj.Autopilot {
			autopilotTurnedOn = true
		}
		proj.Autopilot = *body.Autopilot
	}
	if body.DeliveryMode != nil {
		mode := strings.TrimSpace(*body.DeliveryMode)
		if !store.ValidDeliveryMode(mode) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid delivery_mode"})
			return
		}
		proj.DeliveryMode = mode
	}
	if body.MergeStyle != nil {
		style := strings.TrimSpace(*body.MergeStyle)
		if !store.ValidMergeStyle(style) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid merge_style"})
			return
		}
		proj.MergeStyle = style
	}
	if err := s.store.UpdateProject(r.Context(), proj); err != nil {
		writeWorkError(w, err)
		return
	}
	s.hub.broadcastFleetUpdate("project", proj)
	if autopilotTurnedOn {
		s.hub.kickScheduler()
	}
	writeJSON(w, http.StatusOK, proj)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteProject(r.Context(), id); err != nil {
		writeWorkError(w, err)
		return
	}
	s.hub.broadcastFleetUpdate("project_deleted", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// --- Project Repo Registry (/api/projects/{id}/repos) ---

func (s *Server) handleListProjectRepos(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, err := s.store.GetProject(r.Context(), projectID); err != nil {
		writeWorkError(w, err)
		return
	}
	list, err := s.store.ListRepos(r.Context(), projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": list})
}

func (s *Server) handleCreateProjectRepo(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, err := s.store.GetProject(r.Context(), projectID); err != nil {
		writeWorkError(w, err)
		return
	}
	var body struct {
		Name          string `json:"name"`
		RemoteURL     string `json:"remote_url"`
		DefaultBranch string `json:"default_branch"`
		VerifyCmd     string `json:"verify_cmd"`
		DeliveryMode  string `json:"delivery_mode"`
		MergeStyle    string `json:"merge_style"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.RemoteURL = strings.TrimSpace(body.RemoteURL)
	if body.RemoteURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "remote_url is required"})
		return
	}
	body.DeliveryMode = strings.TrimSpace(body.DeliveryMode)
	body.MergeStyle = strings.TrimSpace(body.MergeStyle)
	if !store.ValidDeliveryMode(body.DeliveryMode) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid delivery_mode"})
		return
	}
	if !store.ValidMergeStyle(body.MergeStyle) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid merge_style"})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = deriveRepoName(body.RemoteURL)
	}
	repoSID, err := s.store.NewSID(r.Context(), ids.Repo)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	repo := store.Repo{
		ID:            ids.UUID(),
		SID:           repoSID,
		ProjectID:     projectID,
		Name:          name,
		RemoteURL:     body.RemoteURL,
		DefaultBranch: strings.TrimSpace(body.DefaultBranch),
		VerifyCmd:     strings.TrimSpace(body.VerifyCmd),
		DeliveryMode:  body.DeliveryMode,
		MergeStyle:    body.MergeStyle,
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.store.CreateRepo(r.Context(), repo); err != nil {
		writeWorkError(w, err)
		return
	}
	s.hub.broadcastFleetUpdate("repo", repo)
	writeJSON(w, http.StatusCreated, repo)
}

func (s *Server) handleUpdateProjectRepo(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	repoID, err := s.store.ResolveID(r.Context(), ids.Repo, r.PathValue("repoId"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	repo, err := s.store.GetRepo(r.Context(), repoID)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	if repo.ProjectID != projectID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "repo not found in project"})
		return
	}
	var body struct {
		Name          *string `json:"name"`
		RemoteURL     *string `json:"remote_url"`
		DefaultBranch *string `json:"default_branch"`
		VerifyCmd     *string `json:"verify_cmd"`
		DeliveryMode  *string `json:"delivery_mode"`
		MergeStyle    *string `json:"merge_style"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name != nil {
		repo.Name = strings.TrimSpace(*body.Name)
	}
	if body.RemoteURL != nil {
		repo.RemoteURL = strings.TrimSpace(*body.RemoteURL)
	}
	if body.DefaultBranch != nil {
		repo.DefaultBranch = strings.TrimSpace(*body.DefaultBranch)
	}
	if body.VerifyCmd != nil {
		repo.VerifyCmd = strings.TrimSpace(*body.VerifyCmd)
	}
	if body.DeliveryMode != nil {
		mode := strings.TrimSpace(*body.DeliveryMode)
		if !store.ValidDeliveryMode(mode) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid delivery_mode"})
			return
		}
		repo.DeliveryMode = mode
	}
	if body.MergeStyle != nil {
		style := strings.TrimSpace(*body.MergeStyle)
		if !store.ValidMergeStyle(style) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid merge_style"})
			return
		}
		repo.MergeStyle = style
	}
	if err := s.store.UpdateRepo(r.Context(), repo); err != nil {
		writeWorkError(w, err)
		return
	}
	s.hub.broadcastFleetUpdate("repo", repo)
	writeJSON(w, http.StatusOK, repo)
}

func (s *Server) handleDeleteProjectRepo(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	repoID, err := s.store.ResolveID(r.Context(), ids.Repo, r.PathValue("repoId"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	repo, err := s.store.GetRepo(r.Context(), repoID)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	if repo.ProjectID != projectID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "repo not found in project"})
		return
	}
	if err := s.store.DeleteRepo(r.Context(), repoID); err != nil {
		writeWorkError(w, err)
		return
	}
	s.hub.broadcastFleetUpdate("repo_deleted", repoID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": repoID})
}

// --- Tickets ---

func (s *Server) handleListTickets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.TicketFilter{
		ProjectID: resolveRef(r.Context(), s.store, ids.Project, strings.TrimSpace(q.Get("project_id"))),
		ParentID:  resolveRef(r.Context(), s.store, ids.Ticket, strings.TrimSpace(q.Get("parent_id"))),
		RootOnly:  q.Get("root") == "true" || q.Get("root") == "1",
		Status:    strings.TrimSpace(q.Get("status")),
		Kind:      strings.TrimSpace(q.Get("kind")),
	}
	if limStr := strings.TrimSpace(q.Get("limit")); limStr != "" {
		if n, err := strconv.Atoi(limStr); err == nil && n > 0 {
			f.Limit = n
		}
	}
	list, err := s.store.ListTickets(r.Context(), f)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tickets": list})
}

func (s *Server) handleCreateTicket(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProjectID    string                 `json:"project_id"`
		ParentID     string                 `json:"parent_id"`
		Kind         string                 `json:"kind"`
		Title        string                 `json:"title"`
		Body         string                 `json:"body"`
		Acceptance   []store.AcceptanceItem `json:"acceptance"`
		Labels       []string               `json:"labels"`
		Priority     string                 `json:"priority"`
		Status       string                 `json:"status"`
		OrphanPolicy string                 `json:"orphan_policy"`
		Hints        store.TicketHints      `json:"hints"`
		VerifyCmd    string                 `json:"verify_cmd"`
		Autonomy     store.AutonomyPolicy   `json:"autonomy"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	projID := resolveRef(r.Context(), s.store, ids.Project, strings.TrimSpace(body.ProjectID))
	if projID == "" {
		projID = store.DefaultProjectID
	}
	parentID := resolveRef(r.Context(), s.store, ids.Ticket, strings.TrimSpace(body.ParentID))
	tk, err := s.store.CreateTicket(r.Context(), store.Ticket{
		ProjectID:    projID,
		ParentID:     parentID,
		Kind:         body.Kind,
		Title:        body.Title,
		Body:         body.Body,
		Acceptance:   body.Acceptance,
		Labels:       body.Labels,
		Priority:     body.Priority,
		Status:       body.Status,
		OrphanPolicy: body.OrphanPolicy,
		Hints:        body.Hints,
		VerifyCmd:    strings.TrimSpace(body.VerifyCmd),
		Autonomy:     body.Autonomy,
	})
	if err != nil {
		writeWorkError(w, err)
		return
	}
	s.hub.broadcastFleetUpdate("ticket", tk)
	writeJSON(w, http.StatusCreated, tk)
}

func (s *Server) handleGetTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tk, err := s.store.GetTicket(r.Context(), id)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	edges, _ := s.store.ListTicketEdges(r.Context(), id)
	bindings, _ := s.store.ListTicketRepoBindings(r.Context(), id)
	refs, _ := s.store.ListExternalRefs(r.Context(), id)
	resp := map[string]any{
		"ticket":        tk,
		"blocked_by":    edges,
		"repo_bindings": bindings,
		"external_refs": refs,
	}
	if store.IsContainerKind(tk.Kind) {
		if rollup, err := s.store.GetTicketRollup(r.Context(), id); err == nil {
			resp["rollup"] = rollup
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleUpdateTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tk, err := s.store.GetTicket(r.Context(), id)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	prevStatus := tk.Status
	var body struct {
		ParentID     *string                 `json:"parent_id"`
		Kind         *string                 `json:"kind"`
		Title        *string                 `json:"title"`
		Body         *string                 `json:"body"`
		Acceptance   *[]store.AcceptanceItem `json:"acceptance"`
		Labels       *[]string               `json:"labels"`
		Priority     *string                 `json:"priority"`
		Status       *string                 `json:"status"`
		OrphanPolicy *string                 `json:"orphan_policy"`
		Hints        *store.TicketHints      `json:"hints"`
		VerifyCmd    *string                 `json:"verify_cmd"`
		Autonomy     *store.AutonomyPolicy   `json:"autonomy"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ParentID != nil {
		tk.ParentID = resolveRef(r.Context(), s.store, ids.Ticket, strings.TrimSpace(*body.ParentID))
	}
	if body.Kind != nil {
		tk.Kind = *body.Kind
	}
	if body.Title != nil {
		tk.Title = *body.Title
	}
	if body.Body != nil {
		tk.Body = *body.Body
	}
	if body.Acceptance != nil {
		tk.Acceptance = *body.Acceptance
	}
	if body.Labels != nil {
		tk.Labels = *body.Labels
	}
	if body.Priority != nil {
		tk.Priority = *body.Priority
	}
	if body.Status != nil {
		tk.Status = *body.Status
	}
	if body.OrphanPolicy != nil {
		tk.OrphanPolicy = *body.OrphanPolicy
	}
	if body.Hints != nil {
		tk.Hints = *body.Hints
	}
	if body.VerifyCmd != nil {
		tk.VerifyCmd = strings.TrimSpace(*body.VerifyCmd)
	}
	if body.Autonomy != nil {
		tk.Autonomy = *body.Autonomy
	}
	updated, err := s.store.UpdateTicket(r.Context(), tk)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	s.hub.broadcastFleetUpdate("ticket", updated)
	if updated.Status == store.TicketStatusDone && prevStatus != store.TicketStatusDone {
		ticketID := updated.ID
		s.hub.goBackground(func(ctx context.Context) {
			_ = s.hub.CleanupTicketWorktrees(ctx, ticketID)
		})
	}
	if updated.Status == store.TicketStatusReady || updated.Status == store.TicketStatusDone || updated.Status == store.TicketStatusReview {
		s.hub.kickScheduler()
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteTicket(r.Context(), id); err != nil {
		writeWorkError(w, err)
		return
	}
	s.hub.broadcastFleetUpdate("ticket_deleted", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func (s *Server) handleGetTicketRollup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rollup, err := s.store.GetTicketRollup(r.Context(), id)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rollup)
}

func (s *Server) handleRequeueTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Note string `json:"note"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &body) {
			return
		}
	}
	updated, err := s.hub.RequeueTicket(r.Context(), id, body.Note)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleCancelTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Note string `json:"note"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &body) {
			return
		}
	}
	updated, err := s.hub.CancelTicket(r.Context(), id, body.Note)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// --- Ticket Edges ---

func (s *Server) handleListTicketEdges(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetTicket(r.Context(), id); err != nil {
		writeWorkError(w, err)
		return
	}
	edges, err := s.store.ListTicketEdges(r.Context(), id)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"edges": edges})
}

func (s *Server) handleAddTicketEdge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		BlockedByID string `json:"blocked_by_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	blockerID := resolveRef(r.Context(), s.store, ids.Ticket, strings.TrimSpace(body.BlockedByID))
	if err := s.store.AddTicketEdge(r.Context(), id, blockerID); err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ticket_id": id, "blocked_by_id": blockerID})
}

func (s *Server) handleRemoveTicketEdge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	blockerID := resolveRef(r.Context(), s.store, ids.Ticket, strings.TrimSpace(r.PathValue("blockerId")))
	if err := s.store.RemoveTicketEdge(r.Context(), id, blockerID); err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ticket_id": id, "blocked_by_id": blockerID})
}

// --- Ticket Repo Bindings ---

func (s *Server) handleListTicketRepoBindings(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetTicket(r.Context(), id); err != nil {
		writeWorkError(w, err)
		return
	}
	bindings, err := s.store.ListTicketRepoBindings(r.Context(), id)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo_bindings": bindings})
}

func (s *Server) handlePutTicketRepoBinding(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	repoID, err := s.store.ResolveID(r.Context(), ids.Repo, r.PathValue("repoId"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	var body struct {
		BaseBranch   string `json:"base_branch"`
		TargetBranch string `json:"target_branch"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	b := store.TicketRepoBinding{
		TicketID:     id,
		RepoID:       repoID,
		BaseBranch:   strings.TrimSpace(body.BaseBranch),
		TargetBranch: strings.TrimSpace(body.TargetBranch),
	}
	if err := s.store.PutTicketRepoBinding(r.Context(), b); err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleRemoveTicketRepoBinding(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	repoID, err := s.store.ResolveID(r.Context(), ids.Repo, r.PathValue("repoId"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	if err := s.store.RemoveTicketRepoBinding(r.Context(), id, repoID); err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ticket_id": id, "repo_id": repoID})
}

// --- External Refs ---

func (s *Server) handleListExternalRefs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetTicket(r.Context(), id); err != nil {
		writeWorkError(w, err)
		return
	}
	refs, err := s.store.ListExternalRefs(r.Context(), id)
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"external_refs": refs})
}

func (s *Server) handlePutExternalRef(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Tracker    string `json:"tracker"`
		ExternalID string `json:"external_id"`
		URL        string `json:"url"`
		Version    string `json:"version"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	ref := store.ExternalRef{
		TicketID:   id,
		Tracker:    strings.TrimSpace(body.Tracker),
		ExternalID: strings.TrimSpace(body.ExternalID),
		URL:        strings.TrimSpace(body.URL),
		Version:    strings.TrimSpace(body.Version),
	}
	if err := s.store.PutExternalRef(r.Context(), ref); err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ref)
}

func (s *Server) handleRemoveExternalRef(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()
	tracker := strings.TrimSpace(q.Get("tracker"))
	externalID := strings.TrimSpace(q.Get("external_id"))
	if tracker == "" || externalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tracker and external_id query params are required"})
		return
	}
	if err := s.store.RemoveExternalRef(r.Context(), id, tracker, externalID); err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Cached Machine Facts & Commander Memory API ---

func (s *Server) handleGetWorkerFacts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wk, err := s.store.GetWorker(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "worker not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"worker_id":  wk.ID,
		"name":       wk.Name,
		"facts":      decodeWorkerFacts(wk.FactsJSON),
		"updated_at": wk.FactsUpdatedAt,
	})
}

func (s *Server) handleListMemory(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	entries, err := s.store.ListMemory(r.Context(), scope, 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleUpsertMemory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scope   string `json:"scope"`
		Key     string `json:"key"`
		Summary string `json:"summary"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Key = strings.TrimSpace(body.Key)
	body.Summary = strings.TrimSpace(body.Summary)
	if body.Key == "" || body.Summary == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key and summary are required"})
		return
	}
	if strings.TrimSpace(body.Scope) == "" {
		body.Scope = "fleet"
	}
	entry := store.MemoryEntry{
		Scope:     strings.TrimSpace(body.Scope),
		Key:       body.Key,
		Summary:   body.Summary,
		UpdatedAt: time.Now().UTC(),
	}
	if err := s.store.UpsertMemory(r.Context(), entry); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if scope == "" {
		scope = "fleet"
	}
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key is required"})
		return
	}
	if err := s.store.DeleteMemory(r.Context(), scope, key); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "memory entry not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleFleetFanOut(w http.ResponseWriter, r *http.Request) {
	var req FanOutRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := s.service.FanOutFleet(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleUpsertQuotas(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Quotas []store.SubscriptionQuota `json:"quotas"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	for _, q := range body.Quotas {
		if strings.TrimSpace(q.Provider) == "" {
			continue
		}
		if err := s.store.UpsertSubscriptionQuota(r.Context(), q); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	sum, err := s.store.GetUsageSummary(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.broadcastJSON(protocol.MsgUsageUpdate, sum)
	writeJSON(w, http.StatusOK, sum)
}
