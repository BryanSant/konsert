package commander

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// assertPaired fails if any tool message is not preceded by an assistant call
// with its ID, or any assistant call lacks a following result.
func assertPaired(t *testing.T, msgs []Message) {
	t.Helper()
	for i, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			want := map[string]bool{}
			for _, tc := range m.ToolCalls {
				want[tc.ID] = true
				if !jsontext.Value(tc.Arguments).IsValid() {
					t.Fatalf("msg %d: invalid arguments %q", i, tc.Arguments)
				}
			}
			for j := i + 1; j < len(msgs) && msgs[j].Role == "tool"; j++ {
				delete(want, msgs[j].ToolCallID)
			}
			if len(want) > 0 {
				t.Fatalf("msg %d: tool calls without results: %v (history %+v)", i, want, msgs)
			}
		}
		if m.Role == "tool" {
			k := i - 1
			for k >= 0 && msgs[k].Role == "tool" {
				k--
			}
			found := false
			if k >= 0 && msgs[k].Role == "assistant" {
				for _, tc := range msgs[k].ToolCalls {
					found = found || tc.ID == m.ToolCallID
				}
			}
			if !found {
				t.Fatalf("msg %d: orphan tool result %q (history %+v)", i, m.ToolCallID, msgs)
			}
		}
	}
}

