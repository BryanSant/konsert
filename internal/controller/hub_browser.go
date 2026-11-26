package controller

import (
	"context"
	"errors"
	"time"

	"github.com/BryanSant/syre/internal/commander"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/coder/websocket"
)

// ServeBrowserConn handles a single authenticated browser PWA WebSocket client.
func (h *Hub) ServeBrowserConn(ctx context.Context, conn *websocket.Conn, user string) {
	conn.SetReadLimit(2 * 1024 * 1024)
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Hijacked WebSocket handlers outlive http.Server.Shutdown; tie them to the hub.
	defer context.AfterFunc(h.ctx, cancel)()

	client := &browserClient{
		user:     user,
		deviceID: identityFrom(ctx).DeviceID(),
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
		h.handleChatSend(ctx, c, env)

	case msgChatRespond, msgChatControl, msgChatClose:
		h.handleAgentChatBrowserMessage(ctx, c, env)

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

// chatSession serializes commander turns for one chat session.
type chatSession struct {
	sem  chan struct{} // capacity 1: held by the running turn
	refs int           // running + waiting turns; guarded by Hub.mu
}

func (h *Hub) subscribeChat(c *browserClient, sessionID string) {
	c.mu.Lock()
	c.chatSubs[sessionID] = true
	c.mu.Unlock()
}

// startChatTurn runs one commander turn on a tracked goroutine. The turn's
// context ends when the client disconnects, the hub shuts down, or
// ChatTurnTimeout elapses. Turns of the same session run one at a time, in
// arrival order; past maxChatTurnsPerSession the request is rejected.
func (h *Hub) startChatTurn(c *browserClient, orch *commander.Service, p protocol.ChatSendPayload) {
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

		err := orch.HandleUserMessage(turnCtx, p.SessionID, p.Content, commander.StreamCallbacks{
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
				Error:     "commander turn timed out",
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

// DisconnectDevice closes every browser socket opened with a device's credential (the
// device was revoked). Returns how many were closed.
func (h *Hub) DisconnectDevice(deviceID string) int {
	if deviceID == "" {
		return 0
	}
	h.mu.RLock()
	var victims []*browserClient
	for c := range h.clients {
		if c.deviceID == deviceID {
			victims = append(victims, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range victims {
		go func() {
			_ = c.conn.Close(websocket.StatusPolicyViolation, "signed out")
			c.cancel()
		}()
	}
	return len(victims)
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
		p.Worker = h.withWakeLock(item)
	case "task":
		p.Task = item
		if t, ok := item.(store.Task); ok {
			h.taskStatusActivity(t)
		}
	case "event":
		p.Event = item
	case "audit":
		p.Audit = item
	case "agent_accounts":
		p.Accounts = item
	case "chat_session":
		p.ChatSession = item
	case "project":
		p.Project = item
	case "repo":
		p.Repo = item
	case "ticket":
		p.Ticket = item
	case "ticket_edge", "ticket_edge_deleted":
		p.TicketEdge = item
	case "fleet_event":
		p.FleetEvent = item
	case "hold":
		p.Hold = item
	case "request":
		p.Request = item
	case "override":
		p.Override = item
	case "veto":
		p.Veto = item
	case "pause":
		p.Pause = item
	case "question":
		p.Question = item
	case "activity":
		p.Activity = item
	case "review":
		p.Review = item
	case "device":
		p.Device = item
	case "shell_reminder":
		p.ShellReminder = item
	case "autopilot":
		p.Autopilot = item
	case "directive":
		p.Directive = item
	case "ticket_deleted", "space_deleted", "worker_deleted", "workspace_deleted", "project_deleted", "repo_deleted", "worktree_deleted", "chat_session_deleted", "device_revoked":
		if id, ok := item.(string); ok {
			p.DeletedID = id
		}
	}
	h.broadcastJSON(protocol.MsgFleetUpdate, p)
	if kind == "ticket" || kind == "ticket_deleted" || kind == "project_deleted" {
		h.broadcastAutopilotUpdate(context.Background())
	}
}
