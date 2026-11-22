package controller

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/auth"
	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/orchestrator"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// Server wires together HTTP REST API, Browser WS, Worker WS, MCP HTTP, and Embedded PWA.
type Server struct {
	cfg     config.ControllerConfig
	store   store.Store
	auth    *auth.Manager
	hub     *Hub
	service *FleetService
	orch    *orchestrator.Service
	logger  *slog.Logger
	mux     *http.ServeMux
	version string
}

// NewServer creates a fully wired Konsert controller HTTP handler.
func NewServer(cfg config.ControllerConfig, st store.Store, logger *slog.Logger, version string) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	authMgr := auth.NewManager(auth.Options{
		FleetToken:            cfg.FleetToken,
		AdminToken:            cfg.AdminToken,
		MCPToken:              cfg.MCPToken,
		NoAuth:                cfg.NoAuth,
		TrustTailscaleHeaders: cfg.TrustTailscaleHeaders,
	})
	hub := NewHub(st, logger)
	svc := NewFleetService(st, hub)
	orch := orchestrator.NewService(cfg.Orchestrator, st, svc)
	hub.SetOrchestrator(orch)

	s := &Server{
		cfg:     cfg,
		store:   st,
		auth:    authMgr,
		hub:     hub,
		service: svc,
		orch:    orch,
		logger:  logger,
		mux:     http.NewServeMux(),
		version: version,
	}
	s.registerRoutes()
	return s
}

func (s *Server) Hub() *Hub                        { return s.hub }
func (s *Server) Service() *FleetService           { return s.service }
func (s *Server) Orchestrator() *orchestrator.Service { return s.orch }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	// Health
	s.mux.HandleFunc("GET /api/health", s.handleHealth)

	// Browser Auth
	s.mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/auth/me", s.handleMe)

	// Fleet & Tasks REST API (protected by browser/token auth)
	s.mux.Handle("GET /api/fleet", s.requireBrowserAuth(s.handleGetFleet))
	s.mux.Handle("GET /api/usage", s.requireBrowserAuth(s.handleGetUsage))
	s.mux.Handle("GET /api/tasks", s.requireBrowserAuth(s.handleListTasks))
	s.mux.Handle("POST /api/tasks", s.requireBrowserAuth(s.handleCreateTask))
	s.mux.Handle("GET /api/tasks/{id}", s.requireBrowserAuth(s.handleGetTask))
	s.mux.Handle("POST /api/tasks/{id}/steer", s.requireBrowserAuth(s.handleSteerTask))
	s.mux.Handle("POST /api/tasks/{id}/cancel", s.requireBrowserAuth(s.handleCancelTask))
	s.mux.Handle("GET /api/tasks/{id}/events", s.requireBrowserAuth(s.handleTaskEvents))

	// Orchestrator Chat REST API
	s.mux.Handle("GET /api/chat/sessions", s.requireBrowserAuth(s.handleListChatSessions))
	s.mux.Handle("POST /api/chat/sessions", s.requireBrowserAuth(s.handleCreateChatSession))
	s.mux.Handle("GET /api/chat/sessions/{id}/messages", s.requireBrowserAuth(s.handleListChatMessages))

	// WebSocket endpoints
	s.mux.HandleFunc("GET /ws/worker", s.handleWorkerWS)
	s.mux.HandleFunc("GET /ws/client", s.handleClientWS)

	// Streamable HTTP / SSE MCP endpoint
	if s.cfg.EnableMCPHTTP {
		mcpHandler := NewMCPHTTPHandler(s.service, s.version)
		s.mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.auth.CheckMCPAuth(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			mcpHandler.ServeHTTP(w, r)
		}))
	}

	// Embedded Angular PWA static assets & SPA fallback
	s.mux.Handle("/", SPAHandler())
}

func (s *Server) requireBrowserAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.auth.AuthenticateBrowser(r); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		next(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.version,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if !s.auth.CheckAdminToken(body.Token) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
		return
	}
	s.auth.SetSessionCookie(w, r, "operator")
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          "operator",
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, _ *http.Request) {
	s.auth.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth.AuthenticateBrowser(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":        ok,
		"auth_required":        s.auth.AuthRequired(),
		"fleet_token_required": s.cfg.FleetToken != "",
		"user":                 user,
		"provider":             s.cfg.Orchestrator.Provider,
		"model":                s.cfg.Orchestrator.Model,
	})
}

func (s *Server) handleGetFleet(w http.ResponseWriter, r *http.Request) {
	res, err := s.service.FleetStatus(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	tasks, _ := s.store.ListTasks(r.Context(), "", "", 100)
	usage, _ := s.store.GetUsageSummary(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"workers":            res.Workers,
		"active_tasks_count": res.ActiveTasksCount,
		"tasks":              tasks,
		"usage":              usage,
	})
}

func (s *Server) handleGetUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.store.GetUsageSummary(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	items, err := s.service.ListTasks(r.Context(), q.Get("status"), q.Get("worker_id"), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": items})
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req DispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	res, err := s.service.DispatchTask(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wait := strings.EqualFold(r.URL.Query().Get("wait"), "true") || r.URL.Query().Get("wait") == "1"
	timeout, _ := strconv.Atoi(r.URL.Query().Get("timeout"))

	res, err := s.service.GetTaskResult(r.Context(), id, wait, timeout)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSteerTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		InputText     string `json:"input_text"`
		AppendNewline *bool  `json:"append_newline"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	appendNL := true
	if body.AppendNewline != nil {
		appendNL = *body.AppendNewline
	}
	res, err := s.service.SteerTask(r.Context(), id, body.InputText, appendNL)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := s.service.CancelTask(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	afterID, _ := strconv.ParseInt(r.URL.Query().Get("after_id"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.ListEvents(r.Context(), id, afterID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if events == nil {
		events = []store.TaskEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) handleListChatSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListChatSessions(r.Context(), 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if sessions == nil {
		sessions = []store.ChatSession{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) handleCreateChatSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title    string `json:"title"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if strings.TrimSpace(body.Title) == "" {
		body.Title = "Orchestrator Session"
	}
	if body.Provider == "" {
		body.Provider = s.cfg.Orchestrator.Provider
	}
	if body.Model == "" {
		body.Model = s.cfg.Orchestrator.Model
	}
	now := time.Now().UTC()
	cs := store.ChatSession{
		ID:        uuid.NewString(),
		Title:     body.Title,
		Provider:  body.Provider,
		Model:     body.Model,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.CreateChatSession(r.Context(), cs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, cs)
}

func (s *Server) handleListChatMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgs, err := s.store.ListChatMessages(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if msgs == nil {
		msgs = []store.ChatMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

func (s *Server) handleWorkerWS(w http.ResponseWriter, r *http.Request) {
	if !s.auth.CheckFleetAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid fleet token"})
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logger.Warn("accept worker ws failed", "err", err)
		return
	}
	s.hub.ServeWorkerConn(r.Context(), conn)
}

func (s *Server) handleClientWS(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth.AuthenticateBrowser(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logger.Warn("accept client ws failed", "err", err)
		return
	}
	s.hub.ServeBrowserConn(r.Context(), conn, user)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
