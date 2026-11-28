package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/linkwatch"
	"github.com/BryanSant/syre/internal/mcprelay"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/coder/websocket"
)

// DisconnectWorker closes any active WebSocket session for workerID with WSCloseWorkerRemoved
// so the remote syred exits cleanly instead of auto-reconnecting.
func (h *Hub) DisconnectWorker(workerID string) {
	h.disconnectWorkerWith(workerID, protocol.WSCloseWorkerRemoved, "worker removed from fleet")
}

// disconnectWorkerWith closes workerID's WebSocket session with the given close code.
func (h *Hub) disconnectWorkerWith(workerID string, code int, reason string) {
	h.mu.Lock()
	wc, ok := h.workers[workerID]
	delete(h.workers, workerID)
	delete(h.workerMetrics, workerID)
	h.mu.Unlock()
	h.forgetHeat(workerID)
	h.ports.forget(workerID)
	if ok && wc != nil {
		// Send the WebSocket close frame BEFORE canceling connCtx so coder/websocket doesn't hard-abort TCP with EOF.
		_ = wc.conn.Close(websocket.StatusCode(code), reason)
		wc.cancel()
	}
}

// ServeWorkerConn handles a single authenticated syred WebSocket connection until disconnect.
func (h *Hub) ServeWorkerConn(ctx context.Context, conn *websocket.Conn) {
	conn.SetReadLimit(20 * 1024 * 1024)
	var connWG sync.WaitGroup
	defer connWG.Wait()
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

	// A connection is bound to exactly one machine (its credential's, or the one the
	// hub attached to over SSH or the local socket): it cannot register as another.
	if b, ok := boundMachineFrom(ctx); ok && b.workerID != hello.Worker.ID {
		h.logger.Warn("rejecting syred: hello names another machine",
			"bound", b.workerID, "credential", b.credSID, "via", b.via, "claimed", hello.Worker.ID)
		_ = conn.Close(websocket.StatusPolicyViolation, "connection belongs to another machine")
		return
	}

	// hello.Worker.ID is syred's persistent UUID: the workers.id. identityMu keeps two
	// simultaneous hellos for the same identity from both taking the slot.
	h.identityMu.Lock()
	if !h.supersedeExistingConn(ctx, hello.Worker.ID) {
		h.identityMu.Unlock()
		h.logger.Warn("rejecting duplicate syred connection; the existing one is still alive",
			"worker_id", hello.Worker.ID, "name", hello.Worker.Name)
		_ = conn.Close(websocket.StatusTryAgainLater, "another syred with this identity is connected")
		return
	}

	wc := &workerConn{
		id:          hello.Worker.ID,
		conn:        conn,
		sendCh:      make(chan wsFrame, 512),
		cancel:      cancel,
		activeTasks: make(map[string]bool),
		loopback:    isLoopbackAddr(workerRemoteAddrFrom(ctx)) || workerTransportFrom(ctx) == TransportLocal,
		name:        hello.Worker.Name,
		hostGroup:   hello.Worker.HostGroup,
		hostname:    hello.Worker.Hostname,
		wg:          &connWG,
	}
	wc.mcp = &mcprelay.Remote{
		Ctx:        connCtx,
		Handler:    h.relayedChatMCPHandler(wc.id),
		Send:       func(ctx context.Context, t string, p any) error { return h.sendWorkerJSONCtx(ctx, wc, t, p) },
		RemoteAddr: "syred:" + wc.id,
	}

	// Register worker in SQLite and Hub.
	now := time.Now().UTC()
	factsJSON := ""
	var factsUpdatedAt *time.Time
	if hello.Worker.Facts != nil {
		h.setWorkerCheckouts(hello.Worker.ID, hello.Worker.Facts.Checkouts)
		if b, mErr := json.Marshal(hello.Worker.Facts, protocol.JSONOptions); mErr == nil {
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
	// UpsertWorker mints the m- sid the first time this machine is seen and keeps it after.
	if err := h.store.UpsertWorker(ctx, wModel); err != nil {
		h.logger.Error("upsert worker failed", "worker_id", wModel.ID, "err", err)
	}
	if stored, err := h.store.GetWorker(ctx, wModel.ID); err == nil {
		wModel.SID = stored.SID
		wModel.Launch, wModel.SSHTarget = stored.Launch, stored.SSHTarget
		wModel.LaunchAt = stored.LaunchAt
		wModel.Drained, wModel.DrainedAt = stored.Drained, stored.DrainedAt
		wModel.DefaultShell = stored.DefaultShell
		wModel.AgentsUseGitBash = stored.AgentsUseGitBash
		// The daemon is up: a pending or failed start is resolved.
		if stored.LaunchState != store.LaunchStateNone {
			if err := h.store.SetWorkerLaunchState(ctx, wModel.ID, store.LaunchStateNone, "", now); err == nil {
				wModel.LaunchAt = &now
			}
		}
	}

	h.mu.Lock()
	prevConn := h.workers[wModel.ID]
	h.workers[wModel.ID] = wc
	h.mu.Unlock()
	h.identityMu.Unlock()

	if prevConn != nil && prevConn != wc {
		// The previous connection failed its liveness ping, so it is a dead or
		// half-open socket. Close it with a normal, reconnectable code: 4001
		// (WSCloseWorkerRemoved) would make syred exit for good. The close
		// handshake can block for seconds on a dead peer, so run it off this path.
		old := prevConn
		if !h.goBackground(func(context.Context) {
			if old.conn != nil {
				_ = old.conn.Close(websocket.StatusNormalClosure, "superseded by newer syred connection")
			}
			old.cancel()
		}) {
			old.cancel()
		}
	}

	// The daemon's spool: tell it where to resume (hub_spool.go). A machine that
	// re-attached ends its wake grace (hub_wake.go).
	h.startSpool(ctx, connCtx, wc, hello)
	h.sleepWake.attach(wc.id)

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
		_, _ = h.setTaskStatus(ctx, at.TaskID, st, "", "", nil, causeWorker)
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
				_, _ = h.setTaskStatus(ctx, pt.ID, protocol.TaskStatusCompleted, "session ended before reconnect", "", nil, causeWorker)
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
		"sid", wModel.SID,
		"name", wModel.Name,
		"agents", wModel.Agents,
		"readopted_tasks", readopted,
	)
	helloView := toWorkerView(wModel, wc.activeCount(), nil)
	helloView.Facts = hello.Worker.Facts
	h.broadcastFleetUpdate("worker", helloView)

	// Agent chats that were working on this worker lost their turn with the old
	// connection, unless it still runs there. With a replay pending, that waits until
	// every spooled event is in (finishReplay).
	if wc.replayDone.Load() {
		h.failInterruptedChats(ctx, wc)
	}

	// Active directives for this machine, then re-suspend if a suspend_running
	// pause is still in force (readopt reports the sessions as running).
	h.deliverWorkerDirectives(ctx, wc)

	// Drain any queued tasks that can now run on this worker.
	h.kickScheduler()

	// Liveness (SYR-G8WA3): ping when the hub has sent nothing for LinkQuiet; a link on
	// which nothing (data or pong) arrived for LinkDead is dead. syred sends a vitals
	// heartbeat whenever it is quiet, so a live daemon is always heard from.
	lv := linkwatch.New(linkwatch.Config{QuietAfter: h.LinkQuiet, DeadAfter: h.LinkDead}, nil)
	var dead atomic.Bool
	connWG.Go(func() {
		var pingWG sync.WaitGroup
		defer pingWG.Wait()
		lv.Monitor(connCtx, func() {
			pingWG.Go(func() {
				pctx, pcancel := context.WithTimeout(connCtx, lv.Config().DeadAfter)
				defer pcancel()
				if conn.Ping(pctx) == nil {
					lv.Received()
				}
			})
		}, func() {
			// On a hub wake every link looks dead at once: note the wake first, while this
			// machine still counts as linked (hub_wake.go).
			h.checkWake(time.Now())
			dead.Store(true)
			cancel()
		})
	})

	// Writer goroutine
	connWG.Go(func() {
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
				lv.Sent()
			}
		}
	})

	// Read loop
	for {
		mt, data, err := conn.Read(connCtx)
		if err != nil {
			break
		}
		lv.Received()
		switch mt {
		case websocket.MessageBinary:
			h.handleWorkerBinary(data)
		case websocket.MessageText:
			env, decErr := protocol.DecodeEnvelope(data)
			if decErr != nil {
				continue
			}
			h.handleWorkerEnvelope(ctx, wc, env)
		}
	}

	// Disconnect cleanup: remove active conn, keep DB status until reaper marks offline
	// OR if no active tasks, mark offline promptly while preserving restart re-adoption window.
	h.checkWake(time.Now())
	h.mu.Lock()
	current := h.workers[wModel.ID] == wc
	if current {
		delete(h.workers, wModel.ID)
	}
	h.mu.Unlock()
	if dead.Load() && current {
		h.logger.Warn("link to "+wModel.Name+" is dead: nothing received for "+lv.Config().DeadAfter.String()+"; marking it offline", "worker_id", wModel.ID)
		h.markLinkDead(wModel.ID)
		return
	}
	h.logger.Info("worker disconnected", "worker_id", wModel.ID)
}

