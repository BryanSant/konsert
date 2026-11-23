package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/BryanSant/konsert/internal/protocol"
)

// MockEngine is an in-process engine with deterministic, keyword-driven behaviour, enabled
// with --enable-mock-driver. It drives hub and web end-to-end tests without paid calls.
//
// Prompt keywords (case-insensitive, first match wins):
//
//	crash       exit error (code 3, stderr tail); the session is dead and the next prompt resumes
//	fail auth   ErrKindAuth with relogin help
//	limit       EventRateLimit (exhausted) then ErrKindUsageLimit, window five_hour, resets_at
//	permission  tool call + permission elicitation; waits for agent.chat.respond
//	ask         question elicitation (options + free text); waits for agent.chat.respond
//	slow        streams a delta per step for SlowDuration; cancellable
//	activity    walks the activity states slowly enough to see each one (ActivityHold per
//	            state): thinking, working (Bash "Run: go test ./..."), speaking, done
//	vague question  calls Konsert's ask_user under-specified (rejected), then properly
//	question    calls Konsert's ask_user (MCP) with a well-formed question and reports the answer
//	(anything)  thought, message deltas, a tool call, message, usage, turn_end
//
// It also forks sessions (AgentChatOpen.ForkFrom), answers Konsert's side-conversation,
// summary and delegation prompts (see mock_questions.go).
type MockEngine struct {
	// StepDelay separates scripted events (default 20 ms).
	StepDelay time.Duration
	// SlowDuration is how long a "slow" turn runs unless cancelled (default 60 s).
	SlowDuration time.Duration
	// ActivityHold is how long an "activity" turn stays in each state (default 1.2 s:
	// longer than the activity publisher's debounce).
	ActivityHold time.Duration
	// Now is the clock used for resets_at (default time.Now).
	Now func() time.Time

	mu      sync.Mutex
	counter int
	// exhausted marks accounts whose "limit" turn ran: Probe then reports the seven_day
	// window exhausted until the time stored here.
	exhausted map[string]time.Time
	// memory: agent session id -> the user prompts it received (forks inherit them).
	memory map[string][]string
}

func (e *MockEngine) markExhausted(accountID string, until time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.exhausted == nil {
		e.exhausted = map[string]time.Time{}
	}
	e.exhausted[accountID] = until
}

func (e *MockEngine) exhaustedUntil(accountID string) (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.exhausted[accountID]
	if ok && !e.now().Before(t) {
		delete(e.exhausted, accountID)
		return time.Time{}, false
	}
	return t, ok
}

// NewMockEngine returns a MockEngine with default timings.
func NewMockEngine() *MockEngine { return &MockEngine{} }

func (e *MockEngine) step() time.Duration {
	if e.StepDelay > 0 {
		return e.StepDelay
	}
	return 20 * time.Millisecond
}

