package chat

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/activity"
)

// Sink receives every stamped event (ChatID, Seq and At set). It must not block: syred
// queues events as control messages.
type Sink func(protocol.AgentChatEvent)

// AccountResolver turns (agent, account id) into a runnable Account. *AccountRegistry
// implements it.
type AccountResolver interface {
	Resolve(agent, accountID string) (Account, error)
}

// Defaults for ManagerOptions.
const (
	DefaultIdleTimeout   = 15 * time.Minute
	DefaultTurnWatchdog  = 10 * time.Minute
	DefaultCancelGrace   = 15 * time.Second
	DefaultOpenTimeout   = 2 * time.Minute
	DefaultCloseTimeout  = 10 * time.Second
	defaultSweepInterval = 30 * time.Second
)

// ManagerOptions configures a Manager.
type ManagerOptions struct {
	// Engines maps agent name to engine. More can be added with Register.
	Engines  map[string]Engine
	Accounts AccountResolver
	Sink     Sink
	Logger   *slog.Logger

	// IdleTimeout closes an engine with no turn running after this long (resumed on the
	// next prompt). Default 15 min.
	IdleTimeout time.Duration
	// TurnWatchdog emits ErrKindTimeout when a running turn produced no event for this
	// long (elicitations waiting for the user do not count). Default 10 min.
	TurnWatchdog time.Duration
	// CancelGrace is how long a cancelled turn may take to end before the manager ends it
	// itself with StopReason "cancelled". Default 15 s.
	CancelGrace time.Duration
	// SweepInterval is how often idle sessions and watchdogs are checked. Default 30 s.
	SweepInterval time.Duration
	// OnFailure is called (on its own goroutine) for every EventError, e.g. to refresh
	// capabilities after auth or usage_limit failures.
	OnFailure func(chatID, accountID string, err protocol.AgentChatError)
	// OnRateLimit is called (on its own goroutine) with the account's subscription window
	// state seen in a chat: every EventRateLimit, and an exhausted window derived from an
	// ErrKindUsageLimit error. syred feeds it into the cached capabilities.
	OnRateLimit func(accountID string, rl protocol.AgentRateLimit)
	// CheckApproval, when set, vets the requested approval mode before an engine starts
	// (syred --sandbox refuses full-access modes). A non-nil error fails the turn.
	CheckApproval func(agent, accountID, mode string) error
	// SeqBase returns the first Seq of a chat in this process. The default is the current
	// time in milliseconds × 1000, so Seq keeps increasing across syred restarts.
	SeqBase func() int64
	// Now is the clock (tests).
	Now func() time.Time
	// OnActivity, when set, receives the chat's derived activity (thinking, working,
	// speaking, ...) whenever it changes. It is called with the chat's lock held and must
	// not block (syred hands it to a rate-limiting activity.Publisher).
	OnActivity func(protocol.SessionActivity)
	// MachineName is included in post-turn RunDigests for ticket-backed chats.
	MachineName string
	// VerifyTimeout overrides the default 20m verification command timeout when
	// AgentChatOpen.VerifyTimeoutSeconds is not set.
	VerifyTimeout time.Duration
	// ProvisionBindings, when set, provisions per-repo ticket worktrees before opening
	// a ticket-backed chat engine.
	ProvisionBindings func(ctx context.Context, ticketSID, requestedRoot string, bindings []protocol.TaskRepoBinding) (ProvisionedWorktrees, error)
}

// Manager owns every chat session on this worker: ChatID → engine session, per-chat Seq,
// one turn at a time, a turn watchdog, idle close with resume, and shutdown.
//
// All operations of one chat run in order on that chat's own goroutine, so callers (the
// daemon's read loop) never block on an engine.
type Manager struct {
	opts ManagerOptions
	log  *slog.Logger

	mu      sync.Mutex
	engines map[string]Engine
	chats   map[string]*chatState
	closed  bool

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// chatState is one chat. Operations are serialized by the op queue; mu guards the fields
// below and event emission (so Seq order equals sink order).
type chatState struct {
	id string

	// lastStatus is the last EventStatus emitted, to drop repeats (engines and the
	// manager both report starting/working); reset by turn_end and error.
	lastStatus string

	qmu     sync.Mutex
	queue   []func()
	running bool

	mu             sync.Mutex
	opts           protocol.AgentChatOpen
	acct           Account
	sess           Session
	gen            int // incremented whenever sess is replaced or dropped
	agentSessionID string
	seq            int64
	turnID         string
	turnActive     bool
	cancelAt       time.Time // when a cancel was requested for the running turn
	lastEvent      time.Time // last event (or user response) of the running turn
	timedOut       bool      // watchdog fired for the current quiet period
	lastActivity   time.Time
	pending        map[string]bool // open elicitations
	errorSeen      bool            // an EventError was emitted by the current open attempt
	// suspended: the user froze this chat's engine process tree. No watchdog, idle close
	// or new turns until Resume.
	suspended bool
	// act derives the chat's live activity from the events it emits.
	act *activity.Chat

	// Ticket-backed chat state (worktree provisioning + per-turn digest signals).
	effectiveDir     string
	worktrees        []protocol.TaskWorktree
	worktreeBaseSHAs map[string]string
	turnAttempt      int
	turnStartedAt    time.Time
	turnMessageBuf   strings.Builder
	turnLastMessage  string
	turnToolOutput   strings.Builder
	verifying        bool
	verifyCancel     context.CancelFunc
	// stderrTail is the engine stderr captured after the session is closed, so
	// inspect can still read it once the process is gone.
	stderrTail string
}

// Suspender is implemented by sessions that can freeze and thaw their engine (every
// real engine via its process tree; the mock in process).
type Suspender interface {
	Suspend() error
	Resume() error
}

// NewManager creates a Manager and starts its sweeper.
func NewManager(opts ManagerOptions) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.TurnWatchdog <= 0 {
		opts.TurnWatchdog = DefaultTurnWatchdog
	}
	if opts.CancelGrace <= 0 {
		opts.CancelGrace = DefaultCancelGrace
	}
	if opts.VerifyTimeout <= 0 {
		opts.VerifyTimeout = DefaultVerifyTimeout
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = defaultSweepInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.SeqBase == nil {
		opts.SeqBase = func() int64 { return time.Now().UnixMilli() * 1000 }
	}
	if opts.Sink == nil {
		opts.Sink = func(protocol.AgentChatEvent) {}
	}
	m := &Manager{
		opts:    opts,
		log:     opts.Logger,
		engines: map[string]Engine{},
		chats:   map[string]*chatState{},
		stop:    make(chan struct{}),
	}
	for agent, e := range opts.Engines {
		if e != nil {
			m.engines[agent] = e
		}
	}
	m.wg.Add(1)
	go m.sweepLoop()
	return m
}

// Register adds (or replaces) the engine for its Agent().
func (m *Manager) Register(e Engine) {
	m.RegisterFor(e.Agent(), e)
}

// RegisterFor adds (or replaces) the engine serving agent (one engine value may serve
// several agents, e.g. the ACP engine).
func (m *Manager) RegisterFor(agent string, e Engine) {
	m.mu.Lock()
	m.engines[agent] = e
	m.mu.Unlock()
}

// Engine returns the engine registered for agent.
func (m *Manager) Engine(agent string) (Engine, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.engines[agent]
	return e, ok
}

// Agents lists agents with a registered engine.
func (m *Manager) Agents() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.engines))
	for a := range m.engines {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// chat returns the chat state, creating it when create is set.
func (m *Manager) chat(id string, create bool) *chatState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	c := m.chats[id]
	if c == nil && create {
		c = &chatState{id: id, seq: m.opts.SeqBase(), pending: map[string]bool{}, lastActivity: m.opts.Now(), act: activity.NewChat(id)}
		m.chats[id] = c
	}
	return c
}

