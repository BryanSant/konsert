package orchestrator

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/BryanSant/konsert/internal/config"
)

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e), &probe)
		b.WriteString("event: " + probe.Type + "\ndata: " + e + "\n\n")
	}
	return b.String()
}

var anthropicToolTurn = sse(
	`{"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":20,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"I should check."}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"ping"}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Let me "}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"look."}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_9","name":"fleet_status","input":{}}}`,
	`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"machine\":"}}`,
	`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":" \"m1\"}"}}`,
	`{"type":"content_block_stop","index":2}`,
	`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`,
	`{"type":"message_stop"}`,
)

var anthropicFinalTurn = sse(
	`{"type":"message_start","message":{"usage":{"input_tokens":150,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"All good."}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
	`{"type":"message_stop"}`,
)

func TestParseAnthropicStreamIncremental(t *testing.T) {
	var deltas []string
	msg, err := parseAnthropicStream(strings.NewReader(anthropicToolTurn), func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "|") != "Let me |look." {
		t.Fatalf("text not streamed incrementally: %q", deltas)
	}
	if msg.Content != "Let me look." || msg.InputTokens != 120 || msg.OutputTokens != 42 {
		t.Fatalf("unexpected message: %+v", msg)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "toolu_9" || string(msg.ToolCalls[0].Arguments) != `{"machine": "m1"}` {
		t.Fatalf("tool call not reassembled: %+v", msg.ToolCalls)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(msg.AnthropicContent, &blocks); err != nil || len(blocks) != 3 {
		t.Fatalf("replay blocks: %s err=%v", msg.AnthropicContent, err)
	}
	if blocks[0]["type"] != "thinking" || blocks[0]["signature"] != "sig-abc" || blocks[0]["thinking"] != "I should check." {
		t.Fatalf("thinking block not preserved: %+v", blocks[0])
	}
}

func TestParseAnthropicStreamMultipleTextBlocksMatchContent(t *testing.T) {
	body := sse(
		`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"one"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"two"}}`,
		`{"type":"message_stop"}`,
	)
	var streamed strings.Builder
	msg, err := parseAnthropicStream(strings.NewReader(body), func(d string) { streamed.WriteString(d) })
	if err != nil || msg.Content != "one\ntwo" || streamed.String() != msg.Content || msg.AnthropicContent != nil {
		t.Fatalf("msg=%+v streamed=%q err=%v", msg, streamed.String(), err)
	}
}

func TestParseAnthropicStreamErrors(t *testing.T) {
	errEvent := sse(
		`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	)
	if _, err := parseAnthropicStream(strings.NewReader(errEvent), nil); err == nil || !strings.Contains(err.Error(), "overloaded_error") {
		t.Fatalf("expected surfaced error event, got %v", err)
	}
	truncated := sse(
		`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}`,
	)
	if _, err := parseAnthropicStream(strings.NewReader(truncated), nil); err == nil || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("expected truncation error, got %v", err)
	}
}

// TestAnthropicServiceStreamsAndReplaysThinking drives a full tool-use turn
// through the Service against an httptest server and checks the request
// shape: streaming on, default model, configured max_tokens, the signed
// thinking block replayed before tool_use, and tool results grouped.
func TestAnthropicServiceStreamsAndReplaysThinking(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = io.WriteString(w, anthropicToolTurn)
		} else {
			_, _ = io.WriteString(w, anthropicFinalTurn)
		}
	}))
	defer srv.Close()

	svc, _, ex := newTestService(t, config.OrchestratorConfig{Provider: "anthropic", Model: "claude-test", BaseURL: srv.URL, APIKey: "ant-key-123456"})
	svc.SetMaxTokens(1234)
	var deltas []string
	if err := svc.HandleUserMessage(context.Background(), "s1", "status?", StreamCallbacks{
		OnDelta: func(_, d string) { deltas = append(deltas, d) },
	}); err != nil {
		t.Fatal(err)
	}
	if len(ex.calls) != 1 || ex.args[0] != `{"machine": "m1"}` {
		t.Fatalf("tool not executed with streamed args: %v %v", ex.calls, ex.args)
	}
	if strings.Join(deltas, "|") != "Let me |look.|All good." {
		t.Fatalf("deltas: %q", deltas)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(bodies))
	}
	first := bodies[0]
	if first["stream"] != true || first["model"] != "claude-test" || first["max_tokens"] != float64(1234) {
		t.Fatalf("request shape: stream=%v model=%v max_tokens=%v", first["stream"], first["model"], first["max_tokens"])
	}
	msgs := bodies[1]["messages"].([]any)
	asst := msgs[len(msgs)-2].(map[string]any)
	content := asst["content"].([]any)
	if asst["role"] != "assistant" || content[0].(map[string]any)["type"] != "thinking" || content[2].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("thinking block not replayed before tool_use: %+v", asst)
	}
	toolMsg := msgs[len(msgs)-1].(map[string]any)
	if toolMsg["role"] != "user" || toolMsg["content"].([]any)[0].(map[string]any)["tool_use_id"] != "toolu_9" {
		t.Fatalf("tool result shape: %+v", toolMsg)
	}
}

func TestAnthropicMessagesGroupsToolResults(t *testing.T) {
	out := anthropicMessages([]Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "a", Name: "x", Arguments: jsontext.Value(`{}`)}, {ID: "b", Name: "x", Arguments: jsontext.Value(`not json`)}}},
		{Role: "tool", ToolCallID: "a", Content: "1"},
		{Role: "tool", ToolCallID: "b", Content: "2"},
		{Role: "assistant", Content: ""},
	})
	if len(out) != 3 {
		t.Fatalf("expected user, assistant, grouped tool results; got %d: %+v", len(out), out)
	}
	if got := len(out[2]["content"].([]map[string]any)); got != 2 {
		t.Fatalf("tool results not grouped: %d", got)
	}
}

func TestNoBuiltInModelDefaults(t *testing.T) {
	if _, err := NewOpenAIProvider("k", "").Complete(context.Background(), "", "", nil, nil, nil); !errors.Is(err, ErrNoModel) {
		t.Fatalf("openai with no model: %v", err)
	}
	if _, err := NewAnthropicProvider("k", "").Complete(context.Background(), "", "", nil, nil, nil); !errors.Is(err, ErrNoModel) {
		t.Fatalf("anthropic with no model: %v", err)
	}
	if _, err := NewGeminiProvider("k", "").Complete(context.Background(), "", "", nil, nil, nil); !errors.Is(err, ErrNoModel) {
		t.Fatalf("gemini with no model: %v", err)
	}
}
