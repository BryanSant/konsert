package chat

// codexEngine drives `codex app-server` (JSON-RPC over stdio): one process per chat,
// a thread per chat (thread/start or thread/resume) and turn/start per user message with
// per-turn model / effort / approvalPolicy / sandboxPolicy. Approval requests arrive as
// server->client requests (item/commandExecution/requestApproval, ...).
//
// Login is checked first (`codex login status`, then account/read): a logged-out codex
// otherwise retries a 401 websocket several times before failing.

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/digest"
	"github.com/BryanSant/syre/internal/worker/proctree"
)

const (
	codexInitTimeout = 60 * time.Second
	codexCallTimeout = 60 * time.Second
)

type codexEngine struct {
	launch      ccLauncher
	run         ccRunner
	now         func() time.Time
	watchdog    time.Duration
	initTimeout time.Duration
	cancelGrace time.Duration
	probeDir    string
	trace       func(dir string, line []byte)
}

// NewCodexEngine returns the engine for the codex CLI.
func NewCodexEngine() Engine { return newCodexEngine() }

func newCodexEngine() *codexEngine {
	return &codexEngine{
		launch:      ccExecLaunch,
		run:         ccExecRun,
		now:         time.Now,
		watchdog:    claudeDefaultWatchdog,
		initTimeout: codexInitTimeout,
		cancelGrace: claudeCancelGrace,
		probeDir:    os.TempDir(),
	}
}

func (e *codexEngine) Agent() string { return "codex" }

