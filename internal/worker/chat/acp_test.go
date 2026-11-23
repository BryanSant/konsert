package chat

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// --- harness ------------------------------------------------------------------------------

type evRecorder struct {
	mu  sync.Mutex
	evs []protocol.AgentChatEvent
	ch  chan struct{}
}

func newRecorder() *evRecorder { return &evRecorder{ch: make(chan struct{}, 1)} }

func (r *evRecorder) emit(ev protocol.AgentChatEvent) {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
	select {
	case r.ch <- struct{}{}:
	default:
	}
}

func (r *evRecorder) all() []protocol.AgentChatEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]protocol.AgentChatEvent(nil), r.evs...)
}

func (r *evRecorder) wait(t *testing.T, what string, pred func(protocol.AgentChatEvent) bool) protocol.AgentChatEvent {
	t.Helper()
	deadline := time.After(15 * time.Second)
	seen := 0
	for {
		evs := r.all()
		for _, ev := range evs[seen:] {
			if pred(ev) {
				return ev
			}
		}
		seen = len(evs)
		select {
		case <-r.ch:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s; events: %s", what, dumpEvents(r.all()))
		}
	}
}

func (r *evRecorder) waitKind(t *testing.T, kind string) protocol.AgentChatEvent {
	t.Helper()
	return r.wait(t, kind, func(ev protocol.AgentChatEvent) bool { return ev.Kind == kind })
}