// enqueue runs op on the chat's goroutine after earlier ops.
func (m *Manager) enqueue(c *chatState, op func()) {
	c.qmu.Lock()
	c.queue = append(c.queue, op)
	if c.running {
		c.qmu.Unlock()
		return
	}
	c.running = true
	c.qmu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			c.qmu.Lock()
			if len(c.queue) == 0 {
				c.running = false
				c.qmu.Unlock()
				return
			}
			next := c.queue[0]
			c.queue[0] = nil
			c.queue = c.queue[1:]
			c.qmu.Unlock()
			next()
		}
	}()
}

// --- public operations (non-blocking) -------------------------------------------------

// Open opens or re-attaches to the chat's engine session. Idempotent per ChatID.
func (m *Manager) Open(o protocol.AgentChatOpen) {
	if o.ChatID == "" {
		return
	}
	c := m.chat(o.ChatID, true)
	if c == nil {
		return
	}
	m.enqueue(c, func() { m.doOpen(c, o) })
}

// Prompt starts a turn (reopening the engine with resume if it was closed).
func (m *Manager) Prompt(p protocol.AgentChatPrompt) {
	if p.ChatID == "" {
		return
	}
	c := m.chat(p.ChatID, true)
	if c == nil {
		return
	}
	m.enqueue(c, func() { m.doPrompt(c, p) })
}

// Respond answers an elicitation.
func (m *Manager) Respond(r protocol.AgentChatRespond) {
	c := m.chat(r.ChatID, false)
	if c == nil {
		m.emitDetached(r.ChatID, "", errEvent(protocol.ErrKindProtocol, "chat is not open on this worker", false))
		return
	}
	m.enqueue(c, func() { m.doRespond(c, r) })
}

// Control changes model/effort/approval mode or cancels the running turn.
func (m *Manager) Control(ctl protocol.AgentChatControl) {
	c := m.chat(ctl.ChatID, false)
	if c == nil {
		return
	}
	m.enqueue(c, func() { m.doControl(c, ctl) })
}

// Suspend freezes the chat's engine process tree (SIGSTOP of its process group on Unix,
// thread suspension on Windows) and reports ChatStatusSuspended. It runs immediately,
// not behind the chat's queued operations, so a long open or prompt can't delay it.
func (m *Manager) Suspend(chatID string) {
	c := m.chat(chatID, true)
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.suspended {
		c.mu.Unlock()
		return
	}
	sess := c.sess
	c.mu.Unlock()
	if sp, ok := sess.(Suspender); ok && sess != nil {
		if err := sp.Suspend(); err != nil {
			m.emitDetachedTurn(c, "", errEvent(protocol.ErrKindOther, "could not suspend the agent: "+err.Error(), true))
			return
		}
	}
	c.mu.Lock()
	c.suspended = true
	m.emitLocked(c, protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusSuspended})
	c.mu.Unlock()
}

// Resume thaws a suspended chat. A turn that was running continues (status working, or
// needs_you while an elicitation waits); the watchdog restarts from now.
func (m *Manager) Resume(chatID string) {
	c := m.chat(chatID, false)
	if c == nil {
		return
	}
	c.mu.Lock()
	if !c.suspended {
		c.mu.Unlock()
		return
	}
	sess := c.sess
	c.mu.Unlock()
	if sp, ok := sess.(Suspender); ok && sess != nil {
		if err := sp.Resume(); err != nil {
			m.emitDetachedTurn(c, "", errEvent(protocol.ErrKindOther, "could not resume the agent: "+err.Error(), true))
			return
		}
	}
	c.mu.Lock()
	c.suspended = false
	now := m.opts.Now()
	c.lastEvent, c.lastActivity, c.timedOut = now, now, false
	switch {
	case c.turnActive && len(c.pending) > 0:
		m.emitLocked(c, protocol.AgentChatEvent{Kind: protocol.EventStatus, TurnID: c.turnID, Status: protocol.ChatStatusNeedsYou})
	case c.turnActive:
		m.emitLocked(c, protocol.AgentChatEvent{Kind: protocol.EventStatus, TurnID: c.turnID, Status: protocol.ChatStatusWorking})
	default:
		c.lastStatus = "" // the hub restores the chat's previous status
	}
	c.mu.Unlock()
}

