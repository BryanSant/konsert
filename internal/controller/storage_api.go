package controller

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/BryanSant/syre/internal/store"
)

// Settings → Storage (SYR-CSCWP): database size and breakdown, retention in plain
// words and as editable periods, Compact now, and the Keep (pin) toggle of tasks and
// chats.

// StorageResponse is the GET /api/storage response.
type StorageResponse struct {
	Stats         store.StorageStats `json:"stats"`
	Retention     RetentionDays      `json:"retention"`
	Defaults      RetentionDays      `json:"defaults"`
	Bounds        retentionBoundsDTO `json:"bounds"`
	RetentionText []string           `json:"retention_text"`
	Compaction    CompactionRun      `json:"compaction"`
}

type retentionBoundsDTO struct {
	DetailDays       store.RetentionBound `json:"detail_days"`
	FleetDays        store.RetentionBound `json:"fleet_days"`
	FleetSummaryDays store.RetentionBound `json:"fleet_summary_days"`
	UsageDays        store.RetentionBound `json:"usage_days"`
	UsageDailyDays   store.RetentionBound `json:"usage_daily_days"`
}

func retentionBounds() retentionBoundsDTO {
	b := store.RetentionBounds
	return retentionBoundsDTO{b.Detail, b.Fleet, b.FleetSummary, b.UsageRaw, b.UsageDaily}
}

func (s *Server) storageResponse(r *http.Request, fresh bool) (StorageResponse, error) {
	stats, err := s.hub.StorageStats(r.Context(), fresh)
	if err != nil {
		return StorageResponse{}, err
	}
	kv, err := s.store.GetAllSettings(r.Context())
	if err != nil {
		return StorageResponse{}, err
	}
	ret := retentionFromSettings(kv)
	return StorageResponse{
		Stats: stats, Retention: ret, Defaults: defaultRetentionDays(), Bounds: retentionBounds(),
		RetentionText: ret.RetentionText(), Compaction: s.hub.CompactionState(),
	}, nil
}

// handleGetStorage serves GET /api/storage[?fresh=1].
func (s *Server) handleGetStorage(w http.ResponseWriter, r *http.Request) {
	fresh, _ := strconv.ParseBool(r.URL.Query().Get("fresh"))
	res, err := s.storageResponse(r, fresh)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleGetCompaction serves GET /api/storage/compact: the pass's progress (cheap, for
// polling while it runs).
func (s *Server) handleGetCompaction(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"compaction": s.hub.CompactionState()})
}

// handlePutRetention serves PUT /api/storage/retention with a full RetentionDays.
func (s *Server) handlePutRetention(w http.ResponseWriter, r *http.Request) {
	var body RetentionDays
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := body.validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	kv := map[string]string{}
	for _, f := range body.fields() {
		kv[f.key] = strconv.Itoa(*f.v)
	}
	if err := s.store.SetSettings(r.Context(), kv); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.recordAudit(r.Context(), store.AuditEvent{
		Action: AuditStorageRetention,
		Message: fmt.Sprintf("Retention: details %d days, fleet timeline %d/%d days, usage %d/%d days",
			body.DetailDays, body.FleetDays, body.FleetSummaryDays, body.UsageDays, body.UsageDailyDays),
		MetaJSON: auditMeta(map[string]any{"retention": body}),
	})
	res, err := s.storageResponse(r, false)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleCompactNow serves POST /api/storage/compact: a full pass in the background.
func (s *Server) handleCompactNow(w http.ResponseWriter, r *http.Request) {
	run, err := s.hub.StartCompaction()
	if errors.Is(err, errCompactionRunning) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "compaction": run})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	s.hub.recordAudit(r.Context(), store.AuditEvent{Action: AuditStorageCompact, Message: "Compact now: started a full retention pass"})
	writeJSON(w, http.StatusAccepted, map[string]any{"compaction": run})
}

// handleKeep serves PUT /api/{tasks|chats}/{id}/keep with {"keep": bool}.
func (s *Server) handleKeep(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Keep bool `json:"keep"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		id := r.PathValue("id")
		verb := "Unpinned"
		if body.Keep {
			verb = "Pinned (Keep)"
		}
		if kind == "task" {
			if err := s.store.SetTaskKeep(r.Context(), id, body.Keep); err != nil {
				writeKeepError(w, err)
				return
			}
			t, err := s.store.GetTask(r.Context(), id)
			if err != nil {
				writeKeepError(w, err)
				return
			}
			s.hub.broadcastFleetUpdate("task", t)
			s.hub.recordAudit(r.Context(), store.AuditEvent{Action: "task.keep", TaskID: t.ID, WorkerID: t.WorkerID, Message: verb + " task " + t.SID})
			writeJSON(w, http.StatusOK, map[string]any{"task": t})
			return
		}
		if err := s.store.SetChatKeep(r.Context(), id, body.Keep); err != nil {
			writeKeepError(w, err)
			return
		}
		cs, err := s.store.GetChatSession(r.Context(), id)
		if err != nil {
			writeKeepError(w, err)
			return
		}
		s.hub.broadcastFleetUpdate("chat_session", cs)
		s.hub.recordAudit(r.Context(), store.AuditEvent{Action: "chat.keep", Message: verb + " chat " + cs.SID,
			MetaJSON: auditMeta(map[string]any{"chat_id": cs.ID})})
		writeJSON(w, http.StatusOK, map[string]any{"chat": cs})
	}
}

func writeKeepError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}
