package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/google/uuid"
)

// maxTurns bounds LLM round-trips (tool-calling rounds) per user message.
const maxTurns = 10

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
	// AnthropicContent holds the raw assistant content blocks (including
	// signed thinking blocks) so a tool-use turn can be continued exactly as
	// the Anthropic API requires. It lives only in memory for the current turn.
	AnthropicContent json.RawMessage `json:"-"`
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
	// maxTokens caps output tokens per LLM call; 0 means provider defaults
	// (DefaultMaxTokens for Anthropic, unset for OpenAI/Gemini).
	maxTokens int
}

func buildProvider(cfg config.OrchestratorConfig, maxTokens int) Provider {
	switch normalizeProvider(cfg.Provider) {
	case "anthropic":
		p := NewAnthropicProvider(cfg.APIKey, cfg.BaseURL)
		p.MaxTokens = maxTokens
		return p
	case "gemini":
		p := NewGeminiProvider(cfg.APIKey, cfg.BaseURL)
		p.MaxTokens = maxTokens
		return p
	default:
		p := NewOpenAIProvider(cfg.APIKey, cfg.BaseURL)
		p.MaxTokens = maxTokens
		return p
	}
}

// NewService creates an Orchestrator Service for the configured LLM provider.
func NewService(cfg config.OrchestratorConfig, st store.Store, executor ToolExecutor) *Service {
	return &Service{
		cfg:      cfg,
		store:    st,
		executor: executor,
		provider: buildProvider(cfg, 0),
	}
}

// UpdateConfig hot-reloads the LLM provider, model, base URL, and API key at runtime.
func (s *Service) UpdateConfig(cfg config.OrchestratorConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	s.provider = buildProvider(cfg, s.maxTokens)
}

// SetMaxTokens sets the per-call output token ceiling (0 restores provider
// defaults) and rebuilds the provider. It exists until OrchestratorConfig grows
// a max_tokens field that NewService/UpdateConfig can read directly.
func (s *Service) SetMaxTokens(n int) {
	if n < 0 {
		n = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxTokens = n
	s.provider = buildProvider(s.cfg, n)
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
	if model == "" {
		model = DefaultModel(cfg.Provider)
	}
	tools := s.executor.ToolDefinitions()

	var lastMsgID int64
	finished := false
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

		// Validate tool calls before anything is persisted: a call with malformed
		// arguments must not be dropped from the assistant row while its tool
		// result is saved (that orphan would 400 every later request).
		invalid := sanitizeToolCalls(reply.ToolCalls)

		toolJSON := ""
		if len(reply.ToolCalls) > 0 {
			b, mErr := json.Marshal(reply.ToolCalls)
			if mErr != nil {
				return fail(fmt.Errorf("encode tool calls: %w", mErr))
			}
			toolJSON = string(b)
		}
		lastMsgID, err = s.store.AppendChatMessage(ctx, store.ChatMessage{
			SessionID: sessionID,
			Role:      "assistant",
			Content:   reply.Content,
			ToolJSON:  toolJSON,
		})
		if err != nil {
			return fail(fmt.Errorf("save assistant message: %w", err))
		}
		history = append(history, reply)

		if len(reply.ToolCalls) == 0 {
			finished = true
			break
		}

		for i, tc := range reply.ToolCalls {
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

			status := "completed"
			var resultPayload any
			if reason, bad := invalid[i]; bad {
				// Never execute a call whose arguments we could not parse; tell the model instead.
				status = "error"
				resultPayload = map[string]string{"error": reason}
			} else {
				res, execErr := s.executor.ExecuteTool(ctx, tc.Name, tc.Arguments)
				resultPayload = res
				if execErr != nil {
					status = "error"
					resultPayload = map[string]string{"error": execErr.Error()}
				}
			}

			resBytes, mErr := json.Marshal(resultPayload)
			if mErr != nil {
				status = "error"
				resultPayload = map[string]string{"error": "tool result could not be encoded: " + mErr.Error()}
				resBytes, _ = json.Marshal(resultPayload) // map[string]string always encodes
			}
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

			metaBytes, mErr := json.Marshal(map[string]any{
				"call_id": tc.ID,
				"name":    tc.Name,
				"status":  status,
			})
			if mErr != nil {
				return fail(fmt.Errorf("encode tool metadata: %w", mErr))
			}
			if _, err := s.store.AppendChatMessage(ctx, store.ChatMessage{
				SessionID: sessionID,
				Role:      "tool",
				Content:   string(resBytes),
				ToolJSON:  string(metaBytes),
			}); err != nil {
				return fail(fmt.Errorf("save tool result: %w", err))
			}

			history = append(history, Message{
				Role:       "tool",
				Content:    string(resBytes),
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
			})
		}
	}

	if !finished {
		// The model was still calling tools when the round limit hit: end with an
		// explicit assistant message instead of silently stopping.
		msg := fmt.Sprintf("I stopped after %d tool-calling rounds without reaching a final answer. Ask me to continue if you want me to keep going.", maxTurns)
		if cb.OnDelta != nil {
			cb.OnDelta(sessionID, msg)
		}
		id, err := s.store.AppendChatMessage(ctx, store.ChatMessage{
			SessionID: sessionID,
			Role:      "assistant",
			Content:   msg,
		})
		if err != nil {
			return fail(fmt.Errorf("save assistant message: %w", err))
		}
		lastMsgID = id
	}

	if cb.OnDone != nil {
		cb.OnDone(sessionID, lastMsgID)
	}
	return nil
}

