package chat

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

const codexAccountOK = `{"account":{"type":"chatgpt","email":"user@example.com","planType":"pro","workspaceName":null},"userId":null,"requiresOpenaiAuth":true}`

const codexRateLimitsOK = `{"ordinaryUsageAllowed":true,"rateLimits":{"limitId":"codex","limitName":null,"normalModelSlug":null,"primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":1795440000},"secondary":{"usedPercent":81,"windowDurationMins":10080,"resetsAt":1795900000},"credits":null,"individualLimit":null,"spendControlReached":null,"planType":"pro","rateLimitReachedType":null},"rateLimitsByLimitId":null,"rateLimitResetCredits":null,"accountId":null,"rateLimitUpsell":null}`

type fakeCodex struct {
	t        *testing.T
	loggedIn bool
	// onTurn runs after turn/start is answered.
	onTurn func(p *ccFakeProc, n int)
	mu     sync.Mutex
	calls  []map[string]any
	turns  int
	// responses to server requests, by id
	responses chan map[string]any
}

func (f *fakeCodex) fixture(name string) json.RawMessage {
	b, err := os.ReadFile("testdata/codex/" + name)
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *fakeCodex) handler(p *ccFakeProc, msg map[string]any) {
	method, _ := msg["method"].(string)
	id, hasID := msg["id"]
	if method == "" && hasID {
		f.responses <- msg
		return
	}
	if !hasID {
		return // notification (initialized)
	}
	f.mu.Lock()
	f.calls = append(f.calls, msg)
	f.mu.Unlock()
	reply := func(result any) { p.send(map[string]any{"id": id, "result": result}) }
	switch method {
	case "initialize":
		reply(f.fixture("initialize_result.json"))
	case "account/read":
		if f.loggedIn {
			reply(json.RawMessage(codexAccountOK))
		} else {
			reply(f.fixture("account_read_logged_out.json"))
		}
	case "model/list":
		reply(f.fixture("model_list.json"))
	case "account/rateLimits/read":
		if f.loggedIn {
			reply(json.RawMessage(codexRateLimitsOK))
		} else {
			p.send(map[string]any{"id": id, "error": f.fixture("rate_limits_logged_out_error.json")})
		}
	case "thread/start", "thread/resume":
		tid := "thr_1"
		if params, _ := msg["params"].(map[string]any); params["threadId"] != nil {
			tid = params["threadId"].(string)
		}
		reply(map[string]any{"thread": map[string]any{"id": tid}, "model": "gpt-6.2-sol", "reasoningEffort": "low"})
	case "turn/start":
		f.mu.Lock()
		f.turns++
		n := f.turns
		f.mu.Unlock()
		reply(map[string]any{"turn": map[string]any{"id": "turn_1", "items": []any{}, "status": "inProgress"}})
		if f.onTurn != nil {
			go f.onTurn(p, n)
		}
	case "turn/interrupt":
		reply(map[string]any{})
		p.send(`{"method":"turn/completed","params":{"threadId":"thr_1","turn":{"id":"turn_1","items":[],"status":"interrupted","error":null}}}`)
	default:
		p.send(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "nope"}})
	}
}

func (f *fakeCodex) lastCall(method string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i]["method"] == method {
			return f.calls[i]
		}
	}
	return nil
}

func newTestCodex(t *testing.T, loggedIn bool) (*codexEngine, *fakeCodex, *ccFakeLauncher) {
	t.Helper()
	f := &fakeCodex{t: t, loggedIn: loggedIn, responses: make(chan map[string]any, 8)}
	l := &ccFakeLauncher{handler: f.handler}
	e := newCodexEngine()
	e.launch = l.launch
	status := struct {
		out  string
		code int
	}{"Not logged in", 1}
	if loggedIn {
		status.out, status.code = "Logged in using ChatGPT", 0
	}
	e.run = fakeRunner(map[string]struct {
		out  string
		code int
	}{"login status": status, "--version": {"codex-cli 0.171.0\n", 0}})
	e.probeDir = t.TempDir()
	e.cancelGrace = 2 * time.Second
	return e, f, l
}

