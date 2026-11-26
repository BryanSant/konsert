package controller

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/store/sqlite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// chatServer is a full controller Server (REST, WebSockets, per-chat MCP) over
// httptest with --no-auth, reusing the testHub dialing helpers.
type chatServer struct {
	*testHub
	srv *Server
}

func newChatServer(t *testing.T) *chatServer {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	srv.hub.hostname = func() string { return "hub-host" }
	th := &testHub{h: srv.hub, st: st, svc: srv.service, ts: httptest.NewServer(srv)}
	t.Cleanup(th.stop)
	return &chatServer{testHub: th, srv: srv}
}

func (cs *chatServer) api(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, cs.ts.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (cs *chatServer) createChat(t *testing.T, body map[string]any) store.ChatSession {
	t.Helper()
	code, raw := cs.api(t, http.MethodPost, "/api/chats", body)
	if code != http.StatusCreated {
		t.Fatalf("create chat: %d %s", code, raw)
	}
	var s store.ChatSession
	_ = json.Unmarshal(raw, &s)
	if !ids.IsUUIDv7(s.ID) || !ids.Valid(ids.Chat, s.SID) {
		t.Fatalf("chat id %q / sid %q: want a UUIDv7 and a c- sid", s.ID, s.SID)
	}
	return s
}

func claudeAccount(id string, def bool) protocol.AgentAccount {
	return protocol.AgentAccount{
		ID: id, Agent: "claude", Label: strings.ToUpper(id[:1]) + id[1:], IsDefault: def, ChatSupported: true,
		Auth:         protocol.AgentAuthStatus{State: protocol.AuthOK},
		DefaultModel: "opus",
		Models: []protocol.AgentModel{{ID: "opus", DisplayName: "Opus", DefaultEffort: "medium",
			Efforts: []protocol.AgentEffort{{ID: "low", Label: "Low"}, {ID: "medium", Label: "Medium"}}}},
		Approvals:       []protocol.AgentApprovalMode{{ID: "default", Label: "Ask"}, {ID: "plan", Label: "Plan"}},
		DefaultApproval: "default",
	}
}

// connectAgentWorker connects a fake worker and reports accounts.
func (cs *chatServer) connectAgentWorker(t *testing.T, id, name string, accts ...protocol.AgentAccount) *fakeWorker {
	t.Helper()
	fw := cs.connectWorker(t, testWorkerInfo(id, name), nil, id)
	fw.send(protocol.MsgAgentCapabilities, protocol.AgentCapabilitiesReport{Accounts: accts})
	waitFor(t, 3*time.Second, "accounts stored", func() bool {
		recs, _ := cs.st.ListAgentAccounts(context.Background(), id)
		return len(recs) == len(accts)
	})
	return fw
}

func expectChatEvent(t *testing.T, fw *fakeWorker, kind string) protocol.AgentChatEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		env := fw.expect(protocol.MsgAgentChatEvent, time.Until(deadline))
		ev, _ := protocol.DecodePayload[protocol.AgentChatEvent](env)
		if ev.Kind == kind {
			return ev
		}
	}
	t.Fatalf("no agent.chat.event of kind %s", kind)
	return protocol.AgentChatEvent{}
}

