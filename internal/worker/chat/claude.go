package chat

// claudeEngine drives Claude Code through its SDK protocol: one persistent
//
//	claude -p --input-format stream-json --output-format stream-json --verbose
//	       --include-partial-messages --permission-prompt-tool stdio ...
//
// process per chat. User turns are written to stdin as stream-json `user` lines; control
// requests (initialize, set_model, set_permission_mode, interrupt, get_usage) go the same
// way and are answered with `control_response` lines. With `--permission-prompt-tool
// stdio` the CLI asks the host for permission with a `control_request` of subtype
// `can_use_tool`, answered by a `control_response` carrying {behavior: allow|deny}.

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/protocol"
)

const (
	claudeDefaultWatchdog = 10 * time.Minute
	claudeInitTimeout     = 90 * time.Second
	claudeCtlTimeout      = 30 * time.Second
	claudeCancelGrace     = 15 * time.Second
	claudeInputMax        = 2000
	claudeOutputMax       = 4000
)

// claudeDropEnv are inherited variables that must not leak into a child claude: nested
// session markers (konsertd may itself run inside Claude Code) and an API key that would
// silently replace the subscription login.
var claudeDropEnv = []string{
	"ANTHROPIC_API_KEY", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID",
	"CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_PID",
	"CLAUDE_CODE_CHILD_NOTES_DIR", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN",
	"CLAUDE_CODE_EXECPATH", "CLAUDE_EFFORT", "CLAUDE_CODE_SSE_PORT",
}

// claudeEngine implements Engine for the claude CLI (any account).
type claudeEngine struct {
	launch      ccLauncher
	run         ccRunner
	tokenReader func(ctx context.Context, acct Account) (string, error)
	httpClient  *http.Client
	usageURL    string
	now         func() time.Time
	watchdog    time.Duration
	initTimeout time.Duration
	cancelGrace time.Duration
	probeDir    string
	// trace, when set (tests), sees every raw line: dir ">" to the CLI, "<" from it.
	trace func(dir string, line []byte)

	usageMu    sync.Mutex
	usageCache map[string]claudeUsageEntry
}

// NewClaudeEngine returns the engine for the claude CLI.
func NewClaudeEngine() Engine { return newClaudeEngine() }

func newClaudeEngine() *claudeEngine {
	return &claudeEngine{
		launch:      ccExecLaunch,
		run:         ccExecRun,
		tokenReader: claudeReadOAuthToken,
		httpClient:  &http.Client{Timeout: 15 * time.Second},
		usageURL:    claudeUsageURL,
		now:         time.Now,
		watchdog:    claudeDefaultWatchdog,
		initTimeout: claudeInitTimeout,
		cancelGrace: claudeCancelGrace,
		probeDir:    os.TempDir(),
		usageCache:  map[string]claudeUsageEntry{},
	}
}

func (e *claudeEngine) Agent() string { return "claude" }

func (e *claudeEngine) binary(acct Account) string {
	if acct.Binary != "" {
		return acct.Binary
	}
	return "claude"
}

// env is the child environment for an account.
func (e *claudeEngine) env(acct Account) []string {
	drop := append([]string(nil), claudeDropEnv...)
	if _, ok := acct.Env["CLAUDE_CONFIG_DIR"]; ok {
		// A separate config dir is a separate account: never inherit the daemon's token.
		drop = append(drop, "CLAUDE_CODE_OAUTH_TOKEN")
	}
	return ccBuildEnv(acct.Env, drop)
}

func (e *claudeEngine) spec(acct Account, dir string, args ...string) ccSpec {
	return ccSpec{
		Binary: e.binary(acct),
		Args:   append(append([]string(nil), acct.ArgsPrefix...), args...),
		Env:    e.env(acct),
		Dir:    dir,
	}
}

// claudeRelogin tells the user how to sign this account in again.
func claudeRelogin(acct Account) *protocol.AgentRelogin {
	env := ccNonSecretEnv(acct.Env, "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	if _, ok := acct.Env["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		return &protocol.AgentRelogin{
			Command: "claude setup-token",
			Env:     env,
			URL:     "https://docs.claude.com/en/docs/claude-code/setup",
			Note:    "This account uses a long-lived setup token: create a new one and store it in the account's token file.",
		}
	}
	return &protocol.AgentRelogin{
		Command: "claude /login",
		Env:     env,
		URL:     "https://docs.claude.com/en/docs/claude-code/setup",
	}
}

// --- Open -----------------------------------------------------------------------------

func (e *claudeEngine) Open(ctx context.Context, acct Account, opts protocol.AgentChatOpen, emit Emit) (Session, error) {
	if st := e.authStatus(ctx, acct); st.State == protocol.AuthLoggedOut {
		cerr := &protocol.AgentChatError{
			Kind:    protocol.ErrKindAuth,
			Message: fmt.Sprintf("%s is not signed in", claudeLabel(acct)),
			Relogin: claudeRelogin(acct),
		}
		emit(protocol.AgentChatEvent{Kind: protocol.EventError, Error: cerr})
		return nil, errors.New(cerr.Message)
	}

	s := &claudeSession{
		eng:     e,
		acct:    acct,
		opts:    opts,
		emit:    emit,
		model:   opts.Model,
		effort:  opts.Effort,
		mode:    claudeModeID(opts.ApprovalMode),
		pending: map[string]*claudePending{},
		waiters: map[string]chan claudeCtlResult{},
		stop:    make(chan struct{}),
	}
	resumeLost := false
	if opts.AgentSessionID != "" && claudeTranscriptExists(acct, opts.Cwd, opts.AgentSessionID) {
		s.sessionID = opts.AgentSessionID
		s.hasTranscript = true
	} else {
		// No stored conversation to resume in this account (e.g. the chat moved from
		// another Claude account, or the transcript was deleted): start a new one rather
		// than failing with "No conversation found".
		resumeLost = opts.AgentSessionID != ""
		s.sessionID = ids.UUID()
	}
	if resumeLost {
		emit(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusStarting,
			Text: "The previous conversation isn't available in " + claudeLabel(acct) + "; starting a new conversation."})
	}
	if err := s.ensureProc(ctx); err != nil {
		s.emitStartError(err)
		_ = s.Close()
		return nil, err
	}
	go s.watchdogLoop()
	s.mu.Lock()
	ready := protocol.AgentChatEvent{
		Kind:           protocol.EventSessionReady,
		AgentSessionID: s.sessionID,
		Model:          s.model,
		Effort:         s.effort,
		ApprovalMode:   s.mode,
	}
	s.mu.Unlock()
	emit(ready)
	return s, nil
}

func claudeLabel(acct Account) string {
	if acct.Label != "" {
		return acct.Label
	}
	if acct.ID != "" {
		return acct.ID
	}
	return "Claude"
}

// --- Session --------------------------------------------------------------------------

type claudePending struct {
	id          string
	kind        string
	toolName    string
	input       jsontext.Value
	suggestions jsontext.Value
	questions   []claudeQuestion
}

