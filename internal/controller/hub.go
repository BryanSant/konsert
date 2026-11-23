package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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

type browserClient struct {
	user   string
	conn   *websocket.Conn
	sendCh chan wsFrame
	cancel context.CancelFunc

	mu   sync.Mutex
	subs map[string]bool // subscribed task IDs
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
	taskWorker    map[string]string                 // taskID -> workerID
	ptySubs       map[string]map[*browserClient]bool // taskID -> client -> needsSnapshot
	waiters       map[string][]chan store.Task
	rpcWaiters    map[string]chan protocol.Envelope
	pendingStart  map[string]protocol.TaskStartPayload // queued tasks awaiting capacity

	orch *orchestrator.Service

	// Configurable for tests (defaults to 30s timeout, 10s check interval).
	StaleWorkerTimeout time.Duration
	ReapInterval       time.Duration
}

// NewHub constructs a Hub backed by store.Store.
func NewHub(st store.Store, logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		store:              st,
		logger:             logger,
		workers:            make(map[string]*workerConn),
		workerMetrics:      make(map[string]*protocol.WorkerMetrics),
		clients:            make(map[*browserClient]struct{}),
		taskWorker:         make(map[string]string),
		ptySubs:            make(map[string]map[*browserClient]bool),
		waiters:            make(map[string][]chan store.Task),
		rpcWaiters:         make(map[string]chan protocol.Envelope),
		pendingStart:       make(map[string]protocol.TaskStartPayload),
		StaleWorkerTimeout: 30 * time.Second,
		ReapInterval:       10 * time.Second,
	}
}

// SetOrchestrator attaches the built-in Orchestrator Chat service to the Hub.
func (h *Hub) SetOrchestrator(o *orchestrator.Service) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.orch = o
}

// Close terminates all active worker and browser WebSocket connections cleanly.
func (h *Hub) Close() {
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
		wc.cancel()
		_ = wc.conn.Close(websocket.StatusGoingAway, "controller shutting down")
	}
	for _, c := range clients {
		c.cancel()
		_ = c.conn.Close(websocket.StatusGoingAway, "controller shutting down")
	}
}

