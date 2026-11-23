package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

const authOK = `{"loggedIn": true, "authMethod": "claude.ai", "email": "user@example.com", "loginExpiresAt": "2099-01-01T00:00:00.000Z", "subscriptionType": "max"}`

type fakeClaudeOpts struct {
	onUser     func(p *ccFakeProc, msg map[string]any, n int)
	onResponse func(p *ccFakeProc, msg map[string]any)
	usage      string // get_usage response body
}

func fakeClaudeHandler(t *testing.T, o fakeClaudeOpts) func(p *ccFakeProc, msg map[string]any) {
	initBody, err := os.ReadFile("testdata/claude/initialize_response.json")
	if err != nil {
		t.Fatal(err)
	}
	var users atomic.Int32
	return func(p *ccFakeProc, msg map[string]any) {
		switch msg["type"] {
		case "control_request":
			id := msg["request_id"]
			req, _ := msg["request"].(map[string]any)
			ok := func(body any) {
				p.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": body}})
			}
			switch req["subtype"] {
			case "initialize":
				ok(json.RawMessage(initBody))
			case "get_usage":
				if o.usage == "" {
					ok(map[string]any{"rate_limits_available": false, "rate_limits": nil})
				} else {
					ok(json.RawMessage(o.usage))
				}
			case "interrupt":
				ok(map[string]any{"still_queued": []any{}})
				p.send(`{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_streaming","usage":{"input_tokens":1,"output_tokens":2}}`)
			case "set_model":
				ok(map[string]any{"model": req["model"]})
			case "set_permission_mode":
				ok(map[string]any{"mode": req["mode"]})
			default:
				p.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": "unsupported"}})
			}
		case "control_response":
			if o.onResponse != nil {
				o.onResponse(p, msg)
			}
		case "user":
			n := int(users.Add(1))
			if o.onUser != nil {
				o.onUser(p, msg, n)
			}
		}
	}
}

func newTestClaude(t *testing.T, o fakeClaudeOpts) (*claudeEngine, *ccFakeLauncher) {
	t.Helper()
	l := &ccFakeLauncher{handler: fakeClaudeHandler(t, o)}
	e := newClaudeEngine()
	e.launch = l.launch
	e.run = fakeRunner(map[string]struct {
		out  string
		code int
	}{
		"auth status": {authOK, 0},
		"--version":   {"2.1.330 (Claude Code)\n", 0},
	})
	e.tokenReader = func(context.Context, Account) (string, error) { return "", os.ErrNotExist }
	e.probeDir = t.TempDir()
	e.cancelGrace = 2 * time.Second
	return e, l
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestClaudeArgvAndMCPConfig(t *testing.T) {
	e, l := newTestClaude(t, fakeClaudeOpts{})
	rec := newCCRecorder()
	acct := Account{ID: "claude", Binary: "/opt/claude", ArgsPrefix: []string{"--prefix"}}
	s, err := e.Open(context.Background(), acct, protocol.AgentChatOpen{
		Cwd: t.TempDir(), Model: "sonnet", Effort: "high", ApprovalMode: "manual",
		SystemPrompt: "You orchestrate.",
		MCPServers:   []protocol.AgentMCPServer{{Name: "konsert", URL: "http://hub/mcp", Headers: map[string]string{"Authorization": "Bearer chat-token"}}},
	}, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	ready, _ := rec.wait(t, isKind(protocol.EventSessionReady))
	if ready.AgentSessionID == "" || ready.ApprovalMode != "default" || ready.Model != "sonnet" {
		t.Fatalf("ready = %+v", ready)
	}
	args := l.last().spec.Args
	want := []string{"--prefix", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--permission-prompt-tool", "stdio"}
	if !slices.Equal(args[:len(want)], want) {
		t.Fatalf("argv prefix = %v", args)
	}
	for flag, v := range map[string]string{"--model": "sonnet", "--effort": "high", "--permission-mode": "default", "--append-system-prompt": "You orchestrate.", "--session-id": ready.AgentSessionID} {
		if got := argValue(args, flag); got != v {
			t.Errorf("%s = %q, want %q (argv %v)", flag, got, v, args)
		}
	}
	if l.last().spec.Binary != "/opt/claude" {
		t.Errorf("binary = %s", l.last().spec.Binary)
	}
	if strings.Contains(strings.Join(args, " "), "chat-token") {
		t.Fatal("MCP bearer token leaked into argv")
	}
	path := argValue(args, "--mcp-config")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mcp config mode %v", st.Mode().Perm())
	}
	b, _ := os.ReadFile(path)
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.MCPServers["konsert"].Type != "http" || cfg.MCPServers["konsert"].Headers["Authorization"] != "Bearer chat-token" {
		t.Fatalf("mcp config = %s (%v)", b, err)
	}
	// The first line sent is the initialize control request.
	first := l.last().writtenMsgs()[0]
	if first["type"] != "control_request" || first["request"].(map[string]any)["subtype"] != "initialize" {
		t.Fatalf("first line = %v", first)
	}
	_ = s.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("mcp config not removed on Close")
	}

	// Resume uses --resume with the given id.
	s2, err := e.Open(context.Background(), acct, protocol.AgentChatOpen{Cwd: t.TempDir(), AgentSessionID: "abc"}, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if got := argValue(l.last().spec.Args, "--resume"); got != "abc" {
		t.Fatalf("--resume = %q", got)
	}
}

func TestClaudeEnvDropsAPIKeyAndNestedSession(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-not-leak")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "daemon-token")
	e := newClaudeEngine()
	env := strings.Join(e.env(Account{Env: map[string]string{"CLAUDE_CONFIG_DIR": "/x/.claude2", "CLAUDE_CODE_OAUTH_TOKEN": "acct-token"}}), "\n")
	if strings.Contains(env, "ANTHROPIC_API_KEY") || strings.Contains(env, "CLAUDECODE=") || strings.Contains(env, "daemon-token") {
		t.Fatalf("env leaks: %s", env)
	}
	if !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=acct-token") || !strings.Contains(env, "CLAUDE_CONFIG_DIR=/x/.claude2") {
		t.Fatal("account env missing")
	}
	// The default account keeps a daemon-provided token (token-only hosts).
	env = strings.Join(e.env(Account{}), "\n")
	if !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=daemon-token") {
		t.Fatal("default account lost the daemon token")
	}
}