func (e *codexEngine) spec(acct Account, dir string, args ...string) ccSpec {
	bin := acct.Binary
	if bin == "" {
		bin = "codex"
	}
	return ccSpec{
		Binary: bin,
		Args:   append(append([]string(nil), acct.ArgsPrefix...), args...),
		Env:    ccBuildEnv(acct.Env, []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"}),
		Dir:    dir,
	}
}

func codexRelogin(acct Account) *protocol.AgentRelogin {
	return &protocol.AgentRelogin{
		Command: "codex login --device-auth",
		Env:     ccNonSecretEnv(acct.Env, "CODEX_HOME"),
		URL:     "https://developers.openai.com/codex/auth",
		Note:    "Sign in with your ChatGPT account; on a machine with a browser `codex login` works too.",
	}
}

func codexLabel(acct Account) string {
	if acct.Label != "" {
		return acct.Label
	}
	return "Codex"
}

// loginStatus runs `codex login status` (exit 1 = not logged in).
func (e *codexEngine) loginStatus(ctx context.Context, acct Account) (loggedIn bool, text string, err error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, code, err := e.run(cctx, e.spec(acct, e.probeDir, "login", "status"))
	if err != nil {
		return false, "", err
	}
	return code == 0, strings.TrimSpace(string(out)), nil
}

type codexAccountRead struct {
	Account *struct {
		Type          string `json:"type"`
		Email         string `json:"email"`
		PlanType      string `json:"planType"`
		WorkspaceName string `json:"workspaceName"`
	} `json:"account"`
	RequiresOpenaiAuth bool `json:"requiresOpenaiAuth"`
}

// --- approval modes -----------------------------------------------------------------------

func codexApprovalModes() []protocol.AgentApprovalMode {
	return []protocol.AgentApprovalMode{
		{ID: "read-only", Label: "Read-only", Description: "Codex can read files and answer questions; edits and commands need your approval (read-only sandbox).", Risk: 0, Icon: "eye"},
		{ID: "untrusted", Label: "Ask", Description: "Codex asks before running anything except trusted read-only commands (workspace-write sandbox).", Risk: 1, Icon: "hand"},
		{ID: "on-request", Label: "On request", Description: "Codex works in the workspace sandbox and asks only when it needs to go beyond it.", Risk: 2, Icon: "edit"},
		{ID: "full-auto", Label: "Full auto", Description: "Workspace-write sandbox, never asks: failures inside the sandbox are reported, not escalated.", Risk: 3, Icon: "shield-check"},
		{ID: "full-access", Label: "Full access", Description: "No sandbox and no approvals (danger-full-access). Use only on a disposable machine.", Risk: 4, Icon: "shield-alert"},
	}
}

// codexMode maps an approval mode id to codex's -a / -s values.
func codexMode(id string) (approval, sandbox string) {
	switch id {
	case "read-only":
		return "on-request", "read-only"
	case "untrusted":
		return "untrusted", "workspace-write"
	case "full-auto":
		return "never", "workspace-write"
	case "full-access":
		return "never", "danger-full-access"
	default: // on-request
		return "on-request", "workspace-write"
	}
}

func codexModeID(id string) string {
	for _, m := range codexApprovalModes() {
		if m.ID == id {
			return id
		}
	}
	return "on-request"
}

// codexSandboxPolicy is the turn/start sandboxPolicy for a sandbox mode.
func codexSandboxPolicy(sandbox, cwd string) map[string]any {
	switch sandbox {
	case "read-only":
		return map[string]any{"type": "readOnly", "networkAccess": false}
	case "danger-full-access":
		return map[string]any{"type": "dangerFullAccess"}
	}
	roots := []string{}
	if cwd != "" {
		roots = append(roots, cwd)
	}
	return map[string]any{"type": "workspaceWrite", "writableRoots": roots, "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
}

// --- models / rate limits -----------------------------------------------------------------

type codexModel struct {
	ID                        string `json:"id"`
	Model                     string `json:"model"`
	DisplayName               string `json:"displayName"`
	Description               string `json:"description"`
	Hidden                    bool   `json:"hidden"`
	IsDefault                 bool   `json:"isDefault"`
	DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
	SupportedReasoningEfforts []struct {
		ReasoningEffort string `json:"reasoningEffort"`
		Description     string `json:"description"`
	} `json:"supportedReasoningEfforts"`
}

func codexListModels(ctx context.Context, c *codexConn) ([]protocol.AgentModel, string, error) {
	var out []protocol.AgentModel
	def := ""
	var cursor *string
	for page := 0; page < 10; page++ {
		params := map[string]any{}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		raw, err := c.call(ctx, "model/list", params)
		if err != nil {
			return out, def, err
		}
		var r struct {
			Data       []codexModel `json:"data"`
			NextCursor *string      `json:"nextCursor"`
		}
		if err := unmarshal(raw, &r); err != nil {
			return out, def, err
		}
		for _, m := range r.Data {
			if m.Hidden {
				continue
			}
			am := protocol.AgentModel{ID: m.ID, DisplayName: m.DisplayName, Description: m.Description, IsDefault: m.IsDefault, DefaultEffort: m.DefaultReasoningEffort}
			if am.DisplayName == "" {
				am.DisplayName = m.ID
			}
			for _, eff := range m.SupportedReasoningEfforts {
				label, short := ccEffortLabel(eff.ReasoningEffort)
				am.Efforts = append(am.Efforts, protocol.AgentEffort{ID: eff.ReasoningEffort, Label: label, Short: short})
			}
			if m.IsDefault {
				def = m.ID
			}
			out = append(out, am)
		}
		if r.NextCursor == nil || *r.NextCursor == "" {
			break
		}
		cursor = r.NextCursor
	}
	return out, def, nil
}

type codexRateWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins *int64  `json:"windowDurationMins"`
	ResetsAt           *int64  `json:"resetsAt"`
}

type codexRateSnapshot struct {
	Primary              *codexRateWindow `json:"primary"`
	Secondary            *codexRateWindow `json:"secondary"`
	PlanType             string           `json:"planType"`
	RateLimitReachedType *string          `json:"rateLimitReachedType"`
}

func codexWindowName(w *codexRateWindow, fallback string) string {
	if w.WindowDurationMins == nil {
		return fallback
	}
	switch m := *w.WindowDurationMins; {
	case m == 300:
		return "five_hour"
	case m == 10080:
		return "seven_day"
	case m == 1440:
		return "daily"
	case m >= 40000 && m <= 46000:
		return "monthly"
	case m%60 == 0:
		return fmt.Sprintf("%d_hour", m/60)
	default:
		return fmt.Sprintf("%d_min", m)
	}
}

// codexLimits converts a rate-limit snapshot into windows.
func codexLimits(s *codexRateSnapshot) []protocol.AgentRateLimit {
	if s == nil {
		return nil
	}
	var out []protocol.AgentRateLimit
	reached := s.RateLimitReachedType != nil && *s.RateLimitReachedType != ""
	for i, w := range []*codexRateWindow{s.Primary, s.Secondary} {
		if w == nil {
			continue
		}
		rl := protocol.AgentRateLimit{Window: codexWindowName(w, []string{"primary", "secondary"}[i]), UsedPercent: w.UsedPercent, State: claudePercentState(w.UsedPercent)}
		if w.ResetsAt != nil && *w.ResetsAt > 0 {
			ts := time.Unix(*w.ResetsAt, 0).UTC()
			rl.ResetsAt = &ts
		}
		out = append(out, rl)
	}
	if reached {
		marked := false
		for i := range out {
			if out[i].UsedPercent >= 100 {
				out[i].State, marked = "exhausted", true
			}
		}
		if !marked && len(out) > 0 {
			// The backend says the limit is reached without a full window: mark the
			// fullest one.
			best := 0
			for i := range out {
				if out[i].UsedPercent > out[best].UsedPercent {
					best = i
				}
			}
			out[best].State = "exhausted"
		}
	}
	return out
}

// --- Probe --------------------------------------------------------------------------------

func (e *codexEngine) Probe(ctx context.Context, acct Account) protocol.AgentAccount {
	out := protocol.AgentAccount{
		ID: acct.ID, Agent: "codex", Label: codexLabel(acct), IsDefault: acct.IsDefault,
		Approvals: codexApprovalModes(), DefaultApproval: "on-request", ChatSupported: true, Fork: true,
		CheckedAt: e.now().UTC(),
	}
	if v, code, err := e.run(ctx, e.spec(acct, e.probeDir, "--version")); err != nil {
		out.ChatSupported = false
		out.Auth = protocol.AgentAuthStatus{State: protocol.AuthUnknown, Detail: "codex is not installed or not runnable: " + err.Error()}
		return out
	} else if code == 0 {
		out.Version = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(v)), "codex-cli"))
	}
	loggedIn, text, err := e.loginStatus(ctx, acct)
	switch {
	case err != nil:
		out.Auth = protocol.AgentAuthStatus{State: protocol.AuthUnknown, Detail: "codex login status failed: " + err.Error()}
	case !loggedIn:
		out.Auth = protocol.AgentAuthStatus{State: protocol.AuthLoggedOut, Detail: "Not signed in.", Relogin: codexRelogin(acct)}
	default:
		out.Auth = protocol.AgentAuthStatus{State: protocol.AuthOK, Detail: text}
		if strings.Contains(strings.ToLower(text), "api key") {
			out.Auth.Method = "api_key"
		} else {
			out.Auth.Method = "oauth"
		}
	}

	cctx, cancel := context.WithTimeout(ctx, e.initTimeout)
	defer cancel()
	proc, err := e.launch(cctx, e.spec(acct, e.probeDir, "app-server"))
	if err != nil {
		out.ModelsSource = "fallback"
		out.UsageNote = "codex app-server did not start."
		return out
	}
	defer proc.Kill()
	conn := newCodexConn(proc)
	conn.trace = e.trace
	conn.onRequest = func(id jsontext.Value, _ string, _ jsontext.Value) {
		_ = conn.respondError(id, -32601, "not supported while probing")
	}
	go conn.serve()
	if err := codexHandshake(cctx, conn); err != nil {
		out.ModelsSource = "fallback"
		out.UsageNote = "codex app-server did not answer."
		return out
	}
	if raw, err := conn.call(cctx, "account/read", map[string]any{"refreshToken": false}); err == nil {
		var ar codexAccountRead
		if unmarshal(raw, &ar) == nil {
			if ar.Account == nil && ar.RequiresOpenaiAuth {
				out.Auth = protocol.AgentAuthStatus{State: protocol.AuthLoggedOut, Detail: "Not signed in.", Relogin: codexRelogin(acct)}
			} else if ar.Account != nil {
				out.Plan = ar.Account.PlanType
				out.Auth.Email = ar.Account.Email
				switch ar.Account.Type {
				case "chatgpt":
					out.Auth.Method = "oauth"
				case "apiKey":
					out.Auth.Method = "api_key"
				}
			}
		}
	}
	if models, def, err := codexListModels(cctx, conn); err == nil && len(models) > 0 {
		out.Models, out.DefaultModel, out.ModelsSource = models, def, "discovered"
	} else {
		out.ModelsSource = "fallback"
	}
	if out.Auth.State != protocol.AuthOK {
		out.UsageNote = "Sign in to see usage."
		return out
	}
	raw, err := conn.call(cctx, "account/rateLimits/read", nil)
	if err != nil {
		out.UsageNote = "Usage unavailable: " + err.Error()
		return out
	}
	var rr struct {
		RateLimits *codexRateSnapshot `json:"rateLimits"`
	}
	if unmarshal(raw, &rr) == nil {
		out.Usage = codexLimits(rr.RateLimits)
		if out.Plan == "" && rr.RateLimits != nil {
			out.Plan = rr.RateLimits.PlanType
		}
	}
	if len(out.Usage) == 0 {
		out.UsageNote = "codex reported no usage windows."
	}
	return out
}

