package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/google/uuid"
)

func derivePathLeaf(p string) string {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" || trimmed == "~" {
		return "~"
	}
	trimmed = strings.TrimRight(trimmed, "/\\")
	parts := strings.FieldsFunc(trimmed, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return trimmed
}

func (s *Server) handleGetTaxonomy(w http.ResponseWriter, r *http.Request) {
	tax, err := s.store.GetTaxonomy(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, tax)
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

// --- Workspaces (Spaces) ---

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListWorkspaces(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": list})
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	ws := store.Workspace{
		ID:        uuid.NewString(),
		Name:      name,
		IsDefault: false,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateWorkspace(r.Context(), ws); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.broadcastFleetUpdate("workspace", ws)
	writeJSON(w, http.StatusCreated, ws)
}

func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteWorkspace(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, store.ErrProtectedDefault):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case errors.Is(err, store.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace not found"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	s.hub.broadcastFleetUpdate("workspace_deleted", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// --- Projects ---

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	wsID := r.URL.Query().Get("workspace_id")
	list, err := s.store.ListProjects(r.Context(), wsID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": list})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkspaceID string `json:"workspace_id"`
		Name        string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	wsID := strings.TrimSpace(body.WorkspaceID)
	if wsID == "" {
		wsID = store.DefaultWorkspaceID
	}
	if _, err := s.store.GetWorkspace(r.Context(), wsID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	proj := store.Project{
		ID:          uuid.NewString(),
		WorkspaceID: wsID,
		Name:        name,
		IsDefault:   false,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.store.CreateProject(r.Context(), proj); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.broadcastFleetUpdate("project", proj)
	writeJSON(w, http.StatusCreated, proj)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteProject(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, store.ErrProtectedDefault):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case errors.Is(err, store.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	s.hub.broadcastFleetUpdate("project_deleted", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// --- Repos ---

func (s *Server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, err := s.store.ListRepos(r.Context(), q.Get("project_id"), q.Get("worker_id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": list})
}

func (s *Server) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProjectID string `json:"project_id"`
		WorkerID  string `json:"worker_id"`
		Name      string `json:"name"`
		Path      string `json:"path"`
		RemoteURL string `json:"remote_url"`
		Branch    string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	body.WorkerID = strings.TrimSpace(body.WorkerID)
	body.Path = strings.TrimSpace(body.Path)
	if body.WorkerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}
	if body.Path == "" {
		body.Path = "~"
	}
	projID := strings.TrimSpace(body.ProjectID)
	if projID == "" {
		projID = store.DefaultProjectID
	}
	if _, err := s.store.GetProject(r.Context(), projID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if _, err := s.store.GetWorker(r.Context(), body.WorkerID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "worker not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = derivePathLeaf(body.Path)
	}
	now := time.Now().UTC()
	repo := store.Repo{
		ID:        uuid.NewString(),
		ProjectID: projID,
		WorkerID:  body.WorkerID,
		Name:      name,
		Path:      body.Path,
		RemoteURL: strings.TrimSpace(body.RemoteURL),
		CreatedAt: now,
	}
	if err := s.store.CreateRepo(r.Context(), repo); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.broadcastFleetUpdate("repo", repo)

	// Automatically create a primary worktree pointing at the repo's main checkout.
	wt := store.Worktree{
		ID:        uuid.NewString(),
		RepoID:    repo.ID,
		Branch:    strings.TrimSpace(body.Branch),
		Path:      repo.Path,
		IsPrimary: true,
		CreatedAt: now,
	}
	if err := s.store.CreateWorktree(r.Context(), wt); err == nil {
		s.hub.broadcastFleetUpdate("worktree", wt)
	}

	writeJSON(w, http.StatusCreated, repo)
}

func (s *Server) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteRepo(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "repo not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.broadcastFleetUpdate("repo_deleted", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// --- Worktrees ---

func (s *Server) handleListWorktrees(w http.ResponseWriter, r *http.Request) {
	repoID := r.URL.Query().Get("repo_id")
	list, err := s.store.ListWorktrees(r.Context(), repoID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"worktrees": list})
}

func (s *Server) handleCreateWorktree(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RepoID    string `json:"repo_id"`
		Branch    string `json:"branch"`
		Path      string `json:"path"`
		IsPrimary bool   `json:"is_primary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	body.RepoID = strings.TrimSpace(body.RepoID)
	if body.RepoID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo_id is required"})
		return
	}
	repo, err := s.store.GetRepo(r.Context(), body.RepoID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "repo not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	wtPath := strings.TrimSpace(body.Path)
	if wtPath == "" {
		wtPath = repo.Path
	}
	wt := store.Worktree{
		ID:        uuid.NewString(),
		RepoID:    repo.ID,
		Branch:    strings.TrimSpace(body.Branch),
		Path:      wtPath,
		IsPrimary: body.IsPrimary,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateWorktree(r.Context(), wt); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.broadcastFleetUpdate("worktree", wt)
	writeJSON(w, http.StatusCreated, wt)
}

func (s *Server) handleDeleteWorktree(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteWorktree(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "worktree not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.broadcastFleetUpdate("worktree_deleted", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// --- Cached Machine Facts & Orchestrator Memory API ---

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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
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
