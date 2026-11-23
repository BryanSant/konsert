package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/coder/websocket"
)

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
		baseURL:     workerBaseURLFrom(ctx),
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
		Version:        strings.TrimSpace(hello.Worker.Version),
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
	helloView := toWorkerView(wModel, wc.activeCount(), nil)
	helloView.Facts = hello.Worker.Facts
	h.broadcastFleetUpdate("worker", helloView)

	// Agent chats that were working on this worker lost their turn with the old connection.
	h.failInterruptedChats(ctx, wc)

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
	if h.deliverRPCResponse(env) {
		return
	}
	switch env.Type {
	case protocol.MsgWorkerHeartbeat:
		h.onWorkerHeartbeat(ctx, wc, env)
	case protocol.MsgWorkerFacts:
		h.onWorkerFacts(ctx, wc, env)
	case protocol.MsgWorkerQuotas:
		h.onWorkerQuotas(ctx, wc, env)
	case protocol.MsgExternalSession:
		h.onExternalSession(ctx, wc, env)
	case protocol.MsgTaskStatus:
		h.onTaskStatus(ctx, wc, env)
	case protocol.MsgTaskEvent:
		h.onTaskEvent(ctx, wc, env)
	case protocol.MsgHostWakeLockState:
		h.onHostWakeLockState(ctx, wc, env)
	case protocol.MsgAgentChatEvent:
		h.onAgentChatEvent(ctx, wc, env)
	case protocol.MsgAgentCapabilities:
		h.onAgentCapabilities(ctx, wc, env)
	}
}

// deliverRPCResponse hands correlated RPC replies (e.g. git.inspect.result) to
// their RequestWorker waiter. It reports whether env was consumed.
func (h *Hub) deliverRPCResponse(env protocol.Envelope) bool {
	if env.ID == "" {
		return false
	}
	h.mu.Lock()
	ch, ok := h.rpcWaiters[env.ID]
	if ok {
		delete(h.rpcWaiters, env.ID)
	}
	h.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- env:
	default:
	}
	return true
}

func (h *Hub) onWorkerHeartbeat(ctx context.Context, wc *workerConn, env protocol.Envelope) {
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

	// Update-only: if the machine was just removed from the fleet, the heartbeat
	// must not recreate its row (a read-then-upsert here left ghost workers).
	if err := h.store.TouchWorker(ctx, wc.id, time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Removed while this connection was still registering: tell konsertd to stop.
			h.logger.Info("heartbeat from a removed worker; closing its connection", "worker_id", wc.id)
			_ = wc.conn.Close(websocket.StatusCode(protocol.WSCloseWorkerRemoved), "worker removed from fleet")
			wc.cancel()
		}
		return
	}
	if w, err := h.store.GetWorker(ctx, wc.id); err == nil {
		h.broadcastFleetUpdate("worker", toWorkerView(w, len(p.ActiveTaskIDs), p.Metrics))
	}
}

func (h *Hub) onWorkerFacts(ctx context.Context, wc *workerConn, env protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.WorkerFactsPayload](env)
	if err != nil {
		return
	}
	b, err := json.Marshal(p.Facts)
	if err != nil {
		return
	}
	if err := h.store.UpdateWorkerFacts(ctx, wc.id, string(b), time.Now().UTC()); err != nil {
		return
	}
	if w, err := h.store.GetWorker(ctx, wc.id); err == nil {
		view := toWorkerView(w, wc.activeCount(), h.WorkerMetrics(wc.id))
		facts := p.Facts
		view.Facts = &facts
		h.broadcastFleetUpdate("worker", view)
	}
}

func (h *Hub) onWorkerQuotas(ctx context.Context, wc *workerConn, env protocol.Envelope) {
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
	h.broadcastUsageSummary(ctx)
}

func (h *Hub) onExternalSession(ctx context.Context, wc *workerConn, env protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.ExternalSessionPayload](env)
	if err != nil || p.ExternalID == "" || p.Agent == "" {
		return
	}
	h.handleExternalSession(ctx, wc.id, p)
}

func (h *Hub) onTaskStatus(ctx context.Context, wc *workerConn, env protocol.Envelope) {
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

	wc.mu.Lock()
	if protocol.IsTerminalTaskStatus(p.Status) {
		delete(wc.activeTasks, p.TaskID)
	} else {
		wc.activeTasks[p.TaskID] = true
	}
	wc.mu.Unlock()

	if updated, err := h.store.GetTask(ctx, p.TaskID); err == nil {
		h.broadcastFleetUpdate("task", updated)
		if protocol.IsTerminalTaskStatus(updated.Status) {
			h.notifyTaskDone(updated)
			h.kickScheduler()
		}
	}
}

func (h *Hub) onTaskEvent(ctx context.Context, wc *workerConn, env protocol.Envelope) {
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
			h.broadcastUsageSummary(ctx)
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
			h.broadcastUsageSummary(ctx)
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

// broadcastUsageSummary pushes the current usage and quota summary to every client.
func (h *Hub) broadcastUsageSummary(ctx context.Context) {
	if sum, err := h.store.GetUsageSummary(ctx); err == nil {
		h.broadcastJSON(protocol.MsgUsageUpdate, sum)
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

// RequestWorker sends a correlated control-plane RPC to a connected worker and awaits its reply.
func (h *Hub) RequestWorker(ctx context.Context, workerID, msgType string, payload any) (protocol.Envelope, error) {
	h.mu.RLock()
	wc, ok := h.workers[workerID]
	h.mu.RUnlock()
	if !ok {
		return protocol.Envelope{}, fmt.Errorf("worker %s is not connected", workerID)
	}

	reqID := ids.UUID()
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
