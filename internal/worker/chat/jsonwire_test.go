package chat

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/BryanSant/konsert/internal/protocol"
)

// Member names are matched case-sensitively (encoding/json/v2): a real stream-json
// line decodes, and the same line with differently cased names does not populate
// the fields (v1 matched them case-insensitively).
func TestClaudeLineCaseSensitive(t *testing.T) {
	lines := readFixtureLines(t, "testdata/claude/turn_tool.jsonl")
	initLine := lines[0] // {"type":"system","subtype":"init",...,"permissionMode":"default",...}
	var l claudeLine
	if err := unmarshal([]byte(initLine), &l); err != nil {
		t.Fatal(err)
	}
	if l.Type != "system" || l.Subtype != "init" || l.SessionID != "00000000-0000-4000-8000-000000000001" ||
		l.Model != "claude-opus-5-5" || l.PermissionMode != "default" {
		t.Fatalf("fixture line = %+v", l)
	}
	mangled := strings.NewReplacer(`"session_id"`, `"Session_ID"`, `"permissionMode"`, `"permissionmode"`).Replace(initLine)
	var m claudeLine
	if err := unmarshal([]byte(mangled), &m); err != nil {
		t.Fatal(err)
	}
	if m.SessionID != "" || m.PermissionMode != "" || m.Type != "system" {
		t.Fatalf("names must match exactly: %+v", m)
	}
}

// A real ACP session/update notification decodes with exact names.
func TestACPUpdateCaseSensitive(t *testing.T) {
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}}`
	var m rpcMessage
	if err := unmarshal([]byte(line), &m); err != nil {
		t.Fatal(err)
	}
	var p acpUpdateParams
	var u acpUpdate
	if err := unmarshal(m.Params, &p); err != nil {
		t.Fatal(err)
	}
	if err := unmarshal(p.Update, &u); err != nil {
		t.Fatal(err)
	}
	if p.SessionID != "s1" || u.SessionUpdate != "agent_message_chunk" {
		t.Fatalf("notification = %+v %+v", p, u)
	}
	var q acpUpdateParams
	var qu acpUpdate
	if err := unmarshal(jsontext.Value(`{"SessionID":"s1","update":{"SessionUpdate":"x"}}`), &q); err != nil {
		t.Fatal(err)
	}
	_ = unmarshal(q.Update, &qu)
	if q.SessionID != "" || qu.SessionUpdate != "" {
		t.Fatalf("names must match exactly: %+v %+v", q, qu)
	}
}

// Agent output with invalid UTF-8 or a duplicated member name still decodes.
func TestAgentLineLenientDecode(t *testing.T) {
	sc := ccLineReader(strings.NewReader("{\"type\":\"result\",\"result\":\"ok \xff\xfe done\"}\n{\"type\":\"a\",\"type\":\"b\"}\n"))
	var got []claudeLine
	for sc.Scan() {
		if !utf8.Valid(sc.Bytes()) {
			t.Fatalf("line not repaired: %q", sc.Bytes())
		}
		var l claudeLine
		if err := unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("decode %q: %v", sc.Bytes(), err)
		}
		got = append(got, l)
	}
	if len(got) != 2 || got[0].Result != "ok � done" || got[1].Type != "b" {
		t.Fatalf("lines = %+v", got)
	}
	// The value decoder alone (no line repair) is lenient too.
	var s string
	if err := unmarshal([]byte("\"a\xffb\""), &s); err != nil || s != "a�b" {
		t.Fatalf("invalid UTF-8 string = %q, %v", s, err)
	}
	// Outgoing messages replace invalid UTF-8 rather than failing.
	b, err := marshalLine(map[string]any{"text": "x\xffy", "b": 1, "a": 2})
	if err != nil || string(b) != `{"a":2,"b":1,"text":"x`+"�"+`y"}` {
		t.Fatalf("marshalLine = %s, %v", b, err)
	}
}

// A tool result with invalid UTF-8 in a real claude turn does not drop the line or
// end the session: the turn completes and the output carries U+FFFD.
func TestClaudeInvalidUTF8ToolOutput(t *testing.T) {
	lines := readFixtureLines(t, "testdata/claude/turn_tool.jsonl")
	for i, l := range lines {
		if strings.Contains(l, "File created successfully") {
			lines[i] = strings.Replace(l, "File created successfully", "File \xff\xfecreated successfully", 1)
		}
	}
	e, _ := newTestClaude(t, fakeClaudeOpts{onUser: func(p *ccFakeProc, _ map[string]any, _ int) { replay(p, lines) }})
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
	end, seen := rec.wait(t, isKind(protocol.EventTurnEnd))
	if end.StopReason != "end_turn" {
		t.Fatalf("turn end = %+v", end)
	}
	var out string
	for _, ev := range seen {
		if ev.Kind == protocol.EventToolCall && ev.ToolCall.Status == "completed" {
			out = ev.ToolCall.Output
		}
	}
	if !strings.Contains(out, "File �created successfully") || !utf8.ValidString(out) {
		t.Fatalf("tool output = %q", out)
	}
	// The event still encodes for the hub.
	if _, err := protocol.NewEnvelope("agent.chat.event", "", end); err != nil {
		t.Fatal(err)
	}
}

// The ACP read loop survives invalid UTF-8 and duplicate names and keeps
// dispatching later lines.
func TestRPCConnLenientLines(t *testing.T) {
	pr, pw := io.Pipe()
	got := make(chan string, 4)
	c := newRPCConn(pr, io.Discard, func(method string, _ jsontext.Value, params jsontext.Value) {
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(params, &p)
		got <- method + ":" + p.Text
	})
	w := bufio.NewWriter(pw)
	_, _ = w.WriteString("{\"jsonrpc\":\"2.0\",\"method\":\"a\",\"params\":{\"text\":\"bad \xff byte\"}}\n")
	_, _ = w.WriteString("{\"jsonrpc\":\"2.0\",\"method\":\"b\",\"method\":\"c\",\"params\":{\"text\":\"dup\"}}\n")
	_, _ = w.WriteString("not json\n")
	_, _ = w.WriteString("{\"jsonrpc\":\"2.0\",\"method\":\"d\",\"params\":{}}\n")
	_ = w.Flush()
	_ = pw.Close()
	<-c.Done()
	close(got)
	var all []string
	for s := range got {
		all = append(all, s)
	}
	want := []string{"a:bad � byte", "c:dup", "d:"}
	if strings.Join(all, "|") != strings.Join(want, "|") {
		t.Fatalf("dispatched %q, want %q", all, want)
	}
}

func TestIDKeys(t *testing.T) {
	for raw, want := range map[string]string{`7`: "7", ` 7 `: "7", `"7"`: "s:7", `null`: "null"} {
		if got := idKey(jsontext.Value(raw)); got != want {
			t.Errorf("idKey(%s) = %q, want %q", raw, got, want)
		}
	}
	if codexIDKey(jsontext.Value(`12`)) != "12" || codexIDKey(jsontext.Value(`"12"`)) != "12" {
		t.Error("codexIDKey")
	}
}
