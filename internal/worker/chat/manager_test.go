package chat

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// --- helpers --------------------------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type collector struct {
	mu     sync.Mutex
	events []protocol.AgentChatEvent
	signal chan struct{}
}

func newCollector() *collector { return &collector{signal: make(chan struct{}, 1)} }

func (c *collector) sink(ev protocol.AgentChatEvent) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

func (c *collector) all() []protocol.AgentChatEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.AgentChatEvent(nil), c.events...)
}

// waitFor returns the first event at index >= from matching pred, and its index.
func (c *collector) waitFor(t *testing.T, from int, what string, pred func(protocol.AgentChatEvent) bool) (protocol.AgentChatEvent, int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		evs := c.all()
		for i := from; i < len(evs); i++ {
			if pred(evs[i]) {
				return evs[i], i
			}
		}
		select {
		case <-c.signal:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s; events: %+v", what, evs)
		}
	}
}

func kind(k string) func(protocol.AgentChatEvent) bool {
	return func(e protocol.AgentChatEvent) bool { return e.Kind == k }
}

func errKind(k string) func(protocol.AgentChatEvent) bool {
	return func(e protocol.AgentChatEvent) bool {
		return e.Kind == protocol.EventError && e.Error != nil && e.Error.Kind == k
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func mockAccounts(t *testing.T) *AccountRegistry {
	t.Helper()
	return NewAccountRegistry(AccountOptions{
		EnableMock: true, DisableWrappers: true, HomeDir: t.TempDir(),
		LookPath: func(string) (string, bool) { return "", false },
	})
}

type testManager struct {
	*Manager
	col   *collector
	clock *fakeClock
	mock  *MockEngine
}

func newTestManager(t *testing.T, mutate func(*ManagerOptions)) *testManager {
	t.Helper()
	col := newCollector()
	clock := newFakeClock()
	mock := &MockEngine{StepDelay: time.Millisecond, SlowDuration: time.Minute}
	opts := ManagerOptions{
		Engines:       map[string]Engine{MockAgent: mock},
		Accounts:      mockAccounts(t),
		Sink:          col.sink,
		SweepInterval: time.Hour, // tests call Sweep themselves
		SeqBase:       func() int64 { return 0 },
		Now:           clock.Now,
	}
	if mutate != nil {
		mutate(&opts)
	}
	m := NewManager(opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		m.Shutdown(ctx)
	})
	return &testManager{Manager: m, col: col, clock: clock, mock: mock}
}

func (tm *testManager) chatInfo(id string) ChatInfo {
	for _, c := range tm.Chats() {
		if c.ChatID == id {
			return c
		}
	}
	return ChatInfo{}
}

// --- tests ----------------------------------------------------------------------------

func TestManagerNormalTurnStampsSeq(t *testing.T) {
	tm := newTestManager(t, nil)
	cwd := t.TempDir() // the cwd must exist ("/tmp" doesn't on Windows)
	tm.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: MockAgent, Cwd: cwd})
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t1", Text: "hello"})
	_, end := tm.col.waitFor(t, 0, "turn_end", kind(protocol.EventTurnEnd))

	evs := tm.col.all()[:end+1]
	var sawReady, sawTool, sawDelta bool
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: %+v", i, ev.Seq, evs)
		}
		if ev.ChatID != "c1" || ev.At.IsZero() {
			t.Fatalf("event not stamped: %+v", ev)
		}
		switch ev.Kind {
		case protocol.EventSessionReady:
			sawReady = ev.AgentSessionID != ""
		case protocol.EventToolCall:
			sawTool = ev.TurnID == "t1"
		case protocol.EventMessageDelta:
			sawDelta = ev.TurnID == "t1"
		}
	}
	if !sawReady || !sawTool || !sawDelta {
		t.Fatalf("missing events (ready=%v tool=%v delta=%v): %+v", sawReady, sawTool, sawDelta, evs)
	}
	if evs[end].StopReason != "end_turn" || evs[end].TurnID != "t1" {
		t.Fatalf("bad turn_end: %+v", evs[end])
	}

	// Re-opening an open chat re-attaches (no new engine) and reports session_ready.
	sid := tm.chatInfo("c1").AgentSessionID
	tm.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: MockAgent, Cwd: cwd})
	ev, _ := tm.col.waitFor(t, end+1, "re-attach session_ready", kind(protocol.EventSessionReady))
	if ev.AgentSessionID != sid {
		t.Fatalf("re-attach changed session: %q vs %q", ev.AgentSessionID, sid)
	}
}