func decodeOpen(t *testing.T, fw *fakeWorker) protocol.AgentChatOpen {
	t.Helper()
	open, err := protocol.DecodePayload[protocol.AgentChatOpen](fw.expect(protocol.MsgAgentChatOpen, 3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return open
}

func mustSession(t *testing.T, st store.Store, id string) store.ChatSession {
	t.Helper()
	s, err := st.GetChatSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAgentChatRoundTripPersistenceAndResume(t *testing.T) {
	cs := newChatServer(t)
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	chat := cs.createChat(t, map[string]any{"title": "hello"})
	if chat.Backend != store.ChatBackendAgent || chat.WorkerID != "w1" || chat.Agent != "claude" || chat.AccountID != "claude" ||
		chat.Model != "opus" || chat.ApprovalMode != "default" || chat.Effort != "medium" {
		t.Fatalf("defaults not taken from capabilities: %+v", chat)
	}
	b := cs.dialBrowser(t)
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat.ID, Content: "hi"})

	open := decodeOpen(t, w)
	if open.ChatID != chat.ID || open.Agent != "claude" || open.Model != "opus" || open.AgentSessionID != "" ||
		!strings.Contains(open.SystemPrompt, "ask_user") || len(open.MCPServers) != 1 {
		t.Fatalf("unexpected open: %+v", open)
	}
	mcpSrv := open.MCPServers[0]
	if mcpSrv.Name != "syre" || mcpSrv.URL != "/mcp/chat/"+chat.ID || !mcpSrv.Relay || !strings.HasPrefix(mcpSrv.Headers["Authorization"], "Bearer ") {
		t.Fatalf("unexpected MCP server: %+v", mcpSrv)
	}
	prompt, _ := protocol.DecodePayload[protocol.AgentChatPrompt](w.expect(protocol.MsgAgentChatPrompt, 3*time.Second))
	if prompt.Text != "hi" || prompt.TurnID == "" {
		t.Fatalf("unexpected prompt: %+v", prompt)
	}
	if ev := expectChatEvent(t, b, eventUserMessage); ev.Text != "hi" || ev.Seq != 1 || ev.TurnID != prompt.TurnID {
		t.Fatalf("user message event: %+v", ev)
	}

	turn := prompt.TurnID
	evs := []protocol.AgentChatEvent{
		{Seq: 1, Kind: protocol.EventSessionReady, AgentSessionID: "sess-1"},
		{Seq: 2, Kind: protocol.EventStatus, Status: protocol.ChatStatusWorking, TurnID: turn},
		{Seq: 3, Kind: protocol.EventMessageDelta, Text: "Hel", TurnID: turn},
		{Seq: 4, Kind: protocol.EventMessageDelta, Text: "lo", TurnID: turn},
		{Seq: 5, Kind: protocol.EventMessage, Text: "Hello", TurnID: turn},
		{Seq: 6, Kind: protocol.EventTurnEnd, StopReason: "end_turn", TurnID: turn},
		{Seq: 6, Kind: protocol.EventTurnEnd, StopReason: "end_turn", TurnID: turn}, // duplicate
	}
	for _, ev := range evs {
		ev.ChatID = chat.ID
		w.send(protocol.MsgAgentChatEvent, ev)
	}
	end := expectChatEvent(t, b, protocol.EventTurnEnd)
	if end.Seq != 7 {
		t.Fatalf("turn_end hub seq = %d, want 7", end.Seq)
	}
	if extra := b.drain(protocol.MsgAgentChatEvent, 200*time.Millisecond); len(extra) != 0 {
		t.Fatalf("duplicate worker event was re-broadcast: %d frames", len(extra))
	}
	s := mustSession(t, cs.st, chat.ID)
	if s.Status != protocol.ChatStatusDone || s.AgentSessionID != "sess-1" {
		t.Fatalf("session after turn: %+v", s)
	}

	// Transcript: deltas coalesced away, message kept.
	code, raw := cs.api(t, http.MethodGet, "/api/chats/"+chat.ID+"/events?after_seq=0", nil)
	var tr struct {
		Events  []protocol.AgentChatEvent `json:"events"`
		LastSeq int64                     `json:"last_seq"`
	}
	_ = json.Unmarshal(raw, &tr)
	var kinds []string
	for _, e := range tr.Events {
		kinds = append(kinds, e.Kind)
	}
	if code != 200 || strings.Join(kinds, ",") != "user_message,session_ready,status,message,turn_end" || tr.LastSeq != 7 {
		t.Fatalf("transcript %d %v last=%d", code, kinds, tr.LastSeq)
	}
	_, raw = cs.api(t, http.MethodGet, "/api/chats/"+chat.ID+"/events?after_seq=5", nil)
	_ = json.Unmarshal(raw, &tr)
	if len(tr.Events) != 2 || tr.Events[0].Seq != 6 {
		t.Fatalf("catch-up after_seq=5: %+v", tr.Events)
	}

	// Second turn starts, then the worker connection drops mid-turn.
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat.ID, Content: "more"})
	decodeOpen(t, w)
	w.expect(protocol.MsgAgentChatPrompt, 3*time.Second)
	w.close()
	waitFor(t, 3*time.Second, "worker to disconnect", func() bool { return !cs.h.IsWorkerConnected("w1") })
	w2 := cs.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	lost := expectChatEvent(t, b, protocol.EventError)
	if lost.Error == nil || lost.Error.Kind != protocol.ErrKindExit {
		t.Fatalf("connection lost event: %+v", lost)
	}
	if s := mustSession(t, cs.st, chat.ID); s.Status != protocol.ChatStatusFailed || !strings.Contains(s.LastErrorJSON, `"exit"`) {
		t.Fatalf("session after connection loss: %+v", s)
	}
	// The next message resumes the CLI session.
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat.ID, Content: "again"})
	if open := decodeOpen(t, w2); open.AgentSessionID != "sess-1" {
		t.Fatalf("resume open: %+v", open)
	}
}

