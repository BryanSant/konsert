package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/orchestrator"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/google/uuid"
)

// Agent chat, hub side (docs/design/agent-chat.md).
//
// Browser messages (hub-local; payloads below):
//
//	chat.send     {session_id, content, attachments?}  -> agent.chat.open + agent.chat.prompt
//	chat.respond  {session_id, elicitation_id, option_id?, text?}
//	chat.control  {session_id, cancel?, model?, effort?, approval_mode?}
//	chat.close    {session_id}
//	agent.chat.event (hub -> browser): protocol.AgentChatEvent with the hub-assigned seq.
//
// Sequence numbers: the hub assigns every stored and broadcast event's Seq itself
// (per chat, max stored seq + 1, under the chat's lock). Worker events keep their
// worker-assigned seq only for de-duplication; hub-generated events (the user's
// message, ask_user questions, refusals, "connection lost") take the next hub seq
// like any other event. One sequence per chat therefore never collides, survives a
// konsertd restart that resets the worker's counter, and is what
// GET /api/chats/{id}/events?after_seq= pages on.
const (
	msgChatRespond = "chat.respond"
	msgChatControl = "chat.control"
	msgChatClose   = "chat.close"

	// eventUserMessage is a hub-generated AgentChatEvent kind recording the user's
	// prompt (Text, TurnID) in the transcript.
	eventUserMessage = "user_message"

	// defaultAskUserTimeout bounds how long ask_user waits for the user.
	defaultAskUserTimeout = 30 * time.Minute

	// orchestratorChatSetting stores the default orchestrator chat's ID.
	orchestratorChatSetting = "orchestrator_chat_id"
)

// chatSendRequest is the browser chat.send payload (protocol.ChatSendPayload plus attachments).
type chatSendRequest struct {
	SessionID   string   `json:"session_id"`
	Content     string   `json:"content"`
	Attachments []string `json:"attachments,omitempty"`
}

type chatRespondRequest struct {
	SessionID     string `json:"session_id"`
	ElicitationID string `json:"elicitation_id"`
	OptionID      string `json:"option_id,omitempty"`
	Text          string `json:"text,omitempty"`
}

type chatControlRequest struct {
	SessionID    string `json:"session_id"`
	Cancel       bool   `json:"cancel,omitempty"`
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	ApprovalMode string `json:"approval_mode,omitempty"`
}

type chatCloseRequest struct {
	SessionID string `json:"session_id"`
}

// agentCapabilitiesRefresh is the agent.capabilities.refresh payload.
type agentCapabilitiesRefresh struct {
	AccountID string `json:"account_id,omitempty"`
}

// askAnswer is the user's reply to an ask_user question.
type askAnswer struct {
	optionID string
	text     string
	reason   string // set when the question ended without an answer (cancelled, closed)
}

type pendingAsk struct {
	options []protocol.AgentElicitationOption
	answer  chan askAnswer // buffered (1)
}

// agentChatRT is the hub's in-memory state for one agent chat.
type agentChatRT struct {
	mu        sync.Mutex
	seq       int64 // last hub-assigned seq
	seqLoaded bool
	// lastWorkerSeq de-duplicates worker events per worker connection.
	lastWorkerSeq int64
	seqConn       *workerConn
	turnID        string // the turn in flight (for hub events raised during it)
	token         string // plaintext MCP bearer token (memory only)
	mcp           http.Handler
	mcpFleet      bool
	asks          map[string]*pendingAsk
}

func (h *Hub) chatRT(chatID string) *agentChatRT {
	h.mu.Lock()
	defer h.mu.Unlock()
	rt := h.agentChats[chatID]
	if rt == nil {
		rt = &agentChatRT{asks: make(map[string]*pendingAsk)}
		h.agentChats[chatID] = rt
	}
	return rt
}

// AgentAccountView is an agent account with the worker hosting it.
type AgentAccountView struct {
	WorkerID     string `json:"worker_id"`
	WorkerName   string `json:"worker_name,omitempty"`
	WorkerOnline bool   `json:"worker_online"`
	protocol.AgentAccount
	UpdatedAt time.Time `json:"updated_at"`
	// Exhausted is true while a usage window is used up (state "exhausted" with a
	// future or unknown reset); ExhaustedWindow/ExhaustedUntil describe it.
	Exhausted       bool       `json:"exhausted"`
	ExhaustedWindow string     `json:"exhausted_window,omitempty"`
	ExhaustedUntil  *time.Time `json:"exhausted_until,omitempty"`
}

// --- event emission -------------------------------------------------------------

// emitOpts tunes emitChatEvent.
type emitOpts struct {
	// fromWorker de-duplicates by the event's worker seq on this connection.
	fromWorker *workerConn
	// update mutates the session alongside the event's own state changes.
	update func(*store.ChatSession)
}