// sanitizeToolCalls makes every tool call safe to persist and replay: missing
// IDs are generated, and arguments that are not a valid JSON object are
// replaced with {}. It returns the indexes of invalid calls with a reason that
// is sent back to the model as the tool's error result.
func sanitizeToolCalls(calls []ToolCall) map[int]string {
	invalid := map[int]string{}
	for i := range calls {
		tc := &calls[i]
		if strings.TrimSpace(tc.ID) == "" {
			tc.ID = "call_" + uuid.NewString()[:12]
		}
		args := bytes.TrimSpace(tc.Arguments)
		if len(args) == 0 {
			tc.Arguments = json.RawMessage("{}")
			continue
		}
		var obj map[string]any
		if !json.Valid(args) || json.Unmarshal(args, &obj) != nil {
			preview := string(args)
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			invalid[i] = fmt.Sprintf("invalid tool arguments: expected a JSON object, got %q; the call was not executed. Retry with valid JSON arguments.", preview)
			tc.Arguments = json.RawMessage("{}")
			continue
		}
		tc.Arguments = json.RawMessage(args)
		if strings.TrimSpace(tc.Name) == "" {
			invalid[i] = "tool call is missing a tool name; the call was not executed."
		}
	}
	return invalid
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
	return pairToolMessages(msgs), checkpoint, nil
}

// pairToolMessages enforces the provider contract that every tool result
// directly follows the assistant message that issued its call, and every
// issued call has a result. Anywhere in the window (not only at its start) it
// drops tool rows with no matching preceding call, and removes calls whose
// results are missing (e.g. a turn interrupted mid-way, or legacy rows whose
// tool calls failed to persist). Otherwise every later request in the session
// would be rejected with HTTP 400.
func pairToolMessages(in []Message) []Message {
	out := make([]Message, 0, len(in))
	for i := 0; i < len(in); i++ {
		m := in[i]
		if m.Role == "tool" {
			continue // a tool row not consumed by the assistant branch below is an orphan
		}
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			if m.Role == "assistant" && strings.TrimSpace(m.Content) == "" {
				continue // empty assistant turns are rejected by some providers
			}
			out = append(out, m)
			continue
		}
		// Collect the contiguous run of tool results after this assistant message.
		pending := make(map[string]bool, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			pending[tc.ID] = true
		}
		var results []Message
		answered := map[string]bool{}
		j := i + 1
		for ; j < len(in) && in[j].Role == "tool"; j++ {
			id := in[j].ToolCallID
			if pending[id] && !answered[id] {
				answered[id] = true
				results = append(results, in[j])
			}
		}
		var kept []ToolCall
		for _, tc := range m.ToolCalls {
			if answered[tc.ID] {
				kept = append(kept, tc)
			}
		}
		m.ToolCalls = kept
		if len(kept) > 0 || strings.TrimSpace(m.Content) != "" {
			out = append(out, m)
			out = append(out, results...)
		}
		i = j - 1
	}
	return out
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
