package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/chat"
	"github.com/coder/websocket"
)

// Agent chat wiring: hub messages → chat.Manager; events and capabilities → hub as
// control messages (never dropped like live PTY data).

// DefaultCapabilitiesInterval is how often agent capabilities are re-sent.
const DefaultCapabilitiesInterval = 10 * time.Minute

// DisableAgentDiscoveryEnv=1 limits agent accounts to configured and mock ones and skips
// CLI status commands (for tests that run a real daemon).
const DisableAgentDiscoveryEnv = "KONSERT_DISABLE_AGENT_DISCOVERY"

// maxPendingChatEvents bounds chat events buffered while disconnected from the hub.
const maxPendingChatEvents = 10000

// newAccountRegistry and chatStatusChecker are replaced by tests so they never run the
// developer's real agent CLIs or read their home directory.
var (
	newAccountRegistry = chat.NewAccountRegistry
	chatStatusChecker  chat.StatusChecker
)

// BuiltinChatEngines returns the chat engines konsertd registers at startup: claude
// (stream-json), codex (app-server) and, with --enable-mock-driver, the mock engine.
// The ACP engine (grok, agy, copilot, cursor) is added by registerACPEngines.
var BuiltinChatEngines = func(cfg config.WorkerConfig, logger *slog.Logger) map[string]chat.Engine {
	engines := map[string]chat.Engine{
		"claude": chat.NewClaudeEngine(),
		"codex":  chat.NewCodexEngine(),
	}
	registerACPEngines(engines)
	if cfg.EnableMockDriver {
		engines[chat.MockAgent] = chat.NewMockEngine()
	}
	return engines
}

func (d *Daemon) initChat() {
	d.capRefresh = make(chan struct{}, 1)
	d.capPush = make(chan struct{}, 1)
	opts := chat.AccountOptions{
		Configured: d.cfg.Accounts,
		EnableMock: d.cfg.EnableMockDriver,
		Logger:     d.logger,
	}
	status := chatStatusChecker
	if v := strings.ToLower(strings.TrimSpace(os.Getenv(DisableAgentDiscoveryEnv))); v == "1" || v == "true" {
		// Only configured and mock accounts; no PATH discovery, wrapper import or CLI
		// status commands (other packages' tests run daemons with this set).
		opts.LookPath = func(string) (string, bool) { return "", false }
		opts.DisableWrappers = true
		status = func(_ context.Context, a chat.Account) protocol.AgentAccount {
			return protocol.AgentAccount{ID: a.ID, Agent: a.Agent, Auth: protocol.AgentAuthStatus{State: protocol.AuthUnknown, Detail: "agent discovery disabled"}}
		}
	}
	d.accounts = newAccountRegistry(opts)
	d.chat = chat.NewManager(chat.ManagerOptions{
		Engines:     BuiltinChatEngines(d.cfg, d.logger),
		Accounts:    d.accounts,
		Sink:        d.sendChatEvent,
		Logger:      d.logger,
		OnFailure:   d.onChatFailure,
		OnRateLimit: d.onChatRateLimit,
	})
	d.caps = chat.NewCapabilities(chat.CapabilitiesOptions{
		Accounts: d.accounts,
		Engine:   d.chat.Engine,
		Status:   status,
		Logger:   d.logger,
		// Exhausted usage windows survive a konsertd restart (no secrets stored).
		PersistPath: usageCachePath(),
	})
	d.runner.SetAccounts(d.accounts)
}

// ChatManager returns the agent chat manager (to register engines, and for tests).
func (d *Daemon) ChatManager() *chat.Manager { return d.chat }

// Accounts returns the agent account registry.
func (d *Daemon) Accounts() *chat.AccountRegistry { return d.accounts }

// RegisterChatEngine registers e for its agent and refreshes capabilities.
func (d *Daemon) RegisterChatEngine(e chat.Engine) {
	d.chat.Register(e)
	d.caps.Invalidate("")
	d.requestCapabilities()
}

func (d *Daemon) requestCapabilities() {
	select {
	case d.capRefresh <- struct{}{}:
	default:
	}
}

func (d *Daemon) pushCapabilities() {
	select {
	case d.capPush <- struct{}{}:
	default:
	}
}

// onChatFailure re-probes an account after an auth failure (rate-limited inside
// Capabilities.Invalidate, so a failing account is never probed in a loop). Usage-limit
// failures arrive through onChatRateLimit and only patch the cached report.
func (d *Daemon) onChatFailure(_, accountID string, err protocol.AgentChatError) {
	if err.Kind == protocol.ErrKindAuth && d.caps.Invalidate(accountID) {
		d.requestCapabilities()
	}
}

// onChatRateLimit folds window state seen in a chat into the cached capabilities and pushes
// them right away when they changed (e.g. a window just became exhausted).
func (d *Daemon) onChatRateLimit(accountID string, rl protocol.AgentRateLimit) {
	if d.caps.NoteRateLimit(accountID, rl) {
		d.pushCapabilities()
	}
}