func (r *evRecorder) count(kind string) int {
	n := 0
	for _, ev := range r.all() {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func dumpEvents(evs []protocol.AgentChatEvent) string {
	var b strings.Builder
	for _, ev := range evs {
		j, _ := json.Marshal(ev)
		b.WriteString("\n  " + string(j))
	}
	return b.String()
}

// isolateHome points HOME/USERPROFILE at temp dirs so nothing touches the real home.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

type fakeOpts struct {
	agent, fixture, scenario string
}

// fakeAccount runs this test binary as a fake ACP agent.
func fakeAccount(t *testing.T, o fakeOpts) (Account, string) {
	t.Helper()
	isolateHome(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	td, _ := filepath.Abs(filepath.Join("testdata", "acp"))
	logPath := filepath.Join(t.TempDir(), "fake.log")
	return Account{
		ID: o.agent, Agent: o.agent, Binary: exe,
		Env: map[string]string{
			"KONSERT_FAKE_ACP": "1", "FAKE_ACP_FIXTURE": o.fixture, "FAKE_ACP_SCENARIO": o.scenario,
			"FAKE_ACP_TESTDATA": td, "FAKE_ACP_LOG": logPath, "FAKE_SECRET_TOKEN": "supersecretvalue123",
		},
	}, logPath
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

func openFake(t *testing.T, o fakeOpts, opts protocol.AgentChatOpen) (*acpSession, *evRecorder, string) {
	t.Helper()
	acct, logPath := fakeAccount(t, o)
	eng, err := NewACPEngine(o.agent)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Cwd == "" {
		opts.Cwd = t.TempDir()
	}
	rec := newRecorder()
	sess, err := eng.Open(context.Background(), acct, opts, rec.emit)
	if err != nil {
		t.Fatalf("Open: %v; events: %s", err, dumpEvents(rec.all()))
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess.(*acpSession), rec, logPath
}

func turnEnd(t *testing.T, rec *evRecorder) protocol.AgentChatEvent {
	t.Helper()
	return rec.waitKind(t, protocol.EventTurnEnd)
}

// --- tests --------------------------------------------------------------------------------

func TestACPBasicTurnMapsUpdates(t *testing.T) {
	s, rec, logPath := openFake(t, fakeOpts{agent: "grok", fixture: "grok", scenario: "basic"}, protocol.AgentChatOpen{
		MCPServers: []protocol.AgentMCPServer{{Name: "konsert", URL: "http://127.0.0.1:1/mcp", Headers: map[string]string{"Authorization": "Bearer x"}}},
	})
	ready := rec.waitKind(t, protocol.EventSessionReady)
	if ready.AgentSessionID != "01a20748-45d5-7292-8507-ef4cca5f35d0" || ready.Model != "grok-4.5" || ready.Effort != "high" || ready.ApprovalMode != "default" {
		t.Fatalf("session_ready = %+v", ready)
	}
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t2", Text: "again"}); err == nil {
		t.Error("second Prompt during a turn should fail")
	}
	end := turnEnd(t, rec)
	if end.StopReason != "end_turn" || end.TurnID != "t1" {
		t.Fatalf("turn_end = %+v", end)
	}
	rec.wait(t, "status done", func(ev protocol.AgentChatEvent) bool {
		return ev.Kind == protocol.EventStatus && ev.Status == protocol.ChatStatusDone
	})

	var deltas, msgs []string
	var thought string
	var tools []protocol.AgentToolCall
	var plan []protocol.AgentPlanItem
	var usage *protocol.AgentTokenUsage
	sawWorking := false
	for _, ev := range rec.all() {
		switch ev.Kind {
		case protocol.EventMessageDelta:
			deltas = append(deltas, ev.Text)
		case protocol.EventMessage:
			msgs = append(msgs, ev.Text)
		case protocol.EventThoughtDelta:
			thought += ev.Text
		case protocol.EventToolCall:
			tools = append(tools, *ev.ToolCall)
		case protocol.EventPlan:
			plan = ev.Plan
		case protocol.EventUsage:
			usage = ev.Usage
		case protocol.EventStatus:
			if ev.Status == protocol.ChatStatusWorking {
				sawWorking = true
			}
		}
	}
	if strings.Join(deltas, "|") != "Hel|lo| world" {
		t.Errorf("deltas = %q", deltas)
	}
	if strings.Join(msgs, "|") != "Hello| world" {
		t.Errorf("coalesced messages = %q", msgs)
	}
	if thought != "thinking…" || !sawWorking {
		t.Errorf("thought=%q working=%v", thought, sawWorking)
	}
	if len(tools) != 2 || tools[0].Status != "pending" || tools[1].Status != "completed" ||
		tools[1].Output != "package main" || tools[1].Kind != "read" || tools[1].Title != "Read main.go" ||
		tools[1].Input != `{"path":"/w/main.go"}` || len(tools[1].Paths) != 1 {
		t.Errorf("tool calls = %+v", tools)
	}
	if len(plan) != 2 || plan[1].Status != "in_progress" || plan[0].Text != "Read code" {
		t.Errorf("plan = %+v", plan)
	}
	if usage == nil || usage.InputTokens != 10 || usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}
	log := readLog(t, logPath)
	if !strings.Contains(log, `"type":"http"`) || !strings.Contains(log, `"headers":[{"name":"Authorization","value":"Bearer x"}]`) {
		t.Errorf("mcpServers not passed as http: %s", log)
	}
	if !strings.Contains(log, "argv agent stdio") || !strings.Contains(log, `"_meta":{"autoMode":false,"yoloMode":false}`) {
		t.Errorf("grok argv: %s", log)
	}
}

func TestACPPermissionRoundTrip(t *testing.T) {
	s, rec, _ := openFake(t, fakeOpts{agent: "agy", fixture: "agy", scenario: "permission"}, protocol.AgentChatOpen{})
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "write"}); err != nil {
		t.Fatal(err)
	}
	el := rec.waitKind(t, protocol.EventElicitation).Elicitation
	if el.Kind != protocol.ElicitPermission || el.Title != "Write hello.txt" || len(el.Options) != 3 ||
		el.Options[0].Kind != "allow_once" || el.ToolCall == nil || el.ToolCall.Kind != "edit" || !strings.Contains(el.Detail, "hello.txt") {
		t.Fatalf("elicitation = %+v", el)
	}
	rec.wait(t, "needs_you", func(ev protocol.AgentChatEvent) bool { return ev.Status == protocol.ChatStatusNeedsYou })
	if err := s.Respond(context.Background(), protocol.AgentChatRespond{ElicitationID: el.ID, OptionID: "bogus"}); err == nil {
		t.Error("unknown option accepted")
	}
	// A generic kind maps to the agent's option id.
	if err := s.Respond(context.Background(), protocol.AgentChatRespond{ElicitationID: el.ID, OptionID: "allow_once"}); err != nil {
		t.Fatal(err)
	}
	ok := rec.waitKind(t, protocol.EventElicitationOK)
	if ok.Elicitation.ID != el.ID || ok.Text != "allow" {
		t.Errorf("elicitation_resolved = %+v", ok)
	}
	turnEnd(t, rec)
	msg := rec.waitKind(t, protocol.EventMessage)
	if msg.Text != "outcome=selected:allow" {
		t.Errorf("agent saw %q", msg.Text)
	}
}

func TestACPCancel(t *testing.T) {
	s, rec, logPath := openFake(t, fakeOpts{agent: "cursor", fixture: "cursor", scenario: "cancel"}, protocol.AgentChatOpen{})
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "go"}); err != nil {
		t.Fatal(err)
	}
	rec.waitKind(t, protocol.EventMessageDelta)
	if err := s.Control(context.Background(), protocol.AgentChatControl{Cancel: true}); err != nil {
		t.Fatal(err)
	}
	if end := turnEnd(t, rec); end.StopReason != "cancelled" {
		t.Fatalf("stop = %q", end.StopReason)
	}
	rec.wait(t, "status cancelled", func(ev protocol.AgentChatEvent) bool { return ev.Status == protocol.ChatStatusCancelled })
	if !strings.Contains(readLog(t, logPath), "session/cancel") {
		t.Error("session/cancel not sent")
	}
}

