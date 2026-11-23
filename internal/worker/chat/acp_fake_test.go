package chat

// A scripted fake ACP agent. The test binary re-executes itself with KONSERT_FAKE_ACP=1
// (handled in init, so no TestMain is needed) and speaks line-delimited JSON-RPC on
// stdio. FAKE_ACP_FIXTURE picks testdata/acp/<name>.json for initialize/session/new;
// FAKE_ACP_SCENARIO scripts session/prompt; FAKE_ACP_LOG records every request.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func init() {
	if os.Getenv("KONSERT_FAKE_ACP") == "1" {
		runFakeACP()
		os.Exit(0)
	}
}

type fakeAgent struct {
	out      *bufio.Writer
	mu       sync.Mutex
	log      *os.File
	fixture  map[string]json.RawMessage
	scenario string
	nextID   int
	replies  map[string]chan json.RawMessage
	cancel   chan struct{}
	session  string
}

func runFakeACP() {
	f := &fakeAgent{out: bufio.NewWriter(os.Stdout), scenario: os.Getenv("FAKE_ACP_SCENARIO"),
		replies: map[string]chan json.RawMessage{}, cancel: make(chan struct{}, 1), session: "sess-1"}
	if p := os.Getenv("FAKE_ACP_LOG"); p != "" {
		f.log, _ = os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		f.logf("argv %s", strings.Join(os.Args[1:], " "))
	}
	if name := os.Getenv("FAKE_ACP_FIXTURE"); name != "" {
		b, err := os.ReadFile(filepath.Join(os.Getenv("FAKE_ACP_TESTDATA"), name+".json"))
		if err == nil {
			_ = json.Unmarshal(b, &f.fixture)
		}
	}
	if f.scenario == "crash_on_start" {
		fmt.Fprintln(os.Stderr, "panic: cannot start (api_key=sk-abcdefghijklmnop)")
		os.Exit(2)
	}
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := in.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			f.dispatch(line)
		}
		if err != nil {
			return
		}
	}
}

func (f *fakeAgent) logf(format string, a ...any) {
	if f.log != nil {
		fmt.Fprintf(f.log, format+"\n", a...)
	}
}

func (f *fakeAgent) send(v any) {
	b, _ := json.Marshal(v)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out.Write(b)
	f.out.WriteByte('\n')
	f.out.Flush()
}

func (f *fakeAgent) result(id json.RawMessage, v any) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": v})
}

func (f *fakeAgent) fail(id json.RawMessage, code int, msg string, data any) {
	e := map[string]any{"code": code, "message": msg}
	if data != nil {
		e["data"] = data
	}
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": e})
}

func (f *fakeAgent) update(u map[string]any) {
	f.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": f.session, "update": u}})
}

func (f *fakeAgent) chunk(kind, text string) {
	f.update(map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": text}})
}

// request sends an agent→client request and waits for the reply.
func (f *fakeAgent) request(method string, params any) json.RawMessage {
	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("srv-%d", f.nextID)
	ch := make(chan json.RawMessage, 1)
	f.replies[id] = ch
	f.mu.Unlock()
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		return nil
	}
}

func (f *fakeAgent) fixtureResult(key string) (json.RawMessage, json.RawMessage) {
	var msg struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if raw, ok := f.fixture[key]; ok {
		_ = json.Unmarshal(raw, &msg)
	}
	return msg.Result, msg.Error
}

func (f *fakeAgent) dispatch(line []byte) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &m) != nil {
		return
	}
	if m.Method == "" { // a reply to one of our requests
		var id string
		_ = json.Unmarshal(m.ID, &id)
		f.mu.Lock()
		ch := f.replies[id]
		delete(f.replies, id)
		f.mu.Unlock()
		if ch != nil {
			if m.Error != nil {
				ch <- m.Error
			} else {
				ch <- m.Result
			}
		}
		return
	}
	f.logf("%s %s", m.Method, string(m.Params))
	switch m.Method {
	case "initialize":
		if f.scenario == "init_auth" {
			f.fail(m.ID, -32000, "Authentication required", "Not signed in. To authenticate run: grok login")
			return
		}
		if res, _ := f.fixtureResult("initialize"); res != nil {
			f.result(m.ID, res)
			return
		}
		f.result(m.ID, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{
			"loadSession": true, "promptCapabilities": map[string]any{"image": true, "embeddedContext": true},
			"mcpCapabilities": map[string]any{"http": true}, "sessionCapabilities": map[string]any{"close": map[string]any{}},
		}})
	case "session/new":
		res, errObj := f.fixtureResult("session_new")
		if errObj != nil {
			f.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": errObj})
			return
		}
		if res == nil {
			res, _ = json.Marshal(map[string]any{"sessionId": f.session})
		}
		var r map[string]any
		_ = json.Unmarshal(res, &r)
		if sid, ok := r["sessionId"].(string); ok {
			f.session = sid
		}
		// A notification before the response (agents do this) must not deadlock the client.
		f.update(map[string]any{"sessionUpdate": "available_commands_update", "availableCommands": []any{map[string]any{"name": "compact"}}})
		f.result(m.ID, res)
	case "session/load", "session/resume":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.SessionID == "missing" {
			f.fail(m.ID, -32002, "Resource not found", "session missing")
			return
		}
		f.session = p.SessionID
		if m.Method == "session/load" {
			f.chunk("user_message_chunk", "old question")
			f.chunk("agent_message_chunk", "old answer")
		}
		f.result(m.ID, map[string]any{})
	case "session/set_mode":
		var p struct {
			ModeID string `json:"modeId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		f.update(map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": p.ModeID})
		f.result(m.ID, map[string]any{})
	case "session/set_model":
		var p struct {
			ModelID string `json:"modelId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if strings.Contains(p.ModelID, "bogus") {
			f.fail(m.ID, -32602, "Invalid params", map[string]any{"message": "Invalid model value: " + p.ModelID})
			return
		}
		f.result(m.ID, map[string]any{})
	case "session/set_config_option":
		f.update(map[string]any{"sessionUpdate": "config_option_update", "configOptions": []any{}})
		f.result(m.ID, map[string]any{})
	case "session/close":
		f.result(m.ID, map[string]any{})
	case "session/cancel":
		select {
		case f.cancel <- struct{}{}:
		default:
		}
	case "session/prompt":
		go f.prompt(m.ID, m.Params)
	default:
		if len(m.ID) > 0 {
			f.fail(m.ID, -32601, "Method not found", nil)
		}
	}
}

