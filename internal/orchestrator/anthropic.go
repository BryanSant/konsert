package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AnthropicProvider talks directly to the Anthropic Messages API.
type AnthropicProvider struct {
	apiKey  string
	baseURL string
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

func (p *AnthropicProvider) Complete(ctx context.Context, model, system string, messages []Message, tools []ToolDef, onDelta func(string)) (Message, error) {
	if model == "" {
		model = "claude-sonnet-4-20250514"
	}

	var apiMsgs []map[string]any
	for _, m := range messages {
		switch m.Role {
		case "user":
			apiMsgs = append(apiMsgs, map[string]any{
				"role":    "user",
				"content": m.Content,
			})
		case "assistant":
			var blocks []map[string]any
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				var input any = map[string]any{}
				if len(tc.Arguments) > 0 {
					_ = json.Unmarshal(tc.Arguments, &input)
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Name,
					"input": input,
				})
			}
			apiMsgs = append(apiMsgs, map[string]any{
				"role":    "assistant",
				"content": blocks,
			})
		case "tool":
			apiMsgs = append(apiMsgs, map[string]any{
				"role": "user",
				"content": []map[string]any{
					{
						"type":        "tool_result",
						"tool_use_id": m.ToolCallID,
						"content":     m.Content,
					},
				},
			})
		}
	}

	reqBody := map[string]any{
		"model":      model,
		"max_tokens": 4096,
		"system":     system,
		"messages":   apiMsgs,
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

	raw, err := json.Marshal(reqBody)
	if err != nil {
		return Message{}, err
	}

	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("x-api-key", p.apiKey)
	hdr.Set("anthropic-version", "2023-06-01")

	resp, err := p.postJSON(ctx, p.baseURL+"/v1/messages", hdr, raw)
	if err != nil {
		return Message{}, fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return Message{}, err
	}
	if resp.StatusCode >= 400 {
		return Message{}, fmt.Errorf("anthropic HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Message{}, fmt.Errorf("decode anthropic response: %w", err)
	}

	out := Message{Role: "assistant"}
	if parsed.Usage != nil {
		out.InputTokens = parsed.Usage.InputTokens
		out.OutputTokens = parsed.Usage.OutputTokens
	}
	var textParts []string
	for _, b := range parsed.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				textParts = append(textParts, b.Text)
				if onDelta != nil {
					onDelta(b.Text)
				}
			}
		case "tool_use":
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: args,
			})
		}
	}
	out.Content = strings.Join(textParts, "\n")
	return out, nil
}