// thawForClose resumes a suspended session before it is closed: a frozen process can't
// act on the termination request.
func (m *Manager) thawForClose(c *chatState, s Session) {
	c.mu.Lock()
	was := c.suspended
	c.suspended = false
	c.mu.Unlock()
	if sp, ok := s.(Suspender); ok && was && s != nil {
		_ = sp.Resume()
	}
}

// CloseChat ends the chat's engine process; the chat can be resumed by a later Open or
// Prompt. Seq continues.
func (m *Manager) CloseChat(chatID string) {
	c := m.chat(chatID, false)
	if c == nil {
		return
	}
	m.enqueue(c, func() { m.closeSession(c, "closed by hub") })
}

// Discarder is implemented by engines that can delete what they stored for a session
// (a side-conversation fork that is no longer needed).
type Discarder interface {
	Discard(acct Account, opts protocol.AgentChatOpen, agentSessionID string) error
}

// DiscardChat ends the chat's engine, deletes what the engine stored for its session
// (engines implementing Discarder; only forks), and forgets the chat.
func (m *Manager) DiscardChat(chatID string) {
	c := m.chat(chatID, false)
	if c == nil {
		return
	}
	m.enqueue(c, func() {
		m.closeSession(c, "discarded by hub")
		c.mu.Lock()
		o, sid, acct := c.opts, c.agentSessionID, c.acct
		c.mu.Unlock()
		if eng, ok := m.Engine(o.Agent); ok {
			if d, ok := eng.(Discarder); ok {
				if cwd, err := ResolveCwd(o.Cwd); err == nil {
					o.Cwd = cwd
				}
				if acct.ID == "" && m.opts.Accounts != nil {
					acct, _ = m.opts.Accounts.Resolve(o.Agent, o.AccountID)
				}
				if err := d.Discard(acct, o, sid); err != nil {
					m.log.Warn("discarding a chat session failed", "chat_id", chatID, "err", err)
				}
			}
		}
		m.mu.Lock()
		if m.chats[chatID] == c {
			delete(m.chats, chatID)
		}
		m.mu.Unlock()
	})
}

// Shutdown closes every session and stops the manager. It waits for engines to exit (each
// bounded by its own Close) or until ctx ends.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	chats := make([]*chatState, 0, len(m.chats))
	for _, c := range m.chats {
		chats = append(chats, c)
	}
	m.mu.Unlock()
	m.stopOnce.Do(func() { close(m.stop) })

	var wg sync.WaitGroup
	for _, c := range chats {
		c.mu.Lock()
		s := c.sess
		vcancel := c.verifyCancel
		c.sess = nil
		c.gen++
		c.turnActive = false
		c.mu.Unlock()
		if vcancel != nil {
			vcancel()
		}
		if s == nil {
			continue
		}
		m.thawForClose(c, s)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Close()
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		m.log.Warn("agent chat shutdown timed out; some engines may still be exiting")
	}
}

// ChatInfo is a snapshot of one chat for diagnostics and tests.
type ChatInfo struct {
	ChatID         string
	Agent          string
	AccountID      string
	Open           bool
	TurnActive     bool
	TurnID         string
	AgentSessionID string
	LastSeq        int64
	Cancelling     bool // a cancel was requested for the running turn
}

// ProcessIDer is implemented by sessions backed by an agent process (every real engine),
// so syred can count the shells under it.
type ProcessIDer interface {
	ProcessID() int
}

// ProcessIDs returns the agent process id of every chat whose engine runs (chat id →
// pid).
func (m *Manager) ProcessIDs() map[string]int {
	m.mu.Lock()
	chats := make([]*chatState, 0, len(m.chats))
	for _, c := range m.chats {
		chats = append(chats, c)
	}
	m.mu.Unlock()
	out := map[string]int{}
	for _, c := range chats {
		c.mu.Lock()
		sess := c.sess
		c.mu.Unlock()
		if p, ok := sess.(ProcessIDer); ok && sess != nil {
			if pid := p.ProcessID(); pid > 0 {
				out[c.id] = pid
			}
		}
	}
	return out
}

// Chats returns snapshots of all known chats.
func (m *Manager) Chats() []ChatInfo {
	m.mu.Lock()
	chats := make([]*chatState, 0, len(m.chats))
	for _, c := range m.chats {
		chats = append(chats, c)
	}
	m.mu.Unlock()
	out := make([]ChatInfo, 0, len(chats))
	for _, c := range chats {
		c.mu.Lock()
		out = append(out, ChatInfo{
			ChatID: c.id, Agent: c.opts.Agent, AccountID: c.acct.ID, Open: c.sess != nil,
			TurnActive: c.turnActive, TurnID: c.turnID, AgentSessionID: c.agentSessionID, LastSeq: c.seq,
			Cancelling: c.turnActive && !c.cancelAt.IsZero(),
		})
		c.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChatID < out[j].ChatID })
	return out
}

// --- operations (run on the chat goroutine) -------------------------------------------