func TestCodexProbeLoggedOut(t *testing.T) {
	e, _, _ := newTestCodex(t, false)
	a := e.Probe(context.Background(), Account{ID: "codex", Env: map[string]string{"CODEX_HOME": "/h/.codex"}})
	if a.Version != "0.171.0" || a.Auth.State != protocol.AuthLoggedOut || a.Auth.Relogin == nil ||
		a.Auth.Relogin.Command != "codex login --device-auth" || a.Auth.Relogin.Env["CODEX_HOME"] != "/h/.codex" {
		t.Fatalf("auth = %+v", a.Auth)
	}
	if a.ModelsSource != "discovered" || a.DefaultModel != "gpt-6.2-sol" || len(a.Models) < 5 {
		t.Fatalf("models = %s %s %d", a.ModelsSource, a.DefaultModel, len(a.Models))
	}
	for _, m := range a.Models {
		if strings.Contains(m.ID, "daybreak") || m.ID == "codex-auto-review" {
			t.Fatalf("hidden model listed: %s", m.ID)
		}
	}
	if len(a.Models[0].Efforts) == 0 || a.Models[0].DefaultEffort == "" {
		t.Fatalf("efforts = %+v", a.Models[0])
	}
	if len(a.Usage) != 0 || a.UsageNote == "" || len(a.Approvals) != 5 || a.DefaultApproval != "on-request" {
		t.Fatalf("usage/approvals = %+v %q %+v", a.Usage, a.UsageNote, a.Approvals)
	}
	for i, m := range a.Approvals {
		if m.Risk != i {
			t.Fatalf("approval modes not risk-ordered: %+v", a.Approvals)
		}
	}
}

func TestCodexProbeLoggedIn(t *testing.T) {
	e, _, _ := newTestCodex(t, true)
	a := e.Probe(context.Background(), Account{ID: "codex"})
	if a.Auth.State != protocol.AuthOK || a.Auth.Method != "oauth" || a.Plan != "pro" {
		t.Fatalf("account = %+v", a)
	}
	if len(a.Usage) != 2 || a.Usage[0].Window != "five_hour" || a.Usage[0].UsedPercent != 25 || a.Usage[1].Window != "seven_day" || a.Usage[1].State != "warning" || a.Usage[1].ResetsAt == nil {
		t.Fatalf("usage = %+v", a.Usage)
	}
}

func TestCodexOpenLoggedOutFailsFast(t *testing.T) {
	e, _, l := newTestCodex(t, false)
	rec := newCCRecorder()
	if _, err := e.Open(context.Background(), Account{ID: "codex"}, protocol.AgentChatOpen{}, rec.emit); err == nil {
		t.Fatal("Open succeeded while logged out")
	}
	ev, _ := rec.wait(t, isKind(protocol.EventError))
	if ev.Error.Kind != protocol.ErrKindAuth || ev.Error.Relogin == nil || ev.Error.Relogin.Command != "codex login --device-auth" {
		t.Fatalf("error = %+v", ev.Error)
	}
	if l.count() != 0 {
		t.Fatal("app-server launched although logged out")
	}
}

func TestCodexOpenAccountNullFailsFast(t *testing.T) {
	e, f, l := newTestCodex(t, true)
	f.loggedIn = false // login status says yes, account/read says no
	rec := newCCRecorder()
	if _, err := e.Open(context.Background(), Account{ID: "codex"}, protocol.AgentChatOpen{}, rec.emit); err == nil {
		t.Fatal("Open succeeded with account null")
	}
	ev, _ := rec.wait(t, isKind(protocol.EventError))
	if ev.Error.Kind != protocol.ErrKindAuth {
		t.Fatalf("error = %+v", ev.Error)
	}
	if f.lastCall("thread/start") != nil {
		t.Fatal("thread started without an account")
	}
	if _, stderr := l.last().Wait(); stderr != "" {
		t.Fatal("unexpected stderr")
	}
}

