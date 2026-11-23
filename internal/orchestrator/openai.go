package orchestrator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAIProvider talks to OpenAI and any OpenAI-compatible endpoint
// (OpenRouter, xAI/Grok, Ollama, vLLM, LiteLLM).
type OpenAIProvider struct {
	apiKey  string
	baseURL string
	// MaxTokens, when >0, is sent as max_completion_tokens.
	MaxTokens int
	httpOpts
}

func NewOpenAIProvider(apiKey, baseURL string) *OpenAIProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &OpenAIProvider{
		apiKey:   apiKey,
		baseURL:  baseURL,
		httpOpts: defaultHTTPOpts(),
	}
}

func (p *OpenAIProvider) Name() string { return "openai" }

func (p *OpenAIProvider) Complete(ctx context.Context, model, system string, messages []Message, tools []ToolDef, onDelta func(string)) (Message, error) {
	if model == "" {
		return Message{}, ErrNoModel
	}
	apiMsgs := make([]map[string]any, 0, len(messages)+1)
	if system != "" {
		apiMsgs = append(apiMsgs, map[string]any{"role": "system", "content": system})
	}
	for _, m := range messages {
		entry := map[string]any{"role": m.Role, "content": m.Content}
		if m.Role == "tool" {
			entry["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			var tcs []map[string]any
			for _, tc := range m.ToolCalls {
				argsStr := string(tc.Arguments)
				if argsStr == "" {
					argsStr = "{}"
				}
				tcs = append(tcs, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": argsStr,
					},
				})
			}
			entry["tool_calls"] = tcs
		}
		apiMsgs = append(apiMsgs, entry)
	}

	reqBody := map[string]any{
		"model":          model,
		"messages":       apiMsgs,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if p.MaxTokens > 0 {
		reqBody["max_completion_tokens"] = p.MaxTokens
	}
	if len(tools) > 0 {
		apiTools := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			apiTools = append(apiTools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
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
	hdr.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		hdr.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.postJSON(ctx, p.baseURL+"/chat/completions", hdr, raw)
	if err != nil {
		return Message{}, fmt.Errorf("openai request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Message{}, fmt.Errorf("openai HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var (
		contentBuf   strings.Builder
		inputTokens  int64
		outputTokens int64
		tcMap        = make(map[int]*struct {
			id   string
			name string
			args strings.Builder
		})
		tcOrder      []int
		sawDone      bool
		finishReason string
	)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var chunk struct {
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    any    `json:"code"`
			} `json:"error"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			// Providers report mid-stream failures (overload, moderation, context
			// length) as an error chunk; surface it instead of a silent short reply.
			msg := strings.TrimSpace(chunk.Error.Message)
			if msg == "" {
				msg = "unknown error"
			}
			if chunk.Error.Type != "" {
				msg = chunk.Error.Type + ": " + msg
			}
			return Message{}, fmt.Errorf("openai stream error: %s", msg)
		}
		if chunk.Usage != nil {
			inputTokens = chunk.Usage.PromptTokens
			outputTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if fr := chunk.Choices[0].FinishReason; fr != "" {
			finishReason = fr
		}
		d := chunk.Choices[0].Delta
		if d.Content != "" {
			contentBuf.WriteString(d.Content)
			if onDelta != nil {
				onDelta(d.Content)
			}
		}
		for _, tc := range d.ToolCalls {
			acc, ok := tcMap[tc.Index]
			if !ok {
				acc = &struct {
					id   string
					name string
					args strings.Builder
				}{}
				tcMap[tc.Index] = acc
				tcOrder = append(tcOrder, tc.Index)
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				acc.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Message{}, fmt.Errorf("openai stream: %w", err)
	}
	if !sawDone && finishReason == "" {
		// The connection closed before the model finished: the reply (and any
		// tool-call arguments) may be truncated, so do not treat it as complete.
		return Message{}, errors.New("openai stream ended unexpectedly (no finish_reason or [DONE]); the response may be truncated")
	}

	out := Message{
		Role:         "assistant",
		Content:      contentBuf.String(),
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	}
	for _, idx := range tcOrder {
		acc := tcMap[idx]
		argStr := strings.TrimSpace(acc.args.String())
		if argStr == "" {
			argStr = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        acc.id,
			Name:      acc.name,
			Arguments: json.RawMessage(argStr),
		})
	}
	return out, nil
}
