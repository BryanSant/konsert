package controller

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

// defaultHostWakeReason is sent to konsertd when the browser gives no reason.
const defaultHostWakeReason = "Konsert Keep Awake"

// maxHostWakeReasonLen bounds the reason forwarded to systemd-inhibit --why.
const maxHostWakeReasonLen = 200

// WakeLockDesire is the hub's wanted host wake lock state.
type WakeLockDesire struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
}

// WorkerWakeLockView is the per-worker wake lock state in WorkerStatusView.
type WorkerWakeLockView struct {
	// Supported is true once the worker has reported host.wakelock.state at least once
	// (konsertd builds without wake lock support never do).
	Supported bool `json:"supported"`
	// Desired is the effective hub-side wish (per-worker override, else the fleet-wide
	// setting); nil when nobody asked for anything.
	Desired *bool `json:"desired,omitzero"`
	protocol.HostWakeLockState
}

// hostWakeLocks keeps desired and reported host wake lock state in memory. Desired state
// does not survive a hub restart; reported state is re-sent by each worker after hello.
type hostWakeLocks struct {
	mu        sync.Mutex
	fleet     *WakeLockDesire                       // fleet-wide wish; nil = none
	overrides map[string]WakeLockDesire             // per-worker wishes, win over fleet
	states    map[string]protocol.HostWakeLockState // last report per worker
	// synced records the connection whose first state report was reconciled, so the
	// desired state is re-applied once per connection (not on every report, which would
	// loop when the worker cannot take the lock).
	synced map[string]*workerConn
}

func (w *hostWakeLocks) initLocked() {
	if w.overrides == nil {
		w.overrides = make(map[string]WakeLockDesire)
		w.states = make(map[string]protocol.HostWakeLockState)
		w.synced = make(map[string]*workerConn)
	}
}

// desiredLocked returns the effective desire for workerID, or nil.
func (w *hostWakeLocks) desiredLocked(workerID string) *WakeLockDesire {
	if d, ok := w.overrides[workerID]; ok {
		return &d
	}
	if w.fleet != nil {
		d := *w.fleet
		return &d
	}
	return nil
}

// workerWakeLockView returns the wake lock view for workerID, or nil if the hub has
// neither a report from it nor a wish for it.
func (h *Hub) workerWakeLockView(workerID string) *WorkerWakeLockView {
	h.wake.mu.Lock()
	defer h.wake.mu.Unlock()
	h.wake.initLocked()
	st, reported := h.wake.states[workerID]
	d := h.wake.desiredLocked(workerID)
	if !reported && d == nil {
		return nil
	}
	v := &WorkerWakeLockView{Supported: reported, HostWakeLockState: st}
	if d != nil {
		enabled := d.Enabled
		v.Desired = &enabled
	}
	return v
}

// withWakeLock attaches the wake lock view to a worker fleet.update payload.
func (h *Hub) withWakeLock(item any) any {
	switch v := item.(type) {
	case WorkerStatusView:
		v.WakeLock = h.workerWakeLockView(v.ID)
		return v
	case *WorkerStatusView:
		if v != nil {
			cp := *v
			cp.WakeLock = h.workerWakeLockView(v.ID)
			return &cp
		}
	}
	return item
}

// onHostWakeLockState records a worker's report and, on the first report of a
// connection, re-applies the hub's desired state (reconnect or hub restart).
func (h *Hub) onHostWakeLockState(ctx context.Context, wc *workerConn, env protocol.Envelope) {
	st, err := protocol.DecodePayload[protocol.HostWakeLockState](env)
	if err != nil {
		return
	}
	h.wake.mu.Lock()
	h.wake.initLocked()
	h.wake.states[wc.id] = st
	var resend *WakeLockDesire
	if h.wake.synced[wc.id] != wc {
		h.wake.synced[wc.id] = wc
		if d := h.wake.desiredLocked(wc.id); d != nil && d.Enabled != st.Active {
			resend = d
		}
	}
	h.wake.mu.Unlock()

	if resend != nil {
		h.logger.Info("re-applying host wake lock after reconnect", "worker_id", wc.id, "enabled", resend.Enabled)
		h.sendWorkerJSON(wc, protocol.MsgHostWakeLock, protocol.HostWakeLockPayload{Enabled: resend.Enabled, Reason: resend.Reason})
	}
	h.broadcastWorkerView(ctx, wc.id)
}

