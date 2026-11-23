package orchestrator

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestOpenAIProviderStreamingAndToolCall(t *testing.T) {
	sseBody := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Checking "}}]}`,
		`data: {"choices":[{"delta":{"content":"fleet..."}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"fleet_status","arguments":"{"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]}`,
		`data: [DONE]`,
		"",
	}, "\n")

	p := NewOpenAIProvider("sk-test", "https://example.invalid/v1")
	p.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Bearer sk-test" {
				t.Errorf("missing bearer auth header")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(sseBody)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	var streamed strings.Builder
	msg, err := p.Complete(context.Background(), "gpt-4o", "sys", []Message{{Role: "user", Content: "hi"}}, nil, func(d string) {
		streamed.WriteString(d)
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if msg.Content != "Checking fleet..." || streamed.String() != "Checking fleet..." {
		t.Fatalf("unexpected content=%q streamed=%q", msg.Content, streamed.String())
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Name != "fleet_status" || string(msg.ToolCalls[0].Arguments) != "{}" {
		t.Fatalf("unexpected tool calls: %+v", msg.ToolCalls)
	}
}

func TestAnthropicProviderToolUse(t *testing.T) {
	respJSON := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking fleet."}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"fleet_status","input":{}}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	p := NewAnthropicProvider("ant-key", "https://example.invalid")
	p.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("x-api-key") != "ant-key" {
				t.Errorf("missing x-api-key header")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(respJSON)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	msg, err := p.Complete(context.Background(), "claude-sonnet-4-20250514", "sys", []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if msg.Content != "Checking fleet." || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Name != "fleet_status" {
		t.Fatalf("unexpected anthropic msg: %+v", msg)
	}
}

func TestGeminiProviderFunctionCall(t *testing.T) {
	respJSON := `{"candidates":[{"content":{"parts":[{"text":"Checking now."},{"functionCall":{"name":"fleet_status","args":{}}}]}}]}`
	p := NewGeminiProvider("gem-key", "https://example.invalid/v1beta")
	p.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("x-goog-api-key") != "gem-key" || r.URL.RawQuery != "" {
				t.Errorf("key must be sent only in the x-goog-api-key header")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(respJSON)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	msg, err := p.Complete(context.Background(), "gemini-2.5-pro", "sys", []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if msg.Content != "Checking now." || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Name != "fleet_status" {
		t.Fatalf("unexpected gemini msg: %+v", msg)
	}
}