func replay(p *ccFakeProc, lines []string) {
	for _, l := range lines {
		p.send(l)
	}
}

func TestClaudeTurnMapping(t *testing.T) {
	lines := readFixtureLines(t, "testdata/claude/turn_tool.jsonl")
	e, l := newTestClaude(t, fakeClaudeOpts{onUser: func(p *ccFakeProc, _ map[string]any, _ int) { replay(p, lines) }})
	rec := newCCRecorder()
	s, err := e.Open(context.Background(), Account{ID: "claude"}, protocol.AgentChatOpen{Cwd: t.TempDir()}, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	rec.wait(t, isKind(protocol.EventSessionReady))
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "go"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t2", Text: "again"}); err == nil {
		t.Fatal("second Prompt during a turn must fail")
	}
	end, seen := rec.wait(t, isKind(protocol.EventTurnEnd))
	if end.StopReason != "end_turn" || end.TurnID != "t1" {
		t.Fatalf("turn end = %+v", end)
	}
	var kinds []string
	var deltas, thought string
	var tools []protocol.AgentToolCall
	var usage *protocol.AgentTokenUsage
	var msg protocol.AgentChatEvent
	var rl *protocol.AgentRateLimit
	for _, ev := range seen {
		kinds = append(kinds, ev.Kind)
		switch ev.Kind {
		case protocol.EventMessageDelta:
			deltas += ev.Text
			if ev.MessageID != "msg_2" {
				t.Errorf("delta message id %q", ev.MessageID)
			}
		case protocol.EventThoughtDelta:
			thought += ev.Text
		case protocol.EventToolCall:
			tools = append(tools, *ev.ToolCall)
		case protocol.EventUsage:
			usage = ev.Usage
		case protocol.EventMessage:
			msg = ev
		case protocol.EventRateLimit:
			rl = ev.RateLimit
		}
	}
	if deltas != "DONE" || thought != "Need to write the file." || msg.Text != "DONE" || msg.MessageID != "msg_2" {
		t.Fatalf("deltas=%q thought=%q msg=%+v kinds=%v", deltas, thought, msg, kinds)
	}
	if len(tools) != 2 || tools[0].Status != "running" || tools[0].Title != "Write: /var/tmp/scratch/a.txt" || tools[0].Kind != "edit" ||
		tools[1].Status != "completed" || !strings.Contains(tools[1].Output, "File created") {
		t.Fatalf("tools = %+v", tools)
	}
	if usage == nil || usage.OutputTokens != 98 || usage.CacheReadTokens != 41967 || usage.CostUSD == 0 {
		t.Fatalf("usage = %+v", usage)
	}
	if rl == nil || rl.Window != "seven_day" || rl.UsedPercent < 40.9 || rl.UsedPercent > 41.1 || rl.State != "ok" || rl.ResetsAt == nil {
		t.Fatalf("rate limit = %+v", rl)
	}
	// system init with the CLI's id equal to ours: no second session_ready. The fixture's
	// id differs from the generated one, so one more session_ready carries it.
	if s.AgentSessionID() != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("session id = %s", s.AgentSessionID())
	}
	// The user line format.
	var userLine map[string]any
	for _, m := range l.last().writtenMsgs() {
		if m["type"] == "user" {
			userLine = m
		}
	}
	content := userLine["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" || content["text"] != "go" {
		t.Fatalf("user line = %v", userLine)
	}
}

