package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

const defaultSystemPrompt = `You are Konsert Orchestrator, coordinating an entire fleet of SSH-connected machines as one logical system.
Token Efficiency Rules:
- ALWAYS prefer deterministic daemon RPC tools (inspect_machine_facts, inspect_git_status, list_remote_dir, scan_temp_worktrees, cleanup_worktree, fleet_status, list_tasks) over spawning coding agents for inspection, file discovery, git status, or cleanup scans.
- Delegate to coding agents (dispatch_task) ONLY for non-deterministic engineering work (writing/refactoring code, debugging, running complex multi-step plans).
- Use cached Machine Facts to place work on the right machine (OS, hardware/load, virtualization, installed tools like postgres 18 or gcloud auth readiness).
- Persist important cross-thread fleet context or task handoff notes with save_memory, and rely on compact structured summaries rather than raw terminal output.
Be concise, structured, and action-oriented.`

// ContextProvider optionally supplies a compact live fleet + taxonomy + memory digest for every LLM turn.
type ContextProvider interface {
	BuildCompactContext(ctx context.Context, sessionID string) string
}

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
	mu       sync.RWMutex
	cfg      config.OrchestratorConfig
	store    store.Store
	executor ToolExecutor
	provider Provider
}

func buildProvider(cfg config.OrchestratorConfig) Provider {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "anthropic":
		return NewAnthropicProvider(cfg.APIKey, cfg.BaseURL)
	case "gemini":
		return NewGeminiProvider(cfg.APIKey, cfg.BaseURL)
	default:
		return NewOpenAIProvider(cfg.APIKey, cfg.BaseURL)
	}
}

// NewService creates an Orchestrator Service for the configured LLM provider.
func NewService(cfg config.OrchestratorConfig, st store.Store, executor ToolExecutor) *Service {
	return &Service{
		cfg:      cfg,
		store:    st,
		executor: executor,
		provider: buildProvider(cfg),
	}
}

// UpdateConfig hot-reloads the LLM provider, model, base URL, and API key at runtime.
func (s *Service) UpdateConfig(cfg config.OrchestratorConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	s.provider = buildProvider(cfg)
}

// SetProvider replaces the LLM provider (useful for tests).
func (s *Service) SetProvider(p Provider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provider = p
}

// HandleUserMessage persists the user's message, runs the LLM + tool-calling loop,
// persists assistant & tool turns, and streams events via cb.
func (s *Service) HandleUserMessage(ctx context.Context, sessionID, content string, cb StreamCallbacks) error {
	s.mu.RLock()
	cfg := s.cfg
	prov := s.provider
	s.mu.RUnlock()

	// fail scrubs secrets from err (errors reach every browser and MCP caller),
	// reports it via OnError and returns the scrubbed error.
	fail := func(err error) error {
		err = redactError(err, cfg.APIKey)
		if cb.OnError != nil {
			cb.OnError(sessionID, err.Error())
		}
		return err
	}

	sess, err := s.store.GetChatSession(ctx, sessionID)
	if err != nil {
		// Auto-create session if needed.
		sess = store.ChatSession{
			ID:        sessionID,
			Title:     "Orchestrator",
			Provider:  cfg.Provider,
			Model:     cfg.Model,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}
		if createErr := s.store.CreateChatSession(ctx, sess); createErr != nil {
			return fail(createErr)
		}
	}

	if _, err := s.store.AppendChatMessage(ctx, store.ChatMessage{
		SessionID: sessionID,
		Role:      "user",
		Content:   content,
	}); err != nil {
		return fail(err)
	}

	if cfg.APIKey == "" && prov != nil && (prov.Name() == "openai" || prov.Name() == "anthropic" || prov.Name() == "gemini") {
		// Allow local OpenAI-compatible servers (e.g. Ollama/vLLM) with custom BaseURL even without APIKey.
		if cfg.BaseURL == "" {
			msg := fmt.Sprintf("Orchestrator LLM API key is not configured for provider %q. Open Settings (bottom-left) or set KONSERT_LLM_API_KEY to configure your provider.", cfg.Provider)
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

	history, checkpoint, err := s.loadBoundedHistory(ctx, sessionID, 24)
	if err != nil {
		return fail(err)
	}

	system := defaultSystemPrompt
	if cp, ok := s.executor.(ContextProvider); ok {
		if compact := strings.TrimSpace(cp.BuildCompactContext(ctx, sessionID)); compact != "" {
			system += "\n\n## Live Fleet & Memory Snapshot\n" + compact
		}
	}
	if checkpoint != "" {
		system += "\n\n## Thread Checkpoint (Earlier Compacted Turns)\n" + checkpoint
	}
	if cfg.SystemExtra != "" {
		system += "\n\n" + cfg.SystemExtra
	}
	model := cfg.Model
	if model == "" {
		model = sess.Model
	}
	tools := s.executor.ToolDefinitions()

	const maxTurns = 10
	var lastMsgID int64
	for turn := 0; turn < maxTurns; turn++ {
		reply, err := prov.Complete(ctx, model, system, history, tools, func(delta string) {
			if cb.OnDelta != nil && delta != "" {
				cb.OnDelta(sessionID, delta)
			}
		})
		if err != nil {
			return fail(err)
		}

		if reply.InputTokens > 0 || reply.OutputTokens > 0 {
			provName := cfg.Provider
			if prov != nil {
				provName = prov.Name()
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

// loadBoundedHistory keeps the prompt compact when threads grow long by summarizing overflow turns
// into persistent SQLite orchestrator_memory ("session:<id>", "thread_checkpoint") so context wipes never lose state.
func (s *Service) loadBoundedHistory(ctx context.Context, sessionID string, maxRecent int) ([]Message, string, error) {
	rows, err := s.store.ListChatMessages(ctx, sessionID)
	if err != nil {
		return nil, "", err
	}
	var checkpoint string
	if maxRecent > 0 && len(rows) > maxRecent {
		overflow := rows[:len(rows)-maxRecent]
		rows = rows[len(rows)-maxRecent:]
		// Never start recent slice on a stray tool result without its parent assistant tool_call.
		for len(rows) > 0 && rows[0].Role == "tool" {
			overflow = append(overflow, rows[0])
			rows = rows[1:]
		}
		checkpoint = compactOverflowDigest(overflow)
		if checkpoint != "" {
			_ = s.store.UpsertMemory(ctx, store.MemoryEntry{
				Scope:   "session:" + sessionID,
				Key:     "thread_checkpoint",
				Summary: checkpoint,
			})
		}
	} else {
		// Re-hydrate any previously persisted checkpoint for this session if present.
		if mems, mErr := s.store.ListMemory(ctx, "session:"+sessionID, 5); mErr == nil {
			for _, m := range mems {
				if m.Key == "thread_checkpoint" {
					checkpoint = m.Summary
					break
				}
			}
		}
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
	return msgs, checkpoint, nil
}

func compactOverflowDigest(rows []store.ChatMessage) string {
	var lines []string
	for _, r := range rows {
		text := strings.TrimSpace(r.Content)
		if text == "" {
			continue
		}
		if r.Role == "tool" {
			var meta struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			}
			_ = json.Unmarshal([]byte(r.ToolJSON), &meta)
			if meta.Name != "" {
				lines = append(lines, fmt.Sprintf("- tool %s (%s)", meta.Name, meta.Status))
			}
			continue
		}
		if len(text) > 140 {
			text = text[:140] + "..."
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", r.Role, strings.ReplaceAll(text, "\n", " ")))
	}
	if len(lines) > 16 {
		lines = lines[len(lines)-16:]
	}
	return strings.Join(lines, "\n")
}