func (m *Manager) doOpen(c *chatState, o protocol.AgentChatOpen) {
	c.mu.Lock()
	if c.effectiveDir != "" && o.TicketSID != "" && o.TicketSID == c.opts.TicketSID && (o.Cwd == "" || o.Cwd == c.effectiveDir) {
		o.Cwd = c.effectiveDir
	}
	alive := c.sess != nil
	sameTarget := c.opts.Agent == o.Agent && c.opts.AccountID == o.AccountID && c.opts.Cwd == o.Cwd && c.opts.UseGitBash == o.UseGitBash
	c.mu.Unlock()
	if alive && sameTarget {
		// Re-attach: apply changed settings live and report the session as ready.
		ctl := protocol.AgentChatControl{ChatID: c.id}
		c.mu.Lock()
		if o.Model != "" && o.Model != c.opts.Model {
			ctl.Model = o.Model
		}
		if o.Effort != "" && o.Effort != c.opts.Effort {
			ctl.Effort = o.Effort
		}
		if o.ApprovalMode != "" && o.ApprovalMode != c.opts.ApprovalMode {
			ctl.ApprovalMode = o.ApprovalMode
		}
		o.AgentSessionID = c.agentSessionID
		c.opts = o
		c.lastActivity = m.opts.Now()
		c.mu.Unlock()
		if ctl.Model != "" || ctl.Effort != "" || ctl.ApprovalMode != "" {
			m.doControl(c, ctl)
		}
		c.mu.Lock()
		m.emitLocked(c, protocol.AgentChatEvent{
			Kind: protocol.EventSessionReady, AgentSessionID: c.agentSessionID,
			Model: c.opts.Model, Effort: c.opts.Effort, ApprovalMode: c.opts.ApprovalMode,
			Workdir: c.effectiveDir, Worktrees: c.worktrees,
		})
		c.mu.Unlock()
		return
	}
	if alive {
		// Different agent/account/cwd: replace the engine.
		m.closeSession(c, "reopened with different settings")
	}
	c.mu.Lock()
	prev := c.agentSessionID
	sameAgent := c.opts.Agent == o.Agent && c.opts.AccountID == o.AccountID && c.opts.Cwd == o.Cwd
	if o.AgentSessionID == "" && sameAgent {
		o.AgentSessionID = prev
	}
	if !sameAgent && o.AgentSessionID == "" {
		// A conversation belongs to one agent account (claude keeps them per config
		// dir): never resume another account's session after switching.
		c.agentSessionID = ""
	}
	c.opts = o
	if o.AgentSessionID != "" {
		c.agentSessionID = o.AgentSessionID
	}
	c.mu.Unlock()
	// The engine is started lazily by the next Prompt: the hub sends open right before
	// prompt, and starting here too made one failure surface twice (and launch twice).
}

// ensureOpen starts the engine session if none is running. It reports success; on
// failure an EventError has been emitted (with turnID, if any).
func (m *Manager) ensureOpen(c *chatState, turnID string) bool {
	if m.isClosed() {
		return false
	}
	c.mu.Lock()
	if c.sess != nil {
		c.mu.Unlock()
		return true
	}
	o := c.opts
	if c.agentSessionID != "" {
		o.AgentSessionID = c.agentSessionID
	}
	c.mu.Unlock()
	if o.Agent == "" {
		m.emitError(c, turnID, errEvent(protocol.ErrKindProtocol, "chat is not open on this worker (agent.chat.open was never received)", false))
		return false
	}
	if len(o.Bindings) > 0 && m.opts.ProvisionBindings != nil {
		c.mu.Lock()
		existingDir := c.effectiveDir
		hasWTs := len(c.worktrees) > 0
		c.mu.Unlock()
		if existingDir != "" && hasWTs {
			o.Cwd = existingDir
		} else {
			pctx, pcancel := context.WithTimeout(context.Background(), DefaultOpenTimeout)
			prov, pErr := m.opts.ProvisionBindings(pctx, cmp.Or(strings.TrimSpace(o.TicketSID), c.id), o.Cwd, o.Bindings)
			pcancel()
			if pErr != nil {
				m.emitError(c, turnID, errEvent(protocol.ErrKindOther, pErr.Error(), false))
				return false
			}
			c.mu.Lock()
			c.effectiveDir = prov.Workdir
			c.worktrees = prov.Worktrees
			c.worktreeBaseSHAs = prov.BaseSHAs
			c.opts.Cwd = prov.Workdir
			c.mu.Unlock()
			o.Cwd = prov.Workdir
		}
	}
	// Resolve the working directory here, for every engine: the hub sends "~" by
	// default, and a missing directory must not be reported as "agent not installed".
	cwd, err := ResolveCwd(o.Cwd)
	if err != nil {
		m.emitError(c, turnID, errEvent(protocol.ErrKindOther, err.Error(), false))
		return false
	}
	o.Cwd = cwd
	if m.opts.CheckApproval != nil && o.ApprovalMode != "" {
		if err := m.opts.CheckApproval(o.Agent, o.AccountID, o.ApprovalMode); err != nil {
			m.emitError(c, turnID, errEvent(protocol.ErrKindOther, err.Error(), false))
			return false
		}
	}
	eng, ok := m.Engine(o.Agent)
	if !ok {
		m.emitError(c, turnID, errEvent(protocol.ErrKindNotFound, fmt.Sprintf("no chat engine for agent %q on this worker", o.Agent), false))
		return false
	}
	if m.opts.Accounts == nil {
		m.emitError(c, turnID, errEvent(protocol.ErrKindOther, "no agent accounts configured", false))
		return false
	}
	acct, err := m.opts.Accounts.Resolve(o.Agent, o.AccountID)
	if err != nil {
		kind := protocol.ErrKindAuth
		if errors.Is(err, ErrAccountNotFound) {
			kind = protocol.ErrKindNotFound
		}
		m.emitError(c, turnID, errEvent(kind, err.Error(), false))
		return false
	}

	c.mu.Lock()
	c.gen++
	gen := c.gen
	c.acct = acct
	c.errorSeen = false
	m.emitLocked(c, protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusStarting})
	c.mu.Unlock()

	c.mu.Lock()
	if len(c.worktrees) > 0 {
		o.EngineWorktrees = append([]protocol.TaskWorktree(nil), c.worktrees...)
	}
	c.mu.Unlock()

	emit := func(ev protocol.AgentChatEvent) { m.onEngineEvent(c, gen, ev) }
	ctx, cancel := context.WithTimeout(context.Background(), DefaultOpenTimeout)
	defer cancel()
	sess, err := eng.Open(ctx, withManagedMarker(acct, c.id), o, emit)
	if err != nil || sess == nil {
		if err == nil {
			err = errors.New("engine returned no session")
		}
		c.mu.Lock()
		// errorSeen is reset at the start of this attempt; an engine exit error bumps
		// c.gen, so don't also require gen to match (that produced duplicate errors).
		seen := c.errorSeen
		c.gen++ // drop any late events of the failed attempt
		c.mu.Unlock()
		if !seen {
			m.emitError(c, turnID, errEvent(protocol.ErrKindOther, fmt.Sprintf("could not start %s: %v", o.Agent, err), false))
		}
		return false
	}
	// Session methods are never called with c.mu held: an engine may emit (which takes
	// c.mu) while holding its own lock.
	sid := sess.AgentSessionID()
	c.mu.Lock()
	if c.gen != gen || m.isClosed() {
		// The session died (exit error), was closed while opening, or syred is stopping.
		c.mu.Unlock()
		_ = sess.Close()
		return false
	}
	c.sess = sess
	if sid != "" {
		c.agentSessionID = sid
	}
	c.lastActivity = m.opts.Now()
	c.mu.Unlock()
	if d, ok := sess.(interface{ Done() <-chan struct{} }); ok {
		m.wg.Add(1)
		go m.watchDone(c, gen, d.Done())
	}
	return true
}