func openCodex(t *testing.T, e *codexEngine, opts protocol.AgentChatOpen) (Session, *ccRecorder) {
	t.Helper()
	rec := newCCRecorder()
	s, err := e.Open(context.Background(), Account{ID: "codex", Label: "Codex"}, opts, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ready, _ := rec.wait(t, isKind(protocol.EventSessionReady))
	if ready.AgentSessionID != "thr_1" || ready.Model == "" {
		t.Fatalf("ready = %+v", ready)
	}
	return s, rec
}

func TestCodexTurnWithApproval(t *testing.T) {
	e, f, _ := newTestCodex(t, true)
	lines := readFixtureLines(t, "testdata/codex/turn_exec.jsonl")
	f.onTurn = func(p *ccFakeProc, _ int) {
		for _, l := range lines {
			if strings.Contains(l, "__wait_response") {
				r := <-f.responses
				if r["result"].(map[string]any)["decision"] != "accept" {
					t.Errorf("approval answer = %v", r)
				}
				continue
			}
			p.send(l)
		}
	}
	s, rec := openCodex(t, e, protocol.AgentChatOpen{Cwd: "/work", ApprovalMode: "untrusted", Model: "gpt-6.2-astra", Effort: "high",
		SystemPrompt: "Be brief.", MCPServers: []protocol.AgentMCPServer{{Name: "konsert", URL: "http://hub/mcp", Headers: map[string]string{"Authorization": "Bearer x"}}}})
	ts := f.lastCall("thread/start")["params"].(map[string]any)
	if ts["approvalPolicy"] != "untrusted" || ts["sandbox"] != "workspace-write" || ts["developerInstructions"] != "Be brief." ||
		ts["config"].(map[string]any)["mcp_servers"].(map[string]any)["konsert"].(map[string]any)["url"] != "http://hub/mcp" {
		t.Fatalf("thread/start = %v", ts)
	}
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "list files"}); err != nil {
		t.Fatal(err)
	}
	tsp := f.lastCall("turn/start")["params"].(map[string]any)
	sp := tsp["sandboxPolicy"].(map[string]any)
	if tsp["model"] != "gpt-6.2-astra" || tsp["effort"] != "high" || tsp["approvalPolicy"] != "untrusted" || sp["type"] != "workspaceWrite" || tsp["threadId"] != "thr_1" {
		t.Fatalf("turn/start = %v", tsp)
	}
	el, before := rec.wait(t, isKind(protocol.EventElicitation))
	if el.Elicitation.Kind != protocol.ElicitPermission || el.Elicitation.ToolCall.Title != "Run: ls -la" || len(el.Elicitation.Options) != 3 {
		t.Fatalf("elicitation = %+v", el.Elicitation)
	}
	if err := s.Respond(context.Background(), protocol.AgentChatRespond{ElicitationID: el.Elicitation.ID, OptionID: "allow_once"}); err != nil {
		t.Fatal(err)
	}
	end, seen := rec.wait(t, isKind(protocol.EventTurnEnd))
	seen = append(before, seen...)
	if end.StopReason != "end_turn" || end.TurnID != "t1" {
		t.Fatalf("end = %+v", end)
	}
	var deltas, thought, msg string
	var tools []protocol.AgentToolCall
	var usage *protocol.AgentTokenUsage
	var plan []protocol.AgentPlanItem
	var limits []protocol.AgentRateLimit
	for _, ev := range seen {
		switch ev.Kind {
		case protocol.EventMessageDelta:
			deltas += ev.Text
		case protocol.EventThoughtDelta:
			thought += ev.Text
		case protocol.EventMessage:
			msg = ev.Text
		case protocol.EventToolCall:
			tools = append(tools, *ev.ToolCall)
		case protocol.EventUsage:
			usage = ev.Usage
		case protocol.EventPlan:
			plan = ev.Plan
		case protocol.EventRateLimit:
			limits = append(limits, *ev.RateLimit)
		}
	}
	if deltas != "Two files." || msg != "Two files." || thought != "Listing files." {
		t.Fatalf("deltas=%q msg=%q thought=%q", deltas, msg, thought)
	}
	if len(tools) != 2 || tools[0].Status != "running" || tools[1].Status != "completed" || tools[1].Output != "a.txt\nb.txt\n" || tools[1].Name != "exec" {
		t.Fatalf("tools = %+v", tools)
	}
	if usage == nil || usage.InputTokens != 1000 || usage.OutputTokens != 200 || usage.CacheReadTokens != 800 {
		t.Fatalf("usage = %+v", usage)
	}
	if len(plan) != 2 || plan[0].Status != "in_progress" {
		t.Fatalf("plan = %+v", plan)
	}
	if len(limits) != 2 || limits[0].Window != "five_hour" || limits[1].Window != "seven_day" {
		t.Fatalf("limits = %+v", limits)
	}
}