func TestInvalidToolArgumentsDoNotPoisonHistory(t *testing.T) {
	ctx := context.Background()
	svc, st, ex := newTestService(t, config.CommanderConfig{})
	prov := &scriptedProvider{replies: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_bad", Name: "fleet_status", Arguments: jsontext.Value(`{"machine": "a`)}, // truncated JSON
			{ID: "call_ok", Name: "fleet_status", Arguments: jsontext.Value(`{"machine":"b"}`)},
		}},
		{Role: "assistant", Content: "done"},
		{Role: "assistant", Content: "second answer"},
	}}
	svc.SetProvider(prov)

	var toolStatuses []string
	if err := svc.HandleUserMessage(ctx, "s1", "check", StreamCallbacks{
		OnTool: func(p protocol.ChatToolPayload) { toolStatuses = append(toolStatuses, p.CallID+":"+p.Status) },
	}); err != nil {
		t.Fatalf("HandleUserMessage: %v", err)
	}
	// The malformed call is never executed; the valid one is.
	if len(ex.calls) != 1 || ex.args[0] != `{"machine":"b"}` {
		t.Fatalf("executor calls=%v args=%v", ex.calls, ex.args)
	}
	if !strings.Contains(strings.Join(toolStatuses, ","), "call_bad:error") {
		t.Fatalf("expected call_bad error status, got %v", toolStatuses)
	}
	// The assistant row keeps both calls (bad one with {} args), so results pair up.
	rows, _ := st.ListChatMessages(ctx, "s1")
	var asst store.ChatMessage
	for _, r := range rows {
		if r.Role == "assistant" && r.ToolJSON != "" {
			asst = r
		}
	}
	var calls []ToolCall
	if err := json.Unmarshal([]byte(asst.ToolJSON), &calls); err != nil || len(calls) != 2 || string(calls[0].Arguments) != "{}" {
		t.Fatalf("assistant tool_json not persisted correctly: %q err=%v", asst.ToolJSON, err)
	}

	// A follow-up message must send a well-formed history to the provider.
	if err := svc.HandleUserMessage(ctx, "s1", "again", StreamCallbacks{}); err != nil {
		t.Fatal(err)
	}
	last := prov.seen[len(prov.seen)-1]
	assertPaired(t, last)
	var badResult string
	for _, m := range last {
		if m.Role == "tool" && m.ToolCallID == "call_bad" {
			badResult = m.Content
		}
	}
	if !strings.Contains(badResult, "invalid tool arguments") {
		t.Fatalf("model should see an error result for call_bad, got %q", badResult)
	}
}

func TestSanitizeToolCalls(t *testing.T) {
	calls := []ToolCall{
		{Name: "a", Arguments: nil},
		{ID: "x", Name: "b", Arguments: jsontext.Value(`[1,2]`)},
		{ID: "y", Name: "", Arguments: jsontext.Value(`{}`)},
		{ID: "z", Name: "c", Arguments: jsontext.Value(` {"k":1} `)},
	}
	bad := sanitizeToolCalls(calls)
	if calls[0].ID == "" || string(calls[0].Arguments) != "{}" {
		t.Fatalf("missing id/args not filled: %+v", calls[0])
	}
	if _, ok := bad[1]; !ok || string(calls[1].Arguments) != "{}" {
		t.Fatalf("non-object args must be invalid: %+v %v", calls[1], bad)
	}
	if _, ok := bad[2]; !ok {
		t.Fatal("missing name must be invalid")
	}
	if _, ok := bad[3]; ok || string(calls[3].Arguments) != `{"k":1}` {
		t.Fatalf("valid call mangled: %+v %v", calls[3], bad)
	}
}

func TestLoadBoundedHistoryStripsOrphansAnywhere(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := newTestService(t, config.CommanderConfig{})
	if err := st.CreateChatSession(ctx, store.ChatSession{ID: "s", Title: "t", Provider: "openai", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	tj := func(calls ...string) string {
		var tcs []ToolCall
		for _, c := range calls {
			tcs = append(tcs, ToolCall{ID: c, Name: "fleet_status", Arguments: jsontext.Value("{}")})
		}
		b, _ := json.Marshal(tcs)
		return string(b)
	}
	meta := func(id string) string { return `{"call_id":"` + id + `","name":"fleet_status","status":"completed"}` }
	rows := []store.ChatMessage{
		{Role: "user", Content: "u1"},
		// Legacy poisoned turn: assistant row saved without its tool calls, results saved anyway.
		{Role: "assistant", Content: ""},
		{Role: "tool", Content: `{"ok":1}`, ToolJSON: meta("lost1")},
		{Role: "tool", Content: `{"ok":1}`, ToolJSON: meta("lost2")},
		{Role: "user", Content: "u2"},
		// Good turn.
		{Role: "assistant", ToolJSON: tj("good")},
		{Role: "tool", Content: `{"ok":1}`, ToolJSON: meta("good")},
		{Role: "assistant", Content: "a2"},
		// Interrupted turn: two calls, only one result; plus a stray result for an unknown id.
		{Role: "user", Content: "u3"},
		{Role: "assistant", Content: "working", ToolJSON: tj("c1", "c2")},
		{Role: "tool", Content: `{"ok":1}`, ToolJSON: meta("c1")},
		{Role: "tool", Content: `{"ok":1}`, ToolJSON: meta("other")},
		// Dangling call with no result at all and no text.
		{Role: "assistant", ToolJSON: tj("c3")},
		{Role: "user", Content: "u4"},
	}
	for _, r := range rows {
		r.SessionID = "s"
		if _, err := st.AppendChatMessage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	msgs, _, err := svc.loadBoundedHistory(ctx, "s", 24)
	if err != nil {
		t.Fatal(err)
	}
	assertPaired(t, msgs)
	var roles []string
	for _, m := range msgs {
		r := m.Role
		if m.Role == "tool" {
			r += "(" + m.ToolCallID + ")"
		}
		roles = append(roles, r)
	}
	got := strings.Join(roles, ",")
	want := "user,user,assistant,tool(good),assistant,user,assistant,tool(c1),user"
	if got != want {
		t.Fatalf("history\n got %s\nwant %s", got, want)
	}
}

func TestMaxTurnsEmitsFinalMessage(t *testing.T) {
	ctx := context.Background()
	svc, st, ex := newTestService(t, config.CommanderConfig{})
	svc.SetProvider(&scriptedProvider{replies: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "loop", Name: "fleet_status", Arguments: jsontext.Value("{}")}}},
	}})
	var deltas strings.Builder
	var doneID int64
	if err := svc.HandleUserMessage(ctx, "s1", "go", StreamCallbacks{
		OnDelta: func(_, d string) { deltas.WriteString(d) },
		OnDone:  func(_ string, id int64) { doneID = id },
	}); err != nil {
		t.Fatal(err)
	}
	if len(ex.calls) != maxTurns {
		t.Fatalf("expected %d tool executions, got %d", maxTurns, len(ex.calls))
	}
	rows, _ := st.ListChatMessages(ctx, "s1")
	last := rows[len(rows)-1]
	if last.Role != "assistant" || !strings.Contains(last.Content, "tool-calling rounds") || last.ID != doneID {
		t.Fatalf("expected final assistant message with OnDone id, got %+v doneID=%d", last, doneID)
	}
	if !strings.Contains(deltas.String(), "tool-calling rounds") {
		t.Fatalf("final message not streamed: %q", deltas.String())
	}
}

func TestOpenAIStreamErrorsSurface(t *testing.T) {
	cases := map[string]string{
		"error chunk": "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\ndata: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\n\n",
		"no done":     "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n",
	}
	for name, body := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		svc, _, _ := newTestService(t, config.CommanderConfig{BaseURL: srv.URL})
		var reported string
		err := svc.HandleUserMessage(context.Background(), "s-"+strings.ReplaceAll(name, " ", ""), "hi", StreamCallbacks{
			OnError: func(_, msg string) { reported = msg },
		})
		srv.Close()
		if err == nil || reported == "" {
			t.Fatalf("%s: expected surfaced error, got err=%v reported=%q", name, err, reported)
		}
	}

	// finish_reason without [DONE] (some compatible servers) is accepted.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
	}))
	defer srv.Close()
	p := NewOpenAIProvider("k", srv.URL)
	if msg, err := p.Complete(context.Background(), "m", "", []Message{{Role: "user", Content: "hi"}}, nil, nil); err != nil || msg.Content != "ok" {
		t.Fatalf("finish_reason stream: %+v %v", msg, err)
	}
}