// watchDone turns an engine process exit that the engine did not report into an exit error.
func (m *Manager) watchDone(c *chatState, gen int, done <-chan struct{}) {
	defer m.wg.Done()
	select {
	case <-m.stop:
		return
	case <-done:
	}
	c.mu.Lock()
	if c.gen != gen || c.sess == nil {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	m.onEngineEvent(c, gen, protocol.AgentChatEvent{
		Kind:  protocol.EventError,
		Error: &protocol.AgentChatError{Kind: protocol.ErrKindExit, Message: c.opts.Agent + " exited unexpectedly"},
	})
}

func (m *Manager) doPrompt(c *chatState, p protocol.AgentChatPrompt) {
	c.mu.Lock()
	if c.suspended {
		c.mu.Unlock()
		m.emitDetachedTurn(c, p.TurnID, errEvent(protocol.ErrKindOther, "this chat is suspended; resume it to continue", true))
		return
	}
	if c.turnActive {
		busy := c.turnID
		c.mu.Unlock()
		ev := errEvent(protocol.ErrKindOther, "a turn is already running in this chat ("+busy+"); wait for it to finish or cancel it", true)
		m.emitDetachedTurn(c, p.TurnID, ev)
		return
	}
	c.lastActivity = m.opts.Now()
	c.mu.Unlock()

	if !m.ensureOpen(c, p.TurnID) {
		return
	}

	c.mu.Lock()
	sess := c.sess
	if sess == nil {
		c.mu.Unlock()
		m.emitError(c, p.TurnID, errEvent(protocol.ErrKindExit, "the agent session ended before the turn could start", true))
		return
	}
	now := m.opts.Now()
	c.turnActive = true
	c.turnID = p.TurnID
	c.turnAttempt++
	c.turnStartedAt = now
	c.turnMessageBuf.Reset()
	c.turnLastMessage = ""
	c.turnToolOutput.Reset()
	c.lastEvent = now
	c.timedOut = false
	c.cancelAt = time.Time{}
	c.pending = map[string]bool{}
	m.emitLocked(c, protocol.AgentChatEvent{Kind: protocol.EventStatus, TurnID: p.TurnID, Status: protocol.ChatStatusWorking})
	c.mu.Unlock()

	if err := sess.Prompt(context.Background(), p); err != nil {
		c.mu.Lock()
		stillOurs := c.turnActive && c.turnID == p.TurnID
		c.mu.Unlock()
		if stillOurs {
			m.emitError(c, p.TurnID, errEvent(protocol.ErrKindOther, "the agent refused the message: "+err.Error(), true))
		}
	}
}

func (m *Manager) doRespond(c *chatState, r protocol.AgentChatRespond) {
	c.mu.Lock()
	sess := c.sess
	if sess != nil {
		c.lastEvent = m.opts.Now()
		c.timedOut = false
		c.lastActivity = c.lastEvent
	}
	c.mu.Unlock()
	if sess == nil {
		m.emitDetachedTurn(c, "", errEvent(protocol.ErrKindProtocol, "the agent session is not running; the question can no longer be answered", false))
		return
	}
	if err := sess.Respond(context.Background(), r); err != nil {
		c.mu.Lock()
		stillPending := c.pending[r.ElicitationID]
		turn := ""
		if c.turnActive {
			turn = c.turnID
		}
		if stillPending {
			// The answer was refused (e.g. an option the request didn't offer) but the agent
			// is still waiting: stay in needs_you rather than reporting the chat as failed.
			m.emitLocked(c, protocol.AgentChatEvent{Kind: protocol.EventStatus, TurnID: turn,
				Status: protocol.ChatStatusNeedsYou, Text: "That answer was not accepted: " + err.Error()})
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		m.emitDetachedTurn(c, "", errEvent(protocol.ErrKindProtocol, "could not deliver the answer: "+err.Error(), false))
		return
	}
	c.mu.Lock()
	delete(c.pending, r.ElicitationID)
	c.mu.Unlock()
}

func (m *Manager) doControl(c *chatState, ctl protocol.AgentChatControl) {
	c.mu.Lock()
	if ctl.Model != "" {
		c.opts.Model = ctl.Model
	}
	if ctl.Effort != "" {
		c.opts.Effort = ctl.Effort
	}
	if ctl.ApprovalMode != "" {
		c.opts.ApprovalMode = ctl.ApprovalMode
	}
	sess := c.sess
	var vcancel context.CancelFunc
	if ctl.Cancel && c.turnActive && c.cancelAt.IsZero() {
		c.cancelAt = m.opts.Now()
	}
	if ctl.Cancel && c.verifyCancel != nil {
		vcancel = c.verifyCancel
	}
	c.lastActivity = m.opts.Now()
	c.mu.Unlock()
	if vcancel != nil {
		vcancel()
	}
	if sess == nil {
		// Applied on the next open.
		return
	}
	if err := sess.Control(context.Background(), ctl); err != nil {
		m.emitDetachedTurn(c, "", errEvent(protocol.ErrKindOther, "could not apply the change: "+err.Error(), false))
	}
}

// closeSession closes the engine (if any) and marks the chat closed; the next prompt
// resumes the agent session.
func (m *Manager) closeSession(c *chatState, why string) {
	c.mu.Lock()
	s := c.sess
	vcancel := c.verifyCancel
	c.sess = nil
	c.gen++
	wasTurn := c.turnActive
	turn := c.turnID
	c.turnActive = false
	c.pending = map[string]bool{}
	if wasTurn {
		m.emitLocked(c, protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: turn, StopReason: "cancelled"})
	}
	if s != nil {
		m.emitLocked(c, protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusIdle, Text: why})
	}
	c.mu.Unlock()
	if vcancel != nil {
		vcancel()
	}
	if s != nil {
		m.thawForClose(c, s)
		m.rememberSessionID(c, s)
		closeWithTimeout(s, m.log, c.id)
		if tail := sessionStderr(s); tail != "" {
			c.mu.Lock()
			c.stderrTail = tail
			c.mu.Unlock()
		}
	}
}