// --- Open / session -----------------------------------------------------------------------

func (e *codexEngine) authError(acct Account, msg string) *protocol.AgentChatError {
	return &protocol.AgentChatError{Kind: protocol.ErrKindAuth, Message: msg, Relogin: codexRelogin(acct)}
}

func (e *codexEngine) Open(ctx context.Context, acct Account, opts protocol.AgentChatOpen, emit Emit) (Session, error) {
	loggedIn, _, err := e.loginStatus(ctx, acct)
	if err != nil {
		cerr := &protocol.AgentChatError{Kind: protocol.ErrKindNotFound, Message: "codex is not installed or not runnable: " + err.Error()}
		emit(protocol.AgentChatEvent{Kind: protocol.EventError, Error: cerr})
		return nil, errors.New(cerr.Message)
	}
	if !loggedIn {
		cerr := e.authError(acct, codexLabel(acct)+" is not signed in")
		emit(protocol.AgentChatEvent{Kind: protocol.EventError, Error: cerr})
		return nil, errors.New(cerr.Message)
	}
	s := &codexSession{
		eng: e, acct: acct, opts: opts, emit: emit,
		model: opts.Model, effort: opts.Effort, mode: codexModeID(opts.ApprovalMode),
		threadID: opts.AgentSessionID,
		pending:  map[string]*codexPending{},
		stop:     make(chan struct{}),
	}
	if err := s.ensureProc(ctx); err != nil {
		var ce *codexStartError
		if errors.As(err, &ce) {
			emit(protocol.AgentChatEvent{Kind: protocol.EventError, Error: ce.cerr})
		} else {
			emit(protocol.AgentChatEvent{Kind: protocol.EventError, Error: &protocol.AgentChatError{Kind: protocol.ErrKindOther, Message: err.Error()}})
		}
		_ = s.Close()
		return nil, err
	}
	go s.watchdogLoop()
	s.mu.Lock()
	ready := protocol.AgentChatEvent{Kind: protocol.EventSessionReady, AgentSessionID: s.threadID, Model: s.reportedModel, Effort: s.reportedEffort, ApprovalMode: s.mode}
	s.mu.Unlock()
	emit(ready)
	return s, nil
}

type codexStartError struct{ cerr *protocol.AgentChatError }

func (e *codexStartError) Error() string { return e.cerr.Message }

type codexPending struct {
	elicID      string
	rpcID       jsontext.Value
	method      string
	kind        string
	permissions jsontext.Value
	questionIDs []string
	options     []string // option labels of the first question
}

type codexTurn struct {
	id        string // syre turn id
	codexID   string
	cancelled bool
	errored   bool
	usage     *protocol.AgentTokenUsage
	items     map[string]*protocol.AgentToolCall
}

type codexSession struct {
	eng  *codexEngine
	acct Account
	opts protocol.AgentChatOpen
	emit Emit

	lifeMu sync.Mutex

	mu             sync.Mutex
	conn           *codexConn
	proc           ccProcess
	procGen        int
	threadID       string
	model          string
	effort         string
	mode           string
	reportedModel  string
	reportedEffort string
	turn           *codexTurn
	pending        map[string]*codexPending
	lastLimits     []protocol.AgentRateLimit
	lastActivity   time.Time
	watchdogFired  bool
	suspended      bool // process tree frozen by the user; the watchdog does not run
	closed         bool
	retainedStderr string // stderr tail captured when the process is closed
	stop           chan struct{}
	stopOnce       sync.Once
}

func (s *codexSession) AgentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threadID
}

// StderrTail is the redacted engine stderr. A closed session returns the tail
// captured when the process exited.
func (s *codexSession) StderrTail(max int) string {
	s.mu.Lock()
	proc := s.proc
	saved := s.retainedStderr
	env := s.acct.Env
	s.mu.Unlock()
	tail := saved
	if proc != nil {
		if live := ccProcessStderr(proc, env); live != "" {
			tail = live
		}
	}
	if max > 0 && len(tail) > max {
		tail = tail[len(tail)-max:]
	}
	return tail
}