type claudeCtlResult struct {
	ok       bool
	response jsontext.Value
	err      string
}

type claudeTurn struct {
	id        string
	cancelled bool
	errored   bool
	lastText  string
	curMsgID  string
	msgText   map[string]string
	tools     map[string]*protocol.AgentToolCall
}

type claudeSession struct {
	eng  *claudeEngine
	acct Account
	opts protocol.AgentChatOpen
	emit Emit

	lifeMu sync.Mutex // serializes process (re)starts

	writeMu sync.Mutex // serializes stdin writes

	ctlMu   sync.Mutex
	waiters map[string]chan claudeCtlResult
	reqSeq  atomic.Int64

	mu             sync.Mutex
	proc           ccProcess
	procGen        int
	model          string
	effort         string
	mode           string
	launchedEffort string
	launchedMode   string
	needRestart    string // non-empty: restart before the next turn (reason)
	sessionID      string
	hasTranscript  bool // the CLI has stored the session: restart with --resume
	turn           *claudeTurn
	pending        map[string]*claudePending
	lastActivity   time.Time
	watchdogFired  bool
	lastRejected   *protocol.AgentRateLimit
	mcpFile        string
	closed         bool
	stop           chan struct{}
	stopOnce       sync.Once
}

func (s *claudeSession) AgentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// args builds the CLI argv (after the account's ArgsPrefix). Caller holds s.mu.
func (s *claudeSession) argsLocked() ([]string, error) {
	args := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--include-partial-messages", "--permission-prompt-tool", "stdio",
	}
	if s.model != "" {
		args = append(args, "--model", s.model)
	}
	if s.effort != "" {
		args = append(args, "--effort", s.effort)
	}
	args = append(args, "--permission-mode", s.mode)
	if len(s.opts.MCPServers) > 0 {
		if s.mcpFile == "" {
			path, err := claudeWriteMCPConfig(s.opts.MCPServers)
			if err != nil {
				return nil, err
			}
			s.mcpFile = path
		}
		args = append(args, "--mcp-config", s.mcpFile)
		// Konsert's own servers (ask_user, fleet tools) are trusted: never ask the user
		// for permission to ask the user a question.
		for _, srv := range s.opts.MCPServers {
			if name := strings.TrimSpace(srv.Name); name != "" {
				args = append(args, "--allowedTools", "mcp__"+name)
			}
		}
	}
	if s.opts.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", s.opts.SystemPrompt)
	}
	if s.hasTranscript {
		args = append(args, "--resume", s.sessionID)
	} else {
		args = append(args, "--session-id", s.sessionID)
	}
	return args, nil
}

// claudeMCPConfig renders MCP servers as claude's --mcp-config JSON (HTTP servers).
func claudeMCPConfig(servers []protocol.AgentMCPServer) ([]byte, error) {
	type srv struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	m := map[string]srv{}
	for _, s := range servers {
		m[s.Name] = srv{Type: "http", URL: s.URL, Headers: s.Headers}
	}
	return marshalLine(map[string]any{"mcpServers": m})
}

// claudeWriteMCPConfig writes the MCP config to a private temp file (the headers carry
// bearer tokens, which must not appear in argv where `ps` can see them).
func claudeWriteMCPConfig(servers []protocol.AgentMCPServer) (string, error) {
	data, err := claudeMCPConfig(servers)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "konsert-mcp-*.json")
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		_ = os.Remove(f.Name())
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// ensureProc starts the CLI process if it is not running (or must restart), and runs
// the initialize handshake.
func (s *claudeSession) ensureProc(ctx context.Context) error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("session closed")
	}
	if s.proc != nil && s.needRestart == "" {
		s.mu.Unlock()
		return nil
	}
	old := s.proc
	s.proc = nil
	s.procGen++
	gen := s.procGen
	s.needRestart = ""
	args, err := s.argsLocked()
	s.launchedEffort, s.launchedMode = s.effort, s.mode
	cwd := s.opts.Cwd
	s.mu.Unlock()
	if old != nil {
		old.Kill()
	}
	if err != nil {
		return err
	}

	proc, err := s.eng.launch(ctx, s.eng.spec(s.acct, cwd, args...))
	if err != nil {
		return &claudeStartError{notFound: IsNotInstalled(err) || strings.Contains(err.Error(), "executable file not found"), err: err}
	}
	s.mu.Lock()
	s.proc = proc
	s.lastActivity = s.eng.now()
	s.mu.Unlock()
	go s.readLoop(proc, gen)

	ictx, cancel := context.WithTimeout(ctx, s.eng.initTimeout)
	defer cancel()
	res, err := s.control(ictx, map[string]any{"subtype": "initialize"})
	if err != nil {
		proc.Kill()
		code, stderr := proc.Wait()
		return &claudeStartError{exitCode: &code, stderr: stderr, err: err}
	}
	var init struct {
		Mode string `json:"current_permission_mode"`
	}
	_ = unmarshal(res, &init)
	if init.Mode != "" {
		s.mu.Lock()
		s.mode = init.Mode
		s.mu.Unlock()
	}
	return nil
}

type claudeStartError struct {
	notFound bool
	exitCode *int
	stderr   string
	err      error
}

func (e *claudeStartError) Error() string { return "claude failed to start: " + e.err.Error() }
func (e *claudeStartError) Unwrap() error { return e.err }

func (s *claudeSession) emitStartError(err error) {
	cerr := &protocol.AgentChatError{Kind: protocol.ErrKindOther, Message: err.Error()}
	var se *claudeStartError
	if errors.As(err, &se) {
		stderr := ccRedact(ccTailLines(se.stderr, 20, 2000), s.acct.Env)
		switch {
		case se.notFound:
			cerr = &protocol.AgentChatError{Kind: protocol.ErrKindNotFound, Message: "claude is not installed on this machine (https://docs.claude.com/en/docs/claude-code/setup)"}
		default:
			kind, retry := claudeClassify(stderr, 0)
			if kind == protocol.ErrKindOther {
				kind = protocol.ErrKindExit
			}
			cerr = &protocol.AgentChatError{Kind: kind, Message: "claude failed to start", ExitCode: se.exitCode, Stderr: stderr, Retryable: retry}
			if se.exitCode != nil && *se.exitCode >= 0 {
				cerr.Message = fmt.Sprintf("claude exited with code %d during startup", *se.exitCode)
			}
			if kind == protocol.ErrKindAuth {
				cerr.Relogin = claudeRelogin(s.acct)
			}
		}
	}
	s.emit(protocol.AgentChatEvent{Kind: protocol.EventError, Error: cerr})
}

