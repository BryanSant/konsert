package controller

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// createChatRequest is the POST /api/chats (and legacy POST /api/chat/sessions) body.
type createChatRequest struct {
	Title        string `json:"title"`
	Backend      string `json:"backend"` // "agent" (default) or "api"
	WorkerID     string `json:"worker_id"`
	Agent        string `json:"agent"`
	AccountID    string `json:"account_id"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	ApprovalMode string `json:"approval_mode"`
	Cwd          string `json:"cwd"`
	FleetTools   bool   `json:"fleet_tools"`
	Provider     string `json:"provider"` // api backend only
}

// createChat creates a chat session. Agent chats fill worker, agent, account,
// model, effort and approval mode from the reported capabilities.
func (s *Server) createChat(ctx context.Context, body createChatRequest) (store.ChatSession, error) {
	chatSID, err := s.store.NewSID(ctx, ids.Chat)
	if err != nil {
		return store.ChatSession{}, err
	}
	now := time.Now().UTC()
	cs := store.ChatSession{
		ID:        ids.UUID(),
		SID:       chatSID,
		Title:     strings.TrimSpace(body.Title),
		CreatedAt: now,
		UpdatedAt: now,
	}
	switch strings.ToLower(strings.TrimSpace(body.Backend)) {
	case store.ChatBackendAPI:
		cs.Backend = store.ChatBackendAPI
		s.cfgMu.RLock()
		cs.Provider, cs.Model = s.cfg.Orchestrator.Provider, effectiveModel(s.cfg.Orchestrator)
		s.cfgMu.RUnlock()
		if v := strings.TrimSpace(body.Provider); v != "" {
			cs.Provider = v
		}
		if v := strings.TrimSpace(body.Model); v != "" {
			cs.Model = v
		}
		if cs.Title == "" {
			cs.Title = "Orchestrator Session"
		}
	case "", store.ChatBackendAgent:
		cs.Backend = store.ChatBackendAgent
		cs.WorkerID = resolveRef(ctx, s.store, ids.Machine, body.WorkerID)
		cs.Agent = strings.TrimSpace(body.Agent)
		cs.AccountID = strings.TrimSpace(body.AccountID)
		cs.Model = strings.TrimSpace(body.Model)
		cs.Effort = strings.TrimSpace(body.Effort)
		cs.ApprovalMode = strings.TrimSpace(body.ApprovalMode)
		cs.Cwd = strings.TrimSpace(body.Cwd)
		cs.FleetTools = body.FleetTools
		cs.Status = "idle"
		if cs.WorkerID != "" {
			if _, err := s.store.GetWorker(ctx, cs.WorkerID); err != nil {
				return cs, errInvalidSettings{errors.New("unknown worker_id " + cs.WorkerID)}
			}
		}
		s.hub.resolveChatTarget(ctx, &cs)
		if cs.Cwd == "" {
			cs.Cwd = "~"
		}
		if cs.Title == "" {
			cs.Title = "New chat"
			if cs.FleetTools {
				cs.Title = "Orchestrator"
			}
		}
	default:
		return cs, errInvalidSettings{errors.New(`backend must be "agent" or "api"`)}
	}
	// A new chat is told about fleet events from now on (recent_events has the past).
	s.hub.initNotesCursor(ctx, &cs)
	if err := s.store.CreateChatSession(ctx, cs); err != nil {
		return cs, err
	}
	// Other browsers (and tabs) learn about chats created elsewhere, e.g. by the CLI,
	// the orchestrator or another tab.
	s.hub.broadcastFleetUpdate("chat_session", cs)
	return cs, nil
}

func (s *Server) handleCreateChatSession(w http.ResponseWriter, r *http.Request) {
	var body createChatRequest
	if requestHasBody(r) && !decodeJSONBody(w, r, &body, true) {
		return
	}
	cs, err := s.createChat(r.Context(), body)
	if err != nil {
		writeSettingsError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, cs)
}

// ensureOrchestratorChat returns the default orchestrator chat, creating it as an
// agent chat with fleet tools on the default worker and account.
func (s *Server) ensureOrchestratorChat(ctx context.Context) (store.ChatSession, error) {
	s.orchChatMu.Lock()
	defer s.orchChatMu.Unlock()
	saved, _ := s.store.GetAllSettings(ctx)
	if id := saved[orchestratorChatSetting]; id != "" {
		if cs, err := s.store.GetChatSession(ctx, id); err == nil {
			if cs.Backend == store.ChatBackendAgent && (cs.WorkerID == "" || cs.Agent == "") {
				s.hub.resolveChatTarget(ctx, &cs)
				if cs.WorkerID != "" {
					_ = s.store.UpdateChatSession(ctx, cs)
				}
			}
			return cs, nil
		}
	}
	cs, err := s.createChat(ctx, createChatRequest{Title: "Orchestrator", FleetTools: true})
	if err != nil {
		return cs, err
	}
	if err := s.store.SetSettings(ctx, map[string]string{orchestratorChatSetting: cs.ID}); err != nil {
		return cs, err
	}
	return cs, nil
}

func (s *Server) handleEnsureOrchestratorChat(w http.ResponseWriter, r *http.Request) {
	cs, err := s.ensureOrchestratorChat(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cs)
}

func (s *Server) handleGetChat(w http.ResponseWriter, r *http.Request) {
	cs, err := s.store.GetChatSession(r.Context(), r.PathValue("id"))
	if err != nil {
		writeChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cs)
}

// handleRenameChatSession serves PATCH /api/chats/{id} and the legacy
// PATCH /api/chat/sessions/{id}: title plus agent chat settings.
func (s *Server) handleRenameChatSession(w http.ResponseWriter, r *http.Request) {
	var p ChatSettingsPatch
	if !decodeJSON(w, r, &p) {
		return
	}
	if p.empty() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nothing to update"})
		return
	}
	cs, err := s.hub.updateChatSettings(r.Context(), r.PathValue("id"), p)
	if err != nil {
		if errors.Is(err, errEmptyTitle) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
			return
		}
		writeChatError(w, err)
		return
	}
	s.hub.broadcastFleetUpdate("chat_session", cs)
	writeJSON(w, http.StatusOK, cs)
}

func (s *Server) handleDeleteChatSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cs, _ := s.store.GetChatSession(r.Context(), id)
	if err := s.store.DeleteChatSession(r.Context(), id); err != nil {
		writeChatError(w, err)
		return
	}
	// The row (and its token hash) is gone; drop the in-memory token, end any
	// pending ask_user and close the engine.
	s.hub.forgetChat(id, cs.WorkerID)
	s.hub.broadcastFleetUpdate("chat_session_deleted", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// handleChatEvents serves GET /api/chats/{id}/events?after_seq=&limit= for the
// transcript and reconnect catch-up. Events are protocol.AgentChatEvent objects.
func (s *Server) handleChatEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cs, err := s.store.GetChatSession(r.Context(), id)
	if err != nil {
		writeChatError(w, err)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after_seq"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	evs, err := s.store.ListChatEvents(r.Context(), id, after, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]jsontext.Value, 0, len(evs))
	last := after
	for _, e := range evs {
		out = append(out, jsontext.Value(e.JSON))
		last = e.Seq
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": cs, "events": out, "last_seq": last})
}

func (s *Server) handleListAgentAccounts(w http.ResponseWriter, r *http.Request) {
	views, err := s.hub.agentAccountViews(r.Context(), strings.TrimSpace(r.URL.Query().Get("worker_id")))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": views})
}

func (s *Server) handleRefreshAgentAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkerID  string `json:"worker_id"`
		AccountID string `json:"account_id"`
	}
	if requestHasBody(r) && !decodeJSON(w, r, &body) {
		return
	}
	ids, err := s.hub.RefreshAgentCapabilities(strings.TrimSpace(body.WorkerID), strings.TrimSpace(body.AccountID))
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "workers": ids})
}

func writeChatError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "chat session not found"})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// --- per-chat MCP endpoint ---------------------------------------------------------

// bearerToken extracts an Authorization: Bearer token.
func bearerToken(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

// handleChatMCP serves /mcp/chat/{id}: a Streamable HTTP MCP server for one agent
// chat, authenticated by that chat's bearer token (never a browser cookie).
func (s *Server) handleChatMCP(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.hub.verifyChatToken(r.Context(), r.PathValue("id"), bearerToken(r))
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	user := "agent-chat:" + cs.ID
	source := auditSourceMCP
	if cs.FleetTools {
		user, source = "orchestrator", auditSourceOrchestrator
	}
	r = r.WithContext(withChatOrigin(withAuditActor(r.Context(), user, source), cs.ID, cs.FleetTools))
	s.hub.chatMCPHandler(cs).ServeHTTP(w, r)
}

// chatMCPHandler returns the chat's cached MCP handler: ask_user always, the
// fleet tools only when fleet_tools is set.
func (h *Hub) chatMCPHandler(cs store.ChatSession) http.Handler {
	rt := h.chatRT(cs.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.mcp != nil && rt.mcpFleet == cs.FleetTools {
		return rt.mcp
	}
	var srv *mcp.Server
	var control HumanControlBackend
	if cs.FleetTools && h.chatFleetBackend != nil {
		srv = newMCPServer(h.chatFleetBackend, h.version, nil)
		control, _ = h.chatFleetBackend.(HumanControlBackend)
	} else {
		v := h.version
		if v == "" {
			v = "1.0.0"
		}
		srv = mcp.NewServer(&mcp.Implementation{Name: "konsert", Version: v}, nil)
	}
	chatID := cs.ID
	mcp.AddTool(srv, &mcp.Tool{
		Name: "ask_user",
		Description: "Ask the user a question in the Konsert chat window and wait for the answer. " +
			"Use it whenever you need a decision or information from the user. Give options for multiple choice; " +
			"allow_text accepts a free-form answer (always on when no options are given). Returns the user's answer text, " +
			"or a line starting with \"no answer\" if the user did not answer.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in askUserInput) (*mcp.CallToolResult, any, error) {
		ans, err := h.askUser(ctx, chatID, in)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: ans}}}, nil, nil
	})
	h.addChatControlTools(srv, cs, control)
	rt.mcp = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	rt.mcpFleet = cs.FleetTools
	return rt.mcp
}