func TestManagerDefaultSeqBaseIsMonotonicAcrossRestarts(t *testing.T) {
	m := NewManager(ManagerOptions{})
	defer m.Shutdown(context.Background())
	base := m.opts.SeqBase()
	if base < time.Now().Add(-time.Minute).UnixMilli()*1000 {
		t.Fatalf("seq base %d is not time based", base)
	}
}

func TestManagerOneTurnAtATime(t *testing.T) {
	tm := newTestManager(t, nil)
	tm.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: MockAgent})
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t1", Text: "be slow please"})
	_, i := tm.col.waitFor(t, 0, "slow delta", kind(protocol.EventMessageDelta))

	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t2", Text: "hello"})
	busy, j := tm.col.waitFor(t, i, "busy error", func(e protocol.AgentChatEvent) bool { return e.Kind == protocol.EventError && e.TurnID == "t2" })
	if busy.Error.Kind != protocol.ErrKindOther || !busy.Error.Retryable {
		t.Fatalf("busy error = %+v", busy.Error)
	}
	if !tm.chatInfo("c1").TurnActive {
		t.Fatal("the busy error must not end the running turn")
	}

	tm.Control(protocol.AgentChatControl{ChatID: "c1", Cancel: true})
	end, k := tm.col.waitFor(t, j, "cancelled turn_end", kind(protocol.EventTurnEnd))
	if end.StopReason != "cancelled" || end.TurnID != "t1" {
		t.Fatalf("turn_end = %+v", end)
	}
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t3", Text: "hello"})
	end, _ = tm.col.waitFor(t, k+1, "t3 turn_end", kind(protocol.EventTurnEnd))
	if end.TurnID != "t3" || end.StopReason != "end_turn" {
		t.Fatalf("t3 turn_end = %+v", end)
	}
}

func TestManagerIdleCloseAndResume(t *testing.T) {
	tm := newTestManager(t, nil)
	tm.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: MockAgent})
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t1", Text: "hello"})
	_, i := tm.col.waitFor(t, 0, "turn_end", kind(protocol.EventTurnEnd))
	sid := tm.chatInfo("c1").AgentSessionID

	tm.clock.Advance(14 * time.Minute)
	tm.Sweep()
	if !tm.chatInfo("c1").Open {
		t.Fatal("closed before the idle timeout")
	}
	tm.clock.Advance(2 * time.Minute)
	tm.Sweep()
	waitUntil(t, "idle close", func() bool { return !tm.chatInfo("c1").Open })
	idle, j := tm.col.waitFor(t, i, "idle status", kind(protocol.EventStatus))
	if idle.Status != protocol.ChatStatusIdle {
		t.Fatalf("idle status = %+v", idle)
	}

	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t2", Text: "again"})
	ready, _ := tm.col.waitFor(t, j, "resumed session_ready", kind(protocol.EventSessionReady))
	if ready.AgentSessionID != sid {
		t.Fatalf("resume used session %q, want %q", ready.AgentSessionID, sid)
	}
	end, _ := tm.col.waitFor(t, j, "t2 turn_end", kind(protocol.EventTurnEnd))
	if end.TurnID != "t2" {
		t.Fatalf("turn_end = %+v", end)
	}
}