// write sends one JSON line to the CLI.
func (s *claudeSession) write(v any) error {
	data, err := marshalLine(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	if proc == nil {
		return errors.New("claude is not running")
	}
	if s.eng.trace != nil {
		s.eng.trace(">", data)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = proc.Stdin().Write(append(data, '\n'))
	return err
}

// control sends a control request and waits for its response.
func (s *claudeSession) control(ctx context.Context, req map[string]any) (jsontext.Value, error) {
	id := fmt.Sprintf("konsert-%d", s.reqSeq.Add(1))
	ch := make(chan claudeCtlResult, 1)
	s.ctlMu.Lock()
	s.waiters[id] = ch
	s.ctlMu.Unlock()
	defer func() {
		s.ctlMu.Lock()
		delete(s.waiters, id)
		s.ctlMu.Unlock()
	}()
	if err := s.write(map[string]any{"type": "control_request", "request_id": id, "request": req}); err != nil {
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, errors.New("claude exited")
		}
		if !r.ok {
			return nil, errors.New(r.err)
		}
		return r.response, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("claude did not answer %v: %w", req["subtype"], ctx.Err())
	}
}

func (s *claudeSession) failWaiters() {
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	for id, ch := range s.waiters {
		close(ch)
		delete(s.waiters, id)
	}
}

// Prompt starts a turn.
func (s *claudeSession) Prompt(ctx context.Context, p protocol.AgentChatPrompt) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("session closed")
	}
	if s.turn != nil {
		s.mu.Unlock()
		return errors.New("a turn is already running")
	}
	restart := s.needRestart
	s.mu.Unlock()
	if restart != "" {
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventStatus, TurnID: p.TurnID, Status: protocol.ChatStatusStarting, Text: "Restarting claude (" + restart + "); the conversation is resumed"})
	}
	if err := s.ensureProc(ctx); err != nil {
		s.emitStartError(err)
		return err
	}
	text := p.Text
	if len(p.Attachments) > 0 {
		text += "\n\nAttached files (read them from disk):\n- " + strings.Join(p.Attachments, "\n- ")
	}
	s.mu.Lock()
	if s.turn != nil {
		s.mu.Unlock()
		return errors.New("a turn is already running")
	}
	s.turn = &claudeTurn{id: p.TurnID, msgText: map[string]string{}, tools: map[string]*protocol.AgentToolCall{}}
	s.lastActivity = s.eng.now()
	s.watchdogFired = false
	s.lastRejected = nil
	s.mu.Unlock()
	msg := map[string]any{
		"type":               "user",
		"session_id":         "",
		"parent_tool_use_id": nil,
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	}
	if err := s.write(msg); err != nil {
		s.mu.Lock()
		s.turn = nil
		s.mu.Unlock()
		return fmt.Errorf("send prompt to claude: %w", err)
	}
	return nil
}

// Respond answers a pending permission or question elicitation.
func (s *claudeSession) Respond(_ context.Context, r protocol.AgentChatRespond) error {
	s.mu.Lock()
	p := s.pending[r.ElicitationID]
	if p == nil {
		s.mu.Unlock()
		return fmt.Errorf("no pending elicitation %q", r.ElicitationID)
	}
	// An answer that matches none of the offered options is a client bug, not a
	// rejection: keep the request pending instead of silently denying the tool call.
	if err := claudeValidateAnswer(p, r); err != nil {
		s.mu.Unlock()
		return err
	}
	delete(s.pending, r.ElicitationID)
	turnID := ""
	if s.turn != nil {
		turnID = s.turn.id
	}
	s.mu.Unlock()
	// Report the resolution before answering: once Claude has the answer it may
	// finish the turn at once, and the resolution must not follow turn_end.
	s.emit(protocol.AgentChatEvent{
		Kind:        protocol.EventElicitationOK,
		TurnID:      turnID,
		Text:        r.OptionID,
		Elicitation: &protocol.AgentElicitation{ID: p.id, Kind: p.kind},
	})
	resp := claudePermissionAnswer(p, r)
	return s.write(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": p.id,
			"response":   resp,
		},
	})
}

// claudeValidateAnswer checks the user's choice against what the elicitation offered.
func claudeValidateAnswer(p *claudePending, r protocol.AgentChatRespond) error {
	if p.kind == protocol.ElicitQuestion {
		if r.OptionID == "" || r.OptionID == "reject" || strings.TrimSpace(r.Text) != "" {
			return nil
		}
		for _, q := range p.questions {
			for i := range q.Options {
				if r.OptionID == fmt.Sprintf("opt_%d", i) {
					return nil
				}
			}
		}
		return fmt.Errorf("unknown option %q for question %s", r.OptionID, p.id)
	}
	switch r.OptionID {
	case "allow_once", "allow", "allow_always", "reject", "reject_once", "reject_always":
		return nil
	}
	return fmt.Errorf("unknown option %q for permission request %s (want allow_once, allow_always or reject)", r.OptionID, p.id)
}

// claudePermissionAnswer builds the can_use_tool response for the user's choice.
func claudePermissionAnswer(p *claudePending, r protocol.AgentChatRespond) map[string]any {
	input := p.input
	if len(input) == 0 {
		input = jsontext.Value("{}")
	}
	if p.kind == protocol.ElicitQuestion {
		if r.OptionID == "reject" {
			return map[string]any{"behavior": "deny", "message": "The user dismissed the question."}
		}
		answer := strings.TrimSpace(r.Text)
		if answer == "" {
			for _, q := range p.questions {
				for i, o := range q.Options {
					if r.OptionID == fmt.Sprintf("opt_%d", i) {
						answer = o.Label
					}
				}
			}
		}
		var in map[string]any
		_ = unmarshal(input, &in)
		if in == nil {
			in = map[string]any{}
		}
		answers := map[string]string{}
		for _, q := range p.questions {
			answers[q.Question] = answer
		}
		in["answers"] = answers
		return map[string]any{"behavior": "allow", "updatedInput": in}
	}
	switch r.OptionID {
	case "allow_always":
		var perms any
		if len(p.suggestions) > 0 && string(p.suggestions) != "null" {
			perms = p.suggestions
		} else {
			perms = []map[string]any{{
				"type":        "addRules",
				"rules":       []map[string]any{{"toolName": p.toolName}},
				"behavior":    "allow",
				"destination": "session",
			}}
		}
		return map[string]any{"behavior": "allow", "updatedInput": input, "updatedPermissions": perms}
	case "allow_once", "allow":
		return map[string]any{"behavior": "allow", "updatedInput": input}
	default:
		msg := "The user rejected this action."
		if t := strings.TrimSpace(r.Text); t != "" {
			msg += " They said: " + t
		}
		return map[string]any{"behavior": "deny", "message": msg}
	}
}

func (s *claudeSession) turnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != nil {
		return s.turn.id
	}
	return ""
}