func TestAgentChatLoggedOutRefusalAndExhaustedHint(t *testing.T) {
	cs := newChatServer(t)
	out := claudeAccount("claude2", false)
	out.Auth = protocol.AgentAuthStatus{State: protocol.AuthLoggedOut, Relogin: &protocol.AgentRelogin{Command: "claude /login", Env: map[string]string{"CLAUDE_CONFIG_DIR": "~/.claude2"}}}
	resets := time.Now().Add(3 * time.Hour).UTC()
	tired := claudeAccount("claude3", false)
	tired.Usage = []protocol.AgentRateLimit{{Window: "seven_day", UsedPercent: 100, State: "exhausted", ResetsAt: &resets}}
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true), out, tired)
	b := cs.dialBrowser(t)

	// Logged out: auth error with relogin, nothing sent to the worker.
	chat := cs.createChat(t, map[string]any{"account_id": "claude2"})
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat.ID, Content: "hi"})
	ev := expectChatEvent(t, b, protocol.EventError)
	if ev.Error == nil || ev.Error.Kind != protocol.ErrKindAuth || ev.Error.Relogin == nil || ev.Error.Relogin.Command != "claude /login" {
		t.Fatalf("logged-out refusal: %+v", ev.Error)
	}
	if got := w.drain(protocol.MsgAgentChatOpen, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("agent.chat.open sent for a logged-out account")
	}

	// Exhausted with a reset: placement substitutes the logged-in account and does not hint rate_limit.
	chat3 := cs.createChat(t, map[string]any{"account_id": "claude3"})
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat3.ID, Content: "hi"})
	open := decodeOpen(t, w)
	if open.Agent != "claude" || open.AccountID != "claude" {
		t.Fatalf("substituted open: %+v", open)
	}
	prompt, _ := protocol.DecodePayload[protocol.AgentChatPrompt](w.expect(protocol.MsgAgentChatPrompt, 3*time.Second))
	// A usage_limit error is persisted on the session.
	w.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: chat3.ID, Seq: 1, TurnID: prompt.TurnID, Kind: protocol.EventError,
		Error: &protocol.AgentChatError{Kind: protocol.ErrKindUsageLimit, Message: "limit", Window: "seven_day", ResetsAt: &resets}})
	deadline := time.Now().Add(3 * time.Second)
	sawError := false
	for time.Now().Before(deadline) && !sawError {
		ev, err := protocol.DecodePayload[protocol.AgentChatEvent](b.expect(protocol.MsgAgentChatEvent, time.Until(deadline)))
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == protocol.EventRateLimit {
			t.Fatalf("rate_limit after substitution: %+v", ev)
		}
		if ev.Kind == protocol.EventError && ev.Error != nil && ev.Error.Kind == protocol.ErrKindUsageLimit {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("usage_limit error was not delivered")
	}
	if s := mustSession(t, cs.st, chat3.ID); s.Status != protocol.ChatStatusFailed || !strings.Contains(s.LastErrorJSON, `"usage_limit"`) || s.AccountID != "claude" {
		t.Fatalf("usage_limit not persisted on the substituted account: %+v", s)
	}

	// /api/agents/accounts exposes auth and exhausted state.
	_, raw := cs.api(t, http.MethodGet, "/api/agents/accounts", nil)
	var res struct {
		Accounts []AgentAccountView `json:"accounts"`
	}
	_ = json.Unmarshal(raw, &res)
	byID := map[string]AgentAccountView{}
	for _, a := range res.Accounts {
		byID[a.ID] = a
	}
	if len(res.Accounts) != 3 || !byID["claude3"].Exhausted || byID["claude3"].ExhaustedUntil == nil || byID["claude"].Exhausted ||
		byID["claude2"].Auth.State != protocol.AuthLoggedOut || !byID["claude"].WorkerOnline || byID["claude"].WorkerName != "box" {
		t.Fatalf("accounts view: %s", raw)
	}
}

func TestExplicitChatKeepsUsageHeldAccount(t *testing.T) {
	cs := newChatServer(t)
	resets := time.Now().Add(2 * time.Hour).UTC()
	tired := func(id string) protocol.AgentAccount {
		a := claudeAccount(id, id == "mock")
		a.Agent = "mock"
		a.Usage = []protocol.AgentRateLimit{{Window: "five_hour", State: "exhausted", ResetsAt: &resets}}
		return a
	}
	w := cs.connectAgentWorker(t, "w1", "box", tired("mock"), tired("mock2"))
	b := cs.dialBrowser(t)
	chat := cs.createChat(t, map[string]any{"account_id": "mock2"})
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat.ID, Content: "still send"})
	open := decodeOpen(t, w)
	if open.AccountID != "mock2" || open.Agent != "mock" {
		t.Fatalf("open: %+v", open)
	}
	prompt, _ := protocol.DecodePayload[protocol.AgentChatPrompt](w.expect(protocol.MsgAgentChatPrompt, 3*time.Second))
	if prompt.Text != "still send" {
		t.Fatalf("prompt: %+v", prompt)
	}

	// An operator disable still refuses the chat.
	until := time.Now().Add(time.Hour).UTC()
	mustDirective(t, cs.h, DirectiveInput{Kind: store.DirectiveProviderDisable, Target: "mock", Until: &until})
	chat2 := cs.createChat(t, map[string]any{"account_id": "mock2"})
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat2.ID, Content: "nope"})
	ev := expectChatEvent(t, b, protocol.EventError)
	if ev.Error == nil || ev.Error.Kind != protocol.ErrKindUsageLimit || !strings.Contains(ev.Error.Message, "disabled") {
		t.Fatalf("operator disable: %+v", ev.Error)
	}
	if got := w.drain(protocol.MsgAgentChatOpen, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("agent.chat.open sent for an operator-disabled agent")
	}
}