// TestClaudeUsageLimitFixture replays the real sequence claude 2.1.330 produced for an
// account at 100% of its weekly limit (sanitized).
func TestClaudeUsageLimitFixture(t *testing.T) {
	lines := readFixtureLines(t, "testdata/claude/usage_limit.jsonl")
	e, _ := newTestClaude(t, fakeClaudeOpts{onUser: func(p *ccFakeProc, _ map[string]any, _ int) { replay(p, lines) }})
	rec := newCCRecorder()
	acct := Account{ID: "claude2", Label: "Claude 2", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/home/u/.claude2", "CLAUDE_CODE_OAUTH_TOKEN": "tok-secret-value"}}
	s, err := e.Open(context.Background(), acct, protocol.AgentChatOpen{Cwd: t.TempDir()}, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "Reply with exactly: OK"}); err != nil {
		t.Fatal(err)
	}
	errEv, seen := rec.wait(t, isKind(protocol.EventError))
	ce := errEv.Error
	wantReset := time.Unix(1795676400, 0).UTC()
	if ce.Kind != protocol.ErrKindUsageLimit || ce.Window != "seven_day" || ce.ResetsAt == nil || !ce.ResetsAt.Equal(wantReset) || ce.Retryable || ce.Relogin != nil {
		t.Fatalf("error = %+v", ce)
	}
	if !strings.HasPrefix(ce.Message, "Claude 2 is out of weekly usage until 2026-11-26 07:00 UTC") {
		t.Fatalf("message = %q", ce.Message)
	}
	for _, ev := range seen {
		if ev.Kind == protocol.EventMessage {
			t.Fatalf("synthetic error text surfaced as a reply: %q", ev.Text)
		}
	}
	// The result arrives after the rate_limit_event: no second error, no turn_end.
	time.Sleep(100 * time.Millisecond)
	for len(rec.ch) > 0 {
		ev := <-rec.ch
		if ev.Kind == protocol.EventError || ev.Kind == protocol.EventTurnEnd {
			t.Fatalf("unexpected %s after usage_limit", ev.Kind)
		}
	}
	// Probe-side usage: the endpoint is unavailable for this setup token, so the cached
	// stream windows show seven_day exhausted before the user sends anything else.
	limits, note := e.usage(context.Background(), acct, nil)
	var seven *protocol.AgentRateLimit
	for i := range limits {
		if limits[i].Window == "seven_day" {
			seven = &limits[i]
		}
	}
	if seven == nil || seven.State != "exhausted" || seven.UsedPercent != 100 || !seven.ResetsAt.Equal(wantReset) || note == "" {
		t.Fatalf("usage = %+v note=%q", limits, note)
	}
	// The process stays up (no retry loop, no restart).
	if s.(*claudeSession).proc == nil {
		t.Fatal("process gone after usage limit")
	}
}