func TestACPCancelResolvesPendingPermission(t *testing.T) {
	s, rec, _ := openFake(t, fakeOpts{agent: "copilot", fixture: "copilot", scenario: "cancel_permission"}, protocol.AgentChatOpen{})
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "go"})
	rec.waitKind(t, protocol.EventElicitation)
	_ = s.Control(context.Background(), protocol.AgentChatControl{Cancel: true})
	ok := rec.waitKind(t, protocol.EventElicitationOK)
	if ok.Text != "cancelled" {
		t.Errorf("resolved = %+v", ok)
	}
	if end := turnEnd(t, rec); end.StopReason != "cancelled" {
		t.Errorf("stop = %q", end.StopReason)
	}
	msg := rec.waitKind(t, protocol.EventMessage)
	if !strings.Contains(msg.Text, `"outcome":"cancelled"`) {
		t.Errorf("agent saw %q", msg.Text)
	}
}

func TestACPAuthRequiredOnOpen(t *testing.T) {
	for _, tc := range []struct{ agent, fixture, scenario string }{
		{"grok", "grok_logged_out", ""},
		{"cursor", "cursor_logged_out", ""},
		{"grok", "grok", "init_auth"},
	} {
		t.Run(tc.fixture+tc.scenario, func(t *testing.T) {
			acct, _ := fakeAccount(t, fakeOpts{agent: tc.agent, fixture: tc.fixture, scenario: tc.scenario})
			eng, _ := NewACPEngine(tc.agent)
			rec := newRecorder()
			_, err := eng.Open(context.Background(), acct, protocol.AgentChatOpen{Cwd: t.TempDir()}, rec.emit)
			if err == nil {
				t.Fatal("Open succeeded while logged out")
			}
			e := rec.waitKind(t, protocol.EventError).Error
			if e.Kind != protocol.ErrKindAuth || e.Relogin == nil || !strings.HasSuffix(e.Relogin.Command, " login") || e.Retryable {
				t.Fatalf("error = %+v relogin=%+v", e, e.Relogin)
			}
			if el := rec.waitKind(t, protocol.EventElicitation).Elicitation; el.Kind != protocol.ElicitAuth {
				t.Errorf("auth elicitation = %+v", el)
			}
			rec.wait(t, "failed", func(ev protocol.AgentChatEvent) bool { return ev.Status == protocol.ChatStatusFailed })
		})
	}
}

func TestACPUnexpectedExitDuringTurn(t *testing.T) {
	s, rec, _ := openFake(t, fakeOpts{agent: "grok", fixture: "grok", scenario: "exit"}, protocol.AgentChatOpen{})
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "go"})
	e := rec.waitKind(t, protocol.EventError).Error
	if e.Kind != protocol.ErrKindExit || e.ExitCode == nil || *e.ExitCode != 3 {
		t.Fatalf("error = %+v", e)
	}
	if !strings.Contains(e.Stderr, "fatal: model backend broke") || strings.Contains(e.Stderr, "abcdefghijklmnopqrstuv") || !strings.Contains(e.Stderr, "[REDACTED]") {
		t.Errorf("stderr not captured/redacted: %q", e.Stderr)
	}
	if !strings.Contains(e.Message, "code 3") {
		t.Errorf("message = %q", e.Message)
	}
	if end := turnEnd(t, rec); end.StopReason != "error" {
		t.Errorf("stop = %q", end.StopReason)
	}
	// The partial message is still delivered.
	if m := rec.waitKind(t, protocol.EventMessage); m.Text != "about to fail" {
		t.Errorf("partial message = %q", m.Text)
	}
}

