package controller

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// Listening ports (SYR-G8WA3). Listing ports forks lsof / ss on the machine, so they are
// not in heartbeats: syred sends worker.ports every 5 minutes, and a machine's Details
// asks for fresh ones (GET /api/workers/{id}/ports -> worker.ports.request). The hub
// keeps the latest list per machine in memory for when the daemon cannot answer.

// portsRequestTimeout bounds the on-demand request to the daemon.
const portsRequestTimeout = 8 * time.Second

type portsCache struct {
	mu sync.Mutex
	m  map[string]protocol.WorkerPortsPayload
}

func (c *portsCache) put(workerID string, p protocol.WorkerPortsPayload) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]protocol.WorkerPortsPayload{}
	}
	c.m[workerID] = p
}

func (c *portsCache) get(workerID string) (protocol.WorkerPortsPayload, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.m[workerID]
	return p, ok
}

func (c *portsCache) forget(workerID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, workerID)
}

// onWorkerPorts caches an unsolicited worker.ports.
func (h *Hub) onWorkerPorts(wc *workerConn, env protocol.Envelope) {
	if p, err := protocol.DecodePayload[protocol.WorkerPortsPayload](env); err == nil {
		h.ports.put(wc.id, p)
	}
}

// WorkerPorts asks a connected worker for its listening ports, falling back to the last
// list it sent. fresh reports whether the list came from the worker just now.
func (h *Hub) WorkerPorts(ctx context.Context, workerID string) (p protocol.WorkerPortsPayload, fresh bool, err error) {
	rctx, cancel := context.WithTimeout(ctx, portsRequestTimeout)
	defer cancel()
	env, err := h.RequestWorker(rctx, workerID, protocol.MsgWorkerPortsRequest, struct{}{})
	if err == nil {
		p, err = protocol.DecodePayload[protocol.WorkerPortsPayload](env)
		if err == nil {
			h.ports.put(workerID, p)
			return p, true, nil
		}
	}
	if cached, ok := h.ports.get(workerID); ok {
		return cached, false, nil
	}
	return protocol.WorkerPortsPayload{}, false, err
}

// handleGetWorkerPorts serves GET /api/workers/{id}/ports.
func (s *Server) handleGetWorkerPorts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, fresh, err := s.hub.WorkerPorts(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	if p.Ports == nil {
		p.Ports = []protocol.ListeningPort{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"worker_id":    id,
		"ports":        p.Ports,
		"collected_at": p.CollectedAt,
		"fresh":        fresh,
	})
}