func TestClaudePermissionElicitation(t *testing.T) {
	answers := make(chan map[string]any, 4)
	e, _ := newTestClaude(t, fakeClaudeOpts{
		onUser: func(p *ccFakeProc, _ map[string]any, n int) {
			p.send(map[string]any{"type": "control_request", "request_id": "perm-" + string(rune('0'+n)), "request": map[string]any{
				"subtype": "can_use_tool", "tool_name": "Bash", "display_name": "Bash",
				"input": map[string]any{"command": "rm -rf build"}, "tool_use_id": "toolu_9",
				"description": "Delete build dir",
			}})
		},
		onResponse: func(p *ccFakeProc, msg map[string]any) {
			answers <- msg
			p.send(`{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","usage":{"output_tokens":1}}`)
		},
	})
	rec := newCCRecorder()
	s, err := e.Open(context.Background(), Account{ID: "claude"}, protocol.AgentChatOpen{Cwd: t.TempDir()}, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	for _, tc := range []struct {
		option   string
		behavior string
	}{{"allow_always", "allow"}, {"reject", "deny"}} {
		if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: tc.option, Text: "clean"}); err != nil {
			t.Fatal(err)
		}
		ev, _ := rec.wait(t, isKind(protocol.EventElicitation))
		el := ev.Elicitation
		if el.Kind != protocol.ElicitPermission || el.ToolCall == nil || el.ToolCall.Title != "Run: rm -rf build" || len(el.Options) != 3 || el.Title != "Allow Run: rm -rf build?" {
			t.Fatalf("elicitation = %+v", el)
		}
		if err := s.Respond(context.Background(), protocol.AgentChatRespond{ElicitationID: el.ID, OptionID: tc.option, Text: "not now"}); err != nil {
			t.Fatal(err)
		}
		ok, _ := rec.wait(t, isKind(protocol.EventElicitationOK))
		if ok.Elicitation.ID != el.ID {
			t.Fatalf("resolved %+v", ok)
		}
		a := <-answers
		resp := a["response"].(map[string]any)
		inner := resp["response"].(map[string]any)
		if resp["request_id"] != el.ID || resp["subtype"] != "success" || inner["behavior"] != tc.behavior {
			t.Fatalf("answer = %v", a)
		}
		switch tc.behavior {
		case "allow":
			if inner["updatedInput"].(map[string]any)["command"] != "rm -rf build" || inner["updatedPermissions"] == nil {
				t.Fatalf("allow answer = %v", inner)
			}
		case "deny":
			if !strings.Contains(inner["message"].(string), "not now") {
				t.Fatalf("deny answer = %v", inner)
			}
		}
		rec.wait(t, isKind(protocol.EventTurnEnd))
	}
	if err := s.Respond(context.Background(), protocol.AgentChatRespond{ElicitationID: "nope", OptionID: "allow_once"}); err == nil {
		t.Fatal("unknown elicitation accepted")
	}
}

func TestClaudeAskUserQuestion(t *testing.T) {
	answers := make(chan map[string]any, 1)
	e, _ := newTestClaude(t, fakeClaudeOpts{
		onUser: func(p *ccFakeProc, _ map[string]any, _ int) {
			p.send(`{"type":"control_request","request_id":"q1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Which DB?","header":"DB","options":[{"label":"SQLite","description":""},{"label":"Postgres","description":""}]}]},"tool_use_id":"toolu_q"}}`)
		},
		onResponse: func(p *ccFakeProc, msg map[string]any) { answers <- msg },
	})
	rec := newCCRecorder()
	s, err := e.Open(context.Background(), Account{ID: "claude"}, protocol.AgentChatOpen{Cwd: t.TempDir()}, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t", Text: "?"})
	ev, _ := rec.wait(t, isKind(protocol.EventElicitation))
	if ev.Elicitation.Kind != protocol.ElicitQuestion || ev.Elicitation.Title != "Which DB?" || len(ev.Elicitation.Options) != 2 || !ev.Elicitation.AllowText {
		t.Fatalf("question = %+v", ev.Elicitation)
	}
	_ = s.Respond(context.Background(), protocol.AgentChatRespond{ElicitationID: "q1", OptionID: "opt_1"})
	a := <-answers
	inner := a["response"].(map[string]any)["response"].(map[string]any)
	if inner["updatedInput"].(map[string]any)["answers"].(map[string]any)["Which DB?"] != "Postgres" {
		t.Fatalf("answer = %v", inner)
	}
}

func TestClaudeAuthErrorResult(t *testing.T) {
	for _, tc := range []struct {
		env     map[string]string
		command string
	}{{nil, "claude /login"}, {map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "x", "CLAUDE_CONFIG_DIR": "/h/.claude2"}, "claude setup-token"}} {
		e, _ := newTestClaude(t, fakeClaudeOpts{onUser: func(p *ccFakeProc, _ map[string]any, _ int) {
			p.send(`{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login","terminal_reason":"api_error"}`)
		}})
		rec := newCCRecorder()
		s, err := e.Open(context.Background(), Account{ID: "a", Env: tc.env}, protocol.AgentChatOpen{Cwd: t.TempDir()}, rec.emit)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t", Text: "hi"})
		ev, _ := rec.wait(t, isKind(protocol.EventError))
		if ev.Error.Kind != protocol.ErrKindAuth || ev.Error.Relogin == nil || ev.Error.Relogin.Command != tc.command {
			t.Fatalf("error = %+v", ev.Error)
		}
		if tc.env != nil && (ev.Error.Relogin.Env["CLAUDE_CONFIG_DIR"] != "/h/.claude2" || ev.Error.Relogin.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "") {
			t.Fatalf("relogin env = %v", ev.Error.Relogin.Env)
		}
		_ = s.Close()
	}
}