func TestACPTurnErrorsClassified(t *testing.T) {
	cases := []struct {
		scenario, kind string
		check          func(*testing.T, *protocol.AgentChatError)
	}{
		{"exit0_auth", protocol.ErrKindAuth, func(t *testing.T, e *protocol.AgentChatError) {
			if e.Relogin == nil || !strings.HasSuffix(e.Relogin.Command, " login") {
				t.Errorf("relogin = %+v", e.Relogin)
			}
		}},
		{"usage", protocol.ErrKindUsageLimit, func(t *testing.T, e *protocol.AgentChatError) {
			if e.ResetsAt == nil || time.Until(*e.ResetsAt) < 2*time.Hour || time.Until(*e.ResetsAt) > 2*time.Hour+28*time.Minute || e.Retryable {
				t.Errorf("usage error = %+v", e)
			}
		}},
		{"auth", protocol.ErrKindAuth, nil},
		{"rate", protocol.ErrKindRateLimit, func(t *testing.T, e *protocol.AgentChatError) {
			if !e.Retryable {
				t.Error("rate limit not retryable")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.scenario, func(t *testing.T) {
			s, rec, _ := openFake(t, fakeOpts{agent: "cursor", fixture: "cursor", scenario: tc.scenario}, protocol.AgentChatOpen{})
			_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "go"})
			e := rec.waitKind(t, protocol.EventError).Error
			if e.Kind != tc.kind {
				t.Fatalf("kind = %s, want %s (%+v)", e.Kind, tc.kind, e)
			}
			if tc.check != nil {
				tc.check(t, e)
			}
			rec.wait(t, "failed", func(ev protocol.AgentChatEvent) bool { return ev.Status == protocol.ChatStatusFailed })
		})
	}
}

func TestACPCrashOnStartRedactsStderr(t *testing.T) {
	acct, _ := fakeAccount(t, fakeOpts{agent: "agy", scenario: "crash_on_start"})
	eng, _ := NewACPEngine("agy")
	rec := newRecorder()
	if _, err := eng.Open(context.Background(), acct, protocol.AgentChatOpen{Cwd: t.TempDir()}, rec.emit); err == nil {
		t.Fatal("expected failure")
	}
	e := rec.waitKind(t, protocol.EventError).Error
	if e.Kind != protocol.ErrKindExit || e.ExitCode == nil || *e.ExitCode != 2 || strings.Contains(e.Stderr, "sk-abcdefghijklmnop") || !strings.Contains(e.Stderr, "panic: cannot start") {
		t.Fatalf("error = %+v", e)
	}
}

func TestACPNotInstalled(t *testing.T) {
	isolateHome(t)
	eng, _ := NewACPEngine("grok")
	acct := Account{ID: "grok", Agent: "grok", Binary: filepath.Join(t.TempDir(), "no-such-grok")}
	rec := newRecorder()
	if _, err := eng.Open(context.Background(), acct, protocol.AgentChatOpen{Cwd: t.TempDir()}, rec.emit); err == nil {
		t.Fatal("expected failure")
	}
	if e := rec.waitKind(t, protocol.EventError).Error; e.Kind != protocol.ErrKindNotFound {
		t.Fatalf("error = %+v", e)
	}
	if got := eng.Probe(context.Background(), acct); got.ChatSupported || got.Auth.State != protocol.AuthUnknown {
		t.Errorf("probe = %+v", got)
	}
}

