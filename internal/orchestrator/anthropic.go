package orchestrator

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// AnthropicProvider talks directly to the Anthropic Messages API, streaming
// responses over SSE.
type AnthropicProvider struct {
	apiKey  string
	baseURL string
	// MaxTokens is the max_tokens sent per request; <=0 means DefaultMaxTokens.
	MaxTokens int
	httpOpts
}

func NewAnthropicProvider(apiKey, baseURL string) *AnthropicProvider {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &AnthropicProvider{
		apiKey:   apiKey,
		baseURL:  baseURL,
		httpOpts: defaultHTTPOpts(),
	}
}

func (p *AnthropicProvider) Name() string { return "anthropic" }

// anthropicMessages converts provider-agnostic history into Messages API turns.
// Consecutive tool results are grouped into one user message, as the API
// expects for parallel tool use.
func anthropicMessages(messages []Message) []map[string]any {
	var apiMsgs []map[string]any
	for _, m := range messages {
		switch m.Role {
		case "user":
			apiMsgs = append(apiMsgs, map[string]any{"role": "user", "content": m.Content})
		case "assistant":
			if len(m.AnthropicContent) > 0 {
				// Same-turn replay: send the blocks exactly as received, including
				// thinking blocks (with signatures) that must precede tool_use.
				apiMsgs = append(apiMsgs, map[string]any{"role": "assistant", "content": m.AnthropicContent})
				continue
			}
			var blocks []map[string]any
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Name,
					"input": toolInputObject(tc.Arguments),
				})
			}
			if len(blocks) == 0 {
				continue
			}
			apiMsgs = append(apiMsgs, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			block := map[string]any{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			}
			if n := len(apiMsgs); n > 0 && apiMsgs[n-1]["role"] == "user" {
				if prev, ok := apiMsgs[n-1]["content"].([]map[string]any); ok && len(prev) > 0 && prev[0]["type"] == "tool_result" {
					apiMsgs[n-1]["content"] = append(prev, block)
					continue
				}
			}
			apiMsgs = append(apiMsgs, map[string]any{"role": "user", "content": []map[string]any{block}})
		}
	}
	return apiMsgs
}

// toolInputObject returns args as a JSON object, or {} if they are not one.
func toolInputObject(args jsontext.Value) jsontext.Value {
	var obj map[string]any
	if len(args) == 0 || json.Unmarshal(args, &obj) != nil || obj == nil {
		return jsontext.Value("{}")
	}
	return args
}

