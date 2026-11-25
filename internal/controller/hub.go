package controller

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BryanSant/konsert/internal/mcprelay"
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

	// mcp serves the per-chat MCP requests konsertd relays over this link (KON-DTN92).
	mcp *mcprelay.Remote

	// loopback is set when the daemon connected from a loopback address; with name,
	// hostGroup and hostname from its hello, it identifies a daemon on the hub's host.
	loopback                  bool
	name, hostGroup, hostname string

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
	user string
	// deviceID is the device credential the socket authenticated with ("" for the
	// recovery key, Tailscale identity or --no-auth); revoking the device closes it.
	deviceID string
	conn     *websocket.Conn
	sendCh   chan wsFrame
	cancel   context.CancelFunc

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
	agentChats    map[string]*agentChatRT // agent chat runtime state (seq, MCP token, ask_user)
	// pendingCauses holds the cause of an intervention sent to a worker (cancel, suspend,
	// resume) until the worker reports the resulting status; keyed by task id or "chat:"+id.
	pendingCauses map[string]pendingCause
	// delivery batches fleet events for the orchestrator chat (see delivery.go).
	delivery deliveryState
	// operatorCommands de-duplicates worker.operator_command reports by command id.
	operatorCommands seenCommands

	// wake holds desired and reported host wake lock (Keep Awake) state.
	wake hostWakeLocks

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

	schedMu sync.Mutex
	// reviewMu serializes review read-modify-writes (reviews.go).
	reviewMu     sync.Mutex
	schedRunning atomic.Bool
	schedDirty   atomic.Bool

	// Configurable for tests (defaults to 30s timeout, 10s check interval).
	StaleWorkerTimeout time.Duration
	ReapInterval       time.Duration

	// LinkQuiet and LinkDead override the worker link liveness intervals (linkwatch:
	// ping after 15 s quiet, dead after 45 s silent). Tests shorten them.
	LinkQuiet time.Duration
	LinkDead  time.Duration

	// LostTaskGrace is how long a task may be missing from a connected worker's
	// hello/heartbeat active list before the hub treats it as lost. konsertd
	// re-sends buffered final statuses right after hello, so this must comfortably
	// exceed that flush and a heartbeat round-trip.
	LostTaskGrace time.Duration

	// ChatTurnTimeout bounds one orchestrator chat turn, including the time it
	// waits behind an earlier turn of the same session (default 15m).
	ChatTurnTimeout time.Duration

	// AskUserTimeout bounds how long the ask_user MCP tool waits (default 30m).
	AskUserTimeout time.Duration
	// hostname returns this machine's hostname (the default orchestrator prefers
	// a worker on the hub's own machine); replaceable in tests.
	hostname func() string
	// chatFleetBackend, when set, serves the fleet tools on per-chat MCP endpoints
	// of chats with fleet_tools.
	chatFleetBackend FleetBackend
	// chatMCP serves a chat's MCP endpoint for a request konsertd relayed over
	// workerID's link (the Server's serveRelayedChatMCP); nil answers 503.
	chatMCP func(w http.ResponseWriter, r *http.Request, workerID string)
	// version is reported by per-chat MCP servers.
	version string

	// questions is the question queue's in-memory state (questions.go).
	questions questionState
	// activity is the latest activity of every session (activity.go); heat is every
	// worker's smoothed load score (heat.go).
	activity activityState
	heat     heatState
	ports    portsCache
	// retention is the history compaction state (retention.go). RetentionEvery is how
	// often a bounded pass runs (0 disables the loop) after RetentionStartDelay;
	// RetentionBatch and RetentionPause size the batches and the yield between them;
	// retentionNow is the compaction clock. Tests change them before using the hub.
	retention           retentionState
	RetentionEvery      time.Duration
	RetentionStartDelay time.Duration
	RetentionBatch      int
	RetentionPause      time.Duration
	retentionNow        func() time.Time
	// shellPolicy is the dangling-shell remedy state (shell_policy.go), applied every
	// ShellPolicyEvery (0 disables the loop) on the shellNow clock.
	shellPolicy      shellPolicyState
	ShellPolicyEvery time.Duration
	shellNow         func() time.Time
	// KaPath is the ka CLI used to resolve ticket references ("" = ka on PATH).
	KaPath string
	// kaShowFn and ghPRFn replace ka/gh in tests.
	kaShowFn func(ctx context.Context, id string) ([]byte, error)
	ghPRFn   func(ctx context.Context, repo, number string) ([]byte, error)
	// restarts is the automatic restart state of hub-started daemons the hub cannot
	// attach to (restart.go); the Server sets it, machine views show it.
	restarts *restartTracker
}

// NewHub constructs a Hub backed by store.Store.
func NewHub(st store.Store, logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	hctx, hcancel := context.WithCancel(context.Background())
	h := &Hub{
		ctx:                 hctx,
		cancel:              hcancel,
		store:               st,
		logger:              logger,
		workers:             make(map[string]*workerConn),
		workerMetrics:       make(map[string]*protocol.WorkerMetrics),
		clients:             make(map[*browserClient]struct{}),
		taskWorker:          make(map[string]string),
		ptySubs:             make(map[string]map[*browserClient]bool),
		waiters:             make(map[string][]chan store.Task),
		rpcWaiters:          make(map[string]chan protocol.Envelope),
		chatSessions:        make(map[string]*chatSession),
		agentChats:          make(map[string]*agentChatRT),
		hostname:            func() string { h, _ := os.Hostname(); return h },
		StaleWorkerTimeout:  150 * time.Second, // > 2x the longest heartbeat gap (60 s) so idle daemons never false-reap
		ReapInterval:        15 * time.Second,
		LostTaskGrace:       45 * time.Second,
		ChatTurnTimeout:     15 * time.Minute,
		RetentionEvery:      retentionEvery,
		RetentionStartDelay: retentionStartDelay,
		RetentionBatch:      retentionBatch,
		RetentionPause:      retentionPause,
		retentionNow:        time.Now,
		ShellPolicyEvery:    shellPolicyEvery,
		shellNow:            time.Now,
	}
	h.questions.init()
	// Machine history: periodic flush, hourly roll-up and retention (heat_history.go).
	h.goBackground(h.vitalsLoop)
	// History retention: bounded compaction passes (retention.go).
	h.goBackground(h.retentionLoop)
	// Shells left running by finished sessions: remind, ask or stop (shell_policy.go).
	h.goBackground(h.shellPolicyLoop)
	return h
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
	// Keep the in-progress history buckets across a restart.
	h.flushVitals(ctx, true)
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