func TestACPOpenAppliesModelEffortModeAndSystemPrompt(t *testing.T) {
	_, rec, logPath := openFake(t, fakeOpts{agent: "agy", fixture: "agy", scenario: "basic"}, protocol.AgentChatOpen{
		Model: "gemini-3.8-flash", Effort: "low", ApprovalMode: "accept-edits", SystemPrompt: "You are the orchestrator.",
		MCPServers: []protocol.AgentMCPServer{{Name: "k", URL: "http://x/mcp"}},
	})
	ready := rec.waitKind(t, protocol.EventSessionReady)
	if ready.Model != "gemini-3.8-flash" || ready.Effort != "low" || ready.ApprovalMode != "accept-edits" {
		t.Errorf("ready = %+v", ready)
	}
	log := readLog(t, logPath)
	for _, want := range []string{
		`"modelId":"gemini-3.8-flash-low"`, `"modeId":"accept-edits"`,
		`"_meta":{"antigravity":{"systemPrompt":{"append":"You are the orchestrator."}}}`,
		`"mcpServers":[]`, // agy does not advertise mcpCapabilities.http
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %s:\n%s", want, log)
		}
	}
}

func TestACPSystemPromptPrependedAndAttachments(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "shot.png")
	txt := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(png, []byte("\x89PNG\r\n\x1a\nfake"), 0o600)
	_ = os.WriteFile(txt, []byte("hello notes"), 0o600)
	s, rec, _ := openFake(t, fakeOpts{agent: "grok", fixture: "grok", scenario: "echo"}, protocol.AgentChatOpen{Cwd: dir, SystemPrompt: "Be brief."})
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "look", Attachments: []string{png, "notes.txt"}})
	turnEnd(t, rec)
	msg := rec.waitKind(t, protocol.EventMessage).Text
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(msg), &blocks); err != nil || len(blocks) != 3 {
		t.Fatalf("prompt blocks = %s (%v)", msg, err)
	}
	if !strings.Contains(blocks[0]["text"].(string), "Be brief.") || !strings.HasSuffix(blocks[0]["text"].(string), "look") {
		t.Errorf("system prompt not prepended: %v", blocks[0])
	}
	if blocks[1]["type"] != "image" || blocks[1]["mimeType"] != "image/png" || blocks[2]["type"] != "resource" {
		t.Errorf("attachments = %v", blocks[1:])
	}
	// Only the first prompt carries the system prompt.
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t2", Text: "again"})
	rec.wait(t, "t2 end", func(ev protocol.AgentChatEvent) bool { return ev.Kind == protocol.EventTurnEnd && ev.TurnID == "t2" })
	for _, ev := range rec.all() {
		if ev.Kind == protocol.EventMessage && ev.TurnID == "t2" && strings.Contains(ev.Text, "Be brief.") {
			t.Error("system prompt repeated")
		}
	}
}

func TestACPControlLiveModelEffortAndMode(t *testing.T) {
	// grok: model via set_model, effort via the reasoning_effort config option.
	s, rec, logPath := openFake(t, fakeOpts{agent: "grok", fixture: "grok", scenario: "basic"}, protocol.AgentChatOpen{})
	rec.waitKind(t, protocol.EventSessionReady)
	if err := s.Control(context.Background(), protocol.AgentChatControl{Model: "grok-4.7", Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	ready := rec.wait(t, "second ready", func(ev protocol.AgentChatEvent) bool {
		return ev.Kind == protocol.EventSessionReady && ev.Model == "grok-4.7"
	})
	if ready.Effort != "low" || ready.ApprovalMode != "default" {
		t.Errorf("ready = %+v", ready)
	}
	log := readLog(t, logPath)
	for _, want := range []string{`"modelId":"grok-4.7"`, `"configId":"reasoning_effort"`, `"value":"low"`} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %s", want)
		}
	}
	if err := s.Control(context.Background(), protocol.AgentChatControl{Model: "bogus-model"}); err == nil {
		t.Error("unknown model accepted")
	}
	if err := s.Control(context.Background(), protocol.AgentChatControl{ApprovalMode: "nope"}); err == nil {
		t.Error("unknown approval accepted")
	}

	// agy: effort is a model variant; modes switch live with set_mode.
	a, arec, alog := openFake(t, fakeOpts{agent: "agy", fixture: "agy", scenario: "basic"}, protocol.AgentChatOpen{})
	arec.waitKind(t, protocol.EventSessionReady)
	if err := a.Control(context.Background(), protocol.AgentChatControl{Effort: "low", ApprovalMode: "plan"}); err != nil {
		t.Fatal(err)
	}
	ready = arec.wait(t, "agy ready", func(ev protocol.AgentChatEvent) bool {
		return ev.Kind == protocol.EventSessionReady && ev.ApprovalMode == "plan"
	})
	if ready.Model != "gemini-4-neon" || ready.Effort != "low" {
		t.Errorf("agy ready = %+v", ready)
	}
	log = readLog(t, alog)
	for _, want := range []string{`"modelId":"gemini-4-neon-low"`, `"modeId":"plan"`} {
		if !strings.Contains(log, want) {
			t.Errorf("agy log lacks %s", want)
		}
	}
}