func TestAgentCapabilitiesBroadcastAndRefresh(t *testing.T) {
	cs := newChatServer(t)
	b := cs.dialBrowser(t)
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	env := b.expect(protocol.MsgFleetUpdate, 3*time.Second)
	for {
		var p struct {
			Kind     string             `json:"kind"`
			WorkerID string             `json:"worker_id"`
			Accounts []AgentAccountView `json:"accounts"`
		}
		_ = json.Unmarshal(env.Payload, &p)
		if p.Kind == "agent_accounts" {
			// The PWA applies account updates per worker and ignores pushes without
			// worker_id (SYR-PYG35: live login/usage changes never reached the UI).
			if p.WorkerID != "w1" || len(p.Accounts) != 1 || p.Accounts[0].WorkerID != "w1" {
				t.Fatalf("agent_accounts update: %s", env.Payload)
			}
			break
		}
		env = b.expect(protocol.MsgFleetUpdate, 3*time.Second)
	}
	code, raw := cs.api(t, http.MethodPost, "/api/agents/accounts/refresh", map[string]string{"worker_id": "w1", "account_id": "claude"})
	if code != http.StatusAccepted {
		t.Fatalf("refresh: %d %s", code, raw)
	}
	var p agentCapabilitiesRefresh
	_ = json.Unmarshal(w.expect(protocol.MsgAgentCapabilitiesRefresh, 3*time.Second).Payload, &p)
	if p.AccountID != "claude" {
		t.Fatalf("refresh payload %+v", p)
	}
	if code, _ := cs.api(t, http.MethodPost, "/api/agents/accounts/refresh", map[string]string{"worker_id": "nope"}); code != http.StatusConflict {
		t.Fatalf("refresh unknown worker: %d", code)
	}
}

// chatBearerTransport adds an Authorization header to every request.
type chatBearerTransport struct{ tok string }

func (b chatBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func connectChatMCP(t *testing.T, url, tok string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: &http.Client{Transport: chatBearerTransport{tok}}, MaxRetries: -1,
	}, nil)
}

func openChat(t *testing.T, cs *chatServer, w, b *fakeWorker, body map[string]any) (store.ChatSession, protocol.AgentChatOpen, string) {
	t.Helper()
	chat := cs.createChat(t, body)
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat.ID, Content: "go"})
	open := decodeOpen(t, w)
	prompt, _ := protocol.DecodePayload[protocol.AgentChatPrompt](w.expect(protocol.MsgAgentChatPrompt, 3*time.Second))
	return chat, open, prompt.TurnID
}