func TestClaudeOpenLoggedOutFailsFast(t *testing.T) {
	e, l := newTestClaude(t, fakeClaudeOpts{})
	e.run = fakeRunner(map[string]struct {
		out  string
		code int
	}{"auth status": {`{"loggedIn": false, "authMethod": "none"}`, 1}})
	rec := newCCRecorder()
	if _, err := e.Open(context.Background(), Account{ID: "claude"}, protocol.AgentChatOpen{}, rec.emit); err == nil {
		t.Fatal("Open succeeded while logged out")
	}
	ev, _ := rec.wait(t, isKind(protocol.EventError))
	if ev.Error.Kind != protocol.ErrKindAuth || ev.Error.Relogin.Command != "claude /login" {
		t.Fatalf("error = %+v", ev.Error)
	}
	if l.count() != 0 {
		t.Fatal("launched claude although logged out")
	}
}

func TestClaudeCancel(t *testing.T) {
	e, l := newTestClaude(t, fakeClaudeOpts{onUser: func(p *ccFakeProc, _ map[string]any, _ int) {
		p.send(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m"}},"parent_tool_use_id":null}`)
		p.send(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"one"}},"parent_tool_use_id":null}`)
	}})
	rec := newCCRecorder()
	s, err := e.Open(context.Background(), Account{ID: "claude"}, protocol.AgentChatOpen{Cwd: t.TempDir()}, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t", Text: "count"})
	rec.wait(t, isKind(protocol.EventMessageDelta))
	if err := s.Control(context.Background(), protocol.AgentChatControl{Cancel: true}); err != nil {
		t.Fatal(err)
	}
	end, _ := rec.wait(t, isKind(protocol.EventTurnEnd))
	if end.StopReason != "cancelled" {
		t.Fatalf("end = %+v", end)
	}
	var sawInterrupt bool
	for _, m := range l.last().writtenMsgs() {
		if r, ok := m["request"].(map[string]any); ok && r["subtype"] == "interrupt" {
			sawInterrupt = true
		}
	}
	if !sawInterrupt {
		t.Fatal("no interrupt control request")
	}
}

func TestClaudeCrashThenResume(t *testing.T) {
	e, l := newTestClaude(t, fakeClaudeOpts{onUser: func(p *ccFakeProc, _ map[string]any, _ int) {
		p.send(`{"type":"system","subtype":"init","session_id":"sess-1","model":"m","permissionMode":"default"}`)
		go p.exit(3, "boom\nfatal: connection lost token=sk-ant-oat01-abcdefghijklmnop")
	}})
	rec := newCCRecorder()
	s, err := e.Open(context.Background(), Account{ID: "claude"}, protocol.AgentChatOpen{Cwd: t.TempDir()}, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "hi"})
	ev, _ := rec.wait(t, isKind(protocol.EventError))
	ce := ev.Error
	if ce.Kind != protocol.ErrKindExit || ce.ExitCode == nil || *ce.ExitCode != 3 || strings.Contains(ce.Stderr, "abcdefghijklmnop") || !strings.Contains(ce.Stderr, "[redacted]") {
		t.Fatalf("error = %+v", ce)
	}
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t2", Text: "again"}); err != nil {
		t.Fatal(err)
	}
	if l.count() != 2 || argValue(l.last().spec.Args, "--resume") != "sess-1" {
		t.Fatalf("restart argv = %v (launches %d)", l.last().spec.Args, l.count())
	}
}

func TestClaudeWatchdog(t *testing.T) {
	e, _ := newTestClaude(t, fakeClaudeOpts{})
	e.watchdog = 50 * time.Millisecond
	rec := newCCRecorder()
	s, err := e.Open(context.Background(), Account{ID: "claude"}, protocol.AgentChatOpen{Cwd: t.TempDir()}, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t", Text: "hang"})
	ev, _ := rec.wait(t, isKind(protocol.EventError))
	if ev.Error.Kind != protocol.ErrKindTimeout || ev.TurnID != "t" {
		t.Fatalf("error = %+v", ev)
	}
}

func TestClaudeControl(t *testing.T) {
	e, l := newTestClaude(t, fakeClaudeOpts{onUser: func(p *ccFakeProc, _ map[string]any, _ int) {
		p.send(`{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn"}`)
	}})
	rec := newCCRecorder()
	s, err := e.Open(context.Background(), Account{ID: "claude"}, protocol.AgentChatOpen{Cwd: t.TempDir(), Effort: "low"}, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if err := s.Control(ctx, protocol.AgentChatControl{Model: "opus", ApprovalMode: "acceptEdits"}); err != nil {
		t.Fatal(err)
	}
	var subtypes []string
	for _, m := range l.last().writtenMsgs() {
		if r, ok := m["request"].(map[string]any); ok {
			subtypes = append(subtypes, r["subtype"].(string))
		}
	}
	if !slices.Contains(subtypes, "set_model") || !slices.Contains(subtypes, "set_permission_mode") || l.count() != 1 {
		t.Fatalf("control requests = %v launches=%d", subtypes, l.count())
	}
	// Effort cannot change live: restart with --resume before the next turn.
	_ = s.Prompt(ctx, protocol.AgentChatPrompt{TurnID: "t1", Text: "x"})
	rec.wait(t, isKind(protocol.EventTurnEnd))
	if err := s.Control(ctx, protocol.AgentChatControl{Effort: "max"}); err != nil {
		t.Fatal(err)
	}
	st, _ := rec.wait(t, isKind(protocol.EventStatus))
	if !strings.Contains(st.Text, "next message") {
		t.Fatalf("status = %+v", st)
	}
	_ = s.Prompt(ctx, protocol.AgentChatPrompt{TurnID: "t2", Text: "y"})
	rec.wait(t, isKind(protocol.EventTurnEnd))
	args := l.last().spec.Args
	if l.count() != 2 || argValue(args, "--effort") != "max" || argValue(args, "--model") != "opus" || argValue(args, "--permission-mode") != "acceptEdits" || argValue(args, "--resume") == "" {
		t.Fatalf("restart argv = %v", args)
	}
	// Bypass needs a restart too.
	if err := s.Control(ctx, protocol.AgentChatControl{ApprovalMode: "bypassPermissions"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Prompt(ctx, protocol.AgentChatPrompt{TurnID: "t3", Text: "z"})
	rec.wait(t, isKind(protocol.EventTurnEnd))
	if l.count() != 3 || argValue(l.last().spec.Args, "--permission-mode") != "bypassPermissions" {
		t.Fatalf("bypass argv = %v", l.last().spec.Args)
	}
}

const usageBody = `{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":12,"resets_at":"2026-11-23T12:00:00.5+00:00"},"seven_day":{"utilization":85,"resets_at":"2026-11-26T23:59:59.536191+00:00"},"seven_day_opus":null,"model_scoped":[{"display_name":"Fable","utilization":52,"resets_at":"2026-11-26T23:59:59+00:00"}]}}`

func TestClaudeProbe(t *testing.T) {
	var usageCalls atomic.Int32
	e, l := newTestClaude(t, fakeClaudeOpts{usage: usageBody})
	inner := l.handler
	l.handler = func(p *ccFakeProc, msg map[string]any) {
		if r, ok := msg["request"].(map[string]any); ok && r["subtype"] == "get_usage" {
			usageCalls.Add(1)
		}
		inner(p, msg)
	}
	now := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	a := e.Probe(context.Background(), Account{ID: "claude", Label: "Claude", IsDefault: true})
	if a.Version != "2.1.330" || a.Auth.State != protocol.AuthOK || a.Auth.Method != "oauth" || a.Plan != "max" || !a.ChatSupported {
		t.Fatalf("account = %+v", a)
	}
	if a.ModelsSource != "discovered" || a.DefaultModel != "default" || len(a.Models) < 5 || !a.Models[0].IsDefault {
		t.Fatalf("models = %+v", a.Models)
	}
	var opus46 protocol.AgentModel
	for _, m := range a.Models {
		if m.ID == "claude-opus-4-6" {
			opus46 = m
		}
	}
	if len(opus46.Efforts) != 4 || opus46.Efforts[3].ID != "max" {
		t.Fatalf("opus 4.6 efforts = %+v", opus46.Efforts)
	}
	if len(a.Approvals) != 5 || a.Approvals[0].ID != "plan" || a.Approvals[4].Risk != 4 || a.DefaultApproval != "default" {
		t.Fatalf("approvals = %+v", a.Approvals)
	}
	if len(a.Usage) != 3 || a.Usage[0].Window != "five_hour" || a.Usage[1].State != "warning" || a.Usage[2].Window != "seven_day_fable" || a.UsageNote != "" {
		t.Fatalf("usage = %+v note=%q", a.Usage, a.UsageNote)
	}
	// Within 5 minutes the cached usage is reused.
	now = now.Add(time.Minute)
	b := e.Probe(context.Background(), Account{ID: "claude"})
	if usageCalls.Load() != 1 || len(b.Usage) != 3 {
		t.Fatalf("usage polled %d times; usage=%+v", usageCalls.Load(), b.Usage)
	}
	now = now.Add(5 * time.Minute)
	e.Probe(context.Background(), Account{ID: "claude"})
	if usageCalls.Load() != 2 {
		t.Fatalf("usage not refreshed after 5 minutes")
	}
	// Opt-out.
	t.Setenv(claudeUsageDisableEnv, "1")
	c := e.Probe(context.Background(), Account{ID: "other"})
	if usageCalls.Load() != 2 || !strings.Contains(c.UsageNote, claudeUsageDisableEnv) {
		t.Fatalf("opt-out ignored: calls=%d note=%q", usageCalls.Load(), c.UsageNote)
	}
}

func TestClaudeProbeLoggedOut(t *testing.T) {
	e, _ := newTestClaude(t, fakeClaudeOpts{})
	e.run = fakeRunner(map[string]struct {
		out  string
		code int
	}{
		"auth status": {`{"loggedIn": false, "authMethod": "none"}`, 1},
		"--version":   {"2.1.330 (Claude Code)", 0},
	})
	a := e.Probe(context.Background(), Account{ID: "claude2", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/h/.claude2"}})
	if a.Auth.State != protocol.AuthLoggedOut || a.Auth.Relogin == nil || a.Auth.Relogin.Command != "claude /login" || a.Auth.Relogin.Env["CLAUDE_CONFIG_DIR"] != "/h/.claude2" {
		t.Fatalf("auth = %+v", a.Auth)
	}
	if len(a.Usage) != 0 || a.UsageNote == "" {
		t.Fatalf("usage = %+v %q", a.Usage, a.UsageNote)
	}
}

func TestClaudeParseAuth(t *testing.T) {
	now := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	a := claudeParseAuth([]byte(`{"loggedIn":true,"authMethod":"oauth_token","loginExpiresAt":null}`), 0, Account{Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "x"}}, now)
	if a.State != protocol.AuthOK || a.Method != "setup_token" || a.ExpiresAt != nil {
		t.Fatalf("setup token = %+v", a)
	}
	a = claudeParseAuth([]byte(`{"loggedIn":true,"authMethod":"claude.ai","loginExpiresAt":"2026-11-23T05:00:00.000Z"}`), 0, Account{}, now)
	if a.State != protocol.AuthExpiring || a.Relogin == nil {
		t.Fatalf("expiring = %+v", a)
	}
	a = claudeParseAuth([]byte(`garbage`), 1, Account{}, now)
	if a.State != protocol.AuthLoggedOut {
		t.Fatalf("logged out = %+v", a)
	}
}

func TestClaudeFetchUsageHTTP(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer oauth-access" || r.Header.Get("anthropic-beta") != claudeOAuthBeta {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":100,"resets_at":"2026-11-23T12:00:00Z"},"seven_day":{"utilization":3,"resets_at":null}}`))
	}))
	defer srv.Close()
	e := newClaudeEngine()
	e.usageURL = srv.URL
	e.tokenReader = func(context.Context, Account) (string, error) { return "oauth-access", nil }
	limits, err := e.fetchUsage(context.Background(), Account{ID: "c"})
	if err != nil || len(limits) != 2 || limits[0].State != "exhausted" || limits[0].ResetsAt == nil || limits[1].ResetsAt != nil {
		t.Fatalf("limits = %+v err=%v", limits, err)
	}
	status.Store(403)
	_, err = e.fetchUsage(context.Background(), Account{ID: "c"})
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "oauth-access") {
		t.Fatalf("403 err = %v", err)
	}
}

func TestClaudeTokenFromCredentials(t *testing.T) {
	tok, err := claudeTokenFromCredentials([]byte(`{"claudeAiOauth":{"accessToken":"at","refreshToken":"rt"}}`))
	if err != nil || tok != "at" {
		t.Fatalf("%q %v", tok, err)
	}
	if _, err := claudeTokenFromCredentials([]byte(`{}`)); err == nil {
		t.Fatal("empty credentials accepted")
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	tok, err = claudeReadOAuthToken(context.Background(), Account{Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "setup-tok"}})
	if err != nil || tok != "setup-tok" {
		t.Fatalf("env token: %q %v", tok, err)
	}
}

func TestClaudeClassify(t *testing.T) {
	for _, tc := range []struct {
		text   string
		status int
		kind   string
		retry  bool
	}{
		{"Not logged in · Please run /login", 0, protocol.ErrKindAuth, false},
		{"OAuth token has expired. Please obtain a new token", 0, protocol.ErrKindAuth, false},
		{"Invalid API key · Fix external API key", 0, protocol.ErrKindAuth, false},
		{"anything", 401, protocol.ErrKindAuth, false},
		{"You've hit your weekly limit · resets Nov 26 at 12am (America/Denver)", 429, protocol.ErrKindUsageLimit, false},
		{"Claude AI usage limit reached|1795676400", 0, protocol.ErrKindUsageLimit, false},
		{"API Error: 529 Overloaded", 0, protocol.ErrKindRateLimit, true},
		{"x", 429, protocol.ErrKindRateLimit, true},
		{"There's an issue with the selected model (claude-nope). It may not exist", 0, protocol.ErrKindModel, false},
		{"something else broke", 0, protocol.ErrKindOther, false},
	} {
		kind, retry := claudeClassify(tc.text, tc.status)
		if kind != tc.kind || retry != tc.retry {
			t.Errorf("%q/%d: got %s/%v want %s/%v", tc.text, tc.status, kind, retry, tc.kind, tc.retry)
		}
	}
	if ts := claudeParseResetEpoch("Claude AI usage limit reached|1795676400"); ts == nil || ts.Unix() != 1795676400 {
		t.Fatalf("reset epoch = %v", ts)
	}
}

func TestCCRedact(t *testing.T) {
	in := `token sk-ant-oat01-AAAABBBBCCCCDDDD and Bearer abcdefghijklmnop and "access_token":"zzzzzzzzzzzz" secret-xyz-123456`
	out := ccRedact(in, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "secret-xyz-123456", "CLAUDE_CONFIG_DIR": "/home/u/.claude2"})
	for _, leak := range []string{"AAAABBBB", "abcdefghijklmnop", "zzzzzzzzzzzz", "secret-xyz-123456"} {
		if strings.Contains(out, leak) {
			t.Fatalf("leaked %q: %s", leak, out)
		}
	}
}