// emitChatEvent assigns the next hub seq, persists ev, applies its state to the
// session, coalesces the turn when it ends, and fans it out to chat subscribers
// as agent.chat.event. It reports false when the event was a duplicate or the
// chat does not exist.
func (h *Hub) emitChatEvent(ctx context.Context, chatID string, ev protocol.AgentChatEvent, o emitOpts) (protocol.AgentChatEvent, bool) {
	ctx = context.WithoutCancel(ctx)
	rt := h.chatRT(chatID)
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if o.fromWorker != nil && ev.Seq > 0 {
		if rt.seqConn == o.fromWorker && ev.Seq <= rt.lastWorkerSeq {
			return ev, false
		}
		rt.seqConn, rt.lastWorkerSeq = o.fromWorker, ev.Seq
	}
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil {
		return ev, false
	}
	if !rt.seqLoaded {
		maxSeq, err := h.store.MaxChatEventSeq(ctx, chatID)
		if err != nil {
			h.logger.Warn("load chat seq failed", "chat_id", chatID, "err", err)
			return ev, false
		}
		rt.seq, rt.seqLoaded = maxSeq, true
	}
	if ev.TurnID == "" {
		ev.TurnID = rt.turnID
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	ev.ChatID = chatID
	ev.Seq = rt.seq + 1
	raw, err := json.Marshal(ev)
	if err != nil {
		return ev, false
	}
	if _, err := h.store.AppendChatEvent(ctx, store.ChatEvent{
		ChatID: chatID, Seq: ev.Seq, TurnID: ev.TurnID, Kind: ev.Kind, JSON: string(raw), CreatedAt: ev.At,
	}); err != nil {
		h.logger.Warn("persist chat event failed", "chat_id", chatID, "kind", ev.Kind, "err", err)
		return ev, false
	}
	rt.seq = ev.Seq

	before := cs
	applyEventToSession(&cs, ev)
	if o.update != nil {
		o.update(&cs)
	}
	if cs != before {
		if err := h.store.UpdateChatSession(ctx, cs); err != nil {
			h.logger.Warn("update chat session failed", "chat_id", chatID, "err", err)
		}
	}
	if ev.TurnID != "" && (ev.Kind == protocol.EventTurnEnd || ev.Kind == protocol.EventError) {
		if err := h.store.CoalesceChatTurn(ctx, chatID, ev.TurnID); err != nil {
			h.logger.Warn("coalesce chat turn failed", "chat_id", chatID, "err", err)
		}
		if rt.turnID == ev.TurnID {
			rt.turnID = ""
		}
	}
	// Broadcast under the chat lock so subscribers see events in seq order
	// (sendChatEvent only enqueues).
	h.sendChatEvent(chatID, protocol.MsgAgentChatEvent, ev)
	return ev, true
}

// applyEventToSession derives the session's status, CLI session id and last error from ev.
func applyEventToSession(cs *store.ChatSession, ev protocol.AgentChatEvent) {
	if ev.AgentSessionID != "" {
		cs.AgentSessionID = ev.AgentSessionID
	}
	switch ev.Kind {
	case protocol.EventStatus:
		if ev.Status != "" {
			cs.Status = ev.Status
		}
	case protocol.EventSessionReady:
		if ev.Status != "" {
			cs.Status = ev.Status
		}
	case protocol.EventElicitation:
		cs.Status = protocol.ChatStatusNeedsYou
	case protocol.EventElicitationOK:
		if cs.Status == protocol.ChatStatusNeedsYou {
			cs.Status = protocol.ChatStatusWorking
		}
	case protocol.EventTurnEnd:
		switch ev.StopReason {
		case "cancelled":
			cs.Status = protocol.ChatStatusCancelled
		case "error":
			cs.Status = protocol.ChatStatusFailed
		default:
			cs.Status = protocol.ChatStatusDone
		}
	case protocol.EventError:
		cs.Status = protocol.ChatStatusFailed
		if ev.Error != nil {
			if b, err := json.Marshal(ev.Error); err == nil {
				cs.LastErrorJSON = string(b)
			}
		}
	}
}

// hubError emits a hub-generated EventError for a chat.
func (h *Hub) hubError(ctx context.Context, chatID string, e protocol.AgentChatError) {
	h.emitChatEvent(ctx, chatID, protocol.AgentChatEvent{Kind: protocol.EventError, Error: &e}, emitOpts{})
}

// --- worker messages --------------------------------------------------------------

// onAgentChatEvent persists and fans out one worker event for a chat this worker hosts.
func (h *Hub) onAgentChatEvent(ctx context.Context, wc *workerConn, env protocol.Envelope) {
	ev, err := protocol.DecodePayload[protocol.AgentChatEvent](env)
	if err != nil || ev.ChatID == "" || ev.Kind == "" {
		return
	}
	cs, err := h.store.GetChatSession(ctx, ev.ChatID)
	if err != nil || cs.Backend != store.ChatBackendAgent || cs.WorkerID != wc.id {
		h.logger.Debug("dropping agent.chat.event for a chat this worker does not host", "chat_id", ev.ChatID, "worker_id", wc.id)
		return
	}
	h.emitChatEvent(ctx, ev.ChatID, ev, emitOpts{fromWorker: wc})
}

// onAgentCapabilities stores a worker's agent accounts and pushes them to browsers.
func (h *Hub) onAgentCapabilities(ctx context.Context, wc *workerConn, env protocol.Envelope) {
	rep, err := protocol.DecodePayload[protocol.AgentCapabilitiesReport](env)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, a := range rep.Accounts {
		if a.ID == "" {
			continue
		}
		b, err := json.Marshal(a)
		if err != nil {
			continue
		}
		if err := h.store.UpsertAgentAccount(ctx, store.AgentAccountRecord{WorkerID: wc.id, AccountID: a.ID, JSON: string(b), UpdatedAt: now}); err != nil {
			h.logger.Warn("store agent account failed", "worker_id", wc.id, "account_id", a.ID, "err", err)
		}
	}
	views, _ := h.agentAccountViews(ctx, wc.id)
	h.broadcastFleetUpdate("agent_accounts", views)
}

// failInterruptedChats runs when a worker (re)connects: a chat that was working on
// it lost its turn with the old connection, so it gets a "connection lost" error.
// A chat blocked only on a hub-side ask_user question is left alone.
func (h *Hub) failInterruptedChats(ctx context.Context, wc *workerConn) {
	sessions, err := h.store.ListChatSessionsByWorker(ctx, wc.id)
	if err != nil {
		return
	}
	for _, cs := range sessions {
		switch cs.Status {
		case protocol.ChatStatusWorking, protocol.ChatStatusStarting, protocol.ChatStatusNeedsYou:
		default:
			continue
		}
		rt := h.chatRT(cs.ID)
		rt.mu.Lock()
		hubAsk := len(rt.asks) > 0
		rt.mu.Unlock()
		if hubAsk {
			continue
		}
		h.hubError(ctx, cs.ID, protocol.AgentChatError{
			Kind:      protocol.ErrKindExit,
			Message:   "Lost the connection to the worker while the agent was working; the turn was interrupted. Send your message again to continue the conversation.",
			Retryable: true,
		})
	}
}

// --- accounts ---------------------------------------------------------------------

func decodeAccount(rec store.AgentAccountRecord) (protocol.AgentAccount, bool) {
	var a protocol.AgentAccount
	if err := json.Unmarshal([]byte(rec.JSON), &a); err != nil {
		return a, false
	}
	if a.ID == "" {
		a.ID = rec.AccountID
	}
	return a, true
}

// workerAccounts returns a worker's reported agent accounts.
func (h *Hub) workerAccounts(ctx context.Context, workerID string) []protocol.AgentAccount {
	recs, err := h.store.ListAgentAccounts(ctx, workerID)
	if err != nil {
		return nil
	}
	out := make([]protocol.AgentAccount, 0, len(recs))
	for _, r := range recs {
		if a, ok := decodeAccount(r); ok {
			out = append(out, a)
		}
	}
	return out
}

// agentAccountViews lists accounts (one worker's, or all when workerID is "").
func (h *Hub) agentAccountViews(ctx context.Context, workerID string) ([]AgentAccountView, error) {
	recs, err := h.store.ListAgentAccounts(ctx, workerID)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	if ws, err := h.store.ListWorkers(ctx); err == nil {
		for _, w := range ws {
			names[w.ID] = w.Name
		}
	}
	out := make([]AgentAccountView, 0, len(recs))
	for _, r := range recs {
		a, ok := decodeAccount(r)
		if !ok {
			continue
		}
		v := AgentAccountView{
			WorkerID: r.WorkerID, WorkerName: names[r.WorkerID], WorkerOnline: h.IsWorkerConnected(r.WorkerID),
			AgentAccount: a, UpdatedAt: r.UpdatedAt,
		}
		if u, ok := exhaustedWindow(a, time.Now()); ok {
			v.Exhausted, v.ExhaustedWindow, v.ExhaustedUntil = true, u.Window, u.ResetsAt
		}
		out = append(out, v)
	}
	return out, nil
}

// findAccount picks the account for (agent, accountID) among accts: the explicit
// accountID, else the agent's default account, else the agent's first account.
func findAccount(accts []protocol.AgentAccount, agent, accountID string) (protocol.AgentAccount, bool) {
	if accountID != "" {
		for _, a := range accts {
			if a.ID == accountID {
				return a, true
			}
		}
		return protocol.AgentAccount{}, false
	}
	if agent == "" {
		return protocol.AgentAccount{}, false
	}
	var first *protocol.AgentAccount
	for i, a := range accts {
		if !strings.EqualFold(a.Agent, agent) {
			continue
		}
		if a.IsDefault {
			return a, true
		}
		if first == nil {
			first = &accts[i]
		}
	}
	if first != nil {
		return *first, true
	}
	return protocol.AgentAccount{}, false
}

// lookupAccount returns the stored account for (worker, agent, accountID).
func (h *Hub) lookupAccount(ctx context.Context, workerID, agent, accountID string) (protocol.AgentAccount, bool) {
	return findAccount(h.workerAccounts(ctx, workerID), agent, accountID)
}

// errAccountLoggedOut is returned when work targets a logged-out account.
type errAccountLoggedOut struct {
	account protocol.AgentAccount
	worker  string
}

func (e errAccountLoggedOut) Error() string {
	label := e.account.Label
	if label == "" {
		label = e.account.ID
	}
	msg := fmt.Sprintf("agent account %q (%s) on worker %s is logged out", e.account.ID, label, e.worker)
	if r := e.account.Auth.Relogin; r != nil && r.Command != "" {
		msg += "; sign in on that host with: " + r.Command
	}
	return msg
}

// checkAccountLoggedIn refuses work for an account the worker reports as logged out.
// Unknown accounts pass (the worker decides).
func (h *Hub) checkAccountLoggedIn(ctx context.Context, workerID, agent, accountID string) error {
	if agent == "shell" || workerID == "" {
		return nil
	}
	a, ok := h.lookupAccount(ctx, workerID, agent, accountID)
	if !ok || a.Auth.State != protocol.AuthLoggedOut {
		return nil
	}
	return errAccountLoggedOut{account: a, worker: workerID}
}

func accountLoggedIn(a protocol.AgentAccount) bool {
	return a.Auth.State == protocol.AuthOK || a.Auth.State == protocol.AuthExpiring
}

func accountChatUsable(a protocol.AgentAccount) bool {
	return a.ChatSupported && a.Auth.State != protocol.AuthLoggedOut
}

// bestAccount picks a worker's preferred chat account: a logged-in claude (the
// default account first), then any logged-in chat-capable account (defaults
// first), then a chat-capable account whose login state is unknown. Accounts
// with an exhausted usage window are chosen only when nothing else qualifies.
func bestAccount(accts []protocol.AgentAccount) (protocol.AgentAccount, bool) {
	var fresh []protocol.AgentAccount
	for _, a := range accts {
		if !accountExhausted(a) {
			fresh = append(fresh, a)
		}
	}
	if a, ok := bestAccountTiered(fresh); ok {
		return a, true
	}
	return bestAccountTiered(accts)
}

func bestAccountTiered(accts []protocol.AgentAccount) (protocol.AgentAccount, bool) {
	tiers := []func(protocol.AgentAccount) bool{
		func(a protocol.AgentAccount) bool { return accountLoggedIn(a) && a.Agent == "claude" && a.IsDefault },
		func(a protocol.AgentAccount) bool { return accountLoggedIn(a) && a.Agent == "claude" },
		func(a protocol.AgentAccount) bool { return accountLoggedIn(a) && a.IsDefault },
		accountLoggedIn,
		func(protocol.AgentAccount) bool { return true },
	}
	for _, tier := range tiers {
		for _, a := range accts {
			if accountChatUsable(a) && tier(a) {
				return a, true
			}
		}
	}
	return protocol.AgentAccount{}, false
}

// defaultAgentTarget picks the worker and account for a new chat (and the default
// orchestrator): the hub's own machine if a connected worker runs there and has a
// usable chat account; otherwise the first connected worker with a logged-in
// claude; then the first with any logged-in chat-capable account; then any
// chat-capable account whose login state is unknown.
func (h *Hub) defaultAgentTarget(ctx context.Context) (string, protocol.AgentAccount, bool) {
	workers, err := h.store.ListWorkers(ctx)
	if err != nil {
		return "", protocol.AgentAccount{}, false
	}
	type cand struct {
		w     store.Worker
		accts []protocol.AgentAccount
	}
	var cands []cand
	for _, w := range workers {
		if !h.IsWorkerConnected(w.ID) {
			continue
		}
		if accts := h.workerAccounts(ctx, w.ID); len(accts) > 0 {
			cands = append(cands, cand{w, accts})
		}
	}
	host := ""
	if h.hostname != nil {
		host = h.hostname()
	}
	// Exhausted accounts (usage window used up) are skipped while any other
	// usable account exists anywhere in the fleet.
	for _, allowExhausted := range []bool{false, true} {
		usable := func(a protocol.AgentAccount) bool {
			return accountChatUsable(a) && (allowExhausted || !accountExhausted(a))
		}
		for _, c := range cands {
			if isLocalhostWorker(c.w, host) {
				if a, ok := bestAccountTiered(filterAccounts(c.accts, usable)); ok {
					return c.w.ID, a, true
				}
			}
		}
		tiers := []func(protocol.AgentAccount) bool{
			func(a protocol.AgentAccount) bool { return accountLoggedIn(a) && a.Agent == "claude" },
			accountLoggedIn,
			func(protocol.AgentAccount) bool { return true },
		}
		for _, tier := range tiers {
			for _, c := range cands {
				sel := filterAccounts(c.accts, func(a protocol.AgentAccount) bool { return usable(a) && tier(a) })
				if a, ok := bestAccountTiered(sel); ok {
					return c.w.ID, a, true
				}
			}
		}
	}
	return "", protocol.AgentAccount{}, false
}

func filterAccounts(accts []protocol.AgentAccount, keep func(protocol.AgentAccount) bool) []protocol.AgentAccount {
	var out []protocol.AgentAccount
	for _, a := range accts {
		if keep(a) {
			out = append(out, a)
		}
	}
	return out
}

// applyAccountDefaults fills model, approval mode and effort from the account's capabilities.
func applyAccountDefaults(cs *store.ChatSession, a protocol.AgentAccount) {
	cs.Agent = a.Agent
	cs.AccountID = a.ID
	if cs.Model == "" {
		cs.Model = a.DefaultModel
		if cs.Model == "" {
			for _, m := range a.Models {
				if m.IsDefault {
					cs.Model = m.ID
					break
				}
			}
		}
	}
	if cs.ApprovalMode == "" {
		cs.ApprovalMode = a.DefaultApproval
	}
	if cs.Effort == "" {
		for _, m := range a.Models {
			if m.ID == cs.Model {
				cs.Effort = m.DefaultEffort
				break
			}
		}
	}
}

// resolveChatTarget fills an agent chat's worker, agent, account and defaults.
// An explicit worker/agent/account is kept; anything missing is chosen from the
// reported capabilities.
func (h *Hub) resolveChatTarget(ctx context.Context, cs *store.ChatSession) {
	if cs.WorkerID == "" {
		if cs.Agent != "" || cs.AccountID != "" {
			// Prefer a worker where the named account is usable; otherwise take
			// one where it exists at all, so sending reports why it cannot run
			// (e.g. logged out, with the re-login command).
			workers, _ := h.store.ListWorkers(ctx)
			for _, strict := range []bool{true, false} {
				for _, w := range workers {
					if !h.IsWorkerConnected(w.ID) {
						continue
					}
					if a, ok := h.lookupAccount(ctx, w.ID, cs.Agent, cs.AccountID); ok && (!strict || accountChatUsable(a)) {
						cs.WorkerID = w.ID
						applyAccountDefaults(cs, a)
						return
					}
				}
			}
		}
		if wid, a, ok := h.defaultAgentTarget(ctx); ok && cs.Agent == "" && cs.AccountID == "" {
			cs.WorkerID = wid
			applyAccountDefaults(cs, a)
		}
		return
	}
	accts := h.workerAccounts(ctx, cs.WorkerID)
	if a, ok := findAccount(accts, cs.Agent, cs.AccountID); ok {
		applyAccountDefaults(cs, a)
		return
	}
	if cs.Agent == "" && cs.AccountID == "" {
		if a, ok := bestAccount(accts); ok {
			applyAccountDefaults(cs, a)
		}
	}
}

// --- MCP token and open payload -------------------------------------------------

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func newChatToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ensureChatToken returns the chat's MCP bearer token, minting one (and storing
// its hash) when the hub holds none in memory, e.g. after a restart.
func (h *Hub) ensureChatToken(ctx context.Context, chatID string) (string, error) {
	rt := h.chatRT(chatID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.token != "" {
		return rt.token, nil
	}
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil {
		return "", err
	}
	tok, err := newChatToken()
	if err != nil {
		return "", err
	}
	cs.MCPTokenHash = hashToken(tok)
	if err := h.store.UpdateChatSession(ctx, cs); err != nil {
		return "", err
	}
	rt.token = tok
	return tok, nil
}

// verifyChatToken checks a presented bearer token against the chat's token
// (in memory, else the stored hash). Deleting the chat revokes it.
func (h *Hub) verifyChatToken(ctx context.Context, chatID, tok string) (store.ChatSession, bool) {
	if chatID == "" || tok == "" {
		return store.ChatSession{}, false
	}
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil || cs.Backend != store.ChatBackendAgent || cs.MCPTokenHash == "" {
		return store.ChatSession{}, false
	}
	if subtle.ConstantTimeCompare([]byte(hashToken(tok)), []byte(cs.MCPTokenHash)) != 1 {
		return store.ChatSession{}, false
	}
	rt := h.chatRT(chatID)
	rt.mu.Lock()
	if rt.token == "" {
		rt.token = tok // recovered after a hub restart
	}
	rt.mu.Unlock()
	return cs, true
}

// requestBaseURL is the controller base URL as the peer reached it (scheme + Host).
func requestBaseURL(r *http.Request) string {
	if r == nil || r.Host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

type workerBaseURLKey struct{}

// withWorkerBaseURL records the URL a worker used to reach the hub (its upgrade
// request) so per-chat MCP URLs handed to its agents are reachable from it.
func withWorkerBaseURL(ctx context.Context, base string) context.Context {
	return context.WithValue(ctx, workerBaseURLKey{}, base)
}

func workerBaseURLFrom(ctx context.Context) string {
	s, _ := ctx.Value(workerBaseURLKey{}).(string)
	return s
}

// chatMCPURL is the worker-reachable per-chat MCP endpoint: the configured public
// URL, else the scheme and host the worker itself connected to.
func (h *Hub) chatMCPURL(wc *workerConn, chatID string) string {
	base := strings.TrimRight(strings.TrimSpace(h.PublicURL), "/")
	if base != "" {
		if u, err := url.Parse(base); err != nil || u.Host == "" {
			base = ""
		}
	}
	if base == "" && wc != nil {
		base = strings.TrimRight(wc.baseURL, "/")
	}
	if base == "" {
		return ""
	}
	return base + "/mcp/chat/" + url.PathEscape(chatID)
}

// buildChatOpen builds the idempotent agent.chat.open for a session.
func (h *Hub) buildChatOpen(ctx context.Context, cs store.ChatSession, wc *workerConn) (protocol.AgentChatOpen, error) {
	open := protocol.AgentChatOpen{
		ChatID:         cs.ID,
		Agent:          cs.Agent,
		AccountID:      cs.AccountID,
		Cwd:            cs.Cwd,
		Model:          cs.Model,
		Effort:         cs.Effort,
		ApprovalMode:   cs.ApprovalMode,
		AgentSessionID: cs.AgentSessionID,
		SystemPrompt:   orchestrator.AgentSystemPrompt(cs.FleetTools),
	}
	if u := h.chatMCPURL(wc, cs.ID); u != "" {
		tok, err := h.ensureChatToken(ctx, cs.ID)
		if err != nil {
			return open, err
		}
		open.MCPServers = []protocol.AgentMCPServer{{
			Name:    "konsert",
			URL:     u,
			Headers: map[string]string{"Authorization": "Bearer " + tok},
		}}
	}
	return open, nil
}

// --- browser messages -------------------------------------------------------------

func (h *Hub) workerConnFor(workerID string) *workerConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.workers[workerID]
}

func (h *Hub) workerLabel(ctx context.Context, workerID string) string {
	if w, err := h.store.GetWorker(ctx, workerID); err == nil && w.Name != "" {
		return w.Name
	}
	if workerID == "" {
		return "(none)"
	}
	return workerID
}

// sendAgentChat runs chat.send for an agent-backend session.
func (h *Hub) sendAgentChat(ctx context.Context, cs store.ChatSession, req chatSendRequest) error {
	if cs.WorkerID == "" || cs.Agent == "" {
		h.resolveChatTarget(ctx, &cs)
		if cs.WorkerID == "" || cs.Agent == "" {
			e := protocol.AgentChatError{
				Kind:    protocol.ErrKindNotFound,
				Message: "No connected worker has a logged-in agent that supports chat. Start konsertd on a machine where an agent CLI (claude, codex, grok, ...) is installed and signed in.",
			}
			h.hubError(ctx, cs.ID, e)
			return errors.New(e.Message)
		}
		if err := h.store.UpdateChatSession(ctx, cs); err != nil {
			return err
		}
	}
	wc := h.workerConnFor(cs.WorkerID)
	if wc == nil {
		e := protocol.AgentChatError{
			Kind:      protocol.ErrKindOther,
			Message:   fmt.Sprintf("Worker %s is not connected. Start konsertd there or move this chat to another worker.", h.workerLabel(ctx, cs.WorkerID)),
			Retryable: true,
		}
		h.hubError(ctx, cs.ID, e)
		return errors.New(e.Message)
	}
	var exhausted *protocol.AgentChatEvent
	if a, ok := h.lookupAccount(ctx, cs.WorkerID, cs.Agent, cs.AccountID); ok {
		if a.Auth.State == protocol.AuthLoggedOut {
			label := a.Label
			if label == "" {
				label = a.ID
			}
			e := protocol.AgentChatError{
				Kind:    protocol.ErrKindAuth,
				Message: fmt.Sprintf("%s needs you to sign in again on %s.", label, h.workerLabel(ctx, cs.WorkerID)),
				Relogin: a.Auth.Relogin,
			}
			h.hubError(ctx, cs.ID, e)
			return errors.New(e.Message)
		}
		if !a.ChatSupported {
			e := protocol.AgentChatError{Kind: protocol.ErrKindOther, Message: fmt.Sprintf("%s does not support chat (no structured headless protocol).", a.Label)}
			h.hubError(ctx, cs.ID, e)
			return errors.New(e.Message)
		}
		// An exhausted usage window is only a warning: the window may have reset
		// early, so the prompt is still forwarded.
		if u, ok := exhaustedWindow(a, time.Now()); ok {
			u := u
			exhausted = &protocol.AgentChatEvent{Kind: protocol.EventRateLimit, RateLimit: &u, Text: exhaustedWarning(a, u)}
		}
	}
	open, err := h.buildChatOpen(ctx, cs, wc)
	if err != nil {
		return err
	}
	turnID := uuid.NewString()
	rt := h.chatRT(cs.ID)
	rt.mu.Lock()
	rt.turnID = turnID
	rt.mu.Unlock()
	if !h.sendWorkerJSON(wc, protocol.MsgAgentChatOpen, open) {
		e := protocol.AgentChatError{Kind: protocol.ErrKindOther, Message: "The worker's send queue is full; try again.", Retryable: true}
		h.hubError(ctx, cs.ID, e)
		return errors.New(e.Message)
	}
	h.emitChatEvent(ctx, cs.ID, protocol.AgentChatEvent{Kind: eventUserMessage, TurnID: turnID, Text: req.Content}, emitOpts{
		update: func(s *store.ChatSession) {
			s.Status = protocol.ChatStatusWorking
			s.LastErrorJSON = ""
		},
	})
	if exhausted != nil {
		exhausted.TurnID = turnID
		h.emitChatEvent(ctx, cs.ID, *exhausted, emitOpts{})
	}
	if !h.sendWorkerJSON(wc, protocol.MsgAgentChatPrompt, protocol.AgentChatPrompt{
		ChatID: cs.ID, TurnID: turnID, Text: req.Content, Attachments: req.Attachments,
	}) {
		e := protocol.AgentChatError{Kind: protocol.ErrKindOther, Message: "The worker's send queue is full; try again.", Retryable: true}
		h.emitChatEvent(ctx, cs.ID, protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: turnID, Error: &e}, emitOpts{})
		return errors.New(e.Message)
	}
	return nil
}

// handleChatSend routes chat.send by the session's backend. Agent sessions go to
// their worker; API-backend (and unknown, auto-created) sessions run the API-key
// orchestrator.
func (h *Hub) handleChatSend(ctx context.Context, c *browserClient, env protocol.Envelope) {
	p, err := protocol.DecodePayload[chatSendRequest](env)
	if err != nil || p.SessionID == "" || strings.TrimSpace(p.Content) == "" {
		return
	}
	h.subscribeChat(c, p.SessionID)
	if cs, err := h.store.GetChatSession(ctx, p.SessionID); err == nil && cs.Backend == store.ChatBackendAgent {
		_ = h.sendAgentChat(withAuditActor(ctx, c.user, auditSourceWS), cs, p)
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
	h.startChatTurn(c, orch, protocol.ChatSendPayload{SessionID: p.SessionID, Content: p.Content})
}

// respondChat answers an elicitation: a hub ask_user question is resolved here;
// anything else is forwarded to the worker.
func (h *Hub) respondChat(ctx context.Context, req chatRespondRequest) error {
	if req.SessionID == "" || req.ElicitationID == "" {
		return errors.New("session_id and elicitation_id are required")
	}
	cs, err := h.store.GetChatSession(ctx, req.SessionID)
	if err != nil {
		return err
	}
	h.recordAudit(ctx, store.AuditEvent{
		Action:   AuditChatRespond,
		WorkerID: cs.WorkerID,
		Message:  fmt.Sprintf("Answered an agent request in chat %s", cs.ID),
		MetaJSON: auditMeta(map[string]any{"chat_id": cs.ID, "option_id": req.OptionID}),
	})
	rt := h.chatRT(cs.ID)
	rt.mu.Lock()
	ask := rt.asks[req.ElicitationID]
	if ask != nil {
		delete(rt.asks, req.ElicitationID)
	}
	rt.mu.Unlock()
	if ask != nil {
		ask.answer <- askAnswer{optionID: req.OptionID, text: req.Text}
		return nil
	}
	wc := h.workerConnFor(cs.WorkerID)
	if wc == nil {
		return fmt.Errorf("worker %s is not connected", h.workerLabel(ctx, cs.WorkerID))
	}
	if !h.sendWorkerJSON(wc, protocol.MsgAgentChatRespond, protocol.AgentChatRespond{
		ChatID: cs.ID, ElicitationID: req.ElicitationID, OptionID: req.OptionID, Text: req.Text,
	}) {
		return errors.New("worker send queue full")
	}
	return nil
}

// resolveAsks ends every pending ask_user question of a chat without an answer.
func (h *Hub) resolveAsks(chatID, reason string) {
	h.mu.RLock()
	rt := h.agentChats[chatID]
	h.mu.RUnlock()
	if rt == nil {
		return
	}
	rt.mu.Lock()
	asks := rt.asks
	rt.asks = make(map[string]*pendingAsk)
	rt.mu.Unlock()
	for _, a := range asks {
		a.answer <- askAnswer{reason: reason}
	}
}

// ChatSettingsPatch is a partial agent chat settings change (REST PATCH and chat.control).
type ChatSettingsPatch struct {
	Title        *string `json:"title,omitempty"`
	WorkerID     *string `json:"worker_id,omitempty"`
	Agent        *string `json:"agent,omitempty"`
	AccountID    *string `json:"account_id,omitempty"`
	Model        *string `json:"model,omitempty"`
	Effort       *string `json:"effort,omitempty"`
	ApprovalMode *string `json:"approval_mode,omitempty"`
	Cwd          *string `json:"cwd,omitempty"`
	FleetTools   *bool   `json:"fleet_tools,omitempty"`
}

func (p ChatSettingsPatch) empty() bool {
	return p.Title == nil && p.WorkerID == nil && p.Agent == nil && p.AccountID == nil && p.Model == nil &&
		p.Effort == nil && p.ApprovalMode == nil && p.Cwd == nil && p.FleetTools == nil
}

var errEmptyTitle = errors.New("title must not be empty")

// updateChatSettings applies a settings patch: live settings (model, effort,
// approval mode) are pushed to the engine with agent.chat.control; changing the
// worker, agent, account, cwd or fleet tools closes the engine so the next
// message reopens it (a different account or agent cannot resume the old
// session, so its agent_session_id is cleared). Model, approval mode and
// account changes are audited.
func (h *Hub) updateChatSettings(ctx context.Context, chatID string, p ChatSettingsPatch) (store.ChatSession, error) {
	rt := h.chatRT(chatID)
	rt.mu.Lock()
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil {
		rt.mu.Unlock()
		return cs, err
	}
	old := cs
	trim := func(v *string) string { return strings.TrimSpace(*v) }
	if p.Title != nil {
		if trim(p.Title) == "" {
			rt.mu.Unlock()
			return cs, errEmptyTitle
		}
		cs.Title = trim(p.Title)
	}
	if p.WorkerID != nil {
		cs.WorkerID = trim(p.WorkerID)
	}
	if p.Agent != nil {
		cs.Agent = trim(p.Agent)
	}
	if p.AccountID != nil {
		cs.AccountID = trim(p.AccountID)
	}
	if p.Cwd != nil {
		cs.Cwd = trim(p.Cwd)
	}
	if p.FleetTools != nil {
		cs.FleetTools = *p.FleetTools
	}
	targetChanged := cs.WorkerID != old.WorkerID || cs.Agent != old.Agent || cs.AccountID != old.AccountID
	if targetChanged && cs.Backend == store.ChatBackendAgent {
		// New target: take its defaults unless this patch names them.
		if p.Model == nil {
			cs.Model = ""
		}
		if p.Effort == nil {
			cs.Effort = ""
		}
		if p.ApprovalMode == nil {
			cs.ApprovalMode = ""
		}
		if p.Agent == nil && p.AccountID != nil {
			cs.Agent = "" // derive from the account
		}
	}
	if p.Model != nil {
		cs.Model = trim(p.Model)
	}
	if p.Effort != nil {
		cs.Effort = trim(p.Effort)
	}
	if p.ApprovalMode != nil {
		cs.ApprovalMode = trim(p.ApprovalMode)
	}
	if targetChanged && cs.Backend == store.ChatBackendAgent {
		h.resolveChatTarget(ctx, &cs)
	}
	restart := targetChanged || cs.Cwd != old.Cwd || cs.FleetTools != old.FleetTools
	if restart {
		cs.AgentSessionID = ""
		if cs.Status == protocol.ChatStatusWorking || cs.Status == protocol.ChatStatusNeedsYou {
			cs.Status = protocol.ChatStatusIdle
		}
		rt.mcp = nil
	}
	if cs != old {
		if err := h.store.UpdateChatSession(ctx, cs); err != nil {
			rt.mu.Unlock()
			return old, err
		}
	}
	rt.mu.Unlock()

	if cs.Backend != store.ChatBackendAgent {
		return cs, nil
	}
	h.auditChatChanges(ctx, old, cs)
	if restart {
		h.resolveAsks(cs.ID, "the chat's settings changed")
		if wc := h.workerConnFor(old.WorkerID); wc != nil {
			h.sendWorkerJSON(wc, protocol.MsgAgentChatClose, protocol.AgentChatClose{ChatID: cs.ID})
		}
		return cs, nil
	}
	ctl := protocol.AgentChatControl{ChatID: cs.ID}
	if cs.Model != old.Model {
		ctl.Model = cs.Model
	}
	if cs.Effort != old.Effort {
		ctl.Effort = cs.Effort
	}
	if cs.ApprovalMode != old.ApprovalMode {
		ctl.ApprovalMode = cs.ApprovalMode
	}
	if ctl.Model != "" || ctl.Effort != "" || ctl.ApprovalMode != "" {
		if wc := h.workerConnFor(cs.WorkerID); wc != nil {
			h.sendWorkerJSON(wc, protocol.MsgAgentChatControl, ctl)
		}
	}
	return cs, nil
}

func (h *Hub) auditChatChanges(ctx context.Context, old, cs store.ChatSession) {
	rec := func(action, what, from, to string) {
		h.recordAudit(ctx, store.AuditEvent{
			Action:   action,
			WorkerID: cs.WorkerID,
			Message:  fmt.Sprintf("Changed chat %s %s from %q to %q", cs.ID, what, from, to),
			MetaJSON: auditMeta(map[string]any{"chat_id": cs.ID, "from": from, "to": to}),
		})
	}
	if old.ApprovalMode != cs.ApprovalMode {
		rec(AuditChatApprovalMode, "approval mode", old.ApprovalMode, cs.ApprovalMode)
	}
	if old.Model != cs.Model {
		rec(AuditChatModel, "model", old.Model, cs.Model)
	}
	if old.AccountID != cs.AccountID || old.WorkerID != cs.WorkerID {
		rec(AuditChatAccount, "account", old.WorkerID+"/"+old.AccountID, cs.WorkerID+"/"+cs.AccountID)
	}
}

// controlChat runs chat.control: cancel and/or live settings.
func (h *Hub) controlChat(ctx context.Context, req chatControlRequest) error {
	cs, err := h.store.GetChatSession(ctx, req.SessionID)
	if err != nil {
		return err
	}
	if cs.Backend != store.ChatBackendAgent {
		return errors.New("chat.control applies to agent chats only")
	}
	if req.Cancel {
		h.resolveAsks(cs.ID, "the user cancelled the turn")
		if wc := h.workerConnFor(cs.WorkerID); wc != nil {
			h.sendWorkerJSON(wc, protocol.MsgAgentChatControl, protocol.AgentChatControl{ChatID: cs.ID, Cancel: true})
		}
	}
	var p ChatSettingsPatch
	if req.Model != "" {
		p.Model = &req.Model
	}
	if req.Effort != "" {
		p.Effort = &req.Effort
	}
	if req.ApprovalMode != "" {
		p.ApprovalMode = &req.ApprovalMode
	}
	if p.empty() {
		return nil
	}
	_, err = h.updateChatSettings(ctx, cs.ID, p)
	return err
}

// closeChat ends the chat's engine on its worker (the chat can be resumed later).
func (h *Hub) closeChat(ctx context.Context, chatID string) {
	h.resolveAsks(chatID, "the chat was closed")
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil || cs.Backend != store.ChatBackendAgent {
		return
	}
	if wc := h.workerConnFor(cs.WorkerID); wc != nil {
		h.sendWorkerJSON(wc, protocol.MsgAgentChatClose, protocol.AgentChatClose{ChatID: chatID})
	}
}

// forgetChat revokes a deleted chat's MCP token and drops its runtime state.
func (h *Hub) forgetChat(chatID, workerID string) {
	h.resolveAsks(chatID, "the chat was deleted")
	h.mu.Lock()
	delete(h.agentChats, chatID)
	wc := h.workers[workerID]
	h.mu.Unlock()
	if wc != nil {
		h.sendWorkerJSON(wc, protocol.MsgAgentChatClose, protocol.AgentChatClose{ChatID: chatID})
	}
}

// handleAgentChatBrowserMessage handles chat.respond, chat.control and chat.close.
// Failures are reported to the sender as chat.error.
func (h *Hub) handleAgentChatBrowserMessage(ctx context.Context, c *browserClient, env protocol.Envelope) {
	ctx = withAuditActor(ctx, c.user, auditSourceWS)
	var sid string
	var err error
	switch env.Type {
	case msgChatRespond:
		var p chatRespondRequest
		if p, err = protocol.DecodePayload[chatRespondRequest](env); err == nil {
			sid = p.SessionID
			err = h.respondChat(ctx, p)
		}
	case msgChatControl:
		var p chatControlRequest
		if p, err = protocol.DecodePayload[chatControlRequest](env); err == nil {
			sid = p.SessionID
			err = h.controlChat(ctx, p)
		}
	case msgChatClose:
		var p chatCloseRequest
		if p, err = protocol.DecodePayload[chatCloseRequest](env); err == nil {
			sid = p.SessionID
			h.closeChat(ctx, p.SessionID)
		}
	}
	if err != nil {
		h.sendBrowserJSON(c, protocol.MsgChatError, protocol.ChatErrorPayload{SessionID: sid, Error: err.Error()})
	}
}

// RefreshAgentCapabilities asks one worker (or every connected worker when
// workerID is "") to re-probe its accounts. It returns the workers asked.
func (h *Hub) RefreshAgentCapabilities(workerID, accountID string) ([]string, error) {
	h.mu.RLock()
	targets := map[string]*workerConn{}
	if workerID != "" {
		if wc := h.workers[workerID]; wc != nil {
			targets[workerID] = wc
		}
	} else {
		for id, wc := range h.workers {
			targets[id] = wc
		}
	}
	h.mu.RUnlock()
	if workerID != "" && len(targets) == 0 {
		return nil, fmt.Errorf("worker %s is not connected", workerID)
	}
	ids := make([]string, 0, len(targets))
	for id, wc := range targets {
		if h.sendWorkerJSON(wc, protocol.MsgAgentCapabilitiesRefresh, agentCapabilitiesRefresh{AccountID: accountID}) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// --- ask_user ---------------------------------------------------------------------

type askUserInput struct {
	Question  string   `json:"question" jsonschema:"The question to ask the user"`
	Options   []string `json:"options,omitempty" jsonschema:"Optional choices to offer (labels)"`
	AllowText *bool    `json:"allow_text,omitempty" jsonschema:"Accept a free-form answer (default true when no options are given)"`
}

// askUser raises a question elicitation in the chat and blocks until the user
// answers (chat.respond), the question is ended (cancel, close, delete), ctx
// ends, or AskUserTimeout passes. It returns the answer text for the agent.
func (h *Hub) askUser(ctx context.Context, chatID string, in askUserInput) (string, error) {
	q := strings.TrimSpace(in.Question)
	if q == "" {
		return "", errors.New("question is required")
	}
	allowText := len(in.Options) == 0
	if in.AllowText != nil {
		allowText = *in.AllowText || len(in.Options) == 0
	}
	opts := make([]protocol.AgentElicitationOption, 0, len(in.Options))
	for i, label := range in.Options {
		if label = strings.TrimSpace(label); label != "" {
			opts = append(opts, protocol.AgentElicitationOption{ID: fmt.Sprintf("option_%d", i+1), Label: label, Kind: "choice"})
		}
	}
	id := "ask-" + uuid.NewString()
	ask := &pendingAsk{options: opts, answer: make(chan askAnswer, 1)}
	rt := h.chatRT(chatID)
	rt.mu.Lock()
	rt.asks[id] = ask
	rt.mu.Unlock()
	if _, ok := h.emitChatEvent(ctx, chatID, protocol.AgentChatEvent{
		Kind: protocol.EventElicitation,
		Elicitation: &protocol.AgentElicitation{
			ID: id, Kind: protocol.ElicitQuestion, Title: q, Options: opts, AllowText: allowText,
		},
	}, emitOpts{}); !ok {
		rt.mu.Lock()
		delete(rt.asks, id)
		rt.mu.Unlock()
		return "", errors.New("chat not found")
	}

	timeout := h.AskUserTimeout
	if timeout <= 0 {
		timeout = defaultAskUserTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var ans askAnswer
	select {
	case ans = <-ask.answer:
	case <-timer.C:
		ans = askAnswer{reason: fmt.Sprintf("no answer: the user did not answer within %s", timeout.Round(time.Second))}
	case <-ctx.Done():
		ans = askAnswer{reason: "no answer: the request was cancelled"}
	case <-h.ctx.Done():
		ans = askAnswer{reason: "no answer: the Konsert hub is shutting down"}
	}
	rt.mu.Lock()
	delete(rt.asks, id)
	rt.mu.Unlock()
	h.emitChatEvent(ctx, chatID, protocol.AgentChatEvent{
		Kind:        protocol.EventElicitationOK,
		Elicitation: &protocol.AgentElicitation{ID: id, Kind: protocol.ElicitQuestion, Title: q},
	}, emitOpts{})
	if ans.reason != "" {
		return ans.reason, nil
	}
	answer := strings.TrimSpace(ans.text)
	for _, o := range opts {
		if o.ID == ans.optionID {
			if answer != "" {
				return o.Label + "\n" + answer, nil
			}
			return o.Label, nil
		}
	}
	if answer == "" {
		return "no answer: the user dismissed the question", nil
	}
	return answer, nil
}

// --- usage windows ----------------------------------------------------------------

// exhaustedWindow returns the account's exhausted usage window whose reset time
// is still in the future (or unknown), if any.
func exhaustedWindow(a protocol.AgentAccount, now time.Time) (protocol.AgentRateLimit, bool) {
	for _, u := range a.Usage {
		if u.State == "exhausted" && (u.ResetsAt == nil || u.ResetsAt.After(now)) {
			return u, true
		}
	}
	return protocol.AgentRateLimit{}, false
}

func accountExhausted(a protocol.AgentAccount) bool {
	_, ok := exhaustedWindow(a, time.Now())
	return ok
}

// exhaustedWarning describes an exhausted window for a warning message.
func exhaustedWarning(a protocol.AgentAccount, u protocol.AgentRateLimit) string {
	label := a.Label
	if label == "" {
		label = a.ID
	}
	msg := fmt.Sprintf("%s has used up its %s usage window", label, u.Window)
	if u.ResetsAt != nil {
		msg += " until " + u.ResetsAt.UTC().Format(time.RFC3339)
	}
	return msg + "; the request may be rejected"
}