func toolText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestAskUserBlocksUntilAnswerAndTokenAuth(t *testing.T) {
	cs := newChatServer(t)
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)
	chat, open, turn := openChat(t, cs, w, b, nil)
	url := w.mcpURL(open.MCPServers[0])
	tok := strings.TrimPrefix(open.MCPServers[0].Headers["Authorization"], "Bearer ")

	// Wrong token: 401.
	resp := doReq(t, http.MethodPost, url, strings.NewReader(`{}`), map[string]string{"Authorization": "Bearer nope", "Content-Type": "application/json"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	resp = doReq(t, http.MethodPost, url, strings.NewReader(`{}`), map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}

	sess, err := connectChatMCP(t, url, tok)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	// Every agent chat gets ask_user plus the human-control tools; no fleet tools.
	if want := []string{"ask_user", "recent_events", "release_hold", "request_user_action"}; !slices.Equal(names, want) {
		t.Fatalf("non-fleet chat tools: %v, want %v", names, want)
	}

	type result struct {
		text string
		err  error
	}
	// An under-specified question is sent back with what to add.
	bad, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "ask_user", Arguments: map[string]any{
		"question": "Which DB?", "options": []string{"postgres", "sqlite"},
	}})
	if err != nil || !bad.IsError || !strings.Contains(toolText(bad), "context") || !strings.Contains(toolText(bad), "consequence") {
		t.Fatalf("under-specified ask_user: %v %+v", err, toolText(bad))
	}
	done := make(chan result, 1)
	go func() {
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "ask_user", Arguments: map[string]any{
			"question": "Which DB?", "context": "Picking the store for the new cache service.", "why_now": "The schema work waits on it.",
			"options": []map[string]any{{"label": "postgres", "consequence": "Shared server"}, {"label": "sqlite", "consequence": "Embedded file"}},
		}})
		if err != nil {
			done <- result{err: err}
			return
		}
		done <- result{text: toolText(res)}
	}()
	q := expectChatEvent(t, b, protocol.EventElicitation)
	if q.Elicitation == nil || q.Elicitation.Kind != protocol.ElicitQuestion || q.Elicitation.Title != "Which DB?" || len(q.Elicitation.Options) != 2 || q.TurnID != turn {
		t.Fatalf("question event: %+v", q)
	}
	if s := mustSession(t, cs.st, chat.ID); s.Status != protocol.ChatStatusNeedsYou {
		t.Fatalf("status while asking: %q", s.Status)
	}
	select {
	case r := <-done:
		t.Fatalf("ask_user returned before the answer: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	b.send(msgChatRespond, chatRespondRequest{SessionID: chat.ID, ElicitationID: q.Elicitation.ID, OptionID: q.Elicitation.Options[1].ID})
	select {
	case r := <-done:
		if r.err != nil || r.text != `The user chose: "sqlite".` {
			t.Fatalf("ask_user result: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ask_user did not return after the answer")
	}
	expectChatEvent(t, b, protocol.EventElicitationOK)
	if got := w.drain(protocol.MsgAgentChatRespond, 100*time.Millisecond); len(got) != 0 {
		t.Fatal("hub ask_user answer was forwarded to the worker")
	}

	// Audit: the response is recorded with the option id only.
	evs, _ := cs.st.ListAudit(context.Background(), store.AuditFilter{Action: AuditChatRespond})
	if len(evs) != 1 || !strings.Contains(evs[0].MetaJSON, `"option_id":"option_2"`) || strings.Contains(evs[0].MetaJSON, "sqlite") || evs[0].Source != auditSourceWS {
		t.Fatalf("respond audit: %+v", evs)
	}

	// Timeout: the question stays open; the agent is told the answer comes as a note.
	cs.h.AskUserTimeout = 150 * time.Millisecond
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "ask_user", Arguments: map[string]any{
		"question": "Still there?", "context": "Checking in.", "why_now": "Nothing else to do."}})
	if err != nil || !strings.Contains(toolText(res), "still open") {
		t.Fatalf("timeout result: %v %v", toolText(res), err)
	}
	if qs := cs.h.pendingQuestions(context.Background(), chat.ID); len(qs) != 1 || !qs[0].Abandoned {
		t.Fatalf("abandoned question: %+v", qs)
	}

	// A cross-site Origin does not block the bearer-authenticated endpoint.
	resp = doReq(t, http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), map[string]string{
		"Authorization": "Bearer " + tok, "Content-Type": "application/json", "Accept": "application/json, text/event-stream",
		"Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site",
	})
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("bearer MCP request with Origin rejected: %d", resp.StatusCode)
	}

	// Deleting the chat revokes the token.
	if code, raw := cs.api(t, http.MethodDelete, "/api/chats/"+chat.ID, nil); code != 200 {
		t.Fatalf("delete: %d %s", code, raw)
	}
	w.expect(protocol.MsgAgentChatClose, 3*time.Second)
	resp = doReq(t, http.MethodPost, url, strings.NewReader(`{}`), map[string]string{"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token after delete: %d", resp.StatusCode)
	}
}