func TestManagerCrashEmitsExitAndResumes(t *testing.T) {
	tm := newTestManager(t, nil)
	tm.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: MockAgent})
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t1", Text: "please crash"})
	ev, i := tm.col.waitFor(t, 0, "exit error", errKind(protocol.ErrKindExit))
	if ev.Error.ExitCode == nil || *ev.Error.ExitCode != 3 || ev.Error.Stderr == "" || ev.TurnID != "t1" {
		t.Fatalf("exit error = %+v", ev.Error)
	}
	waitUntil(t, "session marked closed", func() bool { c := tm.chatInfo("c1"); return !c.Open && !c.TurnActive })
	sid := tm.chatInfo("c1").AgentSessionID

	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t2", Text: "hello"})
	ready, _ := tm.col.waitFor(t, i, "resumed session_ready", kind(protocol.EventSessionReady))
	if ready.AgentSessionID != sid {
		t.Fatalf("resumed %q, want %q", ready.AgentSessionID, sid)
	}
	end, _ := tm.col.waitFor(t, i, "t2 turn_end", kind(protocol.EventTurnEnd))
	if end.TurnID != "t2" || end.StopReason != "end_turn" {
		t.Fatalf("turn_end = %+v", end)
	}
	// Exactly one exit error (the mock's Done channel must not produce a duplicate).
	n := 0
	for _, e := range tm.col.all() {
		if e.Kind == protocol.EventError && e.Error.Kind == protocol.ErrKindExit {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d exit errors", n)
	}
}

// silentEngine accepts prompts and never emits anything (a hung agent).
type silentEngine struct{ closed chan struct{} }

func (e *silentEngine) Agent() string { return "silent" }
func (e *silentEngine) Probe(context.Context, Account) protocol.AgentAccount {
	return protocol.AgentAccount{}
}
func (e *silentEngine) Open(_ context.Context, _ Account, _ protocol.AgentChatOpen, emit Emit) (Session, error) {
	emit(protocol.AgentChatEvent{Kind: protocol.EventSessionReady, AgentSessionID: "s1"})
	return &silentSession{e: e}, nil
}

type silentSession struct{ e *silentEngine }

func (s *silentSession) Prompt(context.Context, protocol.AgentChatPrompt) error   { return nil }
func (s *silentSession) Respond(context.Context, protocol.AgentChatRespond) error { return nil }
func (s *silentSession) Control(context.Context, protocol.AgentChatControl) error { return nil }
func (s *silentSession) AgentSessionID() string                                   { return "s1" }
func (s *silentSession) Close() error {
	select {
	case <-s.e.closed:
	default:
		close(s.e.closed)
	}
	return nil
}

type staticResolver struct{}

func (staticResolver) Resolve(agent, id string) (Account, error) {
	if agent == "missing" {
		return Account{}, ErrAccountNotFound
	}
	return Account{ID: agent, Agent: agent, IsDefault: true}, nil
}

func TestManagerWatchdogAndCancelGrace(t *testing.T) {
	eng := &silentEngine{closed: make(chan struct{})}
	tm := newTestManager(t, func(o *ManagerOptions) {
		o.Engines = map[string]Engine{"silent": eng}
		o.Accounts = staticResolver{}
		o.TurnWatchdog = 10 * time.Minute
	})
	tm.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: "silent"})
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t1", Text: "hi"})
	_, i := tm.col.waitFor(t, 0, "working", func(e protocol.AgentChatEvent) bool { return e.Status == protocol.ChatStatusWorking })

	tm.clock.Advance(9 * time.Minute)
	tm.Sweep()
	tm.clock.Advance(2 * time.Minute)
	tm.Sweep()
	tm.Sweep() // fires once per quiet period
	ev, j := tm.col.waitFor(t, i, "timeout", errKind(protocol.ErrKindTimeout))
	if ev.TurnID != "t1" {
		t.Fatalf("timeout = %+v", ev)
	}
	n := 0
	for _, e := range tm.col.all() {
		if e.Kind == protocol.EventError {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("watchdog fired %d times", n)
	}
	if !tm.chatInfo("c1").TurnActive {
		t.Fatal("the watchdog must keep the turn running (the user can wait or cancel)")
	}

	// The engine ignores the cancel: the manager ends the turn after the grace period.
	tm.Control(protocol.AgentChatControl{ChatID: "c1", Cancel: true})
	waitUntil(t, "cancel recorded", func() bool { return tm.chatInfo("c1").Cancelling })
	tm.clock.Advance(DefaultCancelGrace + time.Second)
	tm.Sweep()
	end, _ := tm.col.waitFor(t, j, "forced turn_end", kind(protocol.EventTurnEnd))
	if end.StopReason != "cancelled" || end.TurnID != "t1" {
		t.Fatalf("turn_end = %+v", end)
	}
}