// ensureProc starts app-server, checks the account and starts/resumes the thread.
func (s *codexSession) ensureProc(ctx context.Context) error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("session closed")
	}
	if s.proc != nil {
		s.mu.Unlock()
		return nil
	}
	s.procGen++
	gen := s.procGen
	cwd := s.opts.Cwd
	s.mu.Unlock()

	spec := s.eng.spec(s.acct, cwd, "app-server")
	spec.Guard = chatGuard(s.opts)
	proc, err := s.eng.launch(ctx, spec)
	if err != nil {
		return &codexStartError{&protocol.AgentChatError{Kind: protocol.ErrKindNotFound, Message: "codex is not installed or not runnable: " + err.Error()}}
	}
	conn := newCodexConn(proc)
	conn.trace = s.eng.trace
	conn.onNotify = func(m string, p jsontext.Value) { s.onNotify(gen, m, p) }
	conn.onRequest = func(id jsontext.Value, m string, p jsontext.Value) { s.onRequest(gen, id, m, p) }
	conn.onActive = func() {
		s.mu.Lock()
		if gen == s.procGen {
			s.lastActivity = s.eng.now()
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.proc, s.conn = proc, conn
	s.lastActivity = s.eng.now()
	s.mu.Unlock()
	go func() {
		conn.serve()
		code, stderr := proc.Wait()
		s.handleExit(gen, code, stderr)
	}()

	fail := func(cerr *protocol.AgentChatError) error {
		s.mu.Lock()
		if s.procGen == gen {
			s.proc, s.conn = nil, nil
			s.procGen++
		}
		s.mu.Unlock()
		proc.Kill()
		return &codexStartError{cerr}
	}
	ictx, cancel := context.WithTimeout(ctx, s.eng.initTimeout)
	defer cancel()
	if err := codexHandshake(ictx, conn); err != nil {
		proc.Kill()
		code, stderr := proc.Wait()
		return fail(&protocol.AgentChatError{Kind: protocol.ErrKindExit, Message: "codex app-server failed to start: " + err.Error(), ExitCode: &code, Stderr: ccRedact(ccTailLines(stderr, 20, 2000), s.acct.Env)})
	}
	raw, err := conn.call(ictx, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return fail(&protocol.AgentChatError{Kind: protocol.ErrKindProtocol, Message: "codex account/read failed: " + err.Error()})
	}
	var ar codexAccountRead
	_ = unmarshal(raw, &ar)
	if ar.Account == nil && ar.RequiresOpenaiAuth {
		return fail(s.eng.authError(s.acct, codexLabel(s.acct)+" is not signed in"))
	}

	s.mu.Lock()
	approval, sandbox := codexMode(s.mode)
	params := map[string]any{"cwd": cwd, "approvalPolicy": approval, "sandbox": sandbox}
	if s.model != "" {
		params["model"] = s.model
	}
	if s.opts.SystemPrompt != "" {
		params["developerInstructions"] = s.opts.SystemPrompt
	}
	if len(s.opts.MCPServers) > 0 {
		params["config"] = map[string]any{"mcp_servers": codexMCPServers(s.opts.MCPServers)}
	}
	method := "thread/start"
	switch {
	case s.threadID != "":
		method = "thread/resume"
		params["threadId"] = s.threadID
		params["excludeTurns"] = true
	case s.opts.ForkFrom != "":
		// A side-conversation fork: a copy of the asking thread that runs on its own,
		// kept out of codex's saved sessions (ephemeral).
		method = "thread/fork"
		params["threadId"] = s.opts.ForkFrom
		params["ephemeral"] = true
		params["excludeTurns"] = true
	}
	s.mu.Unlock()
	raw, err = conn.call(ictx, method, params)
	if err != nil {
		kind := protocol.ErrKindProtocol
		if k, _ := codexClassifyText(err.Error()); k != protocol.ErrKindOther {
			kind = k
		}
		cerr := &protocol.AgentChatError{Kind: kind, Message: "codex " + method + " failed: " + ccRedact(err.Error(), s.acct.Env)}
		if kind == protocol.ErrKindAuth {
			cerr.Relogin = codexRelogin(s.acct)
		}
		return fail(cerr)
	}
	var tr struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Model           string  `json:"model"`
		ReasoningEffort *string `json:"reasoningEffort"`
	}
	_ = unmarshal(raw, &tr)
	s.mu.Lock()
	if tr.Thread.ID != "" {
		s.threadID = tr.Thread.ID
	}
	s.reportedModel = tr.Model
	if s.model != "" {
		s.reportedModel = s.model
	}
	s.reportedEffort = s.effort
	if s.reportedEffort == "" && tr.ReasoningEffort != nil {
		s.reportedEffort = *tr.ReasoningEffort
	}
	s.mu.Unlock()
	return nil
}

func codexMCPServers(servers []protocol.AgentMCPServer) map[string]any {
	m := map[string]any{}
	for _, srv := range servers {
		cfg := map[string]any{"url": srv.URL}
		if len(srv.Headers) > 0 {
			cfg["http_headers"] = srv.Headers
		}
		if srv.ToolTimeoutSeconds > 0 {
			// codex's MCP client gives up on a tool call after 60 s by default.
			cfg["tool_timeout_sec"] = srv.ToolTimeoutSeconds
		}
		m[srv.Name] = cfg
	}
	return m
}

func (s *codexSession) Prompt(ctx context.Context, p protocol.AgentChatPrompt) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("session closed")
	}
	if s.turn != nil {
		s.mu.Unlock()
		return errors.New("a turn is already running")
	}
	s.mu.Unlock()
	if err := s.ensureProc(ctx); err != nil {
		var ce *codexStartError
		if errors.As(err, &ce) {
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: p.TurnID, Error: ce.cerr})
		}
		return err
	}
	input := []map[string]any{{"type": "text", "text": p.Text, "text_elements": []any{}}}
	for _, a := range p.Attachments {
		switch strings.ToLower(fileExt(a)) {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp":
			input = append(input, map[string]any{"type": "localImage", "path": a})
		default:
			input = append(input, map[string]any{"type": "mention", "name": fileBase(a), "path": a})
		}
	}
	s.mu.Lock()
	if s.turn != nil {
		s.mu.Unlock()
		return errors.New("a turn is already running")
	}
	t := &codexTurn{id: p.TurnID, items: map[string]*protocol.AgentToolCall{}}
	s.turn = t
	s.lastActivity = s.eng.now()
	s.watchdogFired = false
	approval, sandbox := codexMode(s.mode)
	params := map[string]any{
		"threadId":       s.threadID,
		"input":          input,
		"approvalPolicy": approval,
		"sandboxPolicy":  codexSandboxPolicy(sandbox, s.opts.Cwd),
	}
	if s.model != "" {
		params["model"] = s.model
	}
	if s.effort != "" {
		params["effort"] = s.effort
	}
	conn := s.conn
	s.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, codexCallTimeout)
	defer cancel()
	raw, err := conn.call(cctx, "turn/start", params)
	if err != nil {
		s.mu.Lock()
		if s.turn == t {
			s.turn = nil
		}
		s.mu.Unlock()
		kind, retry := codexClassifyText(err.Error())
		if kind == protocol.ErrKindOther {
			kind = protocol.ErrKindProtocol
		}
		cerr := &protocol.AgentChatError{Kind: kind, Message: "codex turn/start failed: " + ccRedact(err.Error(), s.acct.Env), Retryable: retry}
		if kind == protocol.ErrKindAuth {
			cerr.Relogin = codexRelogin(s.acct)
		}
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: p.TurnID, Error: cerr})
		return err
	}
	var r struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = unmarshal(raw, &r)
	s.mu.Lock()
	if s.turn == t && t.codexID == "" {
		t.codexID = r.Turn.ID
	}
	s.mu.Unlock()
	return nil
}