// Control changes model / effort / approval mode, or cancels the running turn.
func (s *claudeSession) Control(ctx context.Context, c protocol.AgentChatControl) error {
	if c.Cancel {
		if err := s.cancel(ctx); err != nil {
			return err
		}
	}
	var notes []string
	if c.Model != "" {
		s.mu.Lock()
		running := s.proc != nil
		s.mu.Unlock()
		if running {
			cctx, cancel := context.WithTimeout(ctx, claudeCtlTimeout)
			_, err := s.control(cctx, map[string]any{"subtype": "set_model", "model": c.Model})
			cancel()
			if err != nil {
				return fmt.Errorf("set model %q: %w", c.Model, err)
			}
		}
		s.mu.Lock()
		s.model = c.Model
		s.mu.Unlock()
	}
	if c.Effort != "" {
		s.mu.Lock()
		changed := c.Effort != s.effort
		s.effort = c.Effort
		if changed && s.proc != nil && c.Effort != s.launchedEffort {
			s.needRestart = "effort " + c.Effort
			notes = append(notes, "Effort "+c.Effort+" applies from the next message (claude restarts with the conversation resumed)")
		}
		s.mu.Unlock()
	}
	if c.ApprovalMode != "" {
		mode := claudeModeID(c.ApprovalMode)
		s.mu.Lock()
		running := s.proc != nil
		launchedBypass := s.launchedMode == "bypassPermissions"
		s.mu.Unlock()
		switch {
		case !running:
			s.mu.Lock()
			s.mode = mode
			s.mu.Unlock()
		case mode == "bypassPermissions" && !launchedBypass:
			// The CLI refuses a live switch into bypass unless it was launched with it.
			s.mu.Lock()
			s.mode = mode
			s.needRestart = "approval mode " + mode
			s.mu.Unlock()
			notes = append(notes, "Full access applies from the next message (claude restarts with the conversation resumed)")
		default:
			cctx, cancel := context.WithTimeout(ctx, claudeCtlTimeout)
			_, err := s.control(cctx, map[string]any{"subtype": "set_permission_mode", "mode": mode})
			cancel()
			if err != nil {
				return fmt.Errorf("set approval mode %q: %w", mode, err)
			}
			s.mu.Lock()
			s.mode = mode
			s.mu.Unlock()
		}
	}
	for _, n := range notes {
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventStatus, TurnID: s.turnID(), Text: n})
	}
	return nil
}

// cancel interrupts the running turn. Pending permission prompts are denied.
func (s *claudeSession) cancel(ctx context.Context) error {
	s.mu.Lock()
	t := s.turn
	if t == nil || s.proc == nil {
		s.mu.Unlock()
		return nil
	}
	t.cancelled = true
	pend := s.pending
	s.pending = map[string]*claudePending{}
	gen := s.procGen
	s.mu.Unlock()
	for _, p := range pend {
		_ = s.write(map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype": "success", "request_id": p.id,
				"response": map[string]any{"behavior": "deny", "message": "The user cancelled the turn.", "interrupt": true},
			},
		})
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventElicitationOK, TurnID: t.id, Text: "cancelled", Elicitation: &protocol.AgentElicitation{ID: p.id, Kind: p.kind}})
	}
	cctx, cancel := context.WithTimeout(ctx, claudeCtlTimeout)
	_, err := s.control(cctx, map[string]any{"subtype": "interrupt"})
	cancel()
	// If the CLI does not wind the turn down, kill it; the next prompt resumes.
	go func() {
		select {
		case <-time.After(s.eng.cancelGrace):
		case <-s.stop:
			return
		}
		s.mu.Lock()
		stuck := s.turn == t && s.procGen == gen && s.proc != nil
		var proc ccProcess
		if stuck {
			proc = s.proc
			s.proc = nil
			s.procGen++
			s.turn = nil
		}
		s.mu.Unlock()
		if stuck {
			proc.Kill()
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: t.id, StopReason: "cancelled"})
		}
	}()
	if err != nil {
		return fmt.Errorf("interrupt claude: %w", err)
	}
	return nil
}

// Close terminates the process and waits for it.
func (s *claudeSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	proc := s.proc
	s.proc = nil
	s.procGen++
	mcp := s.mcpFile
	s.mu.Unlock()
	s.stopOnce.Do(func() { close(s.stop) })
	if proc != nil {
		_ = proc.Stdin().Close()
		done := make(chan struct{})
		go func() { proc.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			proc.Kill()
			<-done
		}
	}
	s.failWaiters()
	if mcp != "" {
		_ = os.Remove(mcp)
	}
	return nil
}

// watchdogLoop reports a turn that made no progress for the watchdog period.
func (s *claudeSession) watchdogLoop() {
	period := s.eng.watchdog / 10
	if period > 30*time.Second {
		period = 30 * time.Second
	}
	if period < 10*time.Millisecond {
		period = 10 * time.Millisecond
	}
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
		}
		s.mu.Lock()
		fire := s.turn != nil && !s.watchdogFired && len(s.pending) == 0 &&
			s.eng.now().Sub(s.lastActivity) >= s.eng.watchdog
		var tid string
		if fire {
			s.watchdogFired = true
			tid = s.turn.id
		}
		s.mu.Unlock()
		if fire {
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: tid, Error: &protocol.AgentChatError{
				Kind:    protocol.ErrKindTimeout,
				Message: fmt.Sprintf("claude has made no progress for %s; cancel the turn or keep waiting", s.eng.watchdog.Round(time.Second)),
			}})
		}
	}
}

// --- Reading the stream ---------------------------------------------------------------

type claudeLine struct {
	Type            string         `json:"type"`
	Subtype         string         `json:"subtype"`
	SessionID       string         `json:"session_id"`
	RequestID       string         `json:"request_id"`
	Request         jsontext.Value `json:"request"`
	Response        jsontext.Value `json:"response"`
	Event           jsontext.Value `json:"event"`
	Message         jsontext.Value `json:"message"`
	ParentToolUseID *string        `json:"parent_tool_use_id"`
	Model           string         `json:"model"`
	PermissionMode  string         `json:"permissionMode"`

	IsError        bool                 `json:"is_error"`
	Result         string               `json:"result"`
	TerminalReason string               `json:"terminal_reason"`
	StopReason     *string              `json:"stop_reason"`
	Usage          *claudeUsageJSON     `json:"usage"`
	TotalCostUSD   float64              `json:"total_cost_usd"`
	APIErrorStatus *int                 `json:"api_error_status"`
	Errors         []string             `json:"errors"`
	RateLimitInfo  *claudeRateLimitInfo `json:"rate_limit_info"`

	// assistant: synthetic message carrying an API error (e.g. usage limit text)
	IsAPIErrorMessage bool   `json:"is_api_error_message"`
	APIError          string `json:"api_error"`

	// system api_retry
	Attempt     int  `json:"attempt"`
	ErrorStatus *int `json:"error_status"`
}

type claudeUsageJSON struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CacheReadTokens   int64 `json:"cache_read_input_tokens"`
	CacheCreateTokens int64 `json:"cache_creation_input_tokens"`
}

type claudeRateLimitInfo struct {
	Status         string                       `json:"status"`
	ResetsAt       float64                      `json:"resetsAt"`
	RateLimitType  string                       `json:"rateLimitType"`
	Utilization    *float64                     `json:"utilization"`
	IsUsingOverage bool                         `json:"isUsingOverage"`
	UnifiedWindows map[string]claudeUnifiedWind `json:"unifiedWindows"`
}