func TestManagerElicitationSuspendsWatchdog(t *testing.T) {
	tm := newTestManager(t, nil)
	tm.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: MockAgent})
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t1", Text: "needs permission"})
	el, i := tm.col.waitFor(t, 0, "elicitation", kind(protocol.EventElicitation))
	if el.Elicitation.Kind != protocol.ElicitPermission || len(el.Elicitation.Options) == 0 {
		t.Fatalf("elicitation = %+v", el.Elicitation)
	}
	tm.clock.Advance(time.Hour)
	tm.Sweep()
	tm.Respond(protocol.AgentChatRespond{ChatID: "c1", ElicitationID: el.Elicitation.ID, OptionID: "allow_once"})
	end, _ := tm.col.waitFor(t, i, "turn_end", kind(protocol.EventTurnEnd))
	if end.StopReason != "end_turn" {
		t.Fatalf("turn_end = %+v", end)
	}
	for _, e := range tm.col.all() {
		if e.Kind == protocol.EventError {
			t.Fatalf("unexpected error while waiting for the user: %+v", e.Error)
		}
	}
}

func TestManagerAskQuestion(t *testing.T) {
	tm := newTestManager(t, nil)
	tm.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: MockAgent})
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t1", Text: "ask me"})
	el, i := tm.col.waitFor(t, 0, "question", kind(protocol.EventElicitation))
	if el.Elicitation.Kind != protocol.ElicitQuestion || !el.Elicitation.AllowText {
		t.Fatalf("question = %+v", el.Elicitation)
	}
	tm.Respond(protocol.AgentChatRespond{ChatID: "c1", ElicitationID: el.Elicitation.ID, Text: "purple"})
	msg, _ := tm.col.waitFor(t, i, "answer message", kind(protocol.EventMessage))
	if msg.Text != "You answered: purple" {
		t.Fatalf("message = %q", msg.Text)
	}
}

func TestManagerFailuresAndUsageLimit(t *testing.T) {
	var mu sync.Mutex
	var failures []protocol.AgentChatError
	var limits []protocol.AgentRateLimit
	tm := newTestManager(t, func(o *ManagerOptions) {
		o.OnFailure = func(_, acct string, e protocol.AgentChatError) {
			mu.Lock()
			failures = append(failures, e)
			mu.Unlock()
		}
		o.OnRateLimit = func(acct string, rl protocol.AgentRateLimit) {
			mu.Lock()
			if acct == "mock2" {
				limits = append(limits, rl)
			}
			mu.Unlock()
		}
	})
	tm.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: MockAgent, AccountID: "mock2"})
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t1", Text: "fail auth"})
	ev, i := tm.col.waitFor(t, 0, "auth error", errKind(protocol.ErrKindAuth))
	if ev.Error.Relogin == nil || ev.Error.Relogin.Command == "" {
		t.Fatalf("auth error = %+v", ev.Error)
	}
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t2", Text: "hit the limit"})
	ev, _ = tm.col.waitFor(t, i, "usage_limit", errKind(protocol.ErrKindUsageLimit))
	if ev.Error.Window != "five_hour" || ev.Error.ResetsAt == nil {
		t.Fatalf("usage_limit = %+v", ev.Error)
	}
	waitUntil(t, "callbacks", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(failures) == 2 && len(limits) == 2
	})
	mu.Lock()
	for _, rl := range limits {
		if rl.State != "exhausted" || rl.Window != "five_hour" || rl.ResetsAt == nil {
			t.Errorf("rate limit = %+v", rl)
		}
	}
	mu.Unlock()
	// The usage_limit error does not kill the engine (nothing to reopen or retry).
	if !tm.chatInfo("c1").Open {
		t.Fatal("usage_limit closed the session")
	}
	// The mock's probe now reports the weekly window exhausted.
	acct := tm.mock.Probe(context.Background(), Account{ID: "mock2", Agent: MockAgent})
	found := false
	for _, u := range acct.Usage {
		if u.Window == "seven_day" {
			found = u.State == "exhausted" && u.UsedPercent == 100 && u.ResetsAt != nil && u.ResetsAt.After(time.Now())
		}
	}
	if !found {
		t.Fatalf("mock probe usage after limit = %+v", acct.Usage)
	}
	if other := tm.mock.Probe(context.Background(), Account{ID: "mock", Agent: MockAgent}); other.Usage[1].State == "exhausted" {
		t.Fatal("limit leaked to another account")
	}
}

