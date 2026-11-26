package controller

import (
	"context"
	"sync"
	"time"
)

// inflightTracker counts running request handlers. Unlike http.Server.Shutdown it
// also covers hijacked connections (the /ws/worker and /ws/client handlers run until
// their socket closes), so shutdown can wait for them before the store is closed.
type inflightTracker struct {
	mu sync.Mutex
	n  int
}

func (t *inflightTracker) add() {
	t.mu.Lock()
	t.n++
	t.mu.Unlock()
}

func (t *inflightTracker) done() {
	t.mu.Lock()
	t.n--
	t.mu.Unlock()
}

func (t *inflightTracker) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n
}

// Close shuts down the server's hub (and its background loops and MCP sessions),
// waiting up to five seconds for background work to drain.
func (s *Server) Close() {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
}

// Shutdown stops the server's hub and MCP sessions and waits for tracked background
// goroutines until ctx expires.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	for _, ms := range s.mcpServers {
		closeMCPServerSessions(ms)
	}
	if s.hub == nil {
		return nil
	}
	return s.hub.Shutdown(ctx)
}

// WaitIdle blocks until no request handler is running — including WebSocket
// handlers that http.Server.Shutdown does not track — or ctx is done. Call it after
// Hub().Close() and http.Server.Shutdown, and before closing the store.
func (s *Server) WaitIdle(ctx context.Context) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for s.inflight.count() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	return nil
}