func (p *AnthropicProvider) Complete(ctx context.Context, model, system string, messages []Message, tools []ToolDef, onDelta func(string)) (Message, error) {
	if model == "" {
		return Message{}, ErrNoModel
	}
	maxTokens := p.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	reqBody := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages":   anthropicMessages(messages),
		"stream":     true,
	}
	if system != "" {
		reqBody["system"] = system
	}
	if len(tools) > 0 {
		apiTools := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			apiTools = append(apiTools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": t.Parameters,
			})
		}
		reqBody["tools"] = apiTools
	}

	raw, err := json.Marshal(reqBody, requestJSONOpts)
	if err != nil {
		return Message{}, err
	}

	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Accept", "text/event-stream")
	hdr.Set("x-api-key", p.apiKey)
	hdr.Set("anthropic-version", "2023-06-01")

	resp, err := p.postJSON(ctx, p.baseURL+"/v1/messages", hdr, raw)
	if err != nil {
		return Message{}, fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Message{}, fmt.Errorf("anthropic HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return parseAnthropicStream(resp.Body, onDelta)
}

// anthropicBlock accumulates one streamed content block.
type anthropicBlock struct {
	Type      string
	Text      strings.Builder
	ID        string
	Name      string
	InputJSON strings.Builder
	StartIn   jsontext.Value // input from content_block_start (complete for non-streamed inputs)
	Thinking  strings.Builder
	Signature string
	Data      string // redacted_thinking payload
}

// parseAnthropicStream consumes a Messages API SSE stream. Text deltas are
// forwarded to onDelta as they arrive; tool_use inputs are reassembled from
// input_json_delta fragments; thinking blocks are kept (with signatures) for
// same-turn replay. An "error" event, or a stream that ends before
// message_stop, is returned as an error.
func parseAnthropicStream(r io.Reader, onDelta func(string)) (Message, error) {
	var (
		blocks       = map[int]*anthropicBlock{}
		inputTokens  int64
		outputTokens int64
		stopped      bool
		emittedText  bool
	)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // "event:" lines are redundant with the payload's type field
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message *struct {
				Usage *struct {
					InputTokens              int64 `json:"input_tokens"`
					CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
					OutputTokens             int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock *struct {
				Type      string         `json:"type"`
				Text      string         `json:"text"`
				ID        string         `json:"id"`
				Name      string         `json:"name"`
				Input     jsontext.Value `json:"input"`
				Thinking  string         `json:"thinking"`
				Signature string         `json:"signature"`
				Data      string         `json:"data"`
			} `json:"content_block"`
			Delta *struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage *struct {
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				u := ev.Message.Usage
				inputTokens = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
				outputTokens = u.OutputTokens
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				continue
			}
			cb := ev.ContentBlock
			b := &anthropicBlock{Type: cb.Type, ID: cb.ID, Name: cb.Name, StartIn: cb.Input, Signature: cb.Signature, Data: cb.Data}
			b.Thinking.WriteString(cb.Thinking)
			blocks[ev.Index] = b
			if cb.Type == "text" && cb.Text != "" {
				if emittedText && onDelta != nil {
					onDelta("\n")
				}
				b.Text.WriteString(cb.Text)
				if onDelta != nil {
					onDelta(cb.Text)
				}
				emittedText = true
			}
		case "content_block_delta":
			b, ok := blocks[ev.Index]
			if !ok || ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text == "" {
					continue
				}
				if b.Text.Len() == 0 && emittedText && onDelta != nil {
					onDelta("\n") // separator between text blocks, matching Content
				}
				b.Text.WriteString(ev.Delta.Text)
				emittedText = true
				if onDelta != nil {
					onDelta(ev.Delta.Text)
				}
			case "input_json_delta":
				b.InputJSON.WriteString(ev.Delta.PartialJSON)
			case "thinking_delta":
				b.Thinking.WriteString(ev.Delta.Thinking)
			case "signature_delta":
				b.Signature += ev.Delta.Signature
			}
		case "message_delta":
			if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
				outputTokens = ev.Usage.OutputTokens
			}
		case "message_stop":
			stopped = true
		case "error":
			msg := "unknown error"
			if ev.Error != nil {
				msg = strings.TrimSpace(ev.Error.Type + ": " + ev.Error.Message)
			}
			return Message{}, fmt.Errorf("anthropic stream error: %s", msg)
		}
		if stopped {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return Message{}, fmt.Errorf("anthropic stream: %w", err)
	}
	if !stopped {
		return Message{}, errors.New("anthropic stream ended before message_stop; the response may be truncated")
	}

	idxs := make([]int, 0, len(blocks))
	for i := range blocks {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)

	out := Message{Role: "assistant", InputTokens: inputTokens, OutputTokens: outputTokens}
	var textParts []string
	var replay []map[string]any
	for _, i := range idxs {
		b := blocks[i]
		switch b.Type {
		case "text":
			t := b.Text.String()
			if t == "" {
				continue
			}
			textParts = append(textParts, t)
			replay = append(replay, map[string]any{"type": "text", "text": t})
		case "tool_use":
			args := jsontext.Value(strings.TrimSpace(b.InputJSON.String()))
			if len(args) == 0 {
				args = b.StartIn
			}
			args = toolInputObject(args)
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: args})
			replay = append(replay, map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": args})
		case "thinking":
			replay = append(replay, map[string]any{"type": "thinking", "thinking": b.Thinking.String(), "signature": b.Signature})
		case "redacted_thinking":
			replay = append(replay, map[string]any{"type": "redacted_thinking", "data": b.Data})
		}
	}
	out.Content = strings.Join(textParts, "\n")
	if len(out.ToolCalls) > 0 && len(replay) > 0 {
		// Only needed to continue a tool-use turn; plain answers are replayed from Content.
		if b, err := json.Marshal(replay, storeJSONOpts); err == nil {
			out.AnthropicContent = b
		}
	}
	return out, nil
}
