package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

const defaultSystemPrompt = `You are Konsert Orchestrator, an autonomous distributed fleet coordinator.
You manage coding agents running across distributed worker nodes (hosts, VMs, and containers).
Use your fleet tools to inspect workers (fleet_status), dispatch parallel tasks (dispatch_task), check progress (list_tasks), retrieve concise structured results (get_task_result), steer running agents (steer_task), or cancel tasks (cancel_task).
Never ask for or expect raw terminal logs; rely on concise structured task summaries, exit codes, milestones, and git diff summaries.
Be concise, structured, and action-oriented.`

// ToolDef describes an orchestrator tool in JSON Schema format.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolCall represents a single model-initiated tool invocation.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Message is the provider-agnostic chat message representation.
type Message struct {
	Role         string     `json:"role"` // "user", "assistant", "tool"
	Content      string     `json:"content"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID   string     `json:"tool_call_id,omitempty"`
	ToolName     string     `json:"tool_name,omitempty"`
	InputTokens  int64      `json:"input_tokens,omitempty"`
	OutputTokens int64      `json:"output_tokens,omitempty"`
}

// Provider implements a direct LLM API client.
type Provider interface {
	Name() string
	Complete(ctx context.Context, model, system string, messages []Message, tools []ToolDef, onDelta func(string)) (Message, error)
}

// ToolExecutor bridges the orchestrator loop to the controller's FleetService.
type ToolExecutor interface {
	ToolDefinitions() []ToolDef
	ExecuteTool(ctx context.Context, name string, args json.RawMessage) (any, error)
}

// StreamCallbacks receives real-time chat events for forwarding over browser WebSocket.
type StreamCallbacks struct {
	OnDelta func(sessionID, delta string)
	OnTool  func(payload protocol.ChatToolPayload)
	OnDone  func(sessionID string, messageID int64)
	OnError func(sessionID, errMsg string)
	OnUsage func(summary store.UsageSummary)
}

// Service runs the built-in Orchestrator Chat backend with server-side tool execution.
type Service struct {
	cfg      config.OrchestratorConfig
	store    store.Store
	executor ToolExecutor
	provider Provider
}

// NewService creates an Orchestrator Service for the configured LLM provider.
func NewService(cfg config.OrchestratorConfig, st store.Store, executor ToolExecutor) *Service {
	var prov Provider
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "anthropic":
		prov = NewAnthropicProvider(cfg.APIKey, cfg.BaseURL)
	case "gemini":
		prov = NewGeminiProvider(cfg.APIKey, cfg.BaseURL)
	default:
		prov = NewOpenAIProvider(cfg.APIKey, cfg.BaseURL)
	}
	return &Service{
		cfg:      cfg,
		store:    st,
		executor: executor,
		provider: prov,
	}
}

// SetProvider replaces the LLM provider (useful for tests).
func (s *Service) SetProvider(p Provider) {
	s.provider = p
}

// HandleUserMessage persists the user's message, runs the LLM + tool-calling loop,
// persists assistant & tool turns, and streams events via cb.
func (s *Service) HandleUserMessage(ctx context.Context, sessionID, content string, cb StreamCallbacks) error {
	sess, err := s.store.GetChatSession(ctx, sessionID)
	if err != nil {
		// Auto-create session if needed.
		sess = store.ChatSession{
			ID:        sessionID,
			Title:     "Orchestrator",
			Provider:  s.cfg.Provider,
			Model:     s.cfg.Model,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}
		if createErr := s.store.CreateChatSession(ctx, sess); createErr != nil {
			if cb.OnError != nil {
				cb.OnError(sessionID, createErr.Error())
			}
			return createErr
		}
	}

	if _, err := s.store.AppendChatMessage(ctx, store.ChatMessage{
		SessionID: sessionID,
		Role:      "user",
		Content:   content,
	}); err != nil {
		if cb.OnError != nil {
			cb.OnError(sessionID, err.Error())
		}
		return err
	}

	if s.cfg.APIKey == "" && s.provider != nil && (s.provider.Name() == "openai" || s.provider.Name() == "anthropic" || s.provider.Name() == "gemini") {
		// Allow local OpenAI-compatible servers (e.g. Ollama/vLLM) with custom BaseURL even without APIKey.
		if s.cfg.BaseURL == "" {
			msg := fmt.Sprintf("Orchestrator LLM API key is not configured for provider %q. Set KONSERT_LLM_API_KEY (or OPENAI_API_KEY / ANTHROPIC_API_KEY / GEMINI_API_KEY) or configure an Ollama/vLLM base_url.", s.cfg.Provider)
			if cb.OnDelta != nil {
				cb.OnDelta(sessionID, msg)
			}
			msgID, _ := s.store.AppendChatMessage(ctx, store.ChatMessage{
				SessionID: sessionID,
				Role:      "assistant",
				Content:   msg,
			})
			if cb.OnDone != nil {
				cb.OnDone(sessionID, msgID)
			}
			return nil
		}
	}

	history, err := s.loadHistory(ctx, sessionID)
	if err != nil {
		if cb.OnError != nil {
			cb.OnError(sessionID, err.Error())
		}
		return err
	}

	system := defaultSystemPrompt
	if s.cfg.SystemExtra != "" {
		system += "\n\n" + s.cfg.SystemExtra
	}
	model := sess.Model
	if model == "" {
		model = s.cfg.Model
	}
	tools := s.executor.ToolDefinitions()

	const maxTurns = 10
	var lastMsgID int64
	for turn := 0; turn < maxTurns; turn++ {
		reply, err := s.provider.Complete(ctx, model, system, history, tools, func(delta string) {
			if cb.OnDelta != nil && delta != "" {
				cb.OnDelta(sessionID, delta)
			}
		})
		if err != nil {
			if cb.OnError != nil {
				cb.OnError(sessionID, err.Error())
			}
			return err
		}

		if reply.InputTokens > 0 || reply.OutputTokens > 0 {
			provName := s.cfg.Provider
			if s.provider != nil {
				provName = s.provider.Name()
			}
			_ = s.store.RecordUsage(ctx, store.UsageEvent{
				Provider:     provName,
				Model:        model,
				InputTokens:  reply.InputTokens,
				OutputTokens: reply.OutputTokens,
			})
			if cb.OnUsage != nil {
				if sum, uErr := s.store.GetUsageSummary(ctx); uErr == nil {
					cb.OnUsage(sum)
				}
			}
		}

		toolJSON := ""
		if len(reply.ToolCalls) > 0 {
			b, _ := json.Marshal(reply.ToolCalls)
			toolJSON = string(b)
		}
		lastMsgID, _ = s.store.AppendChatMessage(ctx, store.ChatMessage{
			SessionID: sessionID,
			Role:      "assistant",
			Content:   reply.Content,
			ToolJSON:  toolJSON,
		})
		history = append(history, reply)

		if len(reply.ToolCalls) == 0 {
			break
		}

		for _, tc := range reply.ToolCalls {
			var parsedArgs any
			_ = json.Unmarshal(tc.Arguments, &parsedArgs)

			if cb.OnTool != nil {
				cb.OnTool(protocol.ChatToolPayload{
					SessionID: sessionID,
					CallID:    tc.ID,
					Name:      tc.Name,
					Args:      parsedArgs,
					Status:    "running",
				})
			}

			res, execErr := s.executor.ExecuteTool(ctx, tc.Name, tc.Arguments)
			status := "completed"
			var resultPayload any = res
			if execErr != nil {
				status = "error"
				resultPayload = map[string]string{"error": execErr.Error()}
			}

			resBytes, _ := json.Marshal(resultPayload)
			if cb.OnTool != nil {
				cb.OnTool(protocol.ChatToolPayload{
					SessionID: sessionID,
					CallID:    tc.ID,
					Name:      tc.Name,
					Args:      parsedArgs,
					Result:    resultPayload,
					Status:    status,
				})
			}

			metaBytes, _ := json.Marshal(map[string]any{
				"call_id": tc.ID,
				"name":    tc.Name,
				"status":  status,
			})
			_, _ = s.store.AppendChatMessage(ctx, store.ChatMessage{
				SessionID: sessionID,
				Role:      "tool",
				Content:   string(resBytes),
				ToolJSON:  string(metaBytes),
			})

			history = append(history, Message{
				Role:       "tool",
				Content:    string(resBytes),
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
			})
		}
	}

	if cb.OnDone != nil {
		cb.OnDone(sessionID, lastMsgID)
	}
	return nil
}

func (s *Service) loadHistory(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := s.store.ListChatMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	msgs := make([]Message, 0, len(rows))
	for _, r := range rows {
		m := Message{
			Role:    r.Role,
			Content: r.Content,
		}
		if r.Role == "assistant" && r.ToolJSON != "" {
			_ = json.Unmarshal([]byte(r.ToolJSON), &m.ToolCalls)
		}
		if r.Role == "tool" && r.ToolJSON != "" {
			var meta struct {
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			}
			_ = json.Unmarshal([]byte(r.ToolJSON), &meta)
			m.ToolCallID = meta.CallID
			m.ToolName = meta.Name
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}