// markLinkDead marks a worker whose link died offline at once (the reaper would only do
// so after StaleWorkerTimeout). Its tasks are left alone for the rest of that window, as
// after any disconnect: the daemon keeps them running and re-adopts them in worker.hello;
// if it has not come back by then, the orphan policies apply.
func (h *Hub) markLinkDead(workerID string) {
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	changed, err := h.store.MarkWorkerOffline(ctx, workerID)
	if err == nil && changed {
		if w, err := h.store.GetWorker(ctx, workerID); err == nil {
			h.broadcastFleetUpdate("worker", toWorkerView(w, 0, nil))
		}
	}
	cancel()
	h.forgetHeat(workerID)
	if !changed {
		return
	}
	grace := h.StaleWorkerTimeout - effectiveDeadAfter(h.LinkDead)
	h.goBackground(func(ctx context.Context) {
		t := time.NewTimer(max(grace, 0))
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// After a hub wake, the machine gets the grace to re-attach first.
		if h.inWakeGrace(workerID) {
			h.sleepWake.wait(ctx, workerID)
		}
		h.mu.RLock()
		_, back := h.workers[workerID]
		h.mu.RUnlock()
		if back {
			return
		}
		if w, err := h.store.GetWorker(ctx, workerID); err != nil || w.Status != "offline" {
			return
		}
		h.handleOrphanTasks(ctx, workerID)
	})
}