func TestACPControlLaunchFlagRestartsWithLoad(t *testing.T) {
	s, rec, logPath := openFake(t, fakeOpts{agent: "copilot", fixture: "copilot", scenario: "basic"}, protocol.AgentChatOpen{})
	rec.waitKind(t, protocol.EventSessionReady)
	sid := s.AgentSessionID()
	if err := s.Control(context.Background(), protocol.AgentChatControl{Model: "gpt-6-luna", Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "restart notice", func(ev protocol.AgentChatEvent) bool {
		return ev.Kind == protocol.EventStatus && strings.Contains(ev.Text, "next message")
	})
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	turnEnd(t, rec)
	log := readLog(t, logPath)
	if !strings.Contains(log, "argv --acp --model gpt-6-luna --reasoning-effort low") {
		t.Errorf("restart argv missing:\n%s", log)
	}
	if !strings.Contains(log, `session/load {"cwd"`) || !strings.Contains(log, sid) {
		t.Errorf("session/load missing:\n%s", log)
	}
	if s.AgentSessionID() != sid {
		t.Errorf("session id changed: %s → %s", sid, s.AgentSessionID())
	}
	// Replayed history must not surface as new deltas.
	for _, ev := range rec.all() {
		if strings.Contains(ev.Text, "old answer") {
			t.Errorf("replayed update leaked: %+v", ev)
		}
	}
}

func TestACPResume(t *testing.T) {
	// agy advertises sessionCapabilities.resume → session/resume.
	_, rec, logPath := openFake(t, fakeOpts{agent: "agy", fixture: "agy"}, protocol.AgentChatOpen{AgentSessionID: "prev-123"})
	if r := rec.waitKind(t, protocol.EventSessionReady); r.AgentSessionID != "prev-123" {
		t.Errorf("ready = %+v", r)
	}
	if log := readLog(t, logPath); !strings.Contains(log, "session/resume") || strings.Contains(log, "session/new") {
		t.Errorf("resume not used:\n%s", log)
	}
	// A missing session falls back to session/new with a note.
	_, rec2, logPath2 := openFake(t, fakeOpts{agent: "cursor", fixture: "cursor"}, protocol.AgentChatOpen{AgentSessionID: "missing"})
	rec2.wait(t, "fallback note", func(ev protocol.AgentChatEvent) bool { return strings.Contains(ev.Text, "Could not resume") })
	if log := readLog(t, logPath2); !strings.Contains(log, "session/load") || !strings.Contains(log, "session/new") {
		t.Errorf("fallback missing:\n%s", log)
	}
}

func TestACPRefusesFsRequests(t *testing.T) {
	s, rec, _ := openFake(t, fakeOpts{agent: "grok", fixture: "grok", scenario: "fsrequest"}, protocol.AgentChatOpen{})
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "x"})
	turnEnd(t, rec)
	if m := rec.waitKind(t, protocol.EventMessage); !strings.Contains(m.Text, "-32601") {
		t.Errorf("fs request answered with %q", m.Text)
	}
}

func TestACPWatchdog(t *testing.T) {
	oldW, oldT := acpWatchdog, acpWatchdogTick
	acpWatchdog, acpWatchdogTick = 200*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { acpWatchdog, acpWatchdogTick = oldW, oldT })
	s, rec, _ := openFake(t, fakeOpts{agent: "grok", fixture: "grok", scenario: "hang"}, protocol.AgentChatOpen{})
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "x"})
	if e := rec.waitKind(t, protocol.EventError).Error; e.Kind != protocol.ErrKindTimeout || !e.Retryable {
		t.Fatalf("watchdog error = %+v", e)
	}
	time.Sleep(300 * time.Millisecond)
	if n := rec.count(protocol.EventError); n != 1 {
		t.Errorf("watchdog fired %d times", n)
	}
	_ = s.Control(context.Background(), protocol.AgentChatControl{Cancel: true})
	if end := turnEnd(t, rec); end.StopReason != "cancelled" {
		t.Errorf("stop = %q", end.StopReason)
	}
}

