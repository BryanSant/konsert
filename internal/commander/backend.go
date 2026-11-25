package commander

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// maxTurns bounds LLM round-trips (tool-calling rounds) per user message.
const maxTurns = 10

// defaultSystemPrompt is the API-key backend's system prompt.
const defaultSystemPrompt = RolePrompt + apiToolsPrompt + "\n" + NarrationPrompt

// ContextProvider optionally supplies a compact live fleet + taxonomy + memory digest for every LLM turn.
type ContextProvider interface {
	BuildCompactContext(ctx context.Context, sessionID string) string
}

// ToolDef describes a commander tool in JSON Schema format.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolCall represents a single model-initiated tool invocation.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments jsontext.Value `json:"arguments"`
}

// Message is the provider-agnostic chat message representation.
type Message struct {
	Role         string     `json:"role"` // "user", "assistant", "tool"
	Content      string     `json:"content"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID   string     `json:"tool_call_id,omitempty"`
	ToolName     string     `json:"tool_name,omitempty"`
	InputTokens  int64      `json:"input_tokens,omitzero"`
	OutputTokens int64      `json:"output_tokens,omitzero"`
	// AnthropicContent holds the raw assistant content blocks (including
	// signed thinking blocks) so a tool-use turn can be continued exactly as
	// the Anthropic API requires. It lives only in memory for the current turn.
	AnthropicContent jsontext.Value `json:"-"`
}

// Provider implements a direct LLM API client.
type Provider interface {
	Name() string
	Complete(ctx context.Context, model, system string, messages []Message, tools []ToolDef, onDelta func(string)) (Message, error)
}

// ToolExecutor bridges the commander loop to the controller's FleetService.
type ToolExecutor interface {
	ToolDefinitions() []ToolDef
	ExecuteTool(ctx context.Context, name string, args jsontext.Value) (any, error)
}

// StreamCallbacks receives real-time chat events for forwarding over browser WebSocket.
type StreamCallbacks struct {
	OnDelta func(sessionID, delta string)
	OnTool  func(payload protocol.ChatToolPayload)
	OnDone  func(sessionID string, messageID int64)
	OnError func(sessionID, errMsg string)
	OnUsage func(summary store.UsageSummary)
}

// Service runs the built-in Commander Chat backend with server-side tool execution.
type Service struct {
	mu       sync.RWMutex
	cfg      config.CommanderConfig
	store    store.Store
	executor ToolExecutor
	provider Provider
	// maxTokens caps output tokens per LLM call; 0 means provider defaults
	// (DefaultMaxTokens for Anthropic, unset for OpenAI/Gemini).
	maxTokens int
	// providerErr is set when cfg names an unsupported provider.
	providerErr error
}

// SupportedProviders lists the provider names the commander accepts.
// An empty provider name means "openai".
var SupportedProviders = []string{"openai", "anthropic", "gemini"}

// ErrUnknownProvider is returned (wrapped) for a provider name the commander
// does not support.
var ErrUnknownProvider = errors.New("unknown LLM provider")

// ValidateProvider reports whether name is a supported provider ("" is allowed
// and means openai). Use it to reject bad config before it is applied.
func ValidateProvider(name string) error {
	n := normalizeProvider(name)
	for _, p := range SupportedProviders {
		if n == p {
			return nil
		}
	}
	return fmt.Errorf("%w %q (supported: %s)", ErrUnknownProvider, strings.TrimSpace(name), strings.Join(SupportedProviders, ", "))
}

// buildProvider constructs the configured provider. An unknown provider name is
// an error: silently falling back to OpenAI would send the configured API key
// to api.openai.com.
func buildProvider(cfg config.CommanderConfig, maxTokens int) (Provider, error) {
	if err := ValidateProvider(cfg.Provider); err != nil {
		return nil, err
	}
	switch normalizeProvider(cfg.Provider) {
	case "anthropic":
		p := NewAnthropicProvider(cfg.APIKey, cfg.BaseURL)
		p.MaxTokens = maxTokens
		return p, nil
	case "gemini":
		p := NewGeminiProvider(cfg.APIKey, cfg.BaseURL)
		p.MaxTokens = maxTokens
		return p, nil
	default:
		p := NewOpenAIProvider(cfg.APIKey, cfg.BaseURL)
		p.MaxTokens = maxTokens
		return p, nil
	}
}

// NewService creates a Commander Service for the configured LLM provider.
// If the provider name is unknown the Service is still returned (so the hub can
// start), but every chat turn fails with that error and no request is sent;
// ProviderError reports it.
func NewService(cfg config.CommanderConfig, st store.Store, executor ToolExecutor) *Service {
	s := &Service{
		cfg:      cfg,
		store:    st,
		executor: executor,
	}
	s.provider, s.providerErr = buildProvider(cfg, 0)
	return s
}

// UpdateConfig hot-reloads the LLM provider, model, base URL, and API key at
// runtime. It returns an error for an unknown provider; the new config is
// still recorded and chat turns fail with that error until it is fixed (they
// never fall back to another provider).
func (s *Service) UpdateConfig(cfg config.CommanderConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	s.provider, s.providerErr = buildProvider(cfg, s.maxTokens)
	return s.providerErr
}

// ProviderError returns the error from building the configured provider, if any.
func (s *Service) ProviderError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.providerErr
}

// SetMaxTokens sets the per-call output token ceiling (0 restores provider
// defaults) and rebuilds the provider. It exists until CommanderConfig grows
// a max_tokens field that NewService/UpdateConfig can read directly.
func (s *Service) SetMaxTokens(n int) {
	if n < 0 {
		n = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxTokens = n
	s.provider, s.providerErr = buildProvider(s.cfg, n)
}

// SetProvider replaces the LLM provider (useful for tests).
func (s *Service) SetProvider(p Provider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provider = p
	s.providerErr = nil
}

// HandleUserMessage persists the user's message, runs the LLM + tool-calling loop,
// persists assistant & tool turns, and streams events via cb.
func (s *Service) HandleUserMessage(ctx context.Context, sessionID, content string, cb StreamCallbacks) error {
	s.mu.RLock()
	cfg := s.cfg
	prov := s.provider
	provErr := s.providerErr
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

	if provErr != nil {
		return fail(provErr)
	}
	if prov == nil {
		return fail(errors.New("commander LLM provider is not configured"))
	}

	sess, err := s.store.GetChatSession(ctx, sessionID)
	if err != nil {
		// Auto-create session if needed.
		sess = store.ChatSession{
			ID:        sessionID,
			Title:     "Commander",
			Backend:   store.ChatBackendAPI,
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
			msg := fmt.Sprintf("This chat uses the optional API-key backend, which has no API key configured for provider %q. Agent chats run on your logged-in agent CLIs and need no API key: start a new chat with an agent, or add a key in Settings.", cfg.Provider)
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
			b, mErr := json.Marshal(reply.ToolCalls, storeJSONOpts)
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

			resBytes, mErr := json.Marshal(resultPayload, storeJSONOpts)
			if mErr != nil {
				status = "error"
				resultPayload = map[string]string{"error": "tool result could not be encoded: " + mErr.Error()}
				resBytes, _ = json.Marshal(resultPayload, storeJSONOpts) // map[string]string always encodes
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
			tc.ID = "call_" + strings.ReplaceAll(ids.UUID(), "-", "")
		}
		args := bytes.TrimSpace(tc.Arguments)
		if len(args) == 0 {
			tc.Arguments = jsontext.Value("{}")
			continue
		}
		var obj map[string]any
		if json.Unmarshal(args, &obj) != nil {
			preview := string(args)
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			invalid[i] = fmt.Sprintf("invalid tool arguments: expected a JSON object, got %q; the call was not executed. Retry with valid JSON arguments.", preview)
			tc.Arguments = jsontext.Value("{}")
			continue
		}
		tc.Arguments = jsontext.Value(args)
		if strings.TrimSpace(tc.Name) == "" {
			invalid[i] = "tool call is missing a tool name; the call was not executed."
		}
	}
	return invalid
}

// loadBoundedHistory keeps the prompt compact when threads grow long by summarizing overflow turns
// into persistent SQLite commander_memory ("session:<id>", "thread_checkpoint") so context wipes never lose state.
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

// requestJSONOpts encode provider request bodies. Map keys are sorted (as v1 did)
// so identical requests serialize identically: tool schemas are maps, and a
// stable prefix keeps provider prompt caching effective. Tool results can carry
// command output that is not valid UTF-8; it is replaced with U+FFFD rather than
// failing the request.
var requestJSONOpts = json.JoinOptions(json.Deterministic(true), jsontext.AllowInvalidUTF8(true))

// storeJSONOpts encode JSON the backend persists and replays (tool calls and
// results, Anthropic content blocks).
var storeJSONOpts = jsontext.AllowInvalidUTF8(true)