func (f *fakeAgent) prompt(id json.RawMessage, params json.RawMessage) {
	switch f.scenario {
	case "basic", "":
		f.chunk("agent_thought_chunk", "thinking…")
		f.chunk("agent_message_chunk", "Hel")
		f.chunk("agent_message_chunk", "lo")
		f.update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call-1", "title": "Read main.go", "kind": "read",
			"status": "pending", "rawInput": map[string]any{"path": "/w/main.go"}, "locations": []any{map[string]any{"path": "/w/main.go"}}})
		f.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call-1", "status": "completed",
			"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "package main"}}}})
		f.update(map[string]any{"sessionUpdate": "plan", "entries": []any{
			map[string]any{"content": "Read code", "priority": "high", "status": "completed"},
			map[string]any{"content": "Reply", "priority": "medium", "status": "in_progress"}}})
		f.chunk("agent_message_chunk", " world")
		f.send(map[string]any{"jsonrpc": "2.0", "method": "_vendor/ignored", "params": map[string]any{}})
		f.result(id, map[string]any{"stopReason": "end_turn", "usage": map[string]any{"inputTokens": 10, "outputTokens": 3}})
	case "fsrequest":
		r := f.request("fs/read_text_file", map[string]any{"sessionId": f.session, "path": "/etc/hosts"})
		f.chunk("agent_message_chunk", "fs reply: "+string(r))
		f.result(id, map[string]any{"stopReason": "end_turn"})
	case "permission":
		f.update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call-w", "title": "Write hello.txt", "kind": "edit", "status": "pending"})
		r := f.request("session/request_permission", map[string]any{
			"sessionId": f.session,
			"toolCall":  map[string]any{"toolCallId": "call-w", "title": "Write hello.txt", "kind": "edit", "rawInput": map[string]any{"path": "hello.txt", "content": "hi"}},
			"options": []any{
				map[string]any{"optionId": "allow", "name": "Allow once", "kind": "allow_once"},
				map[string]any{"optionId": "always", "name": "Always allow", "kind": "allow_always"},
				map[string]any{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
			}})
		var out struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		_ = json.Unmarshal(r, &out)
		f.chunk("agent_message_chunk", "outcome="+out.Outcome.Outcome+":"+out.Outcome.OptionID)
		f.result(id, map[string]any{"stopReason": "end_turn"})
	case "cancel":
		f.chunk("agent_message_chunk", "working")
		select {
		case <-f.cancel:
			f.result(id, map[string]any{"stopReason": "cancelled"})
		case <-time.After(20 * time.Second):
			f.result(id, map[string]any{"stopReason": "end_turn"})
		}
	case "cancel_permission":
		r := f.request("session/request_permission", map[string]any{"sessionId": f.session,
			"toolCall": map[string]any{"toolCallId": "c"}, "options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}}})
		f.chunk("agent_message_chunk", "perm="+string(r))
		<-f.cancel
		f.result(id, map[string]any{"stopReason": "cancelled"})
	case "exit":
		f.chunk("agent_message_chunk", "about to fail")
		fmt.Fprintln(os.Stderr, "fatal: model backend broke (Authorization: Bearer abcdefghijklmnopqrstuv)")
		os.Exit(3)
	case "exit0_auth":
		fmt.Fprintln(os.Stderr, "Error: Authentication required. Please run 'agent login' first.")
		os.Exit(0)
	case "usage":
		f.fail(id, -32603, "Internal error", "Individual quota reached for gemini-4-neon. Resets in 2h27m4s")
	case "auth":
		f.fail(id, -32000, "Authentication required", nil)
	case "rate":
		f.fail(id, -32603, "Internal error", "429 Too Many Requests: model is overloaded")
	case "hang":
		<-f.cancel
		f.result(id, map[string]any{"stopReason": "cancelled"})
	case "echo":
		var p struct {
			Prompt []map[string]any `json:"prompt"`
		}
		_ = json.Unmarshal(params, &p)
		b, _ := json.Marshal(p.Prompt)
		f.chunk("agent_message_chunk", string(b))
		f.result(id, map[string]any{"stopReason": "end_turn"})
	default:
		f.result(id, map[string]any{"stopReason": f.scenario})
	}
}
