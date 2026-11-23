package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// GeminiProvider talks directly to the Google Gemini Generative Language API.
type GeminiProvider struct {
	apiKey  string
	baseURL string
	httpOpts
}

func NewGeminiProvider(apiKey, baseURL string) *GeminiProvider {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &GeminiProvider{
		apiKey:   apiKey,
		baseURL:  baseURL,
		httpOpts: defaultHTTPOpts(),
	}
}

func (p *GeminiProvider) Name() string { return "gemini" }

func (p *GeminiProvider) Complete(ctx context.Context, model, system string, messages []Message, tools []ToolDef, onDelta func(string)) (Message, error) {
	if model == "" {
		model = "gemini-2.5-pro"
	}

	var contents []map[string]any
	for _, m := range messages {
		switch m.Role {
		case "user":
			contents = append(contents, map[string]any{
				"role":  "user",
				"parts": []map[string]any{{"text": m.Content}},
			})
		case "assistant":
			var parts []map[string]any
			if m.Content != "" {
				parts = append(parts, map[string]any{"text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				var argsObj map[string]any
				_ = json.Unmarshal(tc.Arguments, &argsObj)
				if argsObj == nil {
					argsObj = map[string]any{}
				}
				parts = append(parts, map[string]any{
					"functionCall": map[string]any{
						"name": tc.Name,
						"args": argsObj,
					},
				})
			}
			contents = append(contents, map[string]any{
				"role":  "model",
				"parts": parts,
			})
		case "tool":
			var respObj map[string]any
			if err := json.Unmarshal([]byte(m.Content), &respObj); err != nil || respObj == nil {
				respObj = map[string]any{"result": m.Content}
			}
			contents = append(contents, map[string]any{
				"role": "user",
				"parts": []map[string]any{
					{
						"functionResponse": map[string]any{
							"name":     m.ToolName,
							"response": respObj,
						},
					},
				},
			})
		}
	}

	reqBody := map[string]any{
		"contents": contents,
	}
	if system != "" {
		reqBody["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system}},
		}
	}
	if len(tools) > 0 {
		decls := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			decls = append(decls, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			})
		}
		reqBody["tools"] = []map[string]any{
			{"functionDeclarations": decls},
		}
	}

	raw, err := json.Marshal(reqBody)
	if err != nil {
		return Message{}, err
	}

	// The API key goes in the x-goog-api-key header, never the URL: *url.Error
	// embeds the full URL in its message, and errors are broadcast to clients.
	endpoint := fmt.Sprintf("%s/models/%s:generateContent", p.baseURL, url.PathEscape(model))
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		hdr.Set("x-goog-api-key", p.apiKey)
	}

	resp, err := p.postJSON(ctx, endpoint, hdr, raw)
	if err != nil {
		return Message{}, fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return Message{}, err
	}
	if resp.StatusCode >= 400 {
		return Message{}, fmt.Errorf("gemini HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		UsageMetadata *struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string          `json:"name"`
						Args json.RawMessage `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Message{}, fmt.Errorf("decode gemini response: %w", err)
	}

	out := Message{Role: "assistant"}
	if parsed.UsageMetadata != nil {
		out.InputTokens = parsed.UsageMetadata.PromptTokenCount
		out.OutputTokens = parsed.UsageMetadata.CandidatesTokenCount
	}
	if len(parsed.Candidates) == 0 {
		return out, nil
	}
	var textParts []string
	for _, part := range parsed.Candidates[0].Content.Parts {
		if part.Text != "" {
			textParts = append(textParts, part.Text)
			if onDelta != nil {
				onDelta(part.Text)
			}
		}
		if part.FunctionCall != nil {
			args := part.FunctionCall.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:        "call_" + uuid.NewString()[:8],
				Name:      part.FunctionCall.Name,
				Arguments: args,
			})
		}
	}
	out.Content = strings.Join(textParts, "\n")
	return out, nil
}