// effectiveDeadAfter is the effective dead-link timeout for a configured value (0: the default).
func effectiveDeadAfter(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return linkwatch.DefaultDeadAfter
}

// supersedePingTimeout bounds the liveness ping sent to an existing connection
// when a new hello arrives for the same worker.
const supersedePingTimeout = 3 * time.Second

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
	if wc.mcp != nil && wc.mcp.Handle(env) {
		return
	}
	switch env.Type {
	case protocol.MsgWorkerHeartbeat:
		h.onWorkerHeartbeat(ctx, wc, env)
	case protocol.MsgWorkerPorts:
		h.onWorkerPorts(wc, env)
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
	case protocol.MsgOperatorCommand:
		h.onOperatorCommand(ctx, wc, env)
	case protocol.MsgProcessExit:
		h.onProcessExit(ctx, wc, env)
	case protocol.MsgObservedEvents:
		h.onObservedEvents(wc, env)
	case protocol.MsgSessionActivity:
		h.onSessionActivity(ctx, wc, env)
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
	// A heartbeat replayed from the daemon's spool only carries history: its vitals are
	// stored at their own time; its task list and load are stale.
	if (!p.At.IsZero() && time.Since(p.At) > lateVitalsAfter) || (env.Seq != 0 && env.Seq <= wc.helloLastSeq && !wc.replayDone.Load()) {
		h.storeLateVitals(ctx, wc, p, env.Seq)
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
		p.Metrics.OpenChats = p.ActiveChatIDs
		h.mu.Lock()
		h.workerMetrics[wc.id] = p.Metrics
		h.mu.Unlock()
		h.foldLoad(wc.id, p.Metrics.Load, time.Now())
	}

	// Update-only: if the machine was just removed from the fleet, the heartbeat
	// must not recreate its row (a read-then-upsert here left ghost workers).
	now := time.Now().UTC()
	if err := h.store.TouchWorker(ctx, wc.id, now); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Removed while this connection was still registering: tell syred to stop.
			h.logger.Info("heartbeat from a removed worker; closing its connection", "worker_id", wc.id)
			_ = wc.conn.Close(websocket.StatusCode(protocol.WSCloseWorkerRemoved), "worker removed from fleet")
			wc.cancel()
		}
		return
	}
	h.renewWorkerTicketLeases(ctx, wc.id, p.ActiveTaskIDs, p.ActiveChatIDs, now)
	if w, err := h.store.GetWorker(ctx, wc.id); err == nil {
		metrics := p.Metrics
		if metrics == nil {
			metrics = h.WorkerMetrics(wc.id)
		}
		cores := 0
		if f := decodeWorkerFacts(w.FactsJSON); f != nil {
			cores = f.CPUCores
		}
		h.updateHeat(wc.id, metrics, cores, len(p.ActiveTaskIDs), w.MaxConcurrent, time.Now(), env.Seq)
		h.broadcastFleetUpdate("worker", toWorkerView(w, len(p.ActiveTaskIDs), p.Metrics))
	}
}