// SetFleetWakeLock records a fleet-wide wish (clearing per-worker overrides) and sends it
// to every connected worker. It returns the IDs of the workers it was sent to.
func (h *Hub) SetFleetWakeLock(ctx context.Context, d WakeLockDesire) []string {
	h.wake.mu.Lock()
	h.wake.initLocked()
	h.wake.fleet = &d
	clear(h.wake.overrides)
	h.wake.mu.Unlock()

	h.mu.RLock()
	conns := make([]*workerConn, 0, len(h.workers))
	for _, wc := range h.workers {
		conns = append(conns, wc)
	}
	h.mu.RUnlock()

	sent := make([]string, 0, len(conns))
	for _, wc := range conns {
		if h.sendWorkerJSON(wc, protocol.MsgHostWakeLock, protocol.HostWakeLockPayload{Enabled: d.Enabled, Reason: d.Reason}) {
			sent = append(sent, wc.id)
		}
	}
	sort.Strings(sent)
	// Offline workers' desired flag changed too; refresh every row.
	if workers, err := h.store.ListWorkers(ctx); err == nil {
		for _, w := range workers {
			h.broadcastWorkerView(ctx, w.ID)
		}
	}
	return sent
}

// SetWorkerWakeLock records a per-worker wish and sends it if the worker is connected
// (otherwise it is applied when the worker next reports its state). It reports whether
// the request was delivered to a live connection.
func (h *Hub) SetWorkerWakeLock(ctx context.Context, workerID string, d WakeLockDesire) bool {
	h.wake.mu.Lock()
	h.wake.initLocked()
	h.wake.overrides[workerID] = d
	h.wake.mu.Unlock()

	h.mu.RLock()
	wc := h.workers[workerID]
	h.mu.RUnlock()
	sent := false
	if wc != nil {
		sent = h.sendWorkerJSON(wc, protocol.MsgHostWakeLock, protocol.HostWakeLockPayload{Enabled: d.Enabled, Reason: d.Reason})
	}
	h.broadcastWorkerView(ctx, workerID)
	return sent
}

// FleetWakeLock returns the fleet-wide wish (nil if none).
func (h *Hub) FleetWakeLock() *WakeLockDesire {
	h.wake.mu.Lock()
	defer h.wake.mu.Unlock()
	if h.wake.fleet == nil {
		return nil
	}
	d := *h.wake.fleet
	return &d
}

// broadcastWorkerView pushes a fresh fleet.update for one worker.
func (h *Hub) broadcastWorkerView(ctx context.Context, workerID string) {
	w, err := h.store.GetWorker(ctx, workerID)
	if err != nil {
		return
	}
	h.mu.RLock()
	wc := h.workers[workerID]
	h.mu.RUnlock()
	active := 0
	if wc != nil {
		active = wc.activeCount()
	}
	view := toWorkerView(w, active, h.WorkerMetrics(workerID))
	if view.Status == "online" && wc == nil {
		view.Status = "offline"
	}
	h.broadcastFleetUpdate("worker", view)
}

// --- REST ---

type wakeLockRequest struct {
	Enabled *bool  `json:"enabled"`
	Reason  string `json:"reason"`
}