// RunReaper periodically marks workers with missed heartbeats offline and applies orphan policies.
func (h *Hub) RunReaper(ctx context.Context) {
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
	for _, t := range tasks {
		if protocol.IsTerminalTaskStatus(t.Status) || t.Status == protocol.TaskStatusQueued {
			continue
		}
		if t.OrphanPolicy == protocol.OrphanPolicyRequeue {
			_ = h.store.UpdateTaskStatus(ctx, t.ID, protocol.TaskStatusQueued, "requeued after worker disconnect", "", nil)
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
		wc.mu.Lock()
		counts[id] = len(wc.activeTasks)
		wc.mu.Unlock()
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

// DisconnectWorker closes any active WebSocket session for workerID and clears cached metrics.
func (h *Hub) DisconnectWorker(workerID string) {
	h.mu.Lock()
	wc, ok := h.workers[workerID]
	delete(h.workers, workerID)
	delete(h.workerMetrics, workerID)
	h.mu.Unlock()
	if ok && wc != nil {
		wc.cancel()
		_ = wc.conn.Close(websocket.StatusNormalClosure, "worker removed by operator")
	}
}

// ServeWorkerConn handles a single authenticated konsertd WebSocket connection until disconnect.
func (h *Hub) ServeWorkerConn(ctx context.Context, conn *websocket.Conn) {
	conn.SetReadLimit(4 * 1024 * 1024)
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	h.mu.Lock()
	if prev, ok := h.workers[wModel.ID]; ok {
		prev.cancel()
	}
	h.workers[wModel.ID] = wc
	h.mu.Unlock()

	// Re-adopt any in-flight tasks reported by worker.hello (controller restart resilience).
	for _, at := range hello.ActiveTasks {
		wc.mu.Lock()
		wc.activeTasks[at.TaskID] = true
		wc.mu.Unlock()

		h.mu.Lock()
		h.taskWorker[at.TaskID] = wModel.ID
		hasSubs := len(h.ptySubs[at.TaskID]) > 0
		h.mu.Unlock()

		if _, err := h.store.GetTask(ctx, at.TaskID); err == nil {
			_ = h.store.UpdateTaskAssignment(ctx, at.TaskID, wModel.ID, at.EffectiveDir)
			st := at.Status
			if st == "" {
				st = protocol.TaskStatusRunning
			}
			_ = h.store.UpdateTaskStatus(ctx, at.TaskID, st, "", "", nil)
			if updated, err := h.store.GetTask(ctx, at.TaskID); err == nil {
				h.broadcastFleetUpdate("task", updated)
			}
		}
		if hasSubs {
			h.sendWorkerJSON(wc, protocol.MsgPtySubscribe, protocol.PtySubscribePayload{TaskID: at.TaskID})
		}
	}

	h.logger.Info("worker registered",
		"worker_id", wModel.ID,
		"name", wModel.Name,
		"agents", wModel.Agents,
		"readopted_tasks", len(hello.ActiveTasks),
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
		ActiveTasks:    len(hello.ActiveTasks),
		LastSeenAt:     wModel.LastSeenAt,
		Facts:          hello.Worker.Facts,
	})

	// Drain any queued tasks that can now run on this worker.
	go h.SchedulePending(context.Background())

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
		wc.activeTasks = make(map[string]bool, len(p.ActiveTaskIDs))
		for _, id := range p.ActiveTaskIDs {
			wc.activeTasks[id] = true
		}
		wc.mu.Unlock()

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
						ActiveTasks:    len(wc.activeTasks),
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
		if err != nil {
			return
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
				go h.SchedulePending(context.Background())
			}
		}

	case protocol.MsgTaskEvent:
		p, err := protocol.DecodePayload[protocol.TaskEventPayload](env)
		if err != nil {
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

	client := &browserClient{
		user:   user,
		conn:   conn,
		sendCh: make(chan wsFrame, 256), // Bounded queue for backpressure protection
		cancel: cancel,
		subs:   make(map[string]bool),
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
		go func() {
			_ = orch.HandleUserMessage(context.Background(), p.SessionID, p.Content, orchestrator.StreamCallbacks{
				OnDelta: func(sid, delta string) {
					h.broadcastJSON(protocol.MsgChatDelta, protocol.ChatDeltaPayload{SessionID: sid, Delta: delta})
				},
				OnTool: func(tp protocol.ChatToolPayload) {
					h.broadcastJSON(protocol.MsgChatTool, tp)
				},
				OnDone: func(sid string, msgID int64) {
					h.broadcastJSON(protocol.MsgChatDone, protocol.ChatDonePayload{SessionID: sid, MessageID: msgID})
				},
				OnError: func(sid, errMsg string) {
					h.broadcastJSON(protocol.MsgChatError, protocol.ChatErrorPayload{SessionID: sid, Error: errMsg})
				},
				OnUsage: func(sum store.UsageSummary) {
					h.broadcastJSON(protocol.MsgUsageUpdate, sum)
				},
			})
		}()
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
		return fmt.Errorf("failed to enqueue task.start to worker %s", workerID)
	}
	return nil
}

// QueuePendingStart remembers a task's start payload when queued waiting for worker capacity.
func (h *Hub) QueuePendingStart(payload protocol.TaskStartPayload, selector WorkerSelector) {
	h.mu.Lock()
	h.pendingStart[payload.TaskID] = payload
	h.mu.Unlock()
}

// SchedulePending tries to dispatch any queued tasks to newly available workers.
func (h *Hub) SchedulePending(ctx context.Context) {
	tasks, err := h.store.ListTasks(ctx, protocol.TaskStatusQueued, "", 100)
	if err != nil || len(tasks) == 0 {
		return
	}
	workers, err := h.store.ListWorkers(ctx)
	if err != nil {
		return
	}
	counts := h.ActiveTaskCounts()

	for _, t := range tasks {
		h.mu.RLock()
		startPayload, hasPayload := h.pendingStart[t.ID]
		h.mu.RUnlock()
		if !hasPayload {
			startPayload = protocol.TaskStartPayload{
				TaskID:         t.ID,
				Agent:          t.Agent,
				Workdir:        t.Workdir,
				Branch:         t.Branch,
				UseWorktree:    t.Branch != "",
				Prompt:         t.Prompt,
				Mode:           t.Mode,
				PermissionMode: string(t.PermissionMode),
				TimeoutSeconds: 3600,
			}
		}

		sel := WorkerSelector{WorkerID: t.WorkerID}
		w, ok := SelectWorker(workers, counts, sel, t.Agent, t.PermissionMode)
		if !ok || !h.IsWorkerConnected(w.ID) {
			continue
		}

		_ = h.store.UpdateTaskAssignment(ctx, t.ID, w.ID, "")
		_ = h.store.UpdateTaskStatus(ctx, t.ID, protocol.TaskStatusAssigned, "", "", nil)
		if err := h.DispatchToWorker(w.ID, startPayload); err != nil {
			continue
		}
		counts[w.ID]++
		h.mu.Lock()
		delete(h.pendingStart, t.ID)
		h.mu.Unlock()

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

func (h *Hub) notifyTaskDone(t store.Task) {
	h.mu.Lock()
	list := h.waiters[t.ID]
	delete(h.waiters, t.ID)
	h.mu.Unlock()

	for _, ch := range list {
		select {
		case ch <- t:
		default:
		}
	}
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
	status := protocol.TaskStatusRunning
	switch strings.ToLower(strings.TrimSpace(p.Status)) {
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
