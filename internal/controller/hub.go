package controller

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BryanSant/konsert/internal/orchestrator"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/coder/websocket"
)

type wsFrame struct {
	msgType websocket.MessageType
	data    []byte
}

type workerConn struct {
	id     string
	conn   *websocket.Conn
	sendCh chan wsFrame
	cancel context.CancelFunc

	mu          sync.Mutex
	activeTasks map[string]bool
}

// activeCount returns the number of tasks the worker is running, under wc.mu.
func (wc *workerConn) activeCount() int {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	return len(wc.activeTasks)
}

type browserClient struct {
	user   string
	conn   *websocket.Conn
	sendCh chan wsFrame
	cancel context.CancelFunc

	ctx context.Context // connection lifetime; also canceled on hub shutdown

	mu       sync.Mutex
	subs     map[string]bool // subscribed task IDs (PTY)
	chatSubs map[string]bool // chat session IDs this client receives events for
}

// Hub manages connected worker daemons, browser PWA subscribers,
// ref-counted on-demand PTY streaming, task waiters, and stale worker reaping.
type Hub struct {
	store  store.Store
	logger *slog.Logger

	mu            sync.RWMutex
	workers       map[string]*workerConn
	workerMetrics map[string]*protocol.WorkerMetrics
	clients       map[*browserClient]struct{}
	taskWorker    map[string]string                  // taskID -> workerID
	ptySubs       map[string]map[*browserClient]bool // taskID -> client -> needsSnapshot
	waiters       map[string][]chan store.Task
	rpcWaiters    map[string]chan protocol.Envelope
	chatSessions  map[string]*chatSession // per-session turn serialization

	orch *orchestrator.Service

	// Hub lifetime: ctx is canceled by Shutdown; wg tracks background goroutines
	// (scheduler runs, delayed reconciliation checks, chat turns, the reaper).
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool // guarded by mu; set once Shutdown starts so no new goroutines are tracked

	// Scheduler single-flight: schedMu serializes SchedulePending runs and
	// schedRunning/schedDirty coalesce kicks into at most one queued rerun.
	// identityMu serializes worker identity resolution and registration on hello.
	identityMu sync.Mutex

	schedMu      sync.Mutex
	schedRunning atomic.Bool
	schedDirty   atomic.Bool

	// Configurable for tests (defaults to 30s timeout, 10s check interval).
	StaleWorkerTimeout time.Duration
	ReapInterval       time.Duration

	// LostTaskGrace is how long a task may be missing from a connected worker's
	// hello/heartbeat active list before the hub treats it as lost. konsertd
	// re-sends buffered final statuses right after hello, so this must comfortably
	// exceed that flush and a heartbeat round-trip.
	LostTaskGrace time.Duration

	// ChatTurnTimeout bounds one orchestrator chat turn, including the time it
	// waits behind an earlier turn of the same session (default 15m).
	ChatTurnTimeout time.Duration
}

// NewHub constructs a Hub backed by store.Store.
func NewHub(st store.Store, logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	hctx, hcancel := context.WithCancel(context.Background())
	return &Hub{
		ctx:                hctx,
		cancel:             hcancel,
		store:              st,
		logger:             logger,
		workers:            make(map[string]*workerConn),
		workerMetrics:      make(map[string]*protocol.WorkerMetrics),
		clients:            make(map[*browserClient]struct{}),
		taskWorker:         make(map[string]string),
		ptySubs:            make(map[string]map[*browserClient]bool),
		waiters:            make(map[string][]chan store.Task),
		rpcWaiters:         make(map[string]chan protocol.Envelope),
		chatSessions:       make(map[string]*chatSession),
		StaleWorkerTimeout: 150 * time.Second, // > 2x TelemetryThrottle.MaxSilent (60s) so idle daemons never false-reap
		ReapInterval:       15 * time.Second,
		LostTaskGrace:      45 * time.Second,
		ChatTurnTimeout:    15 * time.Minute,
	}
}

// SetOrchestrator attaches the built-in Orchestrator Chat service to the Hub.
func (h *Hub) SetOrchestrator(o *orchestrator.Service) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.orch = o
}

// Close shuts the hub down, waiting up to five seconds for background work to drain.
// Prefer Shutdown when the caller has its own deadline.
func (h *Hub) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.Shutdown(ctx)
}

// Shutdown stops the hub: it cancels the hub lifetime context (stopping the
// reaper, scheduler runs, pending reconciliation checks and in-flight chat
// turns), closes every worker and browser WebSocket with StatusGoingAway (which
// konsertd treats as reconnectable), and waits for tracked background goroutines
// to exit. It returns ctx.Err() if ctx expires first. Safe to call more than once.
func (h *Hub) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.cancel()

	h.mu.Lock()
	workers := make([]*workerConn, 0, len(h.workers))
	for _, wc := range h.workers {
		workers = append(workers, wc)
	}
	clients := make([]*browserClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	for _, wc := range workers {
		if wc.conn != nil {
			_ = wc.conn.Close(websocket.StatusGoingAway, "controller shutting down")
		}
		wc.cancel()
	}
	for _, c := range clients {
		c.cancel()
		_ = c.conn.Close(websocket.StatusGoingAway, "controller shutting down")
	}

	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// goBackground runs fn on a goroutine tracked by the hub WaitGroup, passing the
// hub lifetime context. It returns false (and does not run fn) once Shutdown began.
func (h *Hub) goBackground(fn func(ctx context.Context)) bool {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return false
	}
	h.wg.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.wg.Done()
		fn(h.ctx)
	}()
	return true
}

// ActiveTaskCounts returns a map of workerID -> current active task count.
func (h *Hub) ActiveTaskCounts() map[string]int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	counts := make(map[string]int, len(h.workers))
	for id, wc := range h.workers {
		counts[id] = wc.activeCount()
	}
	return counts
}

// IsWorkerConnected checks if a worker has an active live WebSocket connection.
func (h *Hub) IsWorkerConnected(workerID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.workers[workerID]
	return ok
}

// WorkerMetrics returns the most recently reported metrics for a worker, if any.
func (h *Hub) WorkerMetrics(workerID string) *protocol.WorkerMetrics {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.workerMetrics[workerID]
}