func (e *MockEngine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Agent implements Engine.
func (e *MockEngine) Agent() string { return MockAgent }

var mockModels = []protocol.AgentModel{
	{ID: "mock-smart", DisplayName: "Mock Smart", Description: "Deterministic scripted replies", IsDefault: true,
		Efforts: []protocol.AgentEffort{
			{ID: "low", Label: "Low", Short: "Low"}, {ID: "medium", Label: "Medium", Short: "Med"},
			{ID: "high", Label: "High", Short: "High"}, {ID: "max", Label: "Max", Short: "Max"},
		}, DefaultEffort: "high", ContextTokens: 200000},
	{ID: "mock-fast", DisplayName: "Mock Fast", Description: "Same script, fewer efforts",
		Efforts: []protocol.AgentEffort{
			{ID: "low", Label: "Low", Short: "Low"}, {ID: "medium", Label: "Medium", Short: "Med"},
		}, DefaultEffort: "medium", ContextTokens: 100000},
	{ID: "mock-mini", DisplayName: "Mock Mini", Description: "No effort levels", ContextTokens: 32000},
}

var mockApprovals = []protocol.AgentApprovalMode{
	{ID: "plan", Label: "Plan", Description: "Read-only: proposes changes", Risk: 0, Icon: "eye"},
	{ID: "default", Label: "Ask", Description: "Asks before edits and commands", Risk: 1, Icon: "hand"},
	{ID: "accept-edits", Label: "Accept edits", Description: "Edits freely, asks before commands", Risk: 2, Icon: "edit"},
	{ID: "auto", Label: "Auto", Description: "Approves safe actions itself", Risk: 3, Icon: "shield-check"},
	{ID: "full-access", Label: "Full access", Description: "Never asks", Risk: 4, Icon: "shield-alert"},
}

// Probe implements Engine: two plans with different usage, no process spawned.
func (e *MockEngine) Probe(_ context.Context, acct Account) protocol.AgentAccount {
	now := e.now().UTC()
	five := now.Add(3 * time.Hour).Truncate(time.Minute)
	week := now.Add(4 * 24 * time.Hour).Truncate(time.Hour)
	out := protocol.AgentAccount{
		ID: acct.ID, Agent: MockAgent, Label: acct.Label, IsDefault: acct.IsDefault, Version: "mock-1.0",
		Auth:   protocol.AgentAuthStatus{State: protocol.AuthOK, Method: "oauth", Email: "mock@example.com"},
		Plan:   "max",
		Models: append([]protocol.AgentModel(nil), mockModels...), ModelsSource: "discovered", DefaultModel: "mock-smart",
		Approvals: append([]protocol.AgentApprovalMode(nil), mockApprovals...), DefaultApproval: "default",
		Usage: []protocol.AgentRateLimit{
			{Window: "five_hour", UsedPercent: 12, ResetsAt: &five, State: "ok"},
			{Window: "seven_day", UsedPercent: 40, ResetsAt: &week, State: "ok"},
		},
		CheckedAt: now, ChatSupported: true, Fork: true,
	}
	if acct.Env["KONSERT_MOCK_ACCOUNT"] == "mock2" || acct.ID == "mock2" {
		out.Plan = "pro"
		out.Auth.Email = "mock2@example.com"
		out.Usage = []protocol.AgentRateLimit{
			{Window: "five_hour", UsedPercent: 87, ResetsAt: &five, State: "warning"},
			{Window: "seven_day", UsedPercent: 65, ResetsAt: &week, State: "ok"},
		}
	}
	if until, ok := e.exhaustedUntil(acct.ID); ok {
		u := until.UTC()
		for i := range out.Usage {
			if out.Usage[i].Window == "seven_day" {
				out.Usage[i] = protocol.AgentRateLimit{Window: "seven_day", UsedPercent: 100, ResetsAt: &u, State: "exhausted"}
			}
		}
	}
	return out
}

// Open implements Engine.
func (e *MockEngine) Open(ctx context.Context, acct Account, opts protocol.AgentChatOpen, emit Emit) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.counter++
	n := e.counter
	e.mu.Unlock()
	sid := opts.AgentSessionID
	forkOf := ""
	if sid == "" && opts.ForkFrom != "" {
		sid, forkOf = "mock-fork-"+opts.ChatID+"-"+itoa(n), opts.ForkFrom
		e.mu.Lock()
		if e.memory == nil {
			e.memory = map[string][]string{}
		}
		e.memory[sid] = append([]string(nil), e.memory[forkOf]...)
		e.mu.Unlock()
	}
	if sid == "" {
		sid = "mock-session-" + opts.ChatID + "-" + itoa(n)
	}
	model := opts.Model
	if model == "" {
		model = "mock-smart"
	}
	mode := opts.ApprovalMode
	if mode == "" {
		mode = "default"
	}
	s := &mockSession{
		eng: e, acct: acct, emit: emit, sid: sid, opts: opts, forkOf: forkOf,
		model: model, effort: opts.Effort, mode: mode, resumed: opts.AgentSessionID != "",
		done: make(chan struct{}), answers: map[string]chan protocol.AgentChatRespond{},
	}
	emit(protocol.AgentChatEvent{Kind: protocol.EventSessionReady, AgentSessionID: sid, Model: model, Effort: opts.Effort, ApprovalMode: mode})
	return s, nil
}

type mockSession struct {
	eng     *MockEngine
	acct    Account
	emit    Emit
	sid     string
	resumed bool
	opts    protocol.AgentChatOpen
	forkOf  string // the session this one was forked from

	mcpSess         *mcp.ClientSession // to the chat's konsert MCP server (lazy)
	toolN           int
	lastRecommended string

	mu      sync.Mutex
	model   string
	effort  string
	mode    string
	busy    bool
	turn    string // the running turn's id
	turnGen uint64
	cancel  context.CancelFunc
	dead    bool
	closed  bool
	answers map[string]chan protocol.AgentChatRespond
	wg      sync.WaitGroup

	done     chan struct{}
	doneOnce sync.Once

	// thaw is non-nil while the session is suspended; closed on resume.
	thaw chan struct{}
}