type claudeUnifiedWind struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    float64  `json:"resetsAt"`
	Status      string   `json:"status"`
}

type claudeContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Input     jsontext.Value `json:"input"`
	ToolUseID string         `json:"tool_use_id"`
	Content   jsontext.Value `json:"content"`
	IsError   bool           `json:"is_error"`
}

type claudeMessage struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Content jsontext.Value `json:"content"`
}

func (m claudeMessage) blocks() []claudeContentBlock {
	var bs []claudeContentBlock
	if err := unmarshal(m.Content, &bs); err == nil {
		return bs
	}
	var text string
	if err := unmarshal(m.Content, &text); err == nil && text != "" {
		return []claudeContentBlock{{Type: "text", Text: text}}
	}
	return nil
}

func (s *claudeSession) readLoop(proc ccProcess, gen int) {
	sc := ccLineReader(proc.Stdout())
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if s.eng.trace != nil {
			s.eng.trace("<", line)
		}
		var l claudeLine
		if err := unmarshal(line, &l); err != nil {
			continue // non-JSON noise
		}
		s.mu.Lock()
		current := gen == s.procGen
		if current {
			s.lastActivity = s.eng.now()
		}
		s.mu.Unlock()
		if !current {
			continue
		}
		s.handle(&l)
	}
	code, stderr := proc.Wait()
	s.handleExit(gen, code, stderr)
}

func (s *claudeSession) handle(l *claudeLine) {
	switch l.Type {
	case "control_response":
		var r struct {
			Subtype   string         `json:"subtype"`
			RequestID string         `json:"request_id"`
			Response  jsontext.Value `json:"response"`
			Error     string         `json:"error"`
		}
		if unmarshal(l.Response, &r) != nil {
			return
		}
		s.ctlMu.Lock()
		ch := s.waiters[r.RequestID]
		s.ctlMu.Unlock()
		if ch != nil {
			select {
			case ch <- claudeCtlResult{ok: r.Subtype == "success", response: r.Response, err: r.Error}:
			default:
			}
		}
	case "control_request":
		s.handleControlRequest(l)
	case "control_cancel_request":
		s.mu.Lock()
		p := s.pending[l.RequestID]
		delete(s.pending, l.RequestID)
		tid := ""
		if s.turn != nil {
			tid = s.turn.id
		}
		s.mu.Unlock()
		if p != nil {
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventElicitationOK, TurnID: tid, Text: "cancelled", Elicitation: &protocol.AgentElicitation{ID: p.id, Kind: p.kind}})
		}
	case "system":
		s.handleSystem(l)
	case "stream_event":
		s.handleStreamEvent(l)
	case "assistant":
		s.handleAssistant(l)
	case "user":
		s.handleUser(l)
	case "result":
		s.handleResult(l)
	case "rate_limit_event":
		s.handleRateLimit(l)
	}
}

func (s *claudeSession) handleSystem(l *claudeLine) {
	switch l.Subtype {
	case "init":
		s.mu.Lock()
		changed := l.SessionID != "" && l.SessionID != s.sessionID
		if l.SessionID != "" {
			s.sessionID = l.SessionID
		}
		s.hasTranscript = true
		if l.PermissionMode != "" {
			s.mode = l.PermissionMode
		}
		ev := protocol.AgentChatEvent{Kind: protocol.EventSessionReady, AgentSessionID: s.sessionID, Model: l.Model, Effort: s.effort, ApprovalMode: s.mode}
		if s.turn != nil {
			ev.TurnID = s.turn.id
		}
		s.mu.Unlock()
		if changed {
			s.emit(ev)
		}
	case "status":
		if l.PermissionMode != "" {
			s.mu.Lock()
			s.mode = l.PermissionMode
			s.mu.Unlock()
		}
	case "api_retry":
		msg := "claude is retrying the request"
		if l.ErrorStatus != nil {
			msg = fmt.Sprintf("claude is retrying the request (HTTP %d, attempt %d)", *l.ErrorStatus, l.Attempt)
		}
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventStatus, TurnID: s.turnID(), Status: protocol.ChatStatusWorking, Text: msg})
	}
}

func (s *claudeSession) handleStreamEvent(l *claudeLine) {
	if l.ParentToolUseID != nil && *l.ParentToolUseID != "" {
		return // sub-agent chatter; its tool calls still show via assistant/user lines
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	}
	if unmarshal(l.Event, &ev) != nil {
		return
	}
	s.mu.Lock()
	t := s.turn
	if t == nil {
		s.mu.Unlock()
		return
	}
	if ev.Type == "message_start" {
		t.curMsgID = ev.Message.ID
	}
	tid, mid := t.id, t.curMsgID
	s.mu.Unlock()
	if ev.Type != "content_block_delta" {
		return
	}
	switch ev.Delta.Type {
	case "text_delta":
		if ev.Delta.Text != "" {
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, TurnID: tid, MessageID: mid, Text: ev.Delta.Text})
		}
	case "thinking_delta":
		if ev.Delta.Thinking != "" {
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventThoughtDelta, TurnID: tid, MessageID: mid, Text: ev.Delta.Thinking})
		}
	}
}

func (s *claudeSession) handleAssistant(l *claudeLine) {
	var m claudeMessage
	if unmarshal(l.Message, &m) != nil {
		return
	}
	sub := l.ParentToolUseID != nil && *l.ParentToolUseID != ""
	var events []protocol.AgentChatEvent
	s.mu.Lock()
	t := s.turn
	if t == nil {
		s.mu.Unlock()
		return
	}
	textAdded := false
	for _, b := range m.blocks() {
		switch b.Type {
		case "text":
			if sub || b.Text == "" {
				continue
			}
			if l.IsAPIErrorMessage {
				t.lastText = b.Text // reported by the result as an error, not as a reply
				continue
			}
			t.msgText[m.ID] += b.Text
			t.lastText = t.msgText[m.ID]
			textAdded = true
		case "tool_use":
			if b.Name == "TodoWrite" {
				if plan := claudePlan(b.Input); plan != nil {
					events = append(events, protocol.AgentChatEvent{Kind: protocol.EventPlan, TurnID: t.id, Plan: plan})
				}
			}
			tc := claudeToolCall(b.ID, b.Name, b.Input)
			tc.Status = "running"
			t.tools[b.ID] = tc
			cp := *tc
			events = append(events, protocol.AgentChatEvent{Kind: protocol.EventToolCall, TurnID: t.id, ToolCall: &cp})
		}
	}
	if textAdded {
		events = append([]protocol.AgentChatEvent{{Kind: protocol.EventMessage, TurnID: t.id, MessageID: m.ID, Text: t.msgText[m.ID]}}, events...)
	}
	s.mu.Unlock()
	for _, ev := range events {
		s.emit(ev)
	}
}