func fileExt(p string) string {
	if i := strings.LastIndexByte(p, '.'); i >= 0 && !strings.ContainsAny(p[i:], `/\`) {
		return p[i:]
	}
	return ""
}

func fileBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (s *codexSession) Respond(_ context.Context, r protocol.AgentChatRespond) error {
	s.mu.Lock()
	p := s.pending[r.ElicitationID]
	delete(s.pending, r.ElicitationID)
	conn := s.conn
	tid := ""
	if s.turn != nil {
		tid = s.turn.id
	}
	s.mu.Unlock()
	if p == nil {
		return fmt.Errorf("no pending elicitation %q", r.ElicitationID)
	}
	if conn == nil {
		return errors.New("codex is not running")
	}
	if err := conn.respond(p.rpcID, codexAnswer(p, r)); err != nil {
		return err
	}
	s.emit(protocol.AgentChatEvent{Kind: protocol.EventElicitationOK, TurnID: tid, Text: r.OptionID, Elicitation: &protocol.AgentElicitation{ID: p.elicID, Kind: p.kind}})
	return nil
}

// codexAnswer builds the JSON-RPC result for the user's choice.
func codexAnswer(p *codexPending, r protocol.AgentChatRespond) any {
	switch p.method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		switch r.OptionID {
		case "allow_once", "allow":
			return map[string]any{"decision": "accept"}
		case "allow_always":
			return map[string]any{"decision": "acceptForSession"}
		case "cancel":
			return map[string]any{"decision": "cancel"}
		default:
			return map[string]any{"decision": "decline"}
		}
	case "item/permissions/requestApproval":
		perms := jsontext.Value(`{}`)
		scope := "turn"
		switch r.OptionID {
		case "allow_once", "allow":
			perms = codexGranted(p.permissions)
		case "allow_always":
			perms, scope = codexGranted(p.permissions), "session"
		}
		return map[string]any{"permissions": perms, "scope": scope}
	case "item/tool/requestUserInput":
		answer := strings.TrimSpace(r.Text)
		if answer == "" {
			for i, l := range p.options {
				if r.OptionID == fmt.Sprintf("opt_%d", i) {
					answer = l
				}
			}
		}
		answers := map[string]any{}
		for _, id := range p.questionIDs {
			answers[id] = map[string]any{"answers": []string{answer}}
		}
		return map[string]any{"answers": answers}
	}
	return map[string]any{}
}

// codexGranted turns a requested permission profile into a granted one (drops nulls).
func codexGranted(requested jsontext.Value) jsontext.Value {
	var m map[string]jsontext.Value
	if unmarshal(requested, &m) != nil {
		return jsontext.Value(`{}`)
	}
	out := map[string]jsontext.Value{}
	for k, v := range m {
		if string(v) != "null" {
			out[k] = v
		}
	}
	b, _ := marshalLine(out)
	return b
}

func (s *codexSession) Control(ctx context.Context, c protocol.AgentChatControl) error {
	s.mu.Lock()
	if c.Model != "" {
		s.model = c.Model
	}
	if c.Effort != "" {
		s.effort = c.Effort
	}
	if c.ApprovalMode != "" {
		s.mode = codexModeID(c.ApprovalMode)
	}
	s.mu.Unlock()
	if c.Cancel {
		return s.cancel(ctx)
	}
	return nil
}

func (s *codexSession) cancel(ctx context.Context) error {
	s.mu.Lock()
	t := s.turn
	conn := s.conn
	if t == nil || conn == nil {
		s.mu.Unlock()
		return nil
	}
	t.cancelled = true
	pend := s.pending
	s.pending = map[string]*codexPending{}
	threadID, codexID, gen := s.threadID, t.codexID, s.procGen
	s.mu.Unlock()
	for _, p := range pend {
		_ = conn.respond(p.rpcID, codexAnswer(p, protocol.AgentChatRespond{OptionID: "cancel"}))
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventElicitationOK, TurnID: t.id, Text: "cancelled", Elicitation: &protocol.AgentElicitation{ID: p.elicID, Kind: p.kind}})
	}
	var err error
	if codexID != "" {
		cctx, cancel := context.WithTimeout(ctx, codexCallTimeout)
		_, err = conn.call(cctx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": codexID})
		cancel()
	}
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
			s.proc, s.conn = nil, nil
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
		return fmt.Errorf("interrupt codex: %w", err)
	}
	return nil
}

func (s *codexSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	proc := s.proc
	s.proc, s.conn = nil, nil
	s.procGen++
	env := s.acct.Env
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
		if tail := ccProcessStderr(proc, env); tail != "" {
			s.mu.Lock()
			s.retainedStderr = tail
			s.mu.Unlock()
		}
	}
	return nil
}

func (s *codexSession) watchdogLoop() {
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
		fire := s.turn != nil && !s.watchdogFired && !s.suspended && len(s.pending) == 0 && !syreToolRunning(s.turn.items) && s.eng.now().Sub(s.lastActivity) >= s.eng.watchdog
		tid := ""
		if fire {
			s.watchdogFired = true
			tid = s.turn.id
		}
		s.mu.Unlock()
		if fire {
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: tid, Error: &protocol.AgentChatError{
				Kind: protocol.ErrKindTimeout, Message: fmt.Sprintf("codex has made no progress for %s; cancel the turn or keep waiting", s.eng.watchdog.Round(time.Second)),
			}})
		}
	}
}

func (s *codexSession) handleExit(gen, code int, stderr string) {
	s.mu.Lock()
	if gen != s.procGen {
		s.mu.Unlock()
		return
	}
	s.proc, s.conn = nil, nil
	t := s.turn
	s.turn = nil
	s.pending = map[string]*codexPending{}
	closed := s.closed
	s.mu.Unlock()
	if closed || t == nil {
		return
	}
	if t.cancelled {
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: t.id, StopReason: "cancelled"})
		return
	}
	if t.errored {
		return
	}
	tail := ccRedact(ccTailLines(stderr, 20, 2000), s.acct.Env)
	kind, retry := codexClassifyText(tail)
	cerr := &protocol.AgentChatError{Kind: kind, Message: fmt.Sprintf("codex exited with code %d", code), ExitCode: &code, Stderr: tail, Retryable: retry}
	if kind == protocol.ErrKindOther {
		cerr.Kind = protocol.ErrKindExit
	}
	if cerr.Kind == protocol.ErrKindAuth {
		cerr.Relogin = codexRelogin(s.acct)
	}
	s.emit(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: t.id, Error: cerr})
}

// --- notifications ------------------------------------------------------------------------

type codexItem struct {
	Type             string         `json:"type"`
	ID               string         `json:"id"`
	Text             string         `json:"text"`
	Command          string         `json:"command"`
	Cwd              string         `json:"cwd"`
	Status           string         `json:"status"`
	AggregatedOutput *string        `json:"aggregatedOutput"`
	ExitCode         *int           `json:"exitCode"`
	Changes          []codexChange  `json:"changes"`
	Server           string         `json:"server"`
	Tool             string         `json:"tool"`
	Arguments        jsontext.Value `json:"arguments"`
	Result           jsontext.Value `json:"result"`
	Error            *struct {
		Message string `json:"message"`
	} `json:"error"`
	Query   string   `json:"query"`
	Path    string   `json:"path"`
	Summary []string `json:"summary"`
}

type codexChange struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
	Kind struct {
		Type string `json:"type"`
	} `json:"kind"`
}

type codexTurnError struct {
	Message           string         `json:"message"`
	CodexErrorInfo    jsontext.Value `json:"codexErrorInfo"`
	AdditionalDetails *string        `json:"additionalDetails"`
}

// current returns the active turn if the notification belongs to the live process.
func (s *codexSession) current(gen int) (*codexTurn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.procGen {
		return nil, false
	}
	return s.turn, true
}

func (s *codexSession) onNotify(gen int, method string, params jsontext.Value) {
	t, live := s.current(gen)
	if !live {
		return
	}
	switch method {
	case "account/rateLimits/updated":
		var n struct {
			RateLimits *codexRateSnapshot `json:"rateLimits"`
		}
		if unmarshal(params, &n) != nil {
			return
		}
		limits := codexLimits(n.RateLimits)
		s.mu.Lock()
		s.lastLimits = limits
		s.mu.Unlock()
		tid := ""
		if t != nil {
			tid = t.id
		}
		for i := range limits {
			rl := limits[i]
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventRateLimit, TurnID: tid, RateLimit: &rl})
		}
		return
	}
	if t == nil {
		return
	}
	switch method {
	case "turn/started":
		var n struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if unmarshal(params, &n) == nil {
			s.mu.Lock()
			if t.codexID == "" {
				t.codexID = n.Turn.ID
			}
			s.mu.Unlock()
		}
	case "item/agentMessage/delta":
		var n struct {
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if unmarshal(params, &n) == nil && n.Delta != "" {
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, TurnID: t.id, MessageID: n.ItemID, Text: n.Delta})
		}
	case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		var n struct {
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if unmarshal(params, &n) == nil && n.Delta != "" {
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventThoughtDelta, TurnID: t.id, MessageID: n.ItemID, Text: n.Delta})
		}
	case "item/started", "item/completed":
		var n struct {
			Item codexItem `json:"item"`
		}
		if unmarshal(params, &n) != nil {
			return
		}
		s.handleItem(t, n.Item, method == "item/completed")
	case "turn/plan/updated":
		var n struct {
			Plan []struct {
				Step   string `json:"step"`
				Status string `json:"status"`
			} `json:"plan"`
		}
		if unmarshal(params, &n) == nil && len(n.Plan) > 0 {
			plan := make([]protocol.AgentPlanItem, 0, len(n.Plan))
			for _, p := range n.Plan {
				st := p.Status
				if st == "inProgress" {
					st = "in_progress"
				}
				plan = append(plan, protocol.AgentPlanItem{Text: p.Step, Status: st})
			}
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventPlan, TurnID: t.id, Plan: plan})
		}
	case "thread/tokenUsage/updated":
		var n struct {
			TokenUsage struct {
				Last struct {
					InputTokens           int64 `json:"inputTokens"`
					CachedInputTokens     int64 `json:"cachedInputTokens"`
					CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
					OutputTokens          int64 `json:"outputTokens"`
					ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
				} `json:"last"`
			} `json:"tokenUsage"`
		}
		if unmarshal(params, &n) == nil {
			l := n.TokenUsage.Last
			s.mu.Lock()
			t.usage = &protocol.AgentTokenUsage{InputTokens: l.InputTokens, OutputTokens: l.OutputTokens + l.ReasoningOutputTokens, CacheReadTokens: l.CachedInputTokens, CacheCreateTokens: l.CacheWriteInputTokens}
			s.mu.Unlock()
		}
	case "error":
		var n struct {
			Error     codexTurnError `json:"error"`
			WillRetry bool           `json:"willRetry"`
		}
		if unmarshal(params, &n) != nil {
			return
		}
		cerr := s.turnError(n.Error)
		if n.WillRetry && cerr.Kind != protocol.ErrKindAuth {
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventStatus, TurnID: t.id, Status: protocol.ChatStatusWorking, Text: "codex is retrying: " + ccTruncate(cerr.Message, 200)})
			return
		}
		s.failTurn(t, cerr)
		if cerr.Kind == protocol.ErrKindAuth {
			// Never let codex's 401 reconnect loop run: stop the turn and the process.
			s.killProc(gen)
		}
	case "turn/completed":
		var n struct {
			Turn struct {
				ID     string          `json:"id"`
				Status string          `json:"status"`
				Error  *codexTurnError `json:"error"`
			} `json:"turn"`
		}
		if unmarshal(params, &n) != nil {
			return
		}
		s.mu.Lock()
		if s.turn != t {
			s.mu.Unlock()
			return
		}
		s.turn = nil
		s.pending = map[string]*codexPending{}
		usage, cancelled, errored := t.usage, t.cancelled, t.errored
		s.mu.Unlock()
		if usage != nil {
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventUsage, TurnID: t.id, Usage: usage})
		}
		switch {
		case n.Turn.Status == "interrupted" || cancelled:
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: t.id, StopReason: "cancelled"})
		case n.Turn.Status == "failed":
			if !errored {
				te := codexTurnError{Message: "codex turn failed"}
				if n.Turn.Error != nil {
					te = *n.Turn.Error
				}
				s.emit(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: t.id, Error: s.turnError(te)})
			}
		default:
			if !errored {
				s.emit(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: t.id, StopReason: "end_turn"})
			}
		}
	}
}

// failTurn reports a turn failure once.
func (s *codexSession) failTurn(t *codexTurn, cerr *protocol.AgentChatError) {
	s.mu.Lock()
	already := t.errored
	t.errored = true
	s.mu.Unlock()
	if !already {
		s.emit(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: t.id, Error: cerr})
	}
}

// killProc stops the live process; the next Prompt restarts and resumes the thread.
func (s *codexSession) killProc(gen int) {
	s.mu.Lock()
	if gen != s.procGen || s.proc == nil {
		s.mu.Unlock()
		return
	}
	proc := s.proc
	s.proc, s.conn = nil, nil
	s.procGen++
	s.turn = nil
	s.pending = map[string]*codexPending{}
	s.mu.Unlock()
	go proc.Kill()
}

// codexErrorInfo extracts the error code name and an HTTP status from codexErrorInfo,
// which is a string ("usageLimitExceeded") or a one-key object
// ({"httpConnectionFailed": {"httpStatusCode": 401}}).
func codexErrorInfo(raw jsontext.Value) (name string, status int) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", 0
	}
	if unmarshal(raw, &name) == nil {
		return name, 0
	}
	var obj map[string]struct {
		HTTPStatusCode *int `json:"httpStatusCode"`
	}
	if unmarshal(raw, &obj) == nil {
		for k, v := range obj {
			name = k
			if v.HTTPStatusCode != nil {
				status = *v.HTTPStatusCode
			}
		}
	}
	return name, status
}

// turnError classifies a codex TurnError.
func (s *codexSession) turnError(te codexTurnError) *protocol.AgentChatError {
	name, status := codexErrorInfo(te.CodexErrorInfo)
	text := te.Message
	if te.AdditionalDetails != nil {
		text += "\n" + *te.AdditionalDetails
	}
	msg := ccTruncate(ccRedact(text, s.acct.Env), 1000)
	cerr := &protocol.AgentChatError{Kind: protocol.ErrKindOther, Message: msg}
	switch {
	case name == "unauthorized" || status == 401:
		cerr.Kind = protocol.ErrKindAuth
	case name == "usageLimitExceeded" || name == "sessionBudgetExceeded":
		cerr.Kind = protocol.ErrKindUsageLimit
	case name == "rateLimitExceeded" || name == "serverOverloaded" || name == "policyCapacityUnavailable" || status == 429 || status == 503:
		cerr.Kind, cerr.Retryable = protocol.ErrKindRateLimit, true
	case name == "modelNotFound":
		cerr.Kind = protocol.ErrKindModel
	default:
		cerr.Kind, cerr.Retryable = codexClassifyText(text)
	}
	switch cerr.Kind {
	case protocol.ErrKindAuth:
		cerr.Relogin = codexRelogin(s.acct)
		cerr.Message = codexLabel(s.acct) + " needs you to sign in again (" + msg + ")"
	case protocol.ErrKindUsageLimit:
		s.mu.Lock()
		limits := s.lastLimits
		s.mu.Unlock()
		for _, l := range limits {
			if l.State == "exhausted" {
				cerr.Window, cerr.ResetsAt = l.Window, l.ResetsAt
				break
			}
		}
		m := codexLabel(s.acct) + " is out of usage"
		if w := claudeWindowLabel(cerr.Window); w != "" {
			m = codexLabel(s.acct) + " is out of " + w + " usage"
		}
		if cerr.ResetsAt != nil {
			m += " until " + cerr.ResetsAt.UTC().Format("2006-01-02 15:04 MST")
		}
		cerr.Message = m + " (" + msg + ")"
	}
	return cerr
}

func codexClassifyText(text string) (string, bool) {
	if k := digest.ClassifyText(text); k != "" {
		return k, k == protocol.ErrKindRateLimit
	}
	if strings.Contains(strings.ToLower(text), "does not exist") {
		return protocol.ErrKindModel, false
	}
	return protocol.ErrKindOther, false
}

func (s *codexSession) handleItem(t *codexTurn, it codexItem, completed bool) {
	switch it.Type {
	case "agentMessage":
		if completed && it.Text != "" {
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventMessage, TurnID: t.id, MessageID: it.ID, Text: it.Text})
		}
		return
	case "reasoning", "userMessage", "hookPrompt", "contextCompaction", "plan":
		return
	}
	tc := codexToolCall(it)
	if tc == nil {
		return
	}
	s.mu.Lock()
	if prev := t.items[it.ID]; prev != nil {
		if len(tc.Paths) == 0 {
			tc.Paths = prev.Paths
		}
		if tc.Input == "" {
			tc.Input = prev.Input
		}
	}
	if completed {
		delete(t.items, it.ID)
	} else {
		t.items[it.ID] = tc
	}
	s.mu.Unlock()
	if tc.Output != "" {
		tc.Output = ccTruncate(ccRedact(tc.Output, s.acct.Env), claudeOutputMax)
	}
	s.emit(protocol.AgentChatEvent{Kind: protocol.EventToolCall, TurnID: t.id, ToolCall: tc})
}

func codexStatus(st string) string {
	switch st {
	case "inProgress", "":
		return "running"
	case "completed":
		return "completed"
	case "failed", "declined":
		return "failed"
	}
	return st
}

func codexToolCall(it codexItem) *protocol.AgentToolCall {
	tc := &protocol.AgentToolCall{ID: it.ID, Status: codexStatus(it.Status)}
	switch it.Type {
	case "commandExecution":
		tc.Name, tc.Kind = "exec", "execute"
		tc.Title = "Run: " + ccTruncate(strings.TrimSpace(it.Command), 200)
		tc.Input = ccTruncate(it.Command, claudeInputMax)
		if it.AggregatedOutput != nil {
			tc.Output = *it.AggregatedOutput
		}
		if it.ExitCode != nil && *it.ExitCode != 0 && tc.Status == "completed" {
			tc.Status = "failed"
		}
		if it.ExitCode != nil && tc.Output == "" && tc.Status != "running" {
			tc.Output = fmt.Sprintf("exit code %d", *it.ExitCode)
		}
	case "fileChange":
		tc.Name, tc.Kind = "patch", "edit"
		var diffs []string
		for _, c := range it.Changes {
			tc.Paths = append(tc.Paths, c.Path)
			diffs = append(diffs, c.Diff)
		}
		tc.Title = "Edit: " + strings.Join(tc.Paths, ", ")
		if len(tc.Paths) == 0 {
			tc.Title = "Edit files"
		}
		tc.Input = ccTruncate(strings.Join(diffs, "\n"), claudeInputMax)
		if it.Status == "declined" {
			tc.Output = "declined"
		}
	case "mcpToolCall":
		tc.Name, tc.Kind = it.Server+"."+it.Tool, "other"
		tc.Title = it.Server + ": " + it.Tool
		tc.Input = ccTruncate(string(it.Arguments), claudeInputMax)
		if it.Error != nil {
			tc.Output = it.Error.Message
		} else if len(it.Result) > 0 && string(it.Result) != "null" {
			tc.Output = codexMCPResultText(it.Result)
		}
	case "dynamicToolCall":
		tc.Name, tc.Kind, tc.Title = it.Tool, "other", it.Tool
		tc.Input = ccTruncate(string(it.Arguments), claudeInputMax)
	case "webSearch":
		tc.Name, tc.Kind, tc.Title = "web_search", "fetch", "Search the web: "+it.Query
	case "imageView":
		tc.Name, tc.Kind, tc.Title = "view_image", "read", "View: "+it.Path
		tc.Paths = []string{it.Path}
	case "collabAgentToolCall", "subAgentActivity":
		tc.Name, tc.Kind, tc.Title = it.Type, "think", "Sub-agent"
	default:
		return nil
	}
	return tc
}

func codexMCPResultText(raw jsontext.Value) string {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if unmarshal(raw, &r) == nil && len(r.Content) > 0 {
		var parts []string
		for _, c := range r.Content {
			if c.Type == "text" {
				parts = append(parts, c.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// --- server requests ------------------------------------------------------------------------

func (s *codexSession) onRequest(gen int, id jsontext.Value, method string, params jsontext.Value) {
	s.mu.Lock()
	live := gen == s.procGen
	conn := s.conn
	t := s.turn
	s.mu.Unlock()
	if !live || conn == nil {
		return
	}
	elicID := "codex-" + codexIDKey(id)
	p := &codexPending{elicID: elicID, rpcID: id, method: method, kind: protocol.ElicitPermission}
	el := &protocol.AgentElicitation{ID: elicID, Kind: protocol.ElicitPermission}
	permOptions := []protocol.AgentElicitationOption{
		{ID: "allow_once", Label: "Allow once", Kind: "allow_once"},
		{ID: "allow_always", Label: "Allow for this session", Kind: "allow_always"},
		{ID: "reject", Label: "Reject", Kind: "reject_once"},
	}
	var common struct {
		ItemID  string  `json:"itemId"`
		Reason  *string `json:"reason"`
		Command *string `json:"command"`
		Cwd     *string `json:"cwd"`
	}
	_ = unmarshal(params, &common)
	reason := ""
	if common.Reason != nil {
		reason = *common.Reason
	}
	switch method {
	case "item/commandExecution/requestApproval":
		cmd := ""
		if common.Command != nil {
			cmd = *common.Command
		}
		tc := &protocol.AgentToolCall{ID: common.ItemID, Name: "exec", Kind: "execute", Status: "pending", Title: "Run: " + ccTruncate(cmd, 200), Input: ccTruncate(cmd, claudeInputMax)}
		if t != nil {
			s.mu.Lock()
			if prev := t.items[common.ItemID]; prev != nil && cmd == "" {
				tc.Title, tc.Input = prev.Title, prev.Input
			}
			s.mu.Unlock()
		}
		el.ToolCall, el.Title, el.Options = tc, "Allow "+tc.Title+"?", permOptions
		var d []string
		if common.Cwd != nil && *common.Cwd != "" {
			d = append(d, "In "+*common.Cwd)
		}
		if reason != "" {
			d = append(d, reason)
		}
		el.Detail = strings.Join(d, "\n")
	case "item/fileChange/requestApproval":
		tc := &protocol.AgentToolCall{ID: common.ItemID, Name: "patch", Kind: "edit", Status: "pending", Title: "Edit files"}
		if t != nil {
			s.mu.Lock()
			if prev := t.items[common.ItemID]; prev != nil {
				cp := *prev
				tc = &cp
				tc.Status = "pending"
			}
			s.mu.Unlock()
		}
		el.ToolCall, el.Title, el.Options, el.Detail = tc, "Allow "+tc.Title+"?", permOptions, reason
	case "item/permissions/requestApproval":
		var pp struct {
			Permissions jsontext.Value `json:"permissions"`
		}
		_ = unmarshal(params, &pp)
		p.permissions = pp.Permissions
		el.Title = "Allow codex extra permissions?"
		el.Detail = strings.TrimSpace(reason + "\n" + ccTruncate(string(pp.Permissions), 500))
		el.Options = []protocol.AgentElicitationOption{
			{ID: "allow_once", Label: "Allow for this turn", Kind: "allow_once"},
			{ID: "allow_always", Label: "Allow for this session", Kind: "allow_always"},
			{ID: "reject", Label: "Reject", Kind: "reject_once"},
		}
	case "item/tool/requestUserInput":
		var q struct {
			Questions []struct {
				ID       string `json:"id"`
				Header   string `json:"header"`
				Question string `json:"question"`
				Options  []struct {
					Label string `json:"label"`
				} `json:"options"`
			} `json:"questions"`
		}
		if unmarshal(params, &q) != nil || len(q.Questions) == 0 {
			_ = conn.respondError(id, -32602, "no questions")
			return
		}
		p.kind, el.Kind = protocol.ElicitQuestion, protocol.ElicitQuestion
		el.Title, el.AllowText = q.Questions[0].Question, true
		for i, o := range q.Questions[0].Options {
			p.options = append(p.options, o.Label)
			el.Options = append(el.Options, protocol.AgentElicitationOption{ID: fmt.Sprintf("opt_%d", i), Label: o.Label, Kind: "choice"})
		}
		for _, qq := range q.Questions {
			p.questionIDs = append(p.questionIDs, qq.ID)
		}
	case "mcpServer/elicitation/request":
		_ = conn.respond(id, map[string]any{"action": "decline", "content": nil, "_meta": nil})
		return
	default:
		// account/chatgptAuthTokens/refresh, attestation/generate, legacy v1 approvals,
		// item/tool/call (no client tools registered): not supported.
		_ = conn.respondError(id, -32601, "syre does not support "+method)
		return
	}
	s.mu.Lock()
	s.pending[elicID] = p
	tid := ""
	if s.turn != nil {
		tid = s.turn.id
	}
	s.mu.Unlock()
	s.emit(protocol.AgentChatEvent{Kind: protocol.EventElicitation, TurnID: tid, Elicitation: el})
}

// ProcessID is the app-server process id, 0 while none runs (ProcessIDer).
func (s *codexSession) ProcessID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.proc.(interface{ pid() int }); ok {
		return p.pid()
	}
	return 0
}

// Suspend freezes the app-server process tree (Suspender).
func (s *codexSession) Suspend() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.suspended {
		return nil
	}
	if p, ok := s.proc.(interface{ pid() int }); ok {
		if err := proctree.Suspend(p.pid()); err != nil {
			return err
		}
	}
	s.suspended = true
	return nil
}

// Resume thaws the app-server process tree; the watchdog restarts from now (Suspender).
func (s *codexSession) Resume() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.suspended {
		return nil
	}
	if p, ok := s.proc.(interface{ pid() int }); ok {
		if err := proctree.Resume(p.pid()); err != nil && !errors.Is(err, proctree.ErrNoProcess) {
			return err
		}
	}
	s.suspended = false
	s.lastActivity = s.eng.now()
	return nil
}