// Suspend freezes the mock "process": scripted events stop until Resume (Suspender).
func (s *mockSession) Suspend() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.thaw == nil {
		s.thaw = make(chan struct{})
	}
	return nil
}

// Resume lets the scripted events continue (Suspender).
func (s *mockSession) Resume() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.thaw != nil {
		close(s.thaw)
		s.thaw = nil
	}
	return nil
}

// frozen waits while the session is suspended; false when ctx ended.
func (s *mockSession) frozen(ctx context.Context) bool {
	for {
		s.mu.Lock()
		thaw := s.thaw
		s.mu.Unlock()
		if thaw == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-thaw:
		}
	}
}

// Done is closed when the mock "process" exits (crash or Close).
func (s *mockSession) Done() <-chan struct{} { return s.done }

func (s *mockSession) AgentSessionID() string { return s.sid }

func (s *mockSession) Prompt(_ context.Context, p protocol.AgentChatPrompt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.dead || s.closed:
		return errors.New("mock session has exited")
	case s.busy:
		return errors.New("a turn is already running")
	}
	s.busy = true
	s.turn = p.TurnID
	s.turnGen++
	gen := s.turnGen
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		s.run(ctx, p)
		s.mu.Lock()
		if s.turnGen == gen { // a later turn may already be running
			s.busy = false
			s.cancel = nil
		}
		s.mu.Unlock()
	}()
	return nil
}

// emitEv emits ev. Like the real engines, the turn is over before its final
// event (turn_end or error) goes out, so a prompt sent on seeing it is accepted.
func (s *mockSession) emitEv(ev protocol.AgentChatEvent) {
	if ev.Kind == protocol.EventTurnEnd || ev.Kind == protocol.EventError {
		s.mu.Lock()
		if s.busy && ev.TurnID == s.turn {
			s.busy = false
			s.cancel = nil
		}
		s.mu.Unlock()
	}
	s.emit(ev)
}

func (s *mockSession) Respond(_ context.Context, r protocol.AgentChatRespond) error {
	s.mu.Lock()
	ch, ok := s.answers[r.ElicitationID]
	if ok {
		delete(s.answers, r.ElicitationID)
	}
	s.mu.Unlock()
	if !ok {
		return errors.New("no pending elicitation " + r.ElicitationID)
	}
	ch <- r
	return nil
}

func (s *mockSession) Control(_ context.Context, c protocol.AgentChatControl) error {
	s.mu.Lock()
	if c.Model != "" {
		s.model = c.Model
	}
	if c.Effort != "" {
		s.effort = c.Effort
	}
	if c.ApprovalMode != "" {
		s.mode = c.ApprovalMode
	}
	cancel := s.cancel
	model, effort, mode := s.model, s.effort, s.mode
	s.mu.Unlock()
	if c.Cancel && cancel != nil {
		cancel()
	}
	if c.Model != "" || c.Effort != "" || c.ApprovalMode != "" {
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventSessionReady, AgentSessionID: s.sid, Model: model, Effort: effort, ApprovalMode: mode})
	}
	return nil
}

func (s *mockSession) Close() error {
	s.mu.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	mc := s.mcpSess
	s.mcpSess = nil
	s.mu.Unlock()
	if mc != nil {
		_ = mc.Close()
	}
	s.wg.Wait()
	s.doneOnce.Do(func() { close(s.done) })
	return nil
}

// sleep waits one step; false when ctx ended.
func (s *mockSession) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return s.frozen(ctx)
	}
}

func (s *mockSession) cancelled(turn string) {
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: turn, StopReason: "cancelled"})
}

func (s *mockSession) deltas(ctx context.Context, turn, msgID, text string) bool {
	for _, w := range strings.SplitAfter(text, " ") {
		if !s.sleep(ctx, s.eng.step()) {
			return false
		}
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, TurnID: turn, MessageID: msgID, Text: w})
	}
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventMessage, TurnID: turn, MessageID: msgID, Text: text})
	return true
}