func (s *claudeSession) handleUser(l *claudeLine) {
	var m claudeMessage
	if unmarshal(l.Message, &m) != nil {
		return
	}
	var events []protocol.AgentChatEvent
	s.mu.Lock()
	t := s.turn
	if t == nil {
		s.mu.Unlock()
		return
	}
	for _, b := range m.blocks() {
		if b.Type != "tool_result" {
			continue
		}
		tc := t.tools[b.ToolUseID]
		if tc == nil {
			tc = &protocol.AgentToolCall{ID: b.ToolUseID, Name: "tool"}
		}
		tc.Status = "completed"
		if b.IsError {
			tc.Status = "failed"
		}
		tc.Output = ccTruncate(ccRedact(claudeToolResultText(b.Content), s.acct.Env), claudeOutputMax)
		delete(t.tools, b.ToolUseID)
		cp := *tc
		events = append(events, protocol.AgentChatEvent{Kind: protocol.EventToolCall, TurnID: t.id, ToolCall: &cp})
	}
	s.mu.Unlock()
	for _, ev := range events {
		s.emit(ev)
	}
}

// claudeToolResultText flattens a tool_result content (string or content blocks).
func claudeToolResultText(raw jsontext.Value) string {
	var str string
	if unmarshal(raw, &str) == nil {
		return str
	}
	var bs []claudeContentBlock
	if unmarshal(raw, &bs) == nil {
		var parts []string
		for _, b := range bs {
			switch b.Type {
			case "text":
				parts = append(parts, b.Text)
			case "image":
				parts = append(parts, "[image]")
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// claudeFailingTerminal lists result terminal_reason values that mean the turn failed.
var claudeFailingTerminal = map[string]bool{
	"blocking_limit": true, "rapid_refill_breaker": true, "prompt_too_long": true,
	"image_error": true, "model_error": true, "api_error": true,
	"malformed_tool_use_exhausted": true, "budget_exhausted": true,
	"structured_output_retry_exhausted": true, "tool_deferred_unavailable": true,
	"turn_setup_failed": true,
}

func (s *claudeSession) handleResult(l *claudeLine) {
	s.mu.Lock()
	t := s.turn
	s.turn = nil
	s.hasTranscript = true
	for id, p := range s.pending { // a finished turn cannot answer prompts any more
		_ = p
		delete(s.pending, id)
	}
	rejected := s.lastRejected
	s.mu.Unlock()

	tid := ""
	cancelled, errored := false, false
	lastText := ""
	if t != nil {
		tid, cancelled, errored, lastText = t.id, t.cancelled, t.errored, t.lastText
	}
	if l.Usage != nil {
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventUsage, TurnID: tid, Usage: &protocol.AgentTokenUsage{
			InputTokens:       l.Usage.InputTokens,
			OutputTokens:      l.Usage.OutputTokens,
			CacheReadTokens:   l.Usage.CacheReadTokens,
			CacheCreateTokens: l.Usage.CacheCreateTokens,
			CostUSD:           l.TotalCostUSD,
		}})
	}
	if cancelled || l.TerminalReason == "aborted_streaming" || l.TerminalReason == "aborted_tools" {
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: tid, StopReason: "cancelled"})
		return
	}
	failed := l.IsError || (l.Subtype != "" && l.Subtype != "success") || claudeFailingTerminal[l.TerminalReason]
	if !failed {
		stop := "end_turn"
		if l.StopReason != nil && *l.StopReason != "" {
			stop = *l.StopReason
		}
		if l.TerminalReason == "max_turns" {
			stop = "max_turns"
		}
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: tid, StopReason: stop})
		return
	}
	if errored {
		return // already reported (e.g. a rejected rate_limit_event)
	}
	s.emit(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: tid, Error: s.resultError(l, lastText, rejected)})
}

// resultError classifies a failed result.
func (s *claudeSession) resultError(l *claudeLine, lastText string, rejected *protocol.AgentRateLimit) *protocol.AgentChatError {
	text := strings.TrimSpace(strings.Join(append([]string{l.Result}, l.Errors...), "\n"))
	if text == "" {
		text = lastText
	}
	status := 0
	if l.APIErrorStatus != nil {
		status = *l.APIErrorStatus
	}
	kind, retry := claudeClassify(text+" "+l.TerminalReason+" "+l.APIError, status)
	if l.TerminalReason == "blocking_limit" && kind == protocol.ErrKindOther {
		kind = protocol.ErrKindUsageLimit
	}
	msg := ccTruncate(ccRedact(text, s.acct.Env), 1000)
	if msg == "" {
		msg = "claude reported an error"
		if l.TerminalReason != "" {
			msg += " (" + l.TerminalReason + ")"
		}
	}
	cerr := &protocol.AgentChatError{Kind: kind, Message: msg, Retryable: retry}
	switch kind {
	case protocol.ErrKindAuth:
		cerr.Relogin = claudeRelogin(s.acct)
	case protocol.ErrKindUsageLimit:
		if rejected != nil {
			cerr.Window, cerr.ResetsAt = rejected.Window, rejected.ResetsAt
		} else if ts := claudeParseResetEpoch(text); ts != nil {
			cerr.ResetsAt = ts
		}
		detail := cerr.Message
		cerr.Message = claudeUsageMessage(claudeLabel(s.acct), cerr.Window, cerr.ResetsAt)
		if detail != "" {
			cerr.Message += " (claude: " + detail + ")"
		}
	}
	return cerr
}

func (s *claudeSession) handleRateLimit(l *claudeLine) {
	info := l.RateLimitInfo
	if info == nil {
		return
	}
	limits := claudeRateLimits(info)
	s.eng.recordStreamLimits(s.acct.ID, limits)
	tid := s.turnID()
	for i := range limits {
		rl := limits[i]
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventRateLimit, TurnID: tid, RateLimit: &rl})
	}
	if info.Status != "rejected" || info.IsUsingOverage {
		return
	}
	window := info.RateLimitType
	var resets *time.Time
	if info.ResetsAt > 0 {
		ts := time.Unix(int64(info.ResetsAt), 0).UTC()
		resets = &ts
	}
	s.mu.Lock()
	s.lastRejected = &protocol.AgentRateLimit{Window: window, ResetsAt: resets, State: "exhausted"}
	t := s.turn
	report := t != nil && !t.errored
	if report {
		t.errored = true
	}
	s.mu.Unlock()
	if !report {
		return
	}
	msg := claudeUsageMessage(claudeLabel(s.acct), window, resets)
	s.emit(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: tid, Error: &protocol.AgentChatError{
		Kind: protocol.ErrKindUsageLimit, Message: msg, Window: window, ResetsAt: resets,
	}})
}