// StderrTail returns the chat engine's retained stderr. A live session is
// preferred; otherwise the tail snapshotted when the engine was closed. ok is
// false when this manager has no tail for chatID.
func (m *Manager) StderrTail(chatID string) (string, bool) {
	if m == nil {
		return "", false
	}
	c := m.chat(strings.TrimSpace(chatID), false)
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	sess := c.sess
	saved := c.stderrTail
	c.mu.Unlock()
	if sess != nil {
		if tail := sessionStderr(sess); tail != "" {
			return tail, true
		}
		return "", false
	}
	if saved != "" {
		return saved, true
	}
	return "", false
}

// sessionStderr reads an engine's stderr tail when the session exposes one.
func sessionStderr(s Session) string {
	type tailer interface{ StderrTail(int) string }
	t, ok := s.(tailer)
	if !ok || s == nil {
		return ""
	}
	return strings.TrimSpace(t.StderrTail(0))
}

// rememberSessionID records the agent session id of a session being dropped, for resume.
// It runs without c.mu held (see ensureOpen).
func (m *Manager) rememberSessionID(c *chatState, s Session) {
	if id := s.AgentSessionID(); id != "" {
		c.mu.Lock()
		c.agentSessionID = id
		c.mu.Unlock()
	}
}

func closeWithTimeout(s Session, log *slog.Logger, chatID string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Close()
	}()
	select {
	case <-done:
	case <-time.After(DefaultCloseTimeout):
		log.Warn("agent engine did not close in time", "chat_id", chatID)
	}
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// --- events ---------------------------------------------------------------------------