// A chat's MCP token only reaches its own chat's tools, and only through the link of
// the machine the chat runs on; the hub serves no per-chat MCP over HTTP (SYR-DTN92).
func TestRelayedChatMCPAuthIsolation(t *testing.T) {
	cs := newChatServer(t)
	w1 := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	w2 := cs.connectAgentWorker(t, "w2", "pc", claudeAccount("claude", true))
	b := cs.dialBrowser(t)

	open := func(workerID string, w *fakeWorker) (store.ChatSession, protocol.AgentMCPServer, string) {
		t.Helper()
		chat := cs.createChat(t, map[string]any{"worker_id": workerID})
		b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat.ID, Content: "go"})
		o := decodeOpen(t, w)
		if o.ChatID != chat.ID || len(o.MCPServers) != 1 || !o.MCPServers[0].Relay {
			t.Fatalf("open for %s: %+v", workerID, o)
		}
		return chat, o.MCPServers[0], strings.TrimPrefix(o.MCPServers[0].Headers["Authorization"], "Bearer ")
	}
	chatA, srvA, tokA := open("w1", w1)
	chatB, srvB, tokB := open("w1", w1)
	_, srvC, tokC := open("w2", w2)

	// Each chat's own token works through its machine's relay.
	for _, tc := range []struct {
		w   *fakeWorker
		srv protocol.AgentMCPServer
		tok string
	}{{w1, srvA, tokA}, {w1, srvB, tokB}, {w2, srvC, tokC}} {
		sess, err := connectChatMCP(t, tc.w.mcpURL(tc.srv), tc.tok)
		if err != nil {
			t.Fatalf("own token on %s: %v", tc.srv.URL, err)
		}
		if _, err := sess.ListTools(context.Background(), nil); err != nil {
			t.Fatalf("list tools on %s: %v", tc.srv.URL, err)
		}
		sess.Close()
	}

	status := func(url, tok string) int {
		t.Helper()
		hdr := map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
		if tok != "" {
			hdr["Authorization"] = "Bearer " + tok
		}
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
		resp := doReq(t, http.MethodPost, url, strings.NewReader(body), hdr)
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := status(w1.mcpURL(srvB), tokA); got != http.StatusUnauthorized {
		t.Errorf("chat A's token on chat B: %d", got)
	}
	if got := status(w1.mcpURL(srvB), ""); got != http.StatusUnauthorized {
		t.Errorf("no token: %d", got)
	}
	if got := status(w1.mcpURL(srvC), tokC); got != http.StatusUnauthorized {
		t.Errorf("chat C's own token through another machine's link: %d", got)
	}
	if got := status(w2.mcpURL(srvA), tokA); got != http.StatusUnauthorized {
		t.Errorf("chat A's own token through another machine's link: %d", got)
	}
	// The hub's HTTP listener has no per-chat MCP endpoint at all.
	// (Unknown paths fall through to the PWA shell, which is HTML, never MCP.)
	direct := doReq(t, http.MethodPost, cs.ts.URL+"/mcp/chat/"+chatA.ID, strings.NewReader(`{}`), map[string]string{
		"Authorization": "Bearer " + tokA, "Content-Type": "application/json", "Accept": "application/json, text/event-stream",
	})
	direct.Body.Close()
	if ct := direct.Header.Get("Content-Type"); direct.Header.Get("Mcp-Session-Id") != "" ||
		strings.HasPrefix(ct, "application/json") || strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("hub served per-chat MCP over HTTP: %d %s", direct.StatusCode, ct)
	}
	// Deleting the chat revokes its token.
	if code, _ := cs.api(t, http.MethodDelete, "/api/chats/"+chatB.ID, nil); code/100 != 2 {
		t.Fatalf("delete chat: %d", code)
	}
	if got := status(w1.mcpURL(srvB), tokB); got != http.StatusUnauthorized {
		t.Errorf("deleted chat's token: %d", got)
	}
}

func TestChatTokenSurvivesHubRestartViaHash(t *testing.T) {
	cs := newChatServer(t)
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)
	chat, open, _ := openChat(t, cs, w, b, nil)
	tok := strings.TrimPrefix(open.MCPServers[0].Headers["Authorization"], "Bearer ")
	// Simulate a restart: drop the in-memory state; the stored hash still verifies.
	cs.h.mu.Lock()
	delete(cs.h.agentChats, chat.ID)
	cs.h.mu.Unlock()
	if _, ok := cs.h.verifyChatToken(context.Background(), chat.ID, tok); !ok {
		t.Fatal("token not verified from the stored hash")
	}
	if _, ok := cs.h.verifyChatToken(context.Background(), chat.ID, tok+"x"); ok {
		t.Fatal("wrong token verified")
	}
	if s := mustSession(t, cs.st, chat.ID); s.MCPTokenHash == "" || strings.Contains(s.MCPTokenHash, tok) {
		t.Fatalf("token hash: %q", s.MCPTokenHash)
	}
}