// sendChatEvent queues an event as a control message. While disconnected, events are
// buffered (bounded) and flushed after the next worker.hello, so a reconnect does not cut
// a streaming reply.
func (d *Daemon) sendChatEvent(ev protocol.AgentChatEvent) {
	raw, err := protocol.NewEnvelope(protocol.MsgAgentChatEvent, "", ev)
	if err != nil {
		d.logger.Warn("dropping agent chat event: encode failed", "chat_id", ev.ChatID, "err", err)
		return
	}
	msg := outboundMsg{msgType: websocket.MessageText, data: raw}
	d.sendMu.Lock()
	defer d.sendMu.Unlock()
	if d.queues != nil {
		d.queues.push(msg)
		return
	}
	if len(d.chatPending) >= maxPendingChatEvents {
		d.chatPending[0] = outboundMsg{}
		d.chatPending = d.chatPending[1:]
		if !d.chatDropWarned {
			d.chatDropWarned = true
			d.logger.Warn("agent chat events dropped while disconnected from the hub", "kept", maxPendingChatEvents)
		}
	}
	d.chatPending = append(d.chatPending, msg)
}

// attachQueues installs the connection's queues and moves buffered chat events into them
// (under sendMu, so event order is preserved).
func (d *Daemon) attachQueues(q *connQueues) {
	d.sendMu.Lock()
	d.queues = q
	for _, m := range d.chatPending {
		q.push(m)
	}
	d.chatPending = nil
	d.chatDropWarned = false
	d.sendMu.Unlock()
}

func (d *Daemon) sendCapabilities(rep protocol.AgentCapabilitiesReport) {
	if rep.Accounts == nil {
		rep.Accounts = []protocol.AgentAccount{}
	}
	d.sendJSON(protocol.MsgAgentCapabilities, rep)
	// Legacy worker.quotas, now sourced from the capabilities usage (real sources only).
	if qs := quotasFromCapabilities(rep, d.cfg.WorkerID); len(qs) > 0 {
		d.sendJSON(protocol.MsgWorkerQuotas, protocol.WorkerQuotasPayload{Quotas: qs})
	}
}

// capabilitiesLoop sends agent capabilities after hello, every CapabilitiesInterval, on
// refresh requests, and (from cache) whenever chat usage changes. It runs per connection.
func (d *Daemon) capabilitiesLoop(ctx context.Context) {
	interval := d.CapabilitiesInterval
	if interval <= 0 {
		interval = DefaultCapabilitiesInterval
	}
	probe := func() {
		rep := d.caps.Report(ctx, false)
		if ctx.Err() == nil {
			d.sendCapabilities(rep)
		}
	}
	probe()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			probe()
		case <-d.capRefresh:
			probe()
		case <-d.capPush:
			d.sendCapabilities(d.caps.Cached())
		}
	}
}

// capabilitiesRefreshRequest is the optional payload of agent.capabilities.refresh.
type capabilitiesRefreshRequest struct {
	AccountID string `json:"account_id,omitempty"`
}

// handleChatMessage routes agent chat messages. It never blocks the read loop: the
// manager runs each chat's operations on that chat's own goroutine.
func (d *Daemon) handleChatMessage(env protocol.Envelope) bool {
	switch env.Type {
	case protocol.MsgAgentChatOpen:
		if p, err := protocol.DecodePayload[protocol.AgentChatOpen](env); err == nil {
			d.chat.Open(p)
		} else {
			d.logger.Warn("decode agent.chat.open failed", "err", err)
		}
	case protocol.MsgAgentChatPrompt:
		if p, err := protocol.DecodePayload[protocol.AgentChatPrompt](env); err == nil {
			d.chat.Prompt(p)
		} else {
			d.logger.Warn("decode agent.chat.prompt failed", "err", err)
		}
	case protocol.MsgAgentChatRespond:
		if p, err := protocol.DecodePayload[protocol.AgentChatRespond](env); err == nil {
			d.chat.Respond(p)
		}
	case protocol.MsgAgentChatControl:
		if p, err := protocol.DecodePayload[protocol.AgentChatControl](env); err == nil {
			d.chat.Control(p)
		}
	case protocol.MsgAgentChatClose:
		if p, err := protocol.DecodePayload[protocol.AgentChatClose](env); err == nil {
			d.chat.CloseChat(p.ChatID)
		}
	case protocol.MsgAgentCapabilitiesRefresh:
		var req capabilitiesRefreshRequest
		if len(env.Payload) > 0 {
			_ = json.Unmarshal(env.Payload, &req)
		}
		// An explicit refresh always re-probes (it bypasses the failure rate limit).
		d.caps.Forget(req.AccountID)
		d.requestCapabilities()
	default:
		return false
	}
	return true
}

// shutdownChat closes every agent chat engine.
func (d *Daemon) shutdownChat() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	d.chat.Shutdown(ctx)
}

// usageCachePath is <state dir>/usage-cache.json ("" when there is no state dir).
func usageCachePath() string {
	dir := defaultStateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "usage-cache.json")
}