// claudeRateLimits converts a rate_limit_event into windows.
func claudeRateLimits(info *claudeRateLimitInfo) []protocol.AgentRateLimit {
	var out []protocol.AgentRateLimit
	conv := func(window string, util *float64, resetsAt float64, status string) protocol.AgentRateLimit {
		rl := protocol.AgentRateLimit{Window: window, State: claudeLimitState(status)}
		if util != nil {
			pct := *util
			if pct <= 1 {
				pct *= 100
			}
			rl.UsedPercent = pct
		}
		if resetsAt > 0 {
			ts := time.Unix(int64(resetsAt), 0).UTC()
			rl.ResetsAt = &ts
		}
		return rl
	}
	if len(info.UnifiedWindows) > 0 {
		names := make([]string, 0, len(info.UnifiedWindows))
		for n := range info.UnifiedWindows {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			w := info.UnifiedWindows[n]
			st := w.Status
			if st == "" && n == info.RateLimitType {
				st = info.Status
			}
			out = append(out, conv(n, w.Utilization, w.ResetsAt, st))
		}
		return out
	}
	if info.RateLimitType != "" {
		out = append(out, conv(info.RateLimitType, info.Utilization, info.ResetsAt, info.Status))
	}
	return out
}

func claudeLimitState(status string) string {
	switch status {
	case "allowed":
		return "ok"
	case "allowed_warning":
		return "warning"
	case "rejected":
		return "exhausted"
	}
	return ""
}

// claudeWindowLabel names a usage window for messages: "weekly", "5-hour", ...
func claudeWindowLabel(w string) string {
	switch w {
	case "five_hour":
		return "5-hour"
	case "seven_day":
		return "weekly"
	case "seven_day_opus":
		return "weekly Opus"
	case "seven_day_sonnet":
		return "weekly Sonnet"
	case "":
		return ""
	}
	return strings.ReplaceAll(w, "_", " ")
}

// claudeUsageMessage is the actionable usage_limit text, e.g.
// "Claude 2 is out of weekly usage until 2026-11-26 23:59 UTC".
func claudeUsageMessage(label, window string, resets *time.Time) string {
	msg := label + " is out of usage"
	if w := claudeWindowLabel(window); w != "" {
		msg = label + " is out of " + w + " usage"
	}
	if resets != nil {
		msg += " until " + resets.UTC().Format("2006-01-02 15:04 MST")
	}
	return msg + "; wait for the reset or use another account."
}

func (s *claudeSession) handleControlRequest(l *claudeLine) {
	var req struct {
		Subtype       string         `json:"subtype"`
		ToolName      string         `json:"tool_name"`
		DisplayName   string         `json:"display_name"`
		Title         string         `json:"title"`
		Description   string         `json:"description"`
		Input         jsontext.Value `json:"input"`
		Suggestions   jsontext.Value `json:"permission_suggestions"`
		BlockedPath   string         `json:"blocked_path"`
		DecisionRsn   string         `json:"decision_reason"`
		ToolUseID     string         `json:"tool_use_id"`
		ActionSummary string         `json:"action_description"`
	}
	if unmarshal(l.Request, &req) != nil {
		return
	}
	if req.Subtype != "can_use_tool" {
		_ = s.write(map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype": "error", "request_id": l.RequestID,
				"error": "konsert does not support control request " + req.Subtype,
			},
		})
		return
	}
	tc := claudeToolCall(req.ToolUseID, req.ToolName, req.Input)
	tc.Status = "pending"
	p := &claudePending{id: l.RequestID, kind: protocol.ElicitPermission, toolName: req.ToolName, input: req.Input, suggestions: req.Suggestions}
	el := &protocol.AgentElicitation{ID: l.RequestID, Kind: protocol.ElicitPermission, ToolCall: tc}
	if req.ToolName == "AskUserQuestion" {
		if qs := claudeQuestions(req.Input); len(qs) > 0 {
			p.kind = protocol.ElicitQuestion
			p.questions = qs
			el.Kind = protocol.ElicitQuestion
			el.ToolCall = nil
			el.Title = qs[0].Question
			el.AllowText = true
			for i, o := range qs[0].Options {
				el.Options = append(el.Options, protocol.AgentElicitationOption{ID: fmt.Sprintf("opt_%d", i), Label: o.Label, Kind: "choice"})
			}
			if len(qs) > 1 {
				var more []string
				for _, q := range qs[1:] {
					more = append(more, q.Question)
				}
				el.Detail = "Also asked: " + strings.Join(more, " / ")
			}
		}
	}
	if el.Kind == protocol.ElicitPermission {
		name := req.DisplayName
		if name == "" {
			name = req.ToolName
		}
		el.Title = "Allow " + name + "?"
		if tc.Title != "" {
			el.Title = "Allow " + tc.Title + "?"
		}
		var detail []string
		for _, d := range []string{req.Title, req.Description, req.ActionSummary, req.DecisionRsn} {
			if d != "" && d != tc.Title {
				detail = append(detail, d)
			}
		}
		if req.BlockedPath != "" {
			detail = append(detail, "Path: "+req.BlockedPath)
		}
		el.Detail = ccTruncate(strings.Join(detail, "\n"), 1000)
		el.Options = []protocol.AgentElicitationOption{
			{ID: "allow_once", Label: "Allow once", Kind: "allow_once"},
			{ID: "allow_always", Label: "Always allow", Kind: "allow_always"},
			{ID: "reject", Label: "Reject", Kind: "reject_once"},
		}
	}
	s.mu.Lock()
	s.pending[l.RequestID] = p
	tid := ""
	if s.turn != nil {
		tid = s.turn.id
	}
	s.mu.Unlock()
	s.emit(protocol.AgentChatEvent{Kind: protocol.EventElicitation, TurnID: tid, Elicitation: el})
}

type claudeQuestion struct {
	Question string `json:"question"`
	Header   string `json:"header"`
	Options  []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

func claudeQuestions(input jsontext.Value) []claudeQuestion {
	var in struct {
		Questions []claudeQuestion `json:"questions"`
	}
	_ = unmarshal(input, &in)
	return in.Questions
}

func (s *claudeSession) handleExit(gen, code int, stderr string) {
	s.mu.Lock()
	if gen != s.procGen {
		s.mu.Unlock()
		return // a process we replaced or killed on purpose
	}
	s.proc = nil
	t := s.turn
	s.turn = nil
	s.pending = map[string]*claudePending{}
	closed := s.closed
	rejected := s.lastRejected
	s.mu.Unlock()
	s.failWaiters()
	if closed || t == nil {
		return // idle exit: the next Prompt restarts with --resume
	}
	if t.cancelled {
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: t.id, StopReason: "cancelled"})
		return
	}
	if t.errored {
		return
	}
	tail := ccRedact(ccTailLines(stderr, 20, 2000), s.acct.Env)
	kind, retry := claudeClassify(tail+"\n"+t.lastText, 0)
	cerr := &protocol.AgentChatError{Kind: kind, Message: fmt.Sprintf("claude exited with code %d", code), ExitCode: &code, Stderr: tail, Retryable: retry}
	switch kind {
	case protocol.ErrKindOther:
		cerr.Kind = protocol.ErrKindExit
		if first := strings.TrimSpace(ccTailLines(tail, 1, 300)); first != "" {
			cerr.Message += ": " + first
		}
	case protocol.ErrKindAuth:
		cerr.Relogin = claudeRelogin(s.acct)
	case protocol.ErrKindUsageLimit:
		if rejected != nil {
			cerr.Window, cerr.ResetsAt = rejected.Window, rejected.ResetsAt
		}
	}
	s.emit(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: t.id, Error: cerr})
}