func TestChatControlPatchAndAudit(t *testing.T) {
	cs := newChatServer(t)
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true), claudeAccount("claude2", false))
	b := cs.dialBrowser(t)
	chat, _, _ := openChat(t, cs, w, b, nil)

	b.send(msgChatControl, chatControlRequest{SessionID: chat.ID, Model: "sonnet", ApprovalMode: "plan"})
	ctl, _ := protocol.DecodePayload[protocol.AgentChatControl](w.expect(protocol.MsgAgentChatControl, 3*time.Second))
	if ctl.Model != "sonnet" || ctl.ApprovalMode != "plan" || ctl.Cancel {
		t.Fatalf("control: %+v", ctl)
	}
	waitFor(t, 2*time.Second, "settings persisted", func() bool {
		s := mustSession(t, cs.st, chat.ID)
		return s.Model == "sonnet" && s.ApprovalMode == "plan"
	})
	b.send(msgChatControl, chatControlRequest{SessionID: chat.ID, Cancel: true})
	if ctl, _ := protocol.DecodePayload[protocol.AgentChatControl](w.expect(protocol.MsgAgentChatControl, 3*time.Second)); !ctl.Cancel {
		t.Fatalf("cancel control: %+v", ctl)
	}

	// PATCH the account: engine closed, session id cleared, new account defaults.
	s := mustSession(t, cs.st, chat.ID)
	s.AgentSessionID = "sess-x"
	_ = cs.st.UpdateChatSession(context.Background(), s)
	code, raw := cs.api(t, http.MethodPatch, "/api/chats/"+chat.ID, map[string]any{"account_id": "claude2"})
	if code != 200 {
		t.Fatalf("patch: %d %s", code, raw)
	}
	w.expect(protocol.MsgAgentChatClose, 3*time.Second)
	s = mustSession(t, cs.st, chat.ID)
	if s.AccountID != "claude2" || s.AgentSessionID != "" || s.Model != "opus" || s.ApprovalMode != "default" {
		t.Fatalf("after account patch: %+v", s)
	}
	if code, _ := cs.api(t, http.MethodPatch, "/api/chats/"+chat.ID, map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("empty patch: %d", code)
	}

	for _, action := range []string{AuditChatModel, AuditChatApprovalMode, AuditChatAccount} {
		evs, _ := cs.st.ListAudit(context.Background(), store.AuditFilter{Action: action})
		if len(evs) == 0 || !strings.Contains(evs[0].MetaJSON, chat.ID) {
			t.Fatalf("no %s audit entry: %+v", action, evs)
		}
	}

	b.send(msgChatClose, chatCloseRequest{SessionID: chat.ID})
	w.expect(protocol.MsgAgentChatClose, 3*time.Second)

	// Permission elicitations from the worker are answered through the worker.
	b.send(msgChatRespond, chatRespondRequest{SessionID: chat.ID, ElicitationID: "perm-1", OptionID: "allow_once"})
	r, _ := protocol.DecodePayload[protocol.AgentChatRespond](w.expect(protocol.MsgAgentChatRespond, 3*time.Second))
	if r.ElicitationID != "perm-1" || r.OptionID != "allow_once" || r.ChatID != chat.ID {
		t.Fatalf("respond forwarded: %+v", r)
	}
}

func TestDefaultAgentTargetSelection(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	grok := protocol.AgentAccount{ID: "grok", Agent: "grok", IsDefault: true, ChatSupported: true, Auth: protocol.AgentAuthStatus{State: protocol.AuthOK}}
	codexOut := protocol.AgentAccount{ID: "codex", Agent: "codex", IsDefault: true, ChatSupported: true, Auth: protocol.AgentAuthStatus{State: protocol.AuthLoggedOut}}
	cs.connectAgentWorker(t, "w-a", "alpha", grok)
	cs.connectAgentWorker(t, "w-b", "beta", codexOut, claudeAccount("claude", true))

	// No worker on the hub's machine: the first worker with a logged-in claude.
	if wid, a, ok := cs.h.defaultAgentTarget(ctx); !ok || wid != "w-b" || a.ID != "claude" {
		t.Fatalf("got %s/%s %v, want w-b/claude", wid, a.ID, ok)
	}
	// A worker on the hub's own machine wins even without claude.
	cs.h.hostname = func() string { return "alpha" }
	if wid, a, ok := cs.h.defaultAgentTarget(ctx); !ok || wid != "w-a" || a.ID != "grok" {
		t.Fatalf("got %s/%s %v, want w-a/grok", wid, a.ID, ok)
	}
}

func TestDefaultTargetSkipsExhaustedAccount(t *testing.T) {
	cs := newChatServer(t)
	resets := time.Now().Add(time.Hour)
	c2 := claudeAccount("claude2", true) // default but exhausted
	c2.Usage = []protocol.AgentRateLimit{{Window: "seven_day", State: "exhausted", ResetsAt: &resets}}
	cs.connectAgentWorker(t, "w1", "hub-host", c2, claudeAccount("claude", false))
	if wid, a, ok := cs.h.defaultAgentTarget(context.Background()); !ok || wid != "w1" || a.ID != "claude" {
		t.Fatalf("got %s/%s %v, want w1/claude", wid, a.ID, ok)
	}
	// Only an exhausted account with a future reset: a system disable blocks it.
	cs2 := newChatServer(t)
	cs2.connectAgentWorker(t, "w1", "box", c2)
	if _, a, ok := cs2.h.defaultAgentTarget(context.Background()); ok {
		t.Fatalf("exhausted-only with a reset should not be a target: %s", a.ID)
	}
}