func TestACPIdleExitThenRestart(t *testing.T) {
	s, rec, logPath := openFake(t, fakeOpts{agent: "grok", fixture: "grok", scenario: "basic"}, protocol.AgentChatOpen{})
	rec.waitKind(t, protocol.EventSessionReady)
	s.mu.Lock()
	proc := s.conn.proc
	s.mu.Unlock()
	proc.Kill(0)
	if e := rec.waitKind(t, protocol.EventError).Error; e.Kind != protocol.ErrKindExit {
		t.Fatalf("idle exit = %+v", e)
	}
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if end := turnEnd(t, rec); end.StopReason != "end_turn" {
		t.Errorf("stop = %q", end.StopReason)
	}
	// grok advertises sessionCapabilities.resume.
	if !strings.Contains(readLog(t, logPath), "session/resume") {
		t.Error("restart did not resume")
	}
}

func TestACPCloseKillsProcess(t *testing.T) {
	s, _, _ := openFake(t, fakeOpts{agent: "grok", fixture: "grok"}, protocol.AgentChatOpen{})
	s.mu.Lock()
	proc := s.conn.proc
	s.mu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !proc.Exited() {
		t.Fatal("process still running after Close")
	}
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{Text: "x"}); err == nil {
		t.Error("Prompt after Close succeeded")
	}
	_ = s.Close() // idempotent
}

func TestACPProbe(t *testing.T) {
	acct, logPath := fakeAccount(t, fakeOpts{agent: "grok", fixture: "grok"})
	eng, _ := NewACPEngine("grok")
	got := eng.Probe(context.Background(), acct)
	if got.Auth.State != protocol.AuthOK || got.Version != "1.0.92" || got.DefaultModel != "grok-4.5" || got.ModelsSource != "discovered" || len(got.Models) != 4 || !got.ChatSupported {
		t.Fatalf("probe = %+v", got)
	}
	if len(got.Approvals) != 4 || got.DefaultApproval != "default" || got.Approvals[3].Risk != 4 {
		t.Errorf("approvals = %+v", got.Approvals)
	}
	_ = eng.Probe(context.Background(), acct) // cached
	log := readLog(t, logPath)
	if n := strings.Count(log, "initialize "); n != 1 {
		t.Errorf("initialize ran %d times (cache miss)", n)
	}
	if !strings.Contains(log, "session/close") || strings.Contains(log, "session/prompt") {
		t.Errorf("probe traffic:\n%s", log)
	}

	lo, _ := fakeAccount(t, fakeOpts{agent: "cursor", fixture: "cursor_logged_out"})
	lo.ID = "cursor-lo"
	ceng, _ := NewACPEngine("cursor")
	got = ceng.Probe(context.Background(), lo)
	if got.Auth.State != protocol.AuthLoggedOut || got.Auth.Relogin == nil || !strings.HasSuffix(got.Auth.Relogin.Command, " login") {
		t.Fatalf("logged-out probe = %+v", got.Auth)
	}

	cp, _ := fakeAccount(t, fakeOpts{agent: "copilot", fixture: "copilot"})
	peng, _ := NewACPEngine("copilot")
	got = peng.Probe(context.Background(), cp)
	if got.Auth.State != protocol.AuthOK || got.ModelsSource != "fallback" || got.DefaultModel != "auto" || len(got.Approvals) != 4 {
		t.Fatalf("copilot probe = %+v", got)
	}
}