func TestManagerOpenErrors(t *testing.T) {
	tm := newTestManager(t, func(o *ManagerOptions) { o.Accounts = staticResolver{} })
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "never-opened", TurnID: "t1", Text: "hi"})
	ev, _ := tm.col.waitFor(t, 0, "protocol error", errKind(protocol.ErrKindProtocol))
	if ev.TurnID != "t1" {
		t.Fatalf("error = %+v", ev)
	}
	// Open only records the target; the engine starts on the first prompt.
	tm.Open(protocol.AgentChatOpen{ChatID: "c2", Agent: "nope"})
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c2", TurnID: "t2", Text: "hi"})
	tm.col.waitFor(t, 0, "no engine", func(e protocol.AgentChatEvent) bool {
		return e.ChatID == "c2" && e.Kind == protocol.EventError && e.Error.Kind == protocol.ErrKindNotFound
	})
}

// failingEngine fails Open after emitting its own classified error.
type failingEngine struct{}

func (failingEngine) Agent() string { return "failing" }
func (failingEngine) Probe(context.Context, Account) protocol.AgentAccount {
	return protocol.AgentAccount{}
}
func (failingEngine) Open(_ context.Context, _ Account, _ protocol.AgentChatOpen, emit Emit) (Session, error) {
	emit(protocol.AgentChatEvent{Kind: protocol.EventError, Error: &protocol.AgentChatError{Kind: protocol.ErrKindAuth, Message: "login"}})
	return nil, errors.New("not logged in")
}

func TestManagerOpenFailureNotDuplicated(t *testing.T) {
	tm := newTestManager(t, func(o *ManagerOptions) {
		o.Accounts = staticResolver{}
		o.Engines = map[string]Engine{"failing": failingEngine{}}
	})
	// The hub sends open then prompt: one failure must surface exactly once.
	tm.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: "failing"})
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t1", Text: "hi"})
	tm.col.waitFor(t, 0, "auth error", errKind(protocol.ErrKindAuth))
	time.Sleep(30 * time.Millisecond)
	n := 0
	for _, e := range tm.col.all() {
		if e.Kind == protocol.EventError {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d errors for one failed open: %+v", n, tm.col.all())
	}
}

func TestManagerShutdownClosesSessions(t *testing.T) {
	eng := &silentEngine{closed: make(chan struct{})}
	col := newCollector()
	m := NewManager(ManagerOptions{Engines: map[string]Engine{"silent": eng}, Accounts: staticResolver{}, Sink: col.sink})
	m.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: "silent"})
	m.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t0", Text: "start"})
	col.waitFor(t, 0, "ready", kind(protocol.EventSessionReady))
	waitUntil(t, "open", func() bool { return len(m.Chats()) == 1 && m.Chats()[0].Open })
	m.Shutdown(context.Background())
	select {
	case <-eng.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("session not closed on shutdown")
	}
	// Operations after shutdown are ignored.
	m.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t", Text: "x"})
}