func TestCommanderIsAgentChatWithFleetTools(t *testing.T) {
	cs := newChatServer(t)
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	code, raw := cs.api(t, http.MethodPost, "/api/chats/commander", nil)
	var orch store.ChatSession
	_ = json.Unmarshal(raw, &orch)
	if code != 200 || orch.Backend != store.ChatBackendAgent || !orch.FleetTools || orch.WorkerID != "w1" || orch.Agent != "claude" {
		t.Fatalf("commander chat: %d %s", code, raw)
	}
	_, raw = cs.api(t, http.MethodPost, "/api/chats/commander", nil)
	var again store.ChatSession
	_ = json.Unmarshal(raw, &again)
	if again.ID != orch.ID {
		t.Fatal("commander chat not reused")
	}
	_, raw = cs.api(t, http.MethodGet, "/api/settings", nil)
	var settings settingsResponse
	_ = json.Unmarshal(raw, &settings)
	if settings.APIKeyRequired || settings.CommanderBackend != "agent" || settings.CommanderChatID != orch.ID || settings.LLMModel != "" {
		t.Fatalf("settings: %s", raw)
	}

	b := cs.dialBrowser(t)
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: orch.ID, Content: "status?"})
	open := decodeOpen(t, w)
	if !strings.Contains(open.SystemPrompt, "Syre Commander") || !strings.Contains(open.SystemPrompt, "ask_user") {
		t.Fatalf("commander system prompt: %q", open.SystemPrompt)
	}
	tok := strings.TrimPrefix(open.MCPServers[0].Headers["Authorization"], "Bearer ")
	sess, err := connectChatMCP(t, w.mcpURL(open.MCPServers[0]), tok)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	if !names["ask_user"] || !names["dispatch_task"] || !names["fleet_status"] || !names["set_policy"] {
		t.Fatalf("commander tools: %v", names)
	}
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "fleet_status", Arguments: map[string]any{}})
	if err != nil || res.IsError || !strings.Contains(toolText(res), "w1") {
		t.Fatalf("fleet_status via chat MCP: %v %+v", err, res)
	}
}

func TestDispatchAccountChecksAndFields(t *testing.T) {
	cs := newChatServer(t)
	out := claudeAccount("claude2", false)
	out.Auth = protocol.AgentAuthStatus{State: protocol.AuthLoggedOut, Relogin: &protocol.AgentRelogin{Command: "claude /login"}}
	resets := time.Now().Add(time.Hour)
	tired := claudeAccount("claude3", false)
	tired.Usage = []protocol.AgentRateLimit{{Window: "five_hour", State: "exhausted", ResetsAt: &resets}}
	info := testWorkerInfo("w1", "box")
	info.Agents = []string{"claude", "mock"}
	w := cs.connectWorker(t, info, nil, "w1")
	w.send(protocol.MsgAgentCapabilities, protocol.AgentCapabilitiesReport{Accounts: []protocol.AgentAccount{claudeAccount("claude", true), out, tired}})
	waitFor(t, 3*time.Second, "accounts", func() bool {
		r, _ := cs.st.ListAgentAccounts(context.Background(), "w1")
		return len(r) == 3
	})

	code, raw := cs.api(t, http.MethodPost, "/api/tasks", map[string]any{"agent": "claude", "account_id": "claude2", "repo_path": "/tmp", "prompt": "x"})
	if code != http.StatusBadRequest || !strings.Contains(string(raw), "logged out") || !strings.Contains(string(raw), "claude /login") {
		t.Fatalf("logged-out dispatch: %d %s", code, raw)
	}
	code, raw = cs.api(t, http.MethodPost, "/api/tasks", map[string]any{
		"agent": "claude", "account_id": "claude3", "approval_mode": "plan", "effort": "low", "repo_path": "/tmp", "prompt": "x",
	})
	var res DispatchResult
	_ = json.Unmarshal(raw, &res)
	if code != http.StatusCreated || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "substituted claude for claude3") || strings.Contains(res.Warnings[0], "five_hour") {
		t.Fatalf("exhausted dispatch: %d %s", code, raw)
	}
	start, _ := protocol.DecodePayload[protocol.TaskStartPayload](w.expect(protocol.MsgTaskStart, 3*time.Second))
	if start.AccountID != "claude" || start.ApprovalMode != "plan" || start.Effort != "low" {
		t.Fatalf("task.start payload: %+v", start)
	}
}

func TestValidClientTurnID(t *testing.T) {
	for _, ok := range []string{"0b5c2f1e-1a2b-4c3d-9e8f-001122334455", "turn_12345678"} {
		if !validClientTurnID(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "short", "has space in it", "a/b/c/d/e/f", strings.Repeat("x", 65)} {
		if validClientTurnID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The worker's watchdog reports a quiet turn as a timeout error but keeps the turn
// running: the chat must not be shown as failed.
func TestTimeoutErrorDoesNotFailChat(t *testing.T) {
	cs := store.ChatSession{Status: protocol.ChatStatusWorking}
	applyEventToSession(&cs, protocol.AgentChatEvent{Kind: protocol.EventError, Error: &protocol.AgentChatError{Kind: protocol.ErrKindTimeout, Message: "no progress"}})
	if cs.Status != protocol.ChatStatusWorking {
		t.Fatalf("status after watchdog timeout = %q, want working", cs.Status)
	}
	applyEventToSession(&cs, protocol.AgentChatEvent{Kind: protocol.EventError, Error: &protocol.AgentChatError{Kind: protocol.ErrKindExit}})
	if cs.Status != protocol.ChatStatusFailed {
		t.Fatalf("status after exit error = %q, want failed", cs.Status)
	}
}