func (r wakeLockRequest) desire() (WakeLockDesire, bool) {
	if r.Enabled == nil {
		return WakeLockDesire{}, false
	}
	reason := strings.TrimSpace(r.Reason)
	if reason == "" {
		reason = defaultHostWakeReason
	}
	if len(reason) > maxHostWakeReasonLen {
		reason = strings.ToValidUTF8(reason[:maxHostWakeReasonLen], "")
	}
	return WakeLockDesire{Enabled: *r.Enabled, Reason: reason}, true
}

// WakeLockStatus is the GET /api/wakelock response.
type WakeLockStatus struct {
	Fleet   *WakeLockDesire          `json:"fleet"`
	Workers []WorkerWakeLockStatusID `json:"workers"`
}

// WorkerWakeLockStatusID is one worker's wake lock view keyed by ID.
type WorkerWakeLockStatusID struct {
	WorkerID string `json:"worker_id"`
	Online   bool   `json:"online"`
	*WorkerWakeLockView
}

func (s *Server) registerWakeLockRoutes() {
	s.mux.Handle("GET /api/wakelock", s.requireBrowserAuth(s.handleGetWakeLock))
	s.mux.Handle("POST /api/wakelock", s.requireBrowserAuth(s.handleSetFleetWakeLock))
	s.mux.Handle("POST /api/workers/{id}/wakelock", s.requireBrowserAuth(s.handleSetWorkerWakeLock))
}

func (s *Server) wakeLockStatus(ctx context.Context) (WakeLockStatus, error) {
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		return WakeLockStatus{}, err
	}
	out := WakeLockStatus{Fleet: s.hub.FleetWakeLock(), Workers: make([]WorkerWakeLockStatusID, 0, len(workers))}
	for _, w := range workers {
		out.Workers = append(out.Workers, WorkerWakeLockStatusID{
			WorkerID:           w.ID,
			Online:             s.hub.IsWorkerConnected(w.ID),
			WorkerWakeLockView: s.hub.workerWakeLockView(w.ID),
		})
	}
	return out, nil
}

func (s *Server) handleGetWakeLock(w http.ResponseWriter, r *http.Request) {
	st, err := s.wakeLockStatus(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// wakeLockAuditMessage summarizes a Keep Awake change for the audit log.
func wakeLockAuditMessage(scope string, d WakeLockDesire) string {
	if d.Enabled {
		return "Enabled Keep Awake (" + scope + "): " + d.Reason
	}
	return "Disabled Keep Awake (" + scope + ")"
}

// handleSetFleetWakeLock applies {enabled, reason} to every host (POST /api/wakelock).
func (s *Server) handleSetFleetWakeLock(w http.ResponseWriter, r *http.Request) {
	var body wakeLockRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	d, ok := body.desire()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled is required"})
		return
	}
	sent := s.hub.SetFleetWakeLock(r.Context(), d)
	s.hub.recordAudit(r.Context(), store.AuditEvent{
		Action:   AuditWakeLockFleet,
		Message:  wakeLockAuditMessage("fleet", d),
		MetaJSON: auditMeta(map[string]any{"enabled": d.Enabled, "reason": d.Reason, "sent_to": len(sent)}),
	})
	st, err := s.wakeLockStatus(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sent_to": sent, "status": st})
}

// handleSetWorkerWakeLock applies {enabled, reason} to one host
// (POST /api/workers/{id}/wakelock).
func (s *Server) handleSetWorkerWakeLock(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body wakeLockRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	d, ok := body.desire()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled is required"})
		return
	}
	if _, err := s.store.GetWorker(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "worker not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	sent := s.hub.SetWorkerWakeLock(r.Context(), id, d)
	s.hub.recordAudit(r.Context(), store.AuditEvent{
		Action:   AuditWakeLockHost,
		WorkerID: id,
		Message:  wakeLockAuditMessage("host", d),
		MetaJSON: auditMeta(map[string]any{"enabled": d.Enabled, "reason": d.Reason, "sent": sent}),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"worker_id": id,
		"sent":      sent,
		"wake_lock": s.hub.workerWakeLockView(id),
	})
}