func TestCodexAuthErrorStopsRetryLoop(t *testing.T) {
	e, f, l := newTestCodex(t, true)
	f.onTurn = func(p *ccFakeProc, _ int) {
		p.send(`{"method":"error","params":{"threadId":"thr_1","turnId":"turn_1","willRetry":true,"error":{"message":"Reconnecting... 2/5 (unexpected status 401 Unauthorized: Missing bearer or basic authentication in header)","codexErrorInfo":{"responseStreamDisconnected":{"httpStatusCode":401}},"additionalDetails":null}}}`)
	}
	s, rec := openCodex(t, e, protocol.AgentChatOpen{Cwd: "/w"})
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "hi"})
	ev, _ := rec.wait(t, isKind(protocol.EventError))
	if ev.Error.Kind != protocol.ErrKindAuth || ev.Error.Relogin == nil || ev.Error.Retryable {
		t.Fatalf("error = %+v", ev.Error)
	}
	// The process is killed instead of retrying.
	done := make(chan struct{})
	go func() { l.last().Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("app-server still running after 401")
	}
}

func TestCodexUsageLimit(t *testing.T) {
	e, f, _ := newTestCodex(t, true)
	f.onTurn = func(p *ccFakeProc, _ int) {
		p.send(`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":1795440000},"secondary":{"usedPercent":60,"windowDurationMins":10080,"resetsAt":1795900000},"planType":"plus","rateLimitReachedType":"rate_limit_reached"}}}`)
		p.send(`{"method":"turn/completed","params":{"threadId":"thr_1","turn":{"id":"turn_1","items":[],"status":"failed","error":{"message":"You've hit your usage limit.","codexErrorInfo":"usageLimitExceeded","additionalDetails":null}}}}`)
	}
	s, rec := openCodex(t, e, protocol.AgentChatOpen{Cwd: "/w"})
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "hi"})
	ev, seen := rec.wait(t, isKind(protocol.EventError))
	ce := ev.Error
	if ce.Kind != protocol.ErrKindUsageLimit || ce.Window != "five_hour" || ce.ResetsAt == nil || ce.ResetsAt.Unix() != 1795440000 || !strings.HasPrefix(ce.Message, "Codex is out of 5-hour usage until") {
		t.Fatalf("error = %+v", ce)
	}
	var exhausted bool
	for _, e := range seen {
		if e.RateLimit != nil && e.RateLimit.Window == "five_hour" && e.RateLimit.State == "exhausted" {
			exhausted = true
		}
	}
	if !exhausted {
		t.Fatal("no exhausted rate_limit event")
	}
}