// onEngineEvent stamps and forwards an engine event, tracking turn and session state.
func (m *Manager) onEngineEvent(c *chatState, gen int, ev protocol.AgentChatEvent) {
	var (
		gitByRepo map[string]*protocol.GitSummary
		verifyRes *VerifyResult
	)
	if ev.Kind == protocol.EventTurnEnd || ev.Kind == protocol.EventError {
		c.mu.Lock()
		valid := c.gen == gen
		ticketSID := strings.TrimSpace(c.opts.TicketSID)
		wts := append([]protocol.TaskWorktree(nil), c.worktrees...)
		baseSHAs := c.worktreeBaseSHAs
		verifies := append([]protocol.AgentChatVerify(nil), c.opts.Verifies...)
		verifyDir := c.effectiveDir
		fallbackCwd := c.opts.Cwd
		timeoutSec := c.opts.VerifyTimeoutSeconds
		c.mu.Unlock()
		if !valid {
			return
		}
		if ticketSID != "" && len(wts) > 0 {
			gitByRepo = captureWorktreeGit(wts, baseSHAs)
		}
		steps := verifySteps(verifies, wts, verifyDir)
		if ev.Kind == protocol.EventTurnEnd && ev.StopReason != "cancelled" && ev.StopReason != "error" && ticketSID != "" && len(steps) > 0 {
			if verifyDir == "" {
				verifyDir, _ = ResolveCwd(fallbackCwd)
				steps = verifySteps(verifies, wts, verifyDir)
			}
			timeout := m.opts.VerifyTimeout
			if timeoutSec > 0 {
				timeout = time.Duration(timeoutSec) * time.Second
			}
			vctx, vcancel := context.WithCancel(context.Background())
			c.mu.Lock()
			if c.gen != gen {
				c.mu.Unlock()
				vcancel()
				return
			}
			c.verifying = true
			c.verifyCancel = vcancel
			c.mu.Unlock()

			res := RunVerifySteps(vctx, steps, timeout)
			vcancel()

			c.mu.Lock()
			c.verifying = false
			c.verifyCancel = nil
			stillValid := c.gen == gen
			c.mu.Unlock()
			if !stillValid {
				return
			}
			if res.Canceled {
				ev.StopReason = "cancelled"
			} else {
				verifyRes = &res
			}
		}
	}

	c.mu.Lock()
	if c.gen != gen {
		c.mu.Unlock()
		return // a replaced or dead session
	}
	var dead Session
	var failure *protocol.AgentChatError
	switch ev.Kind {
	case protocol.EventSessionReady:
		if ev.AgentSessionID != "" {
			c.agentSessionID = ev.AgentSessionID
		}
		if ev.Model != "" {
			c.opts.Model = ev.Model
		}
		if ev.Workdir == "" && c.effectiveDir != "" {
			ev.Workdir = c.effectiveDir
		}
		if len(ev.Worktrees) == 0 && len(c.worktrees) > 0 {
			ev.Worktrees = c.worktrees
		}
	case protocol.EventMessageDelta:
		if ev.ParentToolCallID == "" && ev.Text != "" {
			c.turnMessageBuf.WriteString(ev.Text)
		}
	case protocol.EventMessage:
		if ev.ParentToolCallID == "" && ev.Text != "" {
			c.turnLastMessage = ev.Text
		}
	case protocol.EventToolCall:
		if ev.ToolCall != nil && ev.ToolCall.Output != "" {
			appendTurnToolOutput(&c.turnToolOutput, ev.ToolCall.Output)
		}
	case protocol.EventElicitation:
		if ev.Elicitation != nil {
			c.pending[ev.Elicitation.ID] = true
		}
		if ev.Digest == nil {
			ev.Digest = m.buildTurnDigestLocked(c, ev, nil, nil)
		}
	case protocol.EventElicitationOK:
		if ev.Elicitation != nil {
			delete(c.pending, ev.Elicitation.ID)
		}
	case protocol.EventTurnEnd:
		if ev.TurnID == "" {
			ev.TurnID = c.turnID
		}
		c.turnActive = false
		c.pending = map[string]bool{}
		if ev.Digest == nil {
			ev.Digest = m.buildTurnDigestLocked(c, ev, gitByRepo, verifyRes)
		}
	case protocol.EventError:
		if ev.TurnID == "" && c.turnActive {
			ev.TurnID = c.turnID
		}
		c.turnActive = false
		c.pending = map[string]bool{}
		c.errorSeen = true
		if ev.Error != nil {
			e := *ev.Error
			failure = &e
			if e.Kind == protocol.ErrKindExit {
				// The engine died: drop it; the next prompt resumes the agent session.
				dead = c.sess
				c.sess = nil
				c.gen++
				// The agent session id is already known from session_ready / Open; the
				// dead session's methods are not called under c.mu.
			}
		}
		if ev.Digest == nil {
			ev.Digest = m.buildTurnDigestLocked(c, ev, gitByRepo, nil)
		}
	}
	if ev.TurnID == "" && c.turnActive && ev.Kind != protocol.EventSessionReady {
		ev.TurnID = c.turnID
	}
	if c.turnActive {
		c.lastEvent = m.opts.Now()
		c.timedOut = false
	}
	c.lastActivity = m.opts.Now()
	m.emitLocked(c, ev)
	acctID := c.acct.ID
	c.mu.Unlock()

	if dead != nil {
		go closeWithTimeout(dead, m.log, c.id)
	}
	if failure != nil && m.opts.OnFailure != nil {
		go m.opts.OnFailure(c.id, acctID, *failure)
	}
	if m.opts.OnRateLimit != nil && acctID != "" {
		if rl, ok := rateLimitOf(ev); ok {
			go m.opts.OnRateLimit(acctID, rl)
		}
	}
}

// rateLimitOf extracts subscription window state from an event.
func rateLimitOf(ev protocol.AgentChatEvent) (protocol.AgentRateLimit, bool) {
	switch {
	case ev.Kind == protocol.EventRateLimit && ev.RateLimit != nil && ev.RateLimit.Window != "":
		return *ev.RateLimit, true
	case ev.Kind == protocol.EventError && ev.Error != nil && ev.Error.Kind == protocol.ErrKindUsageLimit:
		w := ev.Error.Window
		if w == "" {
			w = "unknown"
		}
		return protocol.AgentRateLimit{Window: w, UsedPercent: 100, ResetsAt: ev.Error.ResetsAt, State: "exhausted"}, true
	}
	return protocol.AgentRateLimit{}, false
}

// emitLocked stamps ev and hands it to the sink. c.mu must be held.
func (m *Manager) emitLocked(c *chatState, ev protocol.AgentChatEvent) {
	switch ev.Kind {
	case protocol.EventStatus:
		if ev.Text == "" && ev.Status == c.lastStatus {
			return
		}
		c.lastStatus = ev.Status
	case protocol.EventTurnEnd, protocol.EventError:
		c.lastStatus = ""
	}
	if subagentChatter(ev) {
		// A sub-agent's streamed text and thoughts only feed its live state: the
		// transcript shows the parent's Task call and the sub-agent's tool calls.
		if m.opts.OnActivity != nil && c.act != nil {
			if now := m.opts.Now(); c.act.Observe(ev, now) {
				m.opts.OnActivity(c.act.Activity(now))
			}
		}
		return
	}
	if ev.Kind == protocol.EventElicitation && ev.ParentToolCallID != "" && ev.Elicitation != nil && c.act != nil {
		// A sub-agent asks: the prompt (and the Needs-you item made from it) names it.
		el := *ev.Elicitation
		el.Title = activity.SubagentQuestion(c.act.SubagentTitle(ev.ParentToolCallID), firstNonEmpty(el.Title, "needs a decision"))
		ev.Elicitation = &el
	}
	c.seq++
	ev.Seq = c.seq
	ev.ChatID = c.id
	ev.At = m.opts.Now().UTC()
	m.opts.Sink(ev)
	if m.opts.OnActivity != nil && c.act != nil {
		if now := m.opts.Now(); c.act.Observe(ev, now) {
			m.opts.OnActivity(c.act.Activity(now))
		}
	}
}

// subagentChatter reports a sub-agent's text or thought event (not for the transcript).
func subagentChatter(ev protocol.AgentChatEvent) bool {
	if ev.Kind == protocol.EventSubagent {
		return true
	}
	if ev.ParentToolCallID == "" {
		return false
	}
	switch ev.Kind {
	case protocol.EventMessageDelta, protocol.EventMessage, protocol.EventThoughtDelta:
		return true
	}
	return false
}

