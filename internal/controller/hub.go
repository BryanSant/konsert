package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BryanSant/konsert/internal/orchestrator"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/coder/websocket"
	"github.com/google/uuid"
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

// RunReaper periodically marks workers with missed heartbeats offline and applies orphan policies.
// It returns when ctx is canceled or the hub shuts down.
func (h *Hub) RunReaper(ctx context.Context) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.wg.Add(1)
	h.mu.Unlock()
	defer h.wg.Done()
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	defer context.AfterFunc(h.ctx, stop)()

	interval := h.ReapInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.reapStaleWorkers(ctx)
		}
	}
}

func (h *Hub) reapStaleWorkers(ctx context.Context) {
	timeout := h.StaleWorkerTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cutoff := time.Now().UTC().Add(-timeout)
	staleIDs, err := h.store.MarkStaleWorkersOffline(ctx, cutoff)
	if err != nil || len(staleIDs) == 0 {
		return
	}
	for _, wid := range staleIDs {
		h.logger.Warn("marking stale worker offline", "worker_id", wid)
		h.mu.Lock()
		if wc, ok := h.workers[wid]; ok {
			wc.cancel()
			delete(h.workers, wid)
		}
		h.mu.Unlock()

		if w, err := h.store.GetWorker(ctx, wid); err == nil {
			h.broadcastFleetUpdate("worker", w)
		}
		h.handleOrphanTasks(ctx, wid)
	}
}