// --- Classification ---------------------------------------------------------------------

// claudeClassify maps error text (and an HTTP status when known) to an error kind.
func claudeClassify(text string, status int) (kind string, retryable bool) {
	t := strings.ToLower(text)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(t, s) {
				return true
			}
		}
		return false
	}
	switch {
	case status == 401 || status == 403 || has("please run /login", "not logged in", "oauth token has expired",
		"invalid api key", "authentication_error", "401 unauthorized", "token has been revoked",
		"invalid bearer token", "oauth token is invalid", "run claude auth login", "invalid authentication", "status 401"):
		return protocol.ErrKindAuth, false
	case has("usage limit", "usage_limit", "limit reached", "hit your limit", "quota exceeded", "out of extra usage",
		"weekly limit", "5-hour limit", "session limit"):
		return protocol.ErrKindUsageLimit, false
	case status == 429 || status == 529 || status == 503 || has("rate limit", "rate_limit_error", "overloaded",
		"too many requests", "429", "529"):
		return protocol.ErrKindRateLimit, true
	case status == 404 || has("model not found", "unknown model", "invalid model", "model_not_found",
		"issue with the selected model", "not available for your account", "model: ") && has("model"):
		return protocol.ErrKindModel, false
	}
	return protocol.ErrKindOther, false
}

// claudeParseResetEpoch extracts a "…|<epoch>" reset time from the legacy
// "Claude AI usage limit reached|1700000000" text.
func claudeParseResetEpoch(text string) *time.Time {
	i := strings.LastIndexByte(text, '|')
	if i < 0 {
		return nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(text[i+1:]), 10, 64)
	if err != nil || n < 1_000_000_000 {
		return nil
	}
	ts := time.Unix(n, 0).UTC()
	return &ts
}

// --- Tool calls --------------------------------------------------------------------------

func claudeToolCall(id, name string, input jsontext.Value) *protocol.AgentToolCall {
	var in map[string]any
	_ = unmarshal(input, &in)
	str := func(k string) string {
		if v, ok := in[k].(string); ok {
			return v
		}
		return ""
	}
	tc := &protocol.AgentToolCall{ID: id, Name: name, Kind: "other", Input: ccTruncate(string(input), claudeInputMax)}
	firstLine := func(s string) string {
		s = strings.TrimSpace(s)
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[:i] + " …"
		}
		return ccTruncate(s, 200)
	}
	path := str("file_path")
	if path == "" {
		path = str("notebook_path")
	}
	if path != "" {
		tc.Paths = []string{path}
	}
	switch name {
	case "Bash", "PowerShell":
		tc.Kind, tc.Title = "execute", "Run: "+firstLine(str("command"))
	case "Read":
		tc.Kind, tc.Title = "read", "Read: "+path
	case "Write":
		tc.Kind, tc.Title = "edit", "Write: "+path
	case "Edit", "MultiEdit", "NotebookEdit":
		tc.Kind, tc.Title = "edit", "Edit: "+path
	case "Glob", "Grep":
		tc.Kind, tc.Title = "search", "Search: "+firstLine(str("pattern"))
		if p := str("path"); p != "" {
			tc.Paths = []string{p}
		}
	case "WebFetch":
		tc.Kind, tc.Title = "fetch", "Fetch: "+str("url")
	case "WebSearch":
		tc.Kind, tc.Title = "fetch", "Search the web: "+firstLine(str("query"))
	case "Task", "Agent":
		tc.Kind, tc.Title = "think", "Agent: "+firstLine(str("description"))
	case "TodoWrite":
		tc.Kind, tc.Title = "think", "Update plan"
	default:
		if strings.HasPrefix(name, "mcp__") {
			parts := strings.SplitN(strings.TrimPrefix(name, "mcp__"), "__", 2)
			if len(parts) == 2 {
				tc.Title = parts[0] + ": " + parts[1]
			}
		}
		if tc.Title == "" {
			tc.Title = name
		}
	}
	return tc
}

func claudePlan(input jsontext.Value) []protocol.AgentPlanItem {
	var in struct {
		Todos []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"todos"`
	}
	if unmarshal(input, &in) != nil || len(in.Todos) == 0 {
		return nil
	}
	out := make([]protocol.AgentPlanItem, 0, len(in.Todos))
	for _, t := range in.Todos {
		out = append(out, protocol.AgentPlanItem{Text: t.Content, Status: t.Status})
	}
	return out
}

// --- Approval modes -------------------------------------------------------------------

// claudeModeID normalizes an approval mode id ("manual" is the CLI's flag spelling of the
// mode it reports as "default").
func claudeModeID(id string) string {
	switch id {
	case "", "manual", "ask":
		return "default"
	}
	return id
}

// claudeApprovalModes are the --permission-mode values of claude 2.1.x, risk-ordered.
// (The CLI also accepts dontAsk: deny anything not pre-approved; not offered in the UI.)
func claudeApprovalModes() []protocol.AgentApprovalMode {
	return []protocol.AgentApprovalMode{
		{ID: "plan", Label: "Plan", Description: "Read-only: Claude explores and proposes a plan, but edits nothing and runs no commands.", Risk: 0, Icon: "eye"},
		{ID: "default", Label: "Ask", Description: "Claude asks before editing files or running commands (unless your Claude settings already allow them).", Risk: 1, Icon: "hand"},
		{ID: "acceptEdits", Label: "Accept edits", Description: "File edits in the workspace are applied without asking; commands still ask.", Risk: 2, Icon: "edit"},
		{ID: "auto", Label: "Auto", Description: "A safety classifier approves routine actions and asks only for risky ones.", Risk: 3, Icon: "shield-check"},
		{ID: "bypassPermissions", Label: "Full access", Description: "Never asks: every tool call runs without approval. Use only in a sandbox.", Risk: 4, Icon: "shield-alert"},
	}
}

// claudeScratchName is the project directory name claude uses for a cwd (exported for
// tests that clean up transcripts).
// claudeTranscriptExists reports whether the account has a stored conversation
// sessionID for cwd (claude keeps them in <config dir>/projects/<encoded cwd>/<id>.jsonl).
// When the location can't be determined it assumes the transcript exists (resume as before).
func claudeTranscriptExists(acct Account, cwd, sessionID string) bool {
	if cwd == "" || sessionID == "" {
		return true
	}
	cfg := acct.Env["CLAUDE_CONFIG_DIR"]
	if cfg == "" {
		cfg = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	if cfg == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return true
		}
		cfg = filepath.Join(home, ".claude")
	}
	_, err := os.Stat(filepath.Join(cfg, "projects", claudeProjectDirName(cwd), sessionID+".jsonl"))
	return err == nil
}

func claudeProjectDirName(cwd string) string {
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r
	}
	var b strings.Builder
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