// emitError emits a manager-generated error that ends turnID (if it is the running turn).
func (m *Manager) emitError(c *chatState, turnID string, ev protocol.AgentChatEvent) {
	c.mu.Lock()
	ticketSID := c.opts.TicketSID
	wts := c.worktrees
	baseSHAs := c.worktreeBaseSHAs
	c.mu.Unlock()
	var gitByRepo map[string]*protocol.GitSummary
	if ticketSID != "" && len(wts) > 0 {
		gitByRepo = captureWorktreeGit(wts, baseSHAs)
	}

	c.mu.Lock()
	ev.TurnID = turnID
	if c.turnActive && c.turnID == turnID {
		c.turnActive = false
		c.pending = map[string]bool{}
	}
	c.errorSeen = true
	if ev.Digest == nil {
		ev.Digest = m.buildTurnDigestLocked(c, ev, gitByRepo, nil)
	}
	m.emitLocked(c, ev)
	acctID := c.acct.ID
	c.mu.Unlock()
	if ev.Error != nil && m.opts.OnFailure != nil {
		go m.opts.OnFailure(c.id, acctID, *ev.Error)
	}
}

// emitDetachedTurn emits an error about a request without touching the running turn.
func (m *Manager) emitDetachedTurn(c *chatState, turnID string, ev protocol.AgentChatEvent) {
	c.mu.Lock()
	ev.TurnID = turnID
	m.emitLocked(c, ev)
	c.mu.Unlock()
}

// emitDetached emits an error for a chat this worker does not know.
func (m *Manager) emitDetached(chatID, turnID string, ev protocol.AgentChatEvent) {
	if chatID == "" {
		return
	}
	c := m.chat(chatID, true)
	if c == nil {
		return
	}
	m.emitDetachedTurn(c, turnID, ev)
}

func errEvent(kind, msg string, retryable bool) protocol.AgentChatEvent {
	return protocol.AgentChatEvent{
		Kind:  protocol.EventError,
		Error: &protocol.AgentChatError{Kind: kind, Message: msg, Retryable: retryable},
	}
}

// --- sweeper: watchdog, cancel grace, idle close --------------------------------------

func (m *Manager) sweepLoop() {
	defer m.wg.Done()
	t := time.NewTicker(m.opts.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.Sweep()
		}
	}
}

// Sweep runs one watchdog / idle pass (the sweeper calls it periodically; tests call it
// directly with a fake clock).
func (m *Manager) Sweep() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	chats := make([]*chatState, 0, len(m.chats))
	for _, c := range m.chats {
		chats = append(chats, c)
	}
	m.mu.Unlock()
	now := m.opts.Now()
	for _, c := range chats {
		c.mu.Lock()
		if c.suspended {
			// Frozen by the user: no watchdog, no cancel grace, no idle close.
			c.mu.Unlock()
			continue
		}
		switch {
		case c.turnActive && !c.cancelAt.IsZero() && now.Sub(c.cancelAt) >= m.opts.CancelGrace:
			// The engine did not end a cancelled turn: end it here.
			turn := c.turnID
			c.turnActive = false
			c.pending = map[string]bool{}
			m.emitLocked(c, protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: turn, StopReason: "cancelled"})
		case c.turnActive && !c.verifying && len(c.pending) == 0 && !c.timedOut && now.Sub(c.lastEvent) >= m.opts.TurnWatchdog:
			// The turn stays running (the user may cancel or keep waiting).
			c.timedOut = true
			mins := int(m.opts.TurnWatchdog.Round(time.Minute) / time.Minute)
			msg := fmt.Sprintf("no progress from %s for %d minutes; cancel the turn or keep waiting", c.opts.Agent, mins)
			if mins < 1 {
				msg = fmt.Sprintf("no progress from %s for %s; cancel the turn or keep waiting", c.opts.Agent, m.opts.TurnWatchdog)
			}
			m.emitLocked(c, protocol.AgentChatEvent{
				Kind: protocol.EventError, TurnID: c.turnID,
				Error: &protocol.AgentChatError{Kind: protocol.ErrKindTimeout, Message: msg, Retryable: true},
			})
		}
		idle := c.sess != nil && !c.turnActive && now.Sub(c.lastActivity) >= m.opts.IdleTimeout
		c.mu.Unlock()
		if idle {
			m.enqueue(c, func() {
				c.mu.Lock()
				still := c.sess != nil && !c.turnActive && m.opts.Now().Sub(c.lastActivity) >= m.opts.IdleTimeout
				c.mu.Unlock()
				if still {
					m.log.Debug("closing idle agent chat engine", "chat_id", c.id)
					m.closeSession(c, "engine stopped after idle timeout; it resumes on the next message")
				}
			})
		}
	}
}

// ResolveCwd expands a leading "~" (and an empty value) to the home directory and checks
// that the result is an existing directory.
func ResolveCwd(cwd string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || cwd == "~" || strings.HasPrefix(cwd, "~/") || strings.HasPrefix(cwd, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", fmt.Errorf("cannot resolve the home directory for working directory %q", cwd)
		}
		if len(cwd) <= 1 {
			cwd = home
		} else {
			cwd = filepath.Join(home, cwd[2:])
		}
	}
	st, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("working directory %s does not exist on this machine", cwd)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("working directory %s is not a directory", cwd)
	}
	return cwd, nil
}

// ManagedTaskEnv marks processes started by syred. Hook scripts installed into the
// agents' global config (e.g. claude settings) see it and don't report syred's own
// chat sessions as separate external sessions. Same name as worker.ManagedTaskEnv.
const ManagedTaskEnv = "SYRE_TASK_ID"

// withManagedMarker returns acct with ManagedTaskEnv set for chatID (a copy; the
// registry's account is not modified).
func withManagedMarker(acct Account, chatID string) Account {
	env := make(map[string]string, len(acct.Env)+1)
	for k, v := range acct.Env {
		env[k] = v
	}
	env[ManagedTaskEnv] = "chat:" + chatID
	acct.Env = env
	return acct
}