func (h *Hub) handleOrphanTasks(ctx context.Context, workerID string) {
	tasks, err := h.store.ListTasks(ctx, "", workerID, 200)
	if err != nil {
		return
	}
	requeued := false
	defer func() {
		if requeued {
			h.kickScheduler()
		}
	}()
	for _, t := range tasks {
		if protocol.IsTerminalTaskStatus(t.Status) || t.Status == protocol.TaskStatusQueued {
			continue
		}
		h.forgetTask(t.ID)
		// Passively discovered external sessions should never turn red ("failed") on daemon disconnect.
		if t.Origin == protocol.TaskOriginExternal {
			_ = h.store.UpdateTaskStatus(ctx, t.ID, protocol.TaskStatusCompleted, "external session detached", "", nil)
			if updated, err := h.store.GetTask(ctx, t.ID); err == nil {
				h.broadcastFleetUpdate("task", updated)
			}
			continue
		}
		if t.OrphanPolicy == protocol.OrphanPolicyRequeue {
			// Keep the dead worker only when the task was explicitly pinned to it;
			// otherwise the scheduler may place it on any matching worker.
			_, _, pinned := h.taskStartSpec(ctx, t)
			if ok, _ := h.store.RequeueTask(ctx, t.ID, "requeued after worker disconnect", pinned); ok {
				requeued = true
			}
			if updated, err := h.store.GetTask(ctx, t.ID); err == nil {
				h.broadcastFleetUpdate("task", updated)
			}
			continue
		}
		code := -1
		_ = h.store.UpdateTaskStatus(ctx, t.ID, protocol.TaskStatusFailed, "worker went offline while task was active", "", &code)
		if updated, err := h.store.GetTask(ctx, t.ID); err == nil {
			h.notifyTaskDone(updated)
			h.broadcastFleetUpdate("task", updated)
		}
	}
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

// DisconnectWorker closes any active WebSocket session for workerID with WSCloseWorkerRemoved
// so the remote konsertd exits cleanly instead of auto-reconnecting.
func (h *Hub) DisconnectWorker(workerID string) {
	h.mu.Lock()
	wc, ok := h.workers[workerID]
	delete(h.workers, workerID)
	delete(h.workerMetrics, workerID)
	h.mu.Unlock()
	if ok && wc != nil {
		// Send WebSocket close frame 4001 BEFORE canceling connCtx so coder/websocket doesn't hard-abort TCP with EOF.
		_ = wc.conn.Close(websocket.StatusCode(protocol.WSCloseWorkerRemoved), "worker removed from fleet")
		wc.cancel()
	}
}

// ServeWorkerConn handles a single authenticated konsertd WebSocket connection until disconnect.
func (h *Hub) ServeWorkerConn(ctx context.Context, conn *websocket.Conn) {
	conn.SetReadLimit(20 * 1024 * 1024)
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(h.ctx, cancel)()

	// Expect worker.hello as the first message.
	helloCtx, helloCancel := context.WithTimeout(connCtx, 15*time.Second)
	msgType, raw, err := conn.Read(helloCtx)
	helloCancel()
	if err != nil || msgType != websocket.MessageText {
		_ = conn.Close(websocket.StatusProtocolError, "expected worker.hello")
		return
	}
	env, err := protocol.DecodeEnvelope(raw)
	if err != nil || env.Type != protocol.MsgWorkerHello {
		_ = conn.Close(websocket.StatusProtocolError, "expected worker.hello")
		return
	}
	hello, err := protocol.DecodePayload[protocol.WorkerHelloPayload](env)
	if err != nil || hello.Worker.ID == "" {
		_ = conn.Close(websocket.StatusProtocolError, "invalid worker.hello")
		return
	}

	// Resolve the canonical worker row (machine UUID first, legacy tuple second) and
	// decide whether this connection may take over an existing one. identityMu keeps
	// two simultaneous hellos from adopting the same legacy row.
	h.identityMu.Lock()
	hello.Worker.ID = h.resolveWorkerID(ctx, hello.Worker)
	if !h.supersedeExistingConn(ctx, hello.Worker.ID) {
		h.identityMu.Unlock()
		h.logger.Warn("rejecting duplicate konsertd connection; the existing one is still alive",
			"worker_id", hello.Worker.ID, "machine_id", hello.Worker.MachineID, "name", hello.Worker.Name)
		_ = conn.Close(websocket.StatusTryAgainLater, "another konsertd with this identity is connected")
		return
	}

	wc := &workerConn{
		id:          hello.Worker.ID,
		conn:        conn,
		sendCh:      make(chan wsFrame, 512),
		cancel:      cancel,
		activeTasks: make(map[string]bool),
	}

	// Register worker in SQLite and Hub.
	now := time.Now().UTC()
	factsJSON := ""
	var factsUpdatedAt *time.Time
	if hello.Worker.Facts != nil {
		if b, mErr := json.Marshal(hello.Worker.Facts); mErr == nil {
			factsJSON = string(b)
			tNow := now
			factsUpdatedAt = &tNow
		}
	}
	wModel := store.Worker{
		ID:             hello.Worker.ID,
		Name:           hello.Worker.Name,
		HostGroup:      hello.Worker.HostGroup,
		OS:             hello.Worker.OS,
		Arch:           hello.Worker.Arch,
		Isolation:      hello.Worker.Isolation,
		PermissionMode: store.PermissionMode(hello.Worker.PermissionMode),
		Agents:         hello.Worker.Agents,
		Tags:           hello.Worker.Tags,
		MaxConcurrent:  hello.Worker.MaxConcurrent,
		Status:         "online",
		LastSeenAt:     now,
		FactsJSON:      factsJSON,
		FactsUpdatedAt: factsUpdatedAt,
	}
	if err := h.store.UpsertWorker(ctx, wModel); err != nil {
		h.logger.Error("upsert worker failed", "worker_id", wModel.ID, "err", err)
	}
	if mid := strings.TrimSpace(hello.Worker.MachineID); mid != "" {
		if err := h.store.SetWorkerMachineID(ctx, wModel.ID, mid); err != nil {
			h.logger.Error("recording worker machine id failed", "worker_id", wModel.ID, "err", err)
		}
	}
	removedDups, dErr := h.store.DeduplicateWorkers(ctx)

	h.mu.Lock()
	prevConn := h.workers[wModel.ID]
	h.workers[wModel.ID] = wc
	h.mu.Unlock()
	h.identityMu.Unlock()

	if dErr == nil {
		for _, dupID := range removedDups {
			h.broadcastFleetUpdate("worker_deleted", dupID)
		}
	}
	if prevConn != nil && prevConn != wc {
		// The previous connection failed its liveness ping, so it is a dead or
		// half-open socket. Close it with a normal, reconnectable code: 4001
		// (WSCloseWorkerRemoved) would make konsertd exit for good. The close
		// handshake can block for seconds on a dead peer, so run it off this path.
		go func(old *workerConn) {
			if old.conn != nil {
				_ = old.conn.Close(websocket.StatusNormalClosure, "superseded by newer konsertd connection")
			}
			old.cancel()
		}(prevConn)
	}

	// Re-adopt in-flight tasks reported by worker.hello (controller restart resilience),
	// but never take over a task the store assigns to a different worker.
	readopted := 0
	for _, at := range hello.ActiveTasks {
		t, err := h.store.GetTask(ctx, at.TaskID)
		if err == nil && t.WorkerID != "" && t.WorkerID != wModel.ID {
			h.logger.Warn("ignoring hello.active_tasks entry owned by another worker",
				"task_id", at.TaskID, "worker_id", wModel.ID, "owner", t.WorkerID)
			continue
		}
		// The worker is running it either way, so it counts against capacity.
		wc.mu.Lock()
		wc.activeTasks[at.TaskID] = true
		wc.mu.Unlock()
		if err != nil {
			continue
		}
		readopted++

		h.mu.Lock()
		h.taskWorker[at.TaskID] = wModel.ID
		hasSubs := len(h.ptySubs[at.TaskID]) > 0
		h.mu.Unlock()

		_ = h.store.UpdateTaskAssignment(ctx, at.TaskID, wModel.ID, at.EffectiveDir)
		st := at.Status
		if st == "" {
			st = protocol.TaskStatusRunning
		}
		_ = h.store.UpdateTaskStatus(ctx, at.TaskID, st, "", "", nil)
		if updated, err := h.store.GetTask(ctx, at.TaskID); err == nil {
			h.broadcastFleetUpdate("task", updated)
		}
		if hasSubs {
			h.sendWorkerJSON(wc, protocol.MsgPtySubscribe, protocol.PtySubscribePayload{TaskID: at.TaskID})
		}
	}

	// Clean up stale non-terminal external sessions from prior daemon lifetimes;
	// SessionDiscovery immediately re-reports any still-running external agents right after worker.hello.
	// Hub-managed tasks this worker owns but did not list are checked again after
	// LostTaskGrace (the worker may still deliver their final status from its outbox).
	if prevTasks, err := h.store.ListTasks(ctx, "", wModel.ID, 200); err == nil {
		reported := make(map[string]bool, len(hello.ActiveTasks))
		for _, at := range hello.ActiveTasks {
			reported[at.TaskID] = true
		}
		var missing []string
		for _, pt := range prevTasks {
			if protocol.IsTerminalTaskStatus(pt.Status) {
				continue
			}
			if pt.Origin == protocol.TaskOriginExternal {
				_ = h.store.UpdateTaskStatus(ctx, pt.ID, protocol.TaskStatusCompleted, "session ended before reconnect", "", nil)
				continue
			}
			if pt.Status != protocol.TaskStatusQueued && !reported[pt.ID] {
				missing = append(missing, pt.ID)
			}
		}
		h.scheduleLostTaskCheck(wc, missing)
	}

	h.logger.Info("worker registered",
		"worker_id", wModel.ID,
		"name", wModel.Name,
		"agents", wModel.Agents,
		"readopted_tasks", readopted,
	)
	h.broadcastFleetUpdate("worker", WorkerStatusView{
		ID:             wModel.ID,
		Name:           wModel.Name,
		HostGroup:      wModel.HostGroup,
		OS:             wModel.OS,
		Arch:           wModel.Arch,
		Isolation:      wModel.Isolation,
		PermissionMode: string(wModel.PermissionMode),
		Agents:         wModel.Agents,
		Tags:           wModel.Tags,
		MaxConcurrent:  wModel.MaxConcurrent,
		Status:         wModel.Status,
		ActiveTasks:    wc.activeCount(),
		LastSeenAt:     wModel.LastSeenAt,
		Facts:          hello.Worker.Facts,
	})

	// Drain any queued tasks that can now run on this worker.
	h.kickScheduler()

	// Writer goroutine
	go func() {
		for {
			select {
			case <-connCtx.Done():
				return
			case f := <-wc.sendCh:
				wCtx, wCancel := context.WithTimeout(connCtx, 10*time.Second)
				err := conn.Write(wCtx, f.msgType, f.data)
				wCancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Read loop
	for {
		mt, data, err := conn.Read(connCtx)
		if err != nil {
			break
		}
		switch mt {
		case websocket.MessageBinary:
			h.handleWorkerBinary(data)
		case websocket.MessageText:
			env, decErr := protocol.DecodeEnvelope(data)
			if decErr != nil {
				continue
			}
			h.handleWorkerMessage(ctx, wc, env)
		}
	}

	// Disconnect cleanup: remove active conn, keep DB status until reaper marks offline
	// OR if no active tasks, mark offline promptly while preserving restart re-adoption window.
	h.mu.Lock()
	if h.workers[wModel.ID] == wc {
		delete(h.workers, wModel.ID)
	}
	h.mu.Unlock()
	h.logger.Info("worker disconnected", "worker_id", wModel.ID)
}

// scheduleLostTaskCheck re-examines taskIDs after LostTaskGrace and reconciles
// any that the worker still does not report as active.
func (h *Hub) scheduleLostTaskCheck(wc *workerConn, taskIDs []string) {
	if len(taskIDs) == 0 {
		return
	}
	grace := h.LostTaskGrace
	if grace <= 0 {
		grace = 45 * time.Second
	}
	ids := append([]string(nil), taskIDs...)
	h.goBackground(func(ctx context.Context) {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		h.reconcileLostTasks(ctx, wc, ids)
	})
}

// reconcileLostTasks marks tasks lost that a still-connected worker no longer
// reports and that have not reached a terminal status. Tasks with the requeue
// orphan policy go back to the queue; others fail with exit code -1. If the
// worker disconnected meanwhile, the reaper's orphan handling owns the tasks.
func (h *Hub) reconcileLostTasks(ctx context.Context, wc *workerConn, taskIDs []string) {
	h.mu.RLock()
	current := h.workers[wc.id] == wc
	h.mu.RUnlock()
	if !current {
		return
	}
	kick := false
	for _, id := range taskIDs {
		wc.mu.Lock()
		active := wc.activeTasks[id]
		wc.mu.Unlock()
		if active {
			continue
		}
		t, err := h.store.GetTask(ctx, id)
		if err != nil || t.WorkerID != wc.id || t.Origin == protocol.TaskOriginExternal ||
			t.Status == protocol.TaskStatusQueued || protocol.IsTerminalTaskStatus(t.Status) {
			continue
		}
		h.logger.Warn("worker no longer reports task; reconciling as lost", "task_id", id, "worker_id", wc.id, "status", t.Status)
		h.forgetTask(id)
		if t.OrphanPolicy == protocol.OrphanPolicyRequeue {
			_, _, pinned := h.taskStartSpec(ctx, t)
			if ok, _ := h.store.RequeueTask(ctx, id, "requeued: worker no longer reports the task", pinned); ok {
				kick = true
			}
			if updated, err := h.store.GetTask(ctx, id); err == nil {
				h.broadcastFleetUpdate("task", updated)
			}
			continue
		}
		code := -1
		_ = h.store.UpdateTaskStatus(ctx, id, protocol.TaskStatusFailed, "task lost: worker no longer reports it", "", &code)
		if updated, err := h.store.GetTask(ctx, id); err == nil {
			h.notifyTaskDone(updated)
			h.broadcastFleetUpdate("task", updated)
		}
		kick = true // capacity freed
	}
	if kick {
		h.kickScheduler()
	}
}

// supersedePingTimeout bounds the liveness ping sent to an existing connection
// when a new hello arrives for the same worker.
const supersedePingTimeout = 3 * time.Second

// resolveWorkerID returns the canonical worker ID for a hello. Workers that send
// a stable MachineID are matched on it first; only rows that never recorded a
// machine ID can be adopted through the hello ID or the legacy
// (name, host_group, os, arch, isolation) tuple. That keeps two hosts sharing a
// hostname (cloned VMs) apart. Workers that send no MachineID keep the legacy
// behaviour. The caller holds h.identityMu.
func (h *Hub) resolveWorkerID(ctx context.Context, info protocol.WorkerInfo) string {
	workers, err := h.store.ListWorkers(ctx)
	if err != nil {
		return info.ID
	}
	mids, err := h.store.ListWorkerMachineIDs(ctx)
	if err != nil {
		mids = map[string]string{}
	}
	exists := make(map[string]bool, len(workers))
	for _, ew := range workers {
		exists[ew.ID] = true
	}
	tupleMatch := func(ew store.Worker) bool {
		return strings.EqualFold(ew.Name, info.Name) &&
			strings.EqualFold(ew.HostGroup, info.HostGroup) &&
			strings.EqualFold(ew.OS, info.OS) &&
			strings.EqualFold(ew.Arch, info.Arch) &&
			strings.EqualFold(ew.Isolation, info.Isolation)
	}
	adoptLegacyTuple := func() (string, bool) {
		for _, ew := range workers {
			if mids[ew.ID] == "" && tupleMatch(ew) {
				return ew.ID, true
			}
		}
		return "", false
	}

	if mid := strings.TrimSpace(info.MachineID); mid != "" {
		for _, ew := range workers {
			if mids[ew.ID] == mid {
				return ew.ID
			}
		}
		if exists[info.ID] && mids[info.ID] == "" {
			return info.ID
		}
		if id, ok := adoptLegacyTuple(); ok {
			return id
		}
		if !exists[info.ID] {
			return info.ID
		}
		// The hello ID already belongs to a different machine (e.g. a cloned VM
		// that derived the same deterministic ID): key this one on its machine UUID.
		return mid
	}

	if exists[info.ID] {
		return info.ID
	}
	if id, ok := adoptLegacyTuple(); ok {
		return id
	}
	return info.ID
}

// supersedeExistingConn reports whether a new connection may register as
// workerID. If a connection is already registered, it is pinged: a live one
// keeps its place (the newcomer is a duplicate daemon and should retry later),
// while a dead or half-open one is replaced.
func (h *Hub) supersedeExistingConn(ctx context.Context, workerID string) bool {
	h.mu.RLock()
	prev := h.workers[workerID]
	h.mu.RUnlock()
	if prev == nil || prev.conn == nil {
		return true
	}
	pingCtx, cancel := context.WithTimeout(ctx, supersedePingTimeout)
	defer cancel()
	return prev.conn.Ping(pingCtx) != nil
}

func (h *Hub) handleWorkerMessage(ctx context.Context, wc *workerConn, env protocol.Envelope) {
	// Deliver correlated RPC responses (e.g. git.inspect.result, git.commit.result).
	if env.ID != "" {
		h.mu.Lock()
		ch, ok := h.rpcWaiters[env.ID]
		if ok {
			delete(h.rpcWaiters, env.ID)
		}
		h.mu.Unlock()
		if ok {
			select {
			case ch <- env:
			default:
			}
			return
		}
	}

	switch env.Type {
	case protocol.MsgWorkerHeartbeat:
		p, err := protocol.DecodePayload[protocol.WorkerHeartbeatPayload](env)
		if err != nil {
			return
		}
		wc.mu.Lock()
		prev := wc.activeTasks
		wc.activeTasks = make(map[string]bool, len(p.ActiveTaskIDs))
		for _, id := range p.ActiveTaskIDs {
			wc.activeTasks[id] = true
		}
		var vanished []string
		for id := range prev {
			if !wc.activeTasks[id] {
				vanished = append(vanished, id)
			}
		}
		wc.mu.Unlock()
		// Tasks that dropped off the active list normally end with a task.status
		// shortly before or after; if not, they are reconciled after the grace period.
		h.scheduleLostTaskCheck(wc, vanished)

		if p.Metrics != nil {
			h.mu.Lock()
			h.workerMetrics[wc.id] = p.Metrics
			h.mu.Unlock()
		}

		if w, err := h.store.GetWorker(ctx, wc.id); err == nil {
			w.Status = "online"
			w.LastSeenAt = time.Now().UTC()
			_ = h.store.UpsertWorker(ctx, w)
			h.broadcastFleetUpdate("worker", WorkerStatusView{
				ID:             w.ID,
				Name:           w.Name,
				HostGroup:      w.HostGroup,
				OS:             w.OS,
				Arch:           w.Arch,
				Isolation:      w.Isolation,
				PermissionMode: string(w.PermissionMode),
				Agents:         w.Agents,
				Tags:           w.Tags,
				MaxConcurrent:  w.MaxConcurrent,
				Status:         w.Status,
				ActiveTasks:    len(p.ActiveTaskIDs),
				LastSeenAt:     w.LastSeenAt,
				Metrics:        p.Metrics,
				Facts:          decodeWorkerFacts(w.FactsJSON),
			})
		}

	case protocol.MsgWorkerFacts:
		p, err := protocol.DecodePayload[protocol.WorkerFactsPayload](env)
		if err != nil {
			return
		}
		if b, mErr := json.Marshal(p.Facts); mErr == nil {
			now := time.Now().UTC()
			if err := h.store.UpdateWorkerFacts(ctx, wc.id, string(b), now); err == nil {
				if w, gErr := h.store.GetWorker(ctx, wc.id); gErr == nil {
					factsCopy := p.Facts
					h.broadcastFleetUpdate("worker", WorkerStatusView{
						ID:             w.ID,
						Name:           w.Name,
						HostGroup:      w.HostGroup,
						OS:             w.OS,
						Arch:           w.Arch,
						Isolation:      w.Isolation,
						PermissionMode: string(w.PermissionMode),
						Agents:         w.Agents,
						Tags:           w.Tags,
						MaxConcurrent:  w.MaxConcurrent,
						Status:         w.Status,
						ActiveTasks:    wc.activeCount(),
						LastSeenAt:     w.LastSeenAt,
						Metrics:        h.WorkerMetrics(wc.id),
						Facts:          &factsCopy,
					})
				}
			}
		}

	case protocol.MsgWorkerQuotas:
		p, err := protocol.DecodePayload[protocol.WorkerQuotasPayload](env)
		if err != nil {
			return
		}
		for _, q := range p.Quotas {
			_ = h.store.UpsertSubscriptionQuota(ctx, store.SubscriptionQuota{
				Provider:    q.Provider,
				Label:       q.Label,
				WindowLabel: q.WindowLabel,
				UsedPercent: q.UsedPercent,
				ResetAt:     q.ResetAt,
				Source:      q.Source,
				WorkerID:    wc.id,
				UpdatedAt:   q.UpdatedAt,
			})
		}
		if sum, uErr := h.store.GetUsageSummary(ctx); uErr == nil {
			h.broadcastJSON(protocol.MsgUsageUpdate, sum)
		}

	case protocol.MsgExternalSession:
		p, err := protocol.DecodePayload[protocol.ExternalSessionPayload](env)
		if err != nil || p.ExternalID == "" || p.Agent == "" {
			return
		}
		h.handleExternalSession(ctx, wc.id, p)

	case protocol.MsgTaskStatus:
		p, err := protocol.DecodePayload[protocol.TaskStatusPayload](env)
		if err != nil || p.TaskID == "" {
			return
		}
		cur, err := h.store.GetTask(ctx, p.TaskID)
		if err != nil {
			return
		}
		if cur.WorkerID != "" && cur.WorkerID != wc.id {
			h.logger.Warn("ignoring task.status for a task owned by another worker",
				"task_id", p.TaskID, "worker_id", wc.id, "owner", cur.WorkerID)
			return
		}
		if cur.WorkerID == "" {
			_ = h.store.UpdateTaskAssignment(ctx, p.TaskID, wc.id, "")
		}
		if p.EffectiveDir != "" {
			_ = h.store.UpdateTaskAssignment(ctx, p.TaskID, wc.id, p.EffectiveDir)
		}
		gitJSON := ""
		if p.GitSummary != nil {
			if b, err := json.Marshal(p.GitSummary); err == nil {
				gitJSON = string(b)
			}
		}
		_ = h.store.UpdateTaskStatus(ctx, p.TaskID, p.Status, p.Summary, gitJSON, p.ExitCode)

		if protocol.IsTerminalTaskStatus(p.Status) {
			wc.mu.Lock()
			delete(wc.activeTasks, p.TaskID)
			wc.mu.Unlock()
		} else {
			wc.mu.Lock()
			wc.activeTasks[p.TaskID] = true
			wc.mu.Unlock()
		}

		if updated, err := h.store.GetTask(ctx, p.TaskID); err == nil {
			h.broadcastFleetUpdate("task", updated)
			if protocol.IsTerminalTaskStatus(updated.Status) {
				h.notifyTaskDone(updated)
				h.kickScheduler()
			}
		}

	case protocol.MsgTaskEvent:
		p, err := protocol.DecodePayload[protocol.TaskEventPayload](env)
		if err != nil {
			return
		}
		// Record worker-scraped subscription quota events (e.g. Codex 5h rolling %) directly into subscription_quotas.
		if p.Kind == "quota" && p.Meta != nil {
			prov, _ := p.Meta["provider"].(string)
			label, _ := p.Meta["label"].(string)
			win, _ := p.Meta["window_label"].(string)
			pct, _ := p.Meta["used_percent"].(float64)
			if prov != "" {
				_ = h.store.UpsertSubscriptionQuota(ctx, store.SubscriptionQuota{
					Provider:    prov,
					Label:       label,
					WindowLabel: win,
					UsedPercent: pct,
					Source:      "telemetry",
					WorkerID:    wc.id,
					UpdatedAt:   time.Now().UTC(),
				})
				if sum, uErr := h.store.GetUsageSummary(ctx); uErr == nil {
					h.broadcastJSON(protocol.MsgUsageUpdate, sum)
				}
			}
			return
		}
		// Record worker-scraped agent token usage (e.g. Claude JSONL cost-state) directly into usage_events.
		if p.Kind == "usage" && p.Meta != nil {
			prov, _ := p.Meta["provider"].(string)
			model, _ := p.Meta["model"].(string)
			inTok := metaInt64(p.Meta["input_tokens"])
			outTok := metaInt64(p.Meta["output_tokens"])
			if prov != "" && (inTok > 0 || outTok > 0) {
				_ = h.store.RecordUsage(ctx, store.UsageEvent{
					Provider:     prov,
					Model:        model,
					InputTokens:  inTok,
					OutputTokens: outTok,
				})
				if sum, uErr := h.store.GetUsageSummary(ctx); uErr == nil {
					h.broadcastJSON(protocol.MsgUsageUpdate, sum)
				}
			}
			return
		}
		metaJSON := ""
		if len(p.Meta) > 0 {
			if b, err := json.Marshal(p.Meta); err == nil {
				metaJSON = string(b)
			}
		}
		ev := store.TaskEvent{
			TaskID:    p.TaskID,
			Kind:      p.Kind,
			Message:   p.Message,
			MetaJSON:  metaJSON,
			CreatedAt: time.Now().UTC(),
		}
		if id, err := h.store.AppendEvent(ctx, ev); err == nil {
			ev.ID = id
		}
		h.broadcastFleetUpdate("event", ev)
	}
}

func (h *Hub) handleWorkerBinary(raw []byte) {
	ft, taskID, _, err := protocol.DecodeFrame(raw)
	if err != nil {
		return
	}
	frameCopy := make([]byte, len(raw))
	copy(frameCopy, raw)

	h.mu.Lock()
	subs := h.ptySubs[taskID]
	var targets []*browserClient
	for c, needsSnap := range subs {
		if ft == protocol.FramePtySnapshot {
			if needsSnap {
				targets = append(targets, c)
				subs[c] = false
			}
		} else {
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()

	msg := wsFrame{msgType: websocket.MessageBinary, data: frameCopy}
	for _, c := range targets {
		h.sendBrowser(c, msg)
	}
}

// ServeBrowserConn handles a single authenticated browser PWA WebSocket client.
func (h *Hub) ServeBrowserConn(ctx context.Context, conn *websocket.Conn, user string) {
	conn.SetReadLimit(2 * 1024 * 1024)
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Hijacked WebSocket handlers outlive http.Server.Shutdown; tie them to the hub.
	defer context.AfterFunc(h.ctx, cancel)()

	client := &browserClient{
		user:     user,
		conn:     conn,
		sendCh:   make(chan wsFrame, 256), // Bounded queue for backpressure protection
		cancel:   cancel,
		ctx:      connCtx,
		subs:     make(map[string]bool),
		chatSubs: make(map[string]bool),
	}

	h.mu.Lock()
	h.clients[client] = struct{}{}
	h.mu.Unlock()

	defer func() {
		h.cleanupBrowserClient(client)
	}()

	// Writer goroutine
	go func() {
		for {
			select {
			case <-connCtx.Done():
				return
			case f := <-client.sendCh:
				wCtx, wCancel := context.WithTimeout(connCtx, 10*time.Second)
				err := conn.Write(wCtx, f.msgType, f.data)
				wCancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Read loop
	for {
		mt, data, err := conn.Read(connCtx)
		if err != nil {
			return
		}
		switch mt {
		case websocket.MessageBinary:
			ft, taskID, _, decErr := protocol.DecodeFrame(data)
			if decErr != nil || ft != protocol.FramePtyStdin {
				continue
			}
			h.forwardStdinToWorker(taskID, data)

		case websocket.MessageText:
			env, decErr := protocol.DecodeEnvelope(data)
			if decErr != nil {
				continue
			}
			h.handleBrowserMessage(ctx, client, env)
		}
	}
}

func (h *Hub) handleBrowserMessage(ctx context.Context, c *browserClient, env protocol.Envelope) {
	switch env.Type {
	case protocol.MsgPtySubscribe:
		p, err := protocol.DecodePayload[protocol.PtySubscribePayload](env)
		if err != nil || p.TaskID == "" {
			return
		}
		h.subscribeBrowserPty(c, p.TaskID)

	case protocol.MsgPtyUnsubscribe:
		p, err := protocol.DecodePayload[protocol.PtyUnsubscribePayload](env)
		if err != nil || p.TaskID == "" {
			return
		}
		h.unsubscribeBrowserPty(c, p.TaskID)

	case protocol.MsgPtyResize:
		p, err := protocol.DecodePayload[protocol.PtyResizePayload](env)
		if err != nil || p.TaskID == "" {
			return
		}
		h.sendToTaskWorkerJSON(p.TaskID, protocol.MsgPtyResize, p)

	case protocol.MsgChatSend:
		p, err := protocol.DecodePayload[protocol.ChatSendPayload](env)
		if err != nil || p.SessionID == "" || p.Content == "" {
			return
		}
		h.mu.RLock()
		orch := h.orch
		h.mu.RUnlock()
		if orch == nil {
			h.sendBrowserJSON(c, protocol.MsgChatError, protocol.ChatErrorPayload{
				SessionID: p.SessionID,
				Error:     "orchestrator service not initialized",
			})
			return
		}
		// The sender always receives the events of the turn it started.
		h.subscribeChat(c, p.SessionID)
		h.startChatTurn(c, orch, p)

	case msgChatSubscribe, msgChatUnsubscribe:
		p, err := protocol.DecodePayload[chatSubscribePayload](env)
		if err != nil || p.SessionID == "" {
			return
		}
		if env.Type == msgChatSubscribe {
			h.subscribeChat(c, p.SessionID)
		} else {
			c.mu.Lock()
			delete(c.chatSubs, p.SessionID)
			c.mu.Unlock()
		}
	}
}

// Browser chat subscription messages. These are hub-local until they are added
// to internal/protocol; payload: {"session_id": "..."}.
const (
	msgChatSubscribe   = "chat.subscribe"
	msgChatUnsubscribe = "chat.unsubscribe"
)

type chatSubscribePayload struct {
	SessionID string `json:"session_id"`
}

// maxChatTurnsPerSession bounds the running plus waiting turns for one session.
const maxChatTurnsPerSession = 4

// chatSession serializes orchestrator turns for one chat session.
type chatSession struct {
	sem  chan struct{} // capacity 1: held by the running turn
	refs int           // running + waiting turns; guarded by Hub.mu
}

func (h *Hub) subscribeChat(c *browserClient, sessionID string) {
	c.mu.Lock()
	c.chatSubs[sessionID] = true
	c.mu.Unlock()
}

// startChatTurn runs one orchestrator turn on a tracked goroutine. The turn's
// context ends when the client disconnects, the hub shuts down, or
// ChatTurnTimeout elapses. Turns of the same session run one at a time, in
// arrival order; past maxChatTurnsPerSession the request is rejected.
func (h *Hub) startChatTurn(c *browserClient, orch *orchestrator.Service, p protocol.ChatSendPayload) {
	h.mu.Lock()
	cs := h.chatSessions[p.SessionID]
	if cs == nil {
		cs = &chatSession{sem: make(chan struct{}, 1)}
		h.chatSessions[p.SessionID] = cs
	}
	if cs.refs >= maxChatTurnsPerSession {
		h.mu.Unlock()
		h.sendBrowserJSON(c, protocol.MsgChatError, protocol.ChatErrorPayload{
			SessionID: p.SessionID,
			Error:     "too many pending messages for this chat session; wait for the current reply",
		})
		return
	}
	cs.refs++
	h.mu.Unlock()

	release := func() {
		h.mu.Lock()
		cs.refs--
		if cs.refs == 0 && h.chatSessions[p.SessionID] == cs {
			delete(h.chatSessions, p.SessionID)
		}
		h.mu.Unlock()
	}

	timeout := h.ChatTurnTimeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	started := h.goBackground(func(context.Context) {
		defer release()
		// c.ctx is canceled on client disconnect and on hub shutdown.
		turnCtx, cancel := context.WithTimeout(c.ctx, timeout)
		defer cancel()
		select {
		case cs.sem <- struct{}{}:
		case <-turnCtx.Done():
			return
		}
		defer func() { <-cs.sem }()
		if turnCtx.Err() != nil {
			return
		}

		err := orch.HandleUserMessage(turnCtx, p.SessionID, p.Content, orchestrator.StreamCallbacks{
			OnDelta: func(sid, delta string) {
				h.sendChatEvent(sid, protocol.MsgChatDelta, protocol.ChatDeltaPayload{SessionID: sid, Delta: delta})
			},
			OnTool: func(tp protocol.ChatToolPayload) {
				h.sendChatEvent(tp.SessionID, protocol.MsgChatTool, tp)
			},
			OnDone: func(sid string, msgID int64) {
				h.sendChatEvent(sid, protocol.MsgChatDone, protocol.ChatDonePayload{SessionID: sid, MessageID: msgID})
			},
			OnError: func(sid, errMsg string) {
				h.sendChatEvent(sid, protocol.MsgChatError, protocol.ChatErrorPayload{SessionID: sid, Error: errMsg})
			},
			OnUsage: func(sum store.UsageSummary) {
				h.broadcastJSON(protocol.MsgUsageUpdate, sum)
			},
		})
		if err != nil && errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
			h.sendChatEvent(p.SessionID, protocol.MsgChatError, protocol.ChatErrorPayload{
				SessionID: p.SessionID,
				Error:     "orchestrator turn timed out",
			})
		}
	})
	if !started {
		release()
	}
}

// sendChatEvent delivers a chat frame only to clients subscribed to sessionID.
func (h *Hub) sendChatEvent(sessionID, msgType string, payload any) {
	raw, err := protocol.NewEnvelope(msgType, "", payload)
	if err != nil {
		return
	}
	frame := wsFrame{msgType: websocket.MessageText, data: raw}
	h.mu.RLock()
	targets := make([]*browserClient, 0, len(h.clients))
	for c := range h.clients {
		c.mu.Lock()
		if c.chatSubs[sessionID] {
			targets = append(targets, c)
		}
		c.mu.Unlock()
	}
	h.mu.RUnlock()
	for _, c := range targets {
		h.sendBrowser(c, frame)
	}
}

func (h *Hub) subscribeBrowserPty(c *browserClient, taskID string) {
	c.mu.Lock()
	c.subs[taskID] = true
	c.mu.Unlock()

	h.mu.Lock()
	subs, ok := h.ptySubs[taskID]
	if !ok {
		subs = make(map[*browserClient]bool)
		h.ptySubs[taskID] = subs
	}
	subs[c] = true // Request fresh snapshot delivery for this client
	wc := h.workerForTaskLocked(taskID)
	h.mu.Unlock()

	if wc != nil {
		// Always ask worker for pty.subscribe so the newly attached client gets the 0x01 snapshot.
		h.sendWorkerJSON(wc, protocol.MsgPtySubscribe, protocol.PtySubscribePayload{TaskID: taskID})
	}
}

func (h *Hub) unsubscribeBrowserPty(c *browserClient, taskID string) {
	c.mu.Lock()
	delete(c.subs, taskID)
	c.mu.Unlock()

	var notifyWorker *workerConn
	h.mu.Lock()
	if subs, ok := h.ptySubs[taskID]; ok {
		delete(subs, c)
		if len(subs) == 0 {
			delete(h.ptySubs, taskID)
			notifyWorker = h.workerForTaskLocked(taskID)
		}
	}
	h.mu.Unlock()

	// Ref-count transition 1 -> 0: stop worker streaming!
	if notifyWorker != nil {
		h.sendWorkerJSON(notifyWorker, protocol.MsgPtyUnsubscribe, protocol.PtyUnsubscribePayload{TaskID: taskID})
	}
}

func (h *Hub) cleanupBrowserClient(c *browserClient) {
	c.mu.Lock()
	subIDs := make([]string, 0, len(c.subs))
	for tid := range c.subs {
		subIDs = append(subIDs, tid)
	}
	c.subs = make(map[string]bool)
	c.mu.Unlock()

	for _, tid := range subIDs {
		h.unsubscribeBrowserPty(c, tid)
	}

	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *Hub) forwardStdinToWorker(taskID string, rawFrame []byte) {
	h.mu.RLock()
	wc := h.workerForTaskLocked(taskID)
	h.mu.RUnlock()
	if wc == nil {
		return
	}
	buf := make([]byte, len(rawFrame))
	copy(buf, rawFrame)
	select {
	case wc.sendCh <- wsFrame{msgType: websocket.MessageBinary, data: buf}:
	default:
	}
}

// DispatchToWorker sends task.start to workerID and tracks the assignment.
func (h *Hub) DispatchToWorker(workerID string, payload protocol.TaskStartPayload) error {
	h.mu.Lock()
	wc, ok := h.workers[workerID]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("worker %s is not connected", workerID)
	}
	h.taskWorker[payload.TaskID] = workerID
	h.mu.Unlock()

	wc.mu.Lock()
	wc.activeTasks[payload.TaskID] = true
	wc.mu.Unlock()

	if !h.sendWorkerJSON(wc, protocol.MsgTaskStart, payload) {
		wc.mu.Lock()
		delete(wc.activeTasks, payload.TaskID)
		wc.mu.Unlock()
		h.mu.Lock()
		if h.taskWorker[payload.TaskID] == workerID {
			delete(h.taskWorker, payload.TaskID)
		}
		h.mu.Unlock()
		return fmt.Errorf("failed to enqueue task.start to worker %s", workerID)
	}
	return nil
}

// kickScheduler requests a SchedulePending run on a tracked background goroutine.
// Concurrent kicks coalesce: at most one run is active and at most one more is
// queued behind it, so a burst of hellos or terminal statuses costs one or two passes.
func (h *Hub) kickScheduler() {
	h.schedDirty.Store(true)
	if !h.schedRunning.CompareAndSwap(false, true) {
		return
	}
	started := h.goBackground(func(ctx context.Context) {
		for {
			for h.schedDirty.Swap(false) {
				if ctx.Err() != nil {
					break
				}
				h.SchedulePending(ctx)
			}
			h.schedRunning.Store(false)
			// A kick may have landed between the last Swap and the Store above.
			if ctx.Err() != nil || !h.schedDirty.Load() || !h.schedRunning.CompareAndSwap(false, true) {
				return
			}
		}
	})
	if !started {
		h.schedRunning.Store(false)
	}
}

// taskStartSpec returns the persisted task.start payload and worker selector for t,
// and whether its worker was explicitly pinned. Tasks created before the spec was
// persisted fall back to a payload rebuilt from the task row.
func (h *Hub) taskStartSpec(ctx context.Context, t store.Task) (protocol.TaskStartPayload, WorkerSelector, bool) {
	var (
		payload     protocol.TaskStartPayload
		sel         WorkerSelector
		havePayload bool
		pinned      bool
	)
	spec, err := h.store.GetTaskStartSpec(ctx, t.ID)
	if err == nil && spec.PayloadJSON != "" {
		havePayload = json.Unmarshal([]byte(spec.PayloadJSON), &payload) == nil
	}
	if err == nil && spec.SelectorJSON != "" && json.Unmarshal([]byte(spec.SelectorJSON), &sel) == nil {
		pinned = spec.WorkerPinned
	} else {
		// Legacy row: the only constraint we know is the recorded worker.
		sel = WorkerSelector{WorkerID: t.WorkerID}
		pinned = t.WorkerID != ""
	}
	if !havePayload {
		payload = protocol.TaskStartPayload{
			Agent:          t.Agent,
			Model:          t.Model,
			SessionKind:    t.SessionKind,
			Workdir:        t.Workdir,
			Branch:         t.Branch,
			UseWorktree:    t.Branch != "",
			Prompt:         t.Prompt,
			Mode:           t.Mode,
			PermissionMode: string(t.PermissionMode),
			TimeoutSeconds: 3600,
		}
	}
	payload.TaskID = t.ID
	return payload, sel, pinned
}

// SchedulePending dispatches queued tasks to connected workers with capacity.
// Runs are serialized, and each task is claimed atomically in the store
// (queued -> assigned) before task.start is sent, so a task is never dispatched
// twice. A failed send returns the task to queued.
func (h *Hub) SchedulePending(ctx context.Context) {
	h.schedMu.Lock()
	defer h.schedMu.Unlock()

	tasks, err := h.store.ListTasks(ctx, protocol.TaskStatusQueued, "", 200)
	if err != nil || len(tasks) == 0 {
		return
	}
	all, err := h.store.ListWorkers(ctx)
	if err != nil {
		return
	}
	// Only connected workers can receive task.start; a disconnected-but-online
	// worker would otherwise win placement and leave the task waiting.
	workers := make([]store.Worker, 0, len(all))
	for _, w := range all {
		if h.IsWorkerConnected(w.ID) {
			workers = append(workers, w)
		}
	}
	if len(workers) == 0 {
		return
	}
	counts := h.ActiveTaskCounts()

	// ListTasks returns newest first; dispatch oldest first.
	for i := len(tasks) - 1; i >= 0; i-- {
		t := tasks[i]
		if ctx.Err() != nil {
			return
		}
		if t.Origin == protocol.TaskOriginExternal {
			continue
		}
		payload, sel, pinned := h.taskStartSpec(ctx, t)
		w, ok := SelectWorker(workers, counts, sel, t.Agent, t.PermissionMode)
		if !ok {
			continue
		}
		claimed, err := h.store.ClaimQueuedTask(ctx, t.ID, w.ID)
		if err != nil || !claimed {
			continue
		}
		if err := h.DispatchToWorker(w.ID, payload); err != nil {
			h.logger.Warn("dispatch of queued task failed; returning it to the queue",
				"task_id", t.ID, "worker_id", w.ID, "err", err)
			_, _ = h.store.RequeueTask(ctx, t.ID, "", pinned)
			continue
		}
		counts[w.ID]++
		if updated, err := h.store.GetTask(ctx, t.ID); err == nil {
			h.broadcastFleetUpdate("task", updated)
		}
	}
}

// SendSteer forwards task.steer to the worker running taskID.
func (h *Hub) SendSteer(taskID, inputText string, appendNewline bool) error {
	h.mu.RLock()
	wc := h.workerForTaskLocked(taskID)
	h.mu.RUnlock()
	if wc == nil {
		return fmt.Errorf("no connected worker for task %s", taskID)
	}
	if !h.sendWorkerJSON(wc, protocol.MsgTaskSteer, protocol.TaskSteerPayload{
		TaskID:        taskID,
		InputText:     inputText,
		AppendNewline: appendNewline,
	}) {
		return fmt.Errorf("worker send queue full")
	}
	return nil
}

// SendCancel forwards task.cancel to the worker running taskID.
func (h *Hub) SendCancel(taskID string) bool {
	h.mu.RLock()
	wc := h.workerForTaskLocked(taskID)
	h.mu.RUnlock()
	if wc == nil {
		return false
	}
	return h.sendWorkerJSON(wc, protocol.MsgTaskCancel, protocol.TaskCancelPayload{TaskID: taskID})
}

// WaitForTask returns a channel that receives the task when it reaches a terminal status.
func (h *Hub) WaitForTask(taskID string) (<-chan store.Task, func()) {
	ch := make(chan store.Task, 1)
	h.mu.Lock()
	h.waiters[taskID] = append(h.waiters[taskID], ch)
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		list := h.waiters[taskID]
		for i, item := range list {
			if item == ch {
				h.waiters[taskID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(h.waiters[taskID]) == 0 {
			delete(h.waiters, taskID)
		}
	}
	return ch, cancel
}

// notifyTaskDone wakes waiters for a task that reached a terminal status and
// drops the hub's task->worker routing entry for it.
func (h *Hub) notifyTaskDone(t store.Task) {
	h.mu.Lock()
	list := h.waiters[t.ID]
	delete(h.waiters, t.ID)
	delete(h.taskWorker, t.ID)
	h.mu.Unlock()

	for _, ch := range list {
		select {
		case ch <- t:
		default:
		}
	}
}

// forgetTask drops the task->worker routing entry (task finished, requeued or orphaned).
func (h *Hub) forgetTask(taskID string) {
	h.mu.Lock()
	delete(h.taskWorker, taskID)
	h.mu.Unlock()
}

func (h *Hub) workerForTaskLocked(taskID string) *workerConn {
	wid, ok := h.taskWorker[taskID]
	if !ok {
		return nil
	}
	return h.workers[wid]
}

func (h *Hub) sendToTaskWorkerJSON(taskID, msgType string, payload any) {
	h.mu.RLock()
	wc := h.workerForTaskLocked(taskID)
	h.mu.RUnlock()
	if wc != nil {
		h.sendWorkerJSON(wc, msgType, payload)
	}
}

// RequestWorker sends a correlated control-plane RPC to a connected worker and awaits its reply.
func (h *Hub) RequestWorker(ctx context.Context, workerID, msgType string, payload any) (protocol.Envelope, error) {
	h.mu.RLock()
	wc, ok := h.workers[workerID]
	h.mu.RUnlock()
	if !ok {
		return protocol.Envelope{}, fmt.Errorf("worker %s is not connected", workerID)
	}

	reqID := uuid.NewString()
	raw, err := protocol.NewEnvelope(msgType, reqID, payload)
	if err != nil {
		return protocol.Envelope{}, err
	}

	ch := make(chan protocol.Envelope, 1)
	h.mu.Lock()
	h.rpcWaiters[reqID] = ch
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.rpcWaiters, reqID)
		h.mu.Unlock()
	}()

	select {
	case wc.sendCh <- wsFrame{msgType: websocket.MessageText, data: raw}:
	default:
		return protocol.Envelope{}, fmt.Errorf("worker %s outbound queue full", workerID)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return protocol.Envelope{}, ctx.Err()
	}
}

func (h *Hub) sendWorkerJSON(wc *workerConn, msgType string, payload any) bool {
	raw, err := protocol.NewEnvelope(msgType, "", payload)
	if err != nil {
		return false
	}
	select {
	case wc.sendCh <- wsFrame{msgType: websocket.MessageText, data: raw}:
		return true
	default:
		return false
	}
}

func (h *Hub) sendBrowser(c *browserClient, frame wsFrame) {
	select {
	case c.sendCh <- frame:
	default:
		// Backpressure protection: drop slow browser consumer rather than stalling workers.
		h.logger.Warn("dropping slow browser websocket consumer", "user", c.user)
		c.cancel()
	}
}

func (h *Hub) sendBrowserJSON(c *browserClient, msgType string, payload any) {
	raw, err := protocol.NewEnvelope(msgType, "", payload)
	if err != nil {
		return
	}
	h.sendBrowser(c, wsFrame{msgType: websocket.MessageText, data: raw})
}

func (h *Hub) broadcastJSON(msgType string, payload any) {
	raw, err := protocol.NewEnvelope(msgType, "", payload)
	if err != nil {
		return
	}
	frame := wsFrame{msgType: websocket.MessageText, data: raw}
	h.mu.RLock()
	clients := make([]*browserClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		h.sendBrowser(c, frame)
	}
}

func (h *Hub) broadcastFleetUpdate(kind string, item any) {
	p := protocol.FleetUpdatePayload{Kind: kind}
	switch kind {
	case "worker":
		p.Worker = item
	case "task":
		p.Task = item
	case "event":
		p.Event = item
	case "space":
		p.Space = item
	case "workspace":
		p.Workspace = item
	case "project":
		p.Project = item
	case "repo":
		p.Repo = item
	case "worktree":
		p.Worktree = item
	case "space_deleted", "worker_deleted", "workspace_deleted", "project_deleted", "repo_deleted", "worktree_deleted":
		if id, ok := item.(string); ok {
			p.DeletedID = id
		}
	}
	h.broadcastJSON(protocol.MsgFleetUpdate, p)
}

func (h *Hub) handleExternalSession(ctx context.Context, workerID string, p protocol.ExternalSessionPayload) {
	wsID, projID, repoID, wtID := h.matchTaxonomyByPath(ctx, workerID, p.Cwd)
	status := protocol.TaskStatusIdle
	switch strings.ToLower(strings.TrimSpace(p.Status)) {
	case "working", "running":
		status = protocol.TaskStatusRunning
	case "idle":
		status = protocol.TaskStatusIdle
	case "needs_you", "waiting_input":
		status = protocol.TaskStatusWaitingInput
	case "done", "completed":
		status = protocol.TaskStatusCompleted
	case "fail", "failed":
		status = protocol.TaskStatusFailed
	}

	taskID := "ext-" + workerID + "-" + p.ExternalID
	gitJSON := ""
	if len(p.ChangedFiles) > 0 {
		if b, err := json.Marshal(protocol.GitSummary{DirtyFiles: p.ChangedFiles}); err == nil {
			gitJSON = string(b)
		}
	}
	t := store.Task{
		ID:             taskID,
		WorkerID:       workerID,
		WorkspaceID:    wsID,
		ProjectID:      projID,
		RepoID:         repoID,
		WorktreeID:     wtID,
		Agent:          p.Agent,
		Model:          p.Model,
		SessionKind:    "chat",
		Workdir:        p.Cwd,
		EffectiveDir:   p.Cwd,
		Branch:         p.Branch,
		Mode:           protocol.TaskModeInteractive,
		Origin:         protocol.TaskOriginExternal,
		PermissionMode: store.PermissionFullAccess,
		Status:         status,
		Summary:        p.Summary,
		GitSummaryJSON: gitJSON,
	}
	if err := h.store.UpsertExternalTask(ctx, t); err != nil {
		return
	}
	if len(p.ChangedFiles) > 0 {
		ev := store.TaskEvent{
			TaskID:    taskID,
			Kind:      "file_edit",
			Message:   "Modified " + strings.Join(p.ChangedFiles, ", "),
			CreatedAt: time.Now().UTC(),
		}
		if id, err := h.store.AppendEvent(ctx, ev); err == nil {
			ev.ID = id
		}
		h.broadcastFleetUpdate("event", ev)
	}
	if updated, err := h.store.GetTask(ctx, taskID); err == nil {
		h.broadcastFleetUpdate("task", updated)
	}
}

func (h *Hub) matchTaxonomyByPath(ctx context.Context, workerID, cwd string) (wsID, projID, repoID, wtID string) {
	wsID = store.DefaultWorkspaceID
	projID = store.DefaultProjectID
	cleanCwd := strings.TrimRight(strings.TrimSpace(cwd), "/\\")
	if cleanCwd == "" {
		return
	}
	tax, err := h.store.GetTaxonomy(ctx)
	if err != nil {
		return
	}
	projByID := make(map[string]store.Project, len(tax.Projects))
	for _, pr := range tax.Projects {
		projByID[pr.ID] = pr
	}
	repoByID := make(map[string]store.Repo, len(tax.Repos))
	for _, rp := range tax.Repos {
		repoByID[rp.ID] = rp
	}

	bestLen := -1
	// Prefer longest matching Worktree on this worker.
	for _, wt := range tax.Worktrees {
		rp, ok := repoByID[wt.RepoID]
		if !ok || rp.WorkerID != workerID {
			continue
		}
		p := strings.TrimRight(wt.Path, "/\\")
		if p != "" && (cleanCwd == p || strings.HasPrefix(cleanCwd, p+"/")) && len(p) > bestLen {
			bestLen = len(p)
			wtID = wt.ID
			repoID = rp.ID
			projID = rp.ProjectID
			if pr, ok := projByID[rp.ProjectID]; ok {
				wsID = pr.WorkspaceID
			}
		}
	}
	// Fallback to longest matching Repo on this worker.
	for _, rp := range tax.Repos {
		if rp.WorkerID != workerID {
			continue
		}
		p := strings.TrimRight(rp.Path, "/\\")
		if p != "" && (cleanCwd == p || strings.HasPrefix(cleanCwd, p+"/")) && len(p) > bestLen {
			bestLen = len(p)
			wtID = ""
			repoID = rp.ID
			projID = rp.ProjectID
			if pr, ok := projByID[rp.ProjectID]; ok {
				wsID = pr.WorkspaceID
			}
		}
	}
	return
}

func metaInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}