func TestCodexCancelAndControl(t *testing.T) {
	e, f, _ := newTestCodex(t, true)
	f.onTurn = func(p *ccFakeProc, n int) {
		p.send(`{"method":"item/agentMessage/delta","params":{"threadId":"thr_1","turnId":"turn_1","itemId":"m","delta":"one"}}`)
		if n == 2 {
			p.send(`{"method":"turn/completed","params":{"threadId":"thr_1","turn":{"id":"turn_1","items":[],"status":"completed","error":null}}}`)
		}
	}
	s, rec := openCodex(t, e, protocol.AgentChatOpen{Cwd: "/w"})
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "count"})
	rec.wait(t, isKind(protocol.EventMessageDelta))
	if err := s.Control(context.Background(), protocol.AgentChatControl{Cancel: true}); err != nil {
		t.Fatal(err)
	}
	end, _ := rec.wait(t, isKind(protocol.EventTurnEnd))
	if end.StopReason != "cancelled" {
		t.Fatalf("end = %+v", end)
	}
	if ip := f.lastCall("turn/interrupt"); ip == nil || ip["params"].(map[string]any)["turnId"] != "turn_1" {
		t.Fatalf("interrupt = %v", ip)
	}
	// Model / effort / approval apply to the next turn/start.
	if err := s.Control(context.Background(), protocol.AgentChatControl{Model: "gpt-5.5", Effort: "xhigh", ApprovalMode: "full-access"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t2", Text: "again"})
	rec.wait(t, isKind(protocol.EventTurnEnd))
	p := f.lastCall("turn/start")["params"].(map[string]any)
	if p["model"] != "gpt-5.5" || p["effort"] != "xhigh" || p["approvalPolicy"] != "never" || p["sandboxPolicy"].(map[string]any)["type"] != "dangerFullAccess" {
		t.Fatalf("turn/start = %v", p)
	}
}

func TestCodexFileChangeApprovalAlways(t *testing.T) {
	e, f, _ := newTestCodex(t, true)
	f.onTurn = func(p *ccFakeProc, _ int) {
		p.send(`{"method":"item/started","params":{"threadId":"thr_1","turnId":"turn_1","startedAtMs":1,"item":{"type":"fileChange","id":"fc_1","changes":[{"path":"/w/a.go","kind":{"type":"update","move_path":null},"diff":"@@ -1 +1 @@\n-a\n+b\n"}],"status":"inProgress"}}}`)
		p.send(`{"id":"req-9","method":"item/fileChange/requestApproval","params":{"threadId":"thr_1","turnId":"turn_1","itemId":"fc_1","startedAtMs":2,"reason":null,"grantRoot":null}}`)
		r := <-f.responses
		if r["id"] != "req-9" || r["result"].(map[string]any)["decision"] != "acceptForSession" {
			t.Errorf("answer = %v", r)
		}
		p.send(`{"method":"turn/completed","params":{"threadId":"thr_1","turn":{"id":"turn_1","items":[],"status":"completed","error":null}}}`)
	}
	s, rec := openCodex(t, e, protocol.AgentChatOpen{Cwd: "/w"})
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t1", Text: "edit"})
	el, _ := rec.wait(t, isKind(protocol.EventElicitation))
	if el.Elicitation.ToolCall == nil || el.Elicitation.ToolCall.Title != "Edit: /w/a.go" || el.Elicitation.ToolCall.Paths[0] != "/w/a.go" {
		t.Fatalf("elicitation = %+v", el.Elicitation.ToolCall)
	}
	_ = s.Respond(context.Background(), protocol.AgentChatRespond{ElicitationID: el.Elicitation.ID, OptionID: "allow_always"})
	rec.wait(t, isKind(protocol.EventTurnEnd))
}

func TestCodexLimitsAndErrorInfo(t *testing.T) {
	name, status := codexErrorInfo(json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":401}}`))
	if name != "httpConnectionFailed" || status != 401 {
		t.Fatalf("%s %d", name, status)
	}
	name, _ = codexErrorInfo(json.RawMessage(`"usageLimitExceeded"`))
	if name != "usageLimitExceeded" {
		t.Fatal(name)
	}
	mins := int64(43200)
	reached := "rate_limit_reached"
	l := codexLimits(&codexRateSnapshot{Primary: &codexRateWindow{UsedPercent: 90, WindowDurationMins: &mins}, RateLimitReachedType: &reached})
	if len(l) != 1 || l[0].Window != "monthly" || l[0].State != "exhausted" {
		t.Fatalf("limits = %+v", l)
	}
	if a, sb := codexMode("read-only"); a != "on-request" || sb != "read-only" {
		t.Fatal(a, sb)
	}
	if codexModeID("bogus") != "on-request" {
		t.Fatal("unknown mode not defaulted")
	}
}
