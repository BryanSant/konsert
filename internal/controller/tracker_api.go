package controller

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/tracker"
	"github.com/BryanSant/syre/internal/tracker/katracker"
)

// trackerFactories returns the adapters Syre ships, by tracker kind.
func trackerFactories() map[string]tracker.Factory {
	return map[string]tracker.Factory{
		katracker.Kind: katracker.Factory,
	}
}

func (s *Server) handleListTrackers(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListTrackers(r.Context(), r.PathValue("id"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"trackers": list})
}

func (s *Server) handleCreateTracker(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind   string         `json:"kind"`
		Config map[string]any `json:"config"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	kind := strings.TrimSpace(body.Kind)
	if _, ok := trackerFactories()[kind]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown tracker kind: " + kind})
		return
	}
	cfg, _ := json.Marshal(body.Config)
	if body.Config == nil {
		cfg = []byte("{}")
	}
	t := store.Tracker{ID: kind + "-" + strings.ToLower(ids.UUID()[:8]), ProjectID: r.PathValue("id"), Kind: kind, ConfigJSON: string(cfg), Enabled: true}
	if err := s.store.CreateTracker(r.Context(), t); err != nil {
		writeWorkError(w, err)
		return
	}
	s.hub.trackers.Kick()
	created, _ := s.store.GetTracker(r.Context(), t.ID)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateTracker(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTracker(r.Context(), r.PathValue("id"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	var body struct {
		Enabled *bool          `json:"enabled"`
		Config  map[string]any `json:"config"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled != nil {
		t.Enabled = *body.Enabled
	}
	if body.Config != nil {
		cfg, _ := json.Marshal(body.Config)
		t.ConfigJSON, t.Cursor = string(cfg), "" // a new config starts a full pull
	}
	if err := s.store.UpdateTracker(r.Context(), t); err != nil {
		writeWorkError(w, err)
		return
	}
	s.hub.trackers.Forget(t.ID)
	s.hub.trackers.Kick()
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleDeleteTracker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteTracker(r.Context(), id); err != nil {
		writeWorkError(w, err)
		return
	}
	s.hub.trackers.Forget(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSyncTracker runs one pull-then-push cycle now and returns the tracker row.
func (s *Server) handleSyncTracker(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTracker(r.Context(), r.PathValue("id"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	syncErr := s.hub.trackers.SyncOne(r.Context(), t)
	t, _ = s.store.GetTracker(r.Context(), t.ID)
	if syncErr != nil && !errors.Is(syncErr, store.ErrNotFound) {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": syncErr.Error(), "tracker": t})
		return
	}
	writeJSON(w, http.StatusOK, t)
}