func (s *mockSession) run(ctx context.Context, p protocol.AgentChatPrompt) {
	// Keywords come from the user's own text, not a "[Konsert]" note the hub prepended
	// (its wording would otherwise trigger "ask", "permission", ...).
	text := p.Text
	if strings.HasPrefix(text, "[Konsert]") {
		text = ""
		if _, rest, ok := strings.Cut(p.Text, "\n\n"); ok {
			text = rest
		}
	}
	if text != "" {
		s.eng.mu.Lock()
		if s.eng.memory == nil {
			s.eng.memory = map[string][]string{}
		}
		s.eng.memory[s.sid] = append(s.eng.memory[s.sid], text)
		s.eng.mu.Unlock()
	}
	text = strings.ToLower(text)
	if s.runQuestionTurn(ctx, p, text) {
		return
	}
	turn := p.TurnID
	step := s.eng.step()
	switch {
	case strings.Contains(text, "crash"):
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, TurnID: turn, MessageID: "m-" + turn, Text: "Working on it"})
		if !s.sleep(ctx, step) {
			s.cancelled(turn)
			return
		}
		code := 3
		s.mu.Lock()
		s.dead = true
		s.mu.Unlock()
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: turn, Error: &protocol.AgentChatError{
			Kind: protocol.ErrKindExit, Message: "mock exited with code 3", ExitCode: &code,
			Stderr: "mock: loading session " + s.sid + "\nmock: fatal: simulated crash (keyword \"crash\")",
		}})
		s.doneOnce.Do(func() { close(s.done) })

	case strings.Contains(text, "fail auth"):
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: turn, Error: &protocol.AgentChatError{
			Kind: protocol.ErrKindAuth, Message: s.acct.Label + " needs you to sign in again (mock)",
			Relogin: &protocol.AgentRelogin{Command: "mock login", Env: map[string]string{"KONSERT_MOCK_ACCOUNT": s.acct.ID},
				URL: "https://example.com/mock/login", Note: "Simulated by the mock engine"},
		}})

	case strings.Contains(text, "limit"):
		resets := s.eng.now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
		// The account's weekly window shows as exhausted in later probes.
		s.eng.markExhausted(s.acct.ID, s.eng.now().UTC().Add(3*time.Hour).Truncate(time.Minute))
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventRateLimit, TurnID: turn, RateLimit: &protocol.AgentRateLimit{
			Window: "five_hour", UsedPercent: 100, ResetsAt: &resets, State: "exhausted"}})
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: turn, Error: &protocol.AgentChatError{
			Kind: protocol.ErrKindUsageLimit, Message: "5-hour usage limit reached (mock)", Window: "five_hour", ResetsAt: &resets,
		}})

	case strings.Contains(text, "permission"):
		tool := &protocol.AgentToolCall{ID: "t-" + turn, Name: "Bash", Kind: "execute", Title: "Run: rm -rf build/", Status: "pending", Input: `{"command":"rm -rf build/"}`}
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventToolCall, TurnID: turn, ToolCall: tool})
		el := &protocol.AgentElicitation{ID: "e-" + turn, Kind: protocol.ElicitPermission, Title: "Allow Bash: rm -rf build/?", ToolCall: tool,
			Options: []protocol.AgentElicitationOption{
				{ID: "allow_once", Label: "Allow once", Kind: "allow_once"},
				{ID: "allow_always", Label: "Always allow", Kind: "allow_always"},
				{ID: "reject_once", Label: "Reject", Kind: "reject_once"},
			}}
		ans, ok := s.ask(ctx, turn, el)
		if !ok {
			s.cancelled(turn)
			return
		}
		done := *tool
		reply := "Permission denied, so I left build/ alone."
		if strings.HasPrefix(ans.OptionID, "allow") {
			done.Status, done.Output, reply = "completed", "removed build/", "Done: build/ was removed."
		} else {
			done.Status = "failed"
		}
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventToolCall, TurnID: turn, ToolCall: &done})
		if !s.deltas(ctx, turn, "m-"+turn, reply) {
			s.cancelled(turn)
			return
		}
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: turn, StopReason: "end_turn"})

	case strings.Contains(text, "ask"):
		el := &protocol.AgentElicitation{ID: "e-" + turn, Kind: protocol.ElicitQuestion, Title: "Which color should the button be?",
			Options: []protocol.AgentElicitationOption{
				{ID: "blue", Label: "Blue", Kind: "choice"}, {ID: "green", Label: "Green", Kind: "choice"},
			}, AllowText: true}
		ans, ok := s.ask(ctx, turn, el)
		if !ok {
			s.cancelled(turn)
			return
		}
		answer := ans.Text
		if answer == "" {
			answer = ans.OptionID
		}
		if !s.deltas(ctx, turn, "m-"+turn, "You answered: "+answer) {
			s.cancelled(turn)
			return
		}
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: turn, StopReason: "end_turn"})

	case strings.Contains(text, "activity"):
		hold := s.eng.ActivityHold
		if hold <= 0 {
			hold = 1200 * time.Millisecond
		}
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventThoughtDelta, TurnID: turn, Text: "Planning the change."})
		if !s.sleep(ctx, hold) {
			s.cancelled(turn)
			return
		}
		tool := &protocol.AgentToolCall{ID: "t-" + turn, Name: "Bash", Kind: "execute", Title: "Run: go test ./...", Status: "running", Input: `{"command":"go test ./..."}`}
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventToolCall, TurnID: turn, ToolCall: tool})
		if !s.sleep(ctx, hold) {
			s.cancelled(turn)
			return
		}
		done := *tool
		done.Status, done.Output = "completed", "ok"
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventToolCall, TurnID: turn, ToolCall: &done})
		// Speak for about one hold, a word every 100 ms.
		var words []string
		for i := 0; i < int(hold/(100*time.Millisecond))+1; i++ {
			words = append(words, "word"+itoa(i)+" ")
		}
		for _, w := range words {
			if !s.sleep(ctx, 100*time.Millisecond) {
				s.cancelled(turn)
				return
			}
			s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, TurnID: turn, MessageID: "m-" + turn, Text: w})
		}
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventMessage, TurnID: turn, MessageID: "m-" + turn, Text: strings.Join(words, "")})
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: turn, StopReason: "end_turn"})

	case strings.Contains(text, "slow"):
		dur := s.eng.SlowDuration
		if dur <= 0 {
			dur = 60 * time.Second
		}
		tick := max(step, 200*time.Millisecond)
		i := 0
		// Count ticks rather than wall time, so a suspended turn resumes where it stopped.
		for n := int(dur / tick); i < n; {
			if !s.sleep(ctx, tick) {
				s.cancelled(turn)
				return
			}
			i++
			s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, TurnID: turn, MessageID: "m-" + turn, Text: "tick " + itoa(i) + " "})
		}
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: turn, StopReason: "end_turn"})

	default:
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventThoughtDelta, TurnID: turn, Text: "Thinking about the request."})
		if !s.deltas(ctx, turn, "m1-"+turn, "Let me look at the files.") {
			s.cancelled(turn)
			return
		}
		tool := &protocol.AgentToolCall{ID: "t-" + turn, Name: "Read", Kind: "read", Title: "Read README.md", Status: "running", Paths: []string{"README.md"}}
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventToolCall, TurnID: turn, ToolCall: tool})
		if !s.sleep(ctx, step) {
			s.cancelled(turn)
			return
		}
		done := *tool
		done.Status, done.Output = "completed", "# Konsert\n"
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventToolCall, TurnID: turn, ToolCall: &done})
		s.mu.Lock()
		model := s.model
		s.mu.Unlock()
		if !s.deltas(ctx, turn, "m2-"+turn, "Mock reply from "+model+": "+p.Text) {
			s.cancelled(turn)
			return
		}
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventUsage, TurnID: turn, Usage: &protocol.AgentTokenUsage{InputTokens: 120, OutputTokens: 42, CostUSD: 0.001}})
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: turn, StopReason: "end_turn"})
	}
}

// ask emits an elicitation and waits for the answer (false when cancelled).
func (s *mockSession) ask(ctx context.Context, turn string, el *protocol.AgentElicitation) (protocol.AgentChatRespond, bool) {
	ch := make(chan protocol.AgentChatRespond, 1)
	s.mu.Lock()
	s.answers[el.ID] = ch
	s.mu.Unlock()
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventElicitation, TurnID: turn, Elicitation: el})
	select {
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.answers, el.ID)
		s.mu.Unlock()
		return protocol.AgentChatRespond{}, false
	case ans := <-ch:
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventElicitationOK, TurnID: turn, Elicitation: &protocol.AgentElicitation{ID: el.ID, Kind: el.Kind, Title: el.Title}})
		return ans, true
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