func (h *Hub) onWorkerFacts(ctx context.Context, wc *workerConn, env protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.WorkerFactsPayload](env)
	if err != nil {
		return
	}
	h.setWorkerCheckouts(wc.id, p.Facts.Checkouts)
	b, err := json.Marshal(p.Facts, protocol.JSONOptions)
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
	if len(p.Worktrees) > 0 {
		wts := make([]store.TaskWorktree, len(p.Worktrees))
		for i, wt := range p.Worktrees {
			wts[i] = store.TaskWorktree{
				RepoName:     wt.RepoName,
				CheckoutPath: wt.CheckoutPath,
				WorktreePath: wt.WorktreePath,
				Branch:       wt.Branch,
				BaseSHA:      wt.BaseSHA,
			}
		}
		_ = h.store.UpdateTaskWorktrees(ctx, p.TaskID, wts)
	}
	gitJSON := ""
	if p.GitSummary != nil {
		if b, err := json.Marshal(p.GitSummary, protocol.JSONOptions); err == nil {
			gitJSON = string(b)
		}
	}
	if p.Digest != nil {
		d := *p.Digest
		mergeTaskFollowupsIntoDigest(&d, cur.Followups)
		if b, err := json.Marshal(&d, protocol.JSONOptions); err == nil {
			_ = h.store.UpdateTaskDigest(ctx, p.TaskID, string(b))
		}
		if protocol.IsTerminalTaskStatus(p.Status) {
			h.recordAdHocAttempt(ctx, cur, wc.id, d, p.Status, p.ExitCode)
		}
	} else if protocol.IsTerminalTaskStatus(p.Status) && len(cur.Followups) > 0 && strings.TrimSpace(cur.DigestJSON) != "" {
		var d protocol.RunDigest
		if err := json.Unmarshal([]byte(cur.DigestJSON), &d, protocol.JSONOptions); err == nil {
			mergeTaskFollowupsIntoDigest(&d, cur.Followups)
			if b, err := json.Marshal(&d, protocol.JSONOptions); err == nil {
				_ = h.store.UpdateTaskDigest(ctx, p.TaskID, string(b))
			}
		}
	}
	cause := h.taskCauseForWorkerStatus(cur, p.Status, p.ExitCode)

	wc.mu.Lock()
	if protocol.IsTerminalTaskStatus(p.Status) {
		delete(wc.activeTasks, p.TaskID)
	} else {
		wc.activeTasks[p.TaskID] = true
	}
	wc.mu.Unlock()

	updated, err := h.setTaskStatus(ctx, p.TaskID, p.Status, p.Summary, gitJSON, p.ExitCode, cause)
	if err == nil && protocol.IsTerminalTaskStatus(updated.Status) {
		h.kickScheduler()
		if updated.Status == protocol.TaskStatusCompleted && cur.Status != protocol.TaskStatusCompleted &&
			updated.Origin != protocol.TaskOriginExternal && updated.Origin != protocol.TaskOriginScratch &&
			taskHasGitChanges(updated) {
			h.onTaskEnteredReview(updated)
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
				AccountID:    h.usageAccount(ctx, p.TaskID, p.Meta),
				InputTokens:  inTok,
				OutputTokens: outTok,
			})
			h.broadcastUsageSummary(ctx)
		}
		return
	}
	metaJSON := ""
	if len(p.Meta) > 0 {
		if b, err := json.Marshal(p.Meta, protocol.JSONOptions); err == nil {
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
		// Callers (REST, MCP, commander tools) may name the worker by its sid.
		h.mu.RLock()
		wc, ok = h.workers[resolveRef(ctx, h.store, ids.Machine, workerID)]
		h.mu.RUnlock()
	}
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

// sendWorkerJSONCtx queues a JSON frame for wc, waiting while its send queue is full
// until ctx ends (relayed MCP responses must not be dropped).
func (h *Hub) sendWorkerJSONCtx(ctx context.Context, wc *workerConn, msgType string, payload any) error {
	raw, err := protocol.NewEnvelope(msgType, "", payload)
	if err != nil {
		return err
	}
	select {
	case wc.sendCh <- wsFrame{msgType: websocket.MessageText, data: raw}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// relayedChatMCPHandler serves the per-chat MCP requests syred relays over
// workerID's link.
func (h *Hub) relayedChatMCPHandler(workerID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.chatMCP == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "per-chat MCP is not available"})
			return
		}
		h.chatMCP(w, r, workerID)
	})
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

// usageAccount is the agent account a usage report belongs to: the report's own
// account_id, else the account the task was started with ("" = the default login).
func (h *Hub) usageAccount(ctx context.Context, taskID string, meta map[string]any) string {
	if a, _ := meta["account_id"].(string); a != "" {
		return a
	}
	if taskID == "" {
		return ""
	}
	spec, err := h.store.GetTaskStartSpec(ctx, taskID)
	if err != nil || spec.PayloadJSON == "" {
		return ""
	}
	var start protocol.TaskStartPayload
	if json.Unmarshal([]byte(spec.PayloadJSON), &start) != nil {
		return ""
	}
	return start.AccountID
}