func TestACPRemoveSessionFiles(t *testing.T) {
	home := isolateHome(t)
	cwd := filepath.Join(t.TempDir(), acpProbePrefix+"x")
	_ = os.MkdirAll(cwd, 0o700)
	acct := Account{Env: map[string]string{"HOME": home}}
	mk := func(p string) string {
		_ = os.MkdirAll(p, 0o700)
		_ = os.WriteFile(filepath.Join(p, "meta.json"), []byte("{}"), 0o600)
		return p
	}
	g := mk(filepath.Join(home, ".grok", "sessions", url.PathEscape(cwd), "sess-1"))
	c := mk(filepath.Join(home, ".cursor", "acp-sessions", "sess-1"))
	cp := mk(filepath.Join(home, ".copilot", "session-state", "sess-1"))
	other := mk(filepath.Join(home, ".cursor", "acp-sessions", "sess-2"))
	grokAdapter.removeSessionFiles(acct, "sess-1", cwd)
	cursorAdapter.removeSessionFiles(acct, "sess-1", cwd)
	copilotAdapter.removeSessionFiles(acct, "sess-1", cwd)
	for _, p := range []string{g, filepath.Dir(g), c, cp} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s not removed", p)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("unrelated session removed")
	}
	if got := cursorAdapter.removeSessionFiles(acct, "../etc", cwd); got != nil {
		t.Errorf("unsafe id accepted: %v", got)
	}
}

func TestACPApprovalArgv(t *testing.T) {
	cases := map[string]map[string]string{
		"grok": {
			"plan":           "agent stdio",
			"default":        "agent stdio",
			"auto":           "agent stdio",
			"always-approve": "agent --always-approve stdio",
		},
		"agy": {
			"plan": "acp", "default": "acp", "accept-edits": "acp", "auto": "acp",
			"skip-permissions": "acp --dangerously-skip-permissions",
		},
		"copilot": {"plan": "--acp", "agent": "--acp", "allow-all": "--acp", "autopilot": "--acp"},
		"cursor":  {"ask": "acp", "plan": "acp", "agent": "acp", "full-access": "--force acp"},
	}
	for agent, modes := range cases {
		ad := acpAdapters[agent]
		if len(ad.Approvals) != len(modes) {
			t.Errorf("%s: %d approval modes, test covers %d", agent, len(ad.Approvals), len(modes))
		}
		prev := -1
		for _, ap := range ad.Approvals {
			want, ok := modes[ap.Mode.ID]
			if !ok {
				t.Errorf("%s: untested mode %s", agent, ap.Mode.ID)
				continue
			}
			l := acpLaunch{}
			if ap.Launch {
				l.Approval = ap.Mode.ID
			}
			if got := strings.Join(ad.argv(l), " "); got != want {
				t.Errorf("%s/%s argv = %q, want %q", agent, ap.Mode.ID, got, want)
			}
			if ap.Mode.Risk < prev {
				t.Errorf("%s: modes not risk-ordered at %s", agent, ap.Mode.ID)
			}
			prev = ap.Mode.Risk
			if ap.Mode.Label == "" || ap.Mode.Icon == "" || ap.Mode.Description == "" {
				t.Errorf("%s/%s incomplete: %+v", agent, ap.Mode.ID, ap.Mode)
			}
		}
		if _, ok := ad.approval(ad.DefaultApproval); !ok {
			t.Errorf("%s: default approval %q missing", agent, ad.DefaultApproval)
		}
	}
	if got := strings.Join(copilotAdapter.argv(acpLaunch{Model: "auto", Effort: "high"}), " "); got != "--acp --reasoning-effort high" {
		t.Errorf("copilot model argv = %q", got)
	}
}

func TestACPGrokApprovalViaSessionMeta(t *testing.T) {
	s, rec, logPath := openFake(t, fakeOpts{agent: "grok", fixture: "grok", scenario: "basic"}, protocol.AgentChatOpen{})
	rec.waitKind(t, protocol.EventSessionReady)
	if err := s.Control(context.Background(), protocol.AgentChatControl{ApprovalMode: "always-approve"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	turnEnd(t, rec)
	log := readLog(t, logPath)
	if !strings.Contains(log, "argv agent --always-approve stdio") || !strings.Contains(log, `"_meta":{"yoloMode":true}`) || !strings.Contains(log, "session/resume") {
		t.Errorf("always-approve not applied via restart + resume:\n%s", log)
	}
	ready := rec.wait(t, "ready after restart", func(ev protocol.AgentChatEvent) bool {
		return ev.Kind == protocol.EventSessionReady && ev.ApprovalMode == "always-approve"
	})
	if ready.AgentSessionID != s.AgentSessionID() {
		t.Errorf("ready = %+v", ready)
	}
}
