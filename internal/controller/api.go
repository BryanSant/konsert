package controller

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/auth"
	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/orchestrator"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/secrets"
	"github.com/BryanSant/konsert/internal/sshutil"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/BryanSant/konsert/internal/worker"
	"github.com/coder/websocket"
)

// Server wires together HTTP REST API, Browser WS, Worker WS, MCP HTTP, and Embedded PWA.
type Server struct {
	cfgMu   sync.RWMutex
	cfg     config.ControllerConfig
	store   store.Store
	auth    *auth.Manager
	hub     *Hub
	service *FleetService
	orch    *orchestrator.Service
	logger  *slog.Logger
	mux     *http.ServeMux
	handler http.Handler
	version string

	// originPatterns are the extra browser origins allowed besides the request Host.
	originPatterns []string
	loginLimiter   *auth.LoginLimiter
	// pairLimiter and pairGlobal throttle failed pairing-code redemptions per peer and
	// across all peers; loginCodeLimiter and loginCodeGlobal cap CLI login codes per peer and
	// overall, pollLimiter their polls.
	pairLimiter      *auth.LoginLimiter
	pairGlobal       *auth.LoginLimiter
	loginCodeLimiter *auth.WindowLimiter
	loginCodeGlobal  *auth.WindowLimiter
	pollLimiter      *auth.WindowLimiter
	// rotateRecovery persists a rotated recovery key (guarded by cfgMu; devices.go).
	rotateRecovery RecoveryKeyRotator
	inflight       inflightTracker
	// secrets encrypts the LLM API key before it is stored in the database.
	secrets *secrets.Box
	// orchChatMu serializes creation of the default orchestrator chat.
	orchChatMu sync.Mutex
	// boundAddr is the address the HTTP server listens on (SetListenAddr; guarded by cfgMu).
	boundAddr string
	// launch starts konsertd on the hub's machine and on SSH hosts (launch.go).
	launch *launcher
	// machines holds machine-credential state (machine_auth.go).
	machines *machineAuth
	// attach keeps the hub-dialed links to ssh and local machines (transport.go).
	attach *attacher
	// hubKeyFile overrides where the hub's SSH key lives (tests); hubIDMu serializes
	// creating the hub id.
	hubKeyFile string
	hubIDMu    sync.Mutex
}

// NewServer creates a fully wired Konsert controller HTTP handler.
func NewServer(cfg config.ControllerConfig, st store.Store, logger *slog.Logger, version string) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	// Apply persisted settings from SQLite over startup defaults if present.
	// Saved values that fail validation (e.g. a provider typo written by an older
	// build) are ignored so the controller never silently sends the key elsewhere.
	box := loadSecretBox(cfg, logger)
	idleDays := defaultDeviceIdleDays
	if saved, err := st.GetAllSettings(context.Background()); err == nil {
		idleDays = deviceIdleDays(saved)
		migratePlaintextSecrets(st, box, saved, logger)
		merged := cfg.Orchestrator
		applySavedOrchestratorSettings(&merged, saved, box, logger)
		if err := validateOrchestratorConfig(merged); err != nil {
			logger.Error("ignoring invalid saved orchestrator settings; using startup config", "err", err)
		} else {
			cfg.Orchestrator = merged
		}
	}
	authMgr := auth.NewManager(auth.Options{
		RecoveryKey:           cfg.RecoveryKey,
		NoAuth:                cfg.NoAuth,
		TrustTailscaleHeaders: cfg.TrustTailscaleHeaders,
		Devices:               st,
		IdleExpiry:            idleDuration(idleDays),
	})
	hub := NewHub(st, logger)
	svc := NewFleetService(st, hub)
	orch := orchestrator.NewService(cfg.Orchestrator, st, svc)
	orch.SetMaxTokens(cfg.Orchestrator.MaxTokens)
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

		originPatterns: buildOriginPatterns(cfg),
		loginLimiter:   auth.NewLoginLimiter(),
		pairLimiter:    auth.NewLoginLimiter(),
		pairGlobal: &auth.LoginLimiter{FreeAttempts: 50, BaseDelay: time.Second, MaxDelay: time.Minute,
			Now: time.Now},
		loginCodeLimiter: auth.NewWindowLimiter(10, time.Minute),
		loginCodeGlobal:  auth.NewWindowLimiter(30, time.Minute),
		pollLimiter:      auth.NewWindowLimiter(120, time.Minute),
		secrets:          box,
		launch:           newLauncher(),
		machines:         newMachineAuth(),
	}
	s.attach = newAttacher(s)
	hub.version = version
	hub.chatFleetBackend = serverControlPlane{FleetService: svc, srv: s, admin: false}
	hub.chatMCP = s.serveRelayedChatMCP
	s.registerRoutes()
	s.handler = s.guard(s.mux)
	return s
}

func (s *Server) Hub() *Hub                           { return s.hub }
func (s *Server) Service() *FleetService              { return s.service }
func (s *Server) Orchestrator() *orchestrator.Service { return s.orch }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.inflight.add()
	defer s.inflight.done()
	s.handler.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	// Health
	s.mux.HandleFunc("GET /api/health", s.handleHealth)

	// Browser Auth
	s.mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/auth/me", s.handleMe)
	s.mux.HandleFunc("POST /api/auth/pair", s.handlePair)
	s.mux.HandleFunc("POST /api/auth/device-code", s.handleStartLogin)
	s.mux.HandleFunc("POST /api/auth/device-code/poll", s.handlePollLogin)
	s.mux.Handle("GET /api/auth/device-code/{code}", s.requireBrowserAuth(s.handleGetLogin))
	s.mux.Handle("POST /api/auth/device-code/{code}/approve", s.requireBrowserAuth(s.handleDecideLogin(true)))
	s.mux.Handle("POST /api/auth/device-code/{code}/deny", s.requireBrowserAuth(s.handleDecideLogin(false)))
	s.mux.Handle("POST /api/auth/recovery-key/rotate", s.requireBrowserAuth(s.handleRotateRecoveryKey))
	s.mux.Handle("POST /api/pairing-codes", s.requireBrowserAuth(s.handleCreatePairingCode))
	s.mux.Handle("GET /api/devices", s.requireBrowserAuth(s.handleListDevices))
	s.mux.Handle("POST /api/devices/mcp", s.requireBrowserAuth(s.handleCreateMCPDevice))
	s.mux.Handle("PATCH /api/devices/{id}", s.requireBrowserAuth(s.byRef(ids.Device, s.handleRenameDevice)))
	s.mux.Handle("DELETE /api/devices/{id}", s.requireBrowserAuth(s.byRef(ids.Device, s.handleRevokeDevice)))

	// Fleet & Tasks REST API (protected by browser/token auth)
	s.mux.Handle("GET /api/fleet", s.requireBrowserAuth(s.handleGetFleet))
	s.mux.Handle("GET /api/fleet/heat", s.requireBrowserAuth(s.handleFleetHeat))
	s.mux.Handle("POST /api/fleet/fanout", s.requireBrowserAuth(s.handleFleetFanOut))
	s.mux.Handle("DELETE /api/workers/{id}", s.requireBrowserAuth(s.byRef(ids.Machine, s.handleDeleteWorker)))
	s.mux.Handle("GET /api/workers/{id}/facts", s.requireBrowserAuth(s.byRef(ids.Machine, s.handleGetWorkerFacts)))
	s.mux.Handle("GET /api/workers/{id}/ports", s.requireBrowserAuth(s.byRef(ids.Machine, s.handleGetWorkerPorts)))
	s.mux.Handle("POST /api/workers/local", s.requireBrowserAuth(s.handleAddLocalWorker))
	s.mux.Handle("POST /api/workers/{id}/start", s.requireBrowserAuth(s.byRef(ids.Machine, s.handleStartWorker)))

	// Machine credentials and join codes (machine_auth.go). /api/machines/join is
	// authenticated by the join code itself and rate-limited.
	s.mux.HandleFunc("POST /api/machines/join", s.handleMachineJoin)
	s.mux.Handle("POST /api/machines/join-codes", s.requireBrowserAuth(s.handleCreateJoinCode))
	s.mux.Handle("GET /api/machines/join-codes/{id}", s.requireBrowserAuth(s.handleGetJoinCode))
	s.mux.Handle("POST /api/machines/credentials/rotate", s.requireBrowserAuth(s.handleRotateMachineCredentials))
	s.mux.Handle("GET /api/workers/{id}/credential", s.requireBrowserAuth(s.byRef(ids.Machine, s.handleGetMachineCredential)))
	s.mux.Handle("POST /api/workers/{id}/credential/revoke", s.requireBrowserAuth(s.byRef(ids.Machine, s.handleRevokeMachineCredential)))
	s.mux.Handle("GET /api/setup", s.requireBrowserAuth(s.handleGetSetup))
	s.mux.Handle("POST /api/setup/dismiss", s.requireBrowserAuth(s.handleDismissSetup))
	s.registerWakeLockRoutes()
	s.mux.Handle("GET /api/orchestrator/memory", s.requireBrowserAuth(s.handleListMemory))
	s.mux.Handle("POST /api/orchestrator/memory", s.requireBrowserAuth(s.handleUpsertMemory))
	s.mux.Handle("DELETE /api/orchestrator/memory", s.requireBrowserAuth(s.handleDeleteMemory))
	s.mux.Handle("GET /api/fs/list", s.requireBrowserAuth(s.handleFSList))
	s.mux.Handle("GET /api/ssh/hosts", s.requireBrowserAuth(s.handleSSHHosts))
	s.mux.Handle("POST /api/ssh/bootstrap", s.requireBrowserAuth(s.handleSSHBootstrap))
	s.mux.Handle("GET /api/usage", s.requireBrowserAuth(s.handleGetUsage))
	s.mux.Handle("POST /api/usage/quotas", s.requireBrowserAuth(s.handleUpsertQuotas))
	s.mux.Handle("GET /api/spaces", s.requireBrowserAuth(s.handleListSpaces))
	s.mux.Handle("POST /api/spaces", s.requireBrowserAuth(s.handleCreateSpace))
	s.mux.Handle("DELETE /api/spaces/{id}", s.requireBrowserAuth(s.byRef(ids.Space, s.handleDeleteSpace)))
	s.mux.Handle("GET /api/taxonomy", s.requireBrowserAuth(s.handleGetTaxonomy))
	s.mux.Handle("GET /api/workspaces", s.requireBrowserAuth(s.handleListWorkspaces))
	s.mux.Handle("POST /api/workspaces", s.requireBrowserAuth(s.handleCreateWorkspace))
	s.mux.Handle("DELETE /api/workspaces/{id}", s.requireBrowserAuth(s.byRef(ids.Workspace, s.handleDeleteWorkspace)))
	s.mux.Handle("GET /api/projects", s.requireBrowserAuth(s.handleListProjects))
	s.mux.Handle("POST /api/projects", s.requireBrowserAuth(s.handleCreateProject))
	s.mux.Handle("DELETE /api/projects/{id}", s.requireBrowserAuth(s.byRef(ids.Project, s.handleDeleteProject)))
	s.mux.Handle("GET /api/repos", s.requireBrowserAuth(s.handleListRepos))
	s.mux.Handle("POST /api/repos", s.requireBrowserAuth(s.handleCreateRepo))
	s.mux.Handle("DELETE /api/repos/{id}", s.requireBrowserAuth(s.byRef(ids.Repo, s.handleDeleteRepo)))
	s.mux.Handle("GET /api/worktrees", s.requireBrowserAuth(s.handleListWorktrees))
	s.mux.Handle("POST /api/worktrees", s.requireBrowserAuth(s.handleCreateWorktree))
	s.mux.Handle("DELETE /api/worktrees/{id}", s.requireBrowserAuth(s.byRef(ids.Worktree, s.handleDeleteWorktree)))
	s.mux.Handle("GET /api/sessions/defaults", s.requireBrowserAuth(s.handleSessionDefaults))
	s.mux.Handle("POST /api/sessions", s.requireBrowserAuth(s.handleCreateTask))
	s.mux.Handle("GET /api/settings", s.requireBrowserAuth(s.handleGetSettings))
	s.mux.Handle("PUT /api/settings", s.requireBrowserAuth(s.handleUpdateSettings))
	s.mux.Handle("GET /api/git/inspect", s.requireBrowserAuth(s.handleGitInspect))
	s.mux.Handle("POST /api/git/commit", s.requireBrowserAuth(s.handleGitCommit))
	s.mux.Handle("POST /api/git/worktree/cleanup", s.requireBrowserAuth(s.handleGitWorktreeCleanup))
	s.mux.Handle("GET /api/tasks", s.requireBrowserAuth(s.handleListTasks))
	s.mux.Handle("POST /api/tasks", s.requireBrowserAuth(s.handleCreateTask))
	s.mux.Handle("GET /api/tasks/{id}", s.requireBrowserAuth(s.byRef(ids.Task, s.handleGetTask)))
	s.mux.Handle("POST /api/tasks/{id}/steer", s.requireBrowserAuth(s.byRef(ids.Task, s.handleSteerTask)))
	s.mux.Handle("POST /api/tasks/{id}/cancel", s.requireBrowserAuth(s.byRef(ids.Task, s.handleCancelTask)))
	s.mux.Handle("GET /api/tasks/{id}/events", s.requireBrowserAuth(s.byRef(ids.Task, s.handleTaskEvents)))
	s.mux.Handle("POST /api/tasks/{id}/control", s.requireBrowserAuth(s.byRef(ids.Task, s.handleTaskControl)))
	s.mux.Handle("POST /api/tasks/{id}/review", s.requireBrowserAuth(s.byRef(ids.Task, s.handleReviewTask)))
	s.mux.Handle("GET /api/tasks/{id}/acceptance", s.requireBrowserAuth(s.byRef(ids.Task, s.handleTaskAcceptance)))
	s.mux.Handle("PUT /api/tasks/{id}/expedite", s.requireBrowserAuth(s.byRef(ids.Task, s.handleExpedite("task"))))
	s.mux.Handle("PUT /api/chats/{id}/expedite", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleExpedite("chat"))))
	s.mux.Handle("PUT /api/tasks/{id}/keep", s.requireBrowserAuth(s.byRef(ids.Task, s.handleKeep("task"))))
	s.mux.Handle("PUT /api/chats/{id}/keep", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleKeep("chat"))))
	s.mux.Handle("GET /api/storage", s.requireBrowserAuth(s.handleGetStorage))
	s.mux.Handle("PUT /api/storage/retention", s.requireBrowserAuth(s.handlePutRetention))
	s.mux.Handle("GET /api/storage/compact", s.requireBrowserAuth(s.handleGetCompaction))
	s.mux.Handle("POST /api/storage/compact", s.requireBrowserAuth(s.handleCompactNow))
	s.mux.Handle("POST /api/tasks/{id}/shells/kill", s.requireBrowserAuth(s.byRef(ids.Task, s.handleShellAction("task", "kill"))))
	s.mux.Handle("POST /api/tasks/{id}/shells/ask", s.requireBrowserAuth(s.byRef(ids.Task, s.handleShellAction("task", "ask"))))
	s.mux.Handle("POST /api/chats/{id}/shells/kill", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleShellAction("chat", "kill"))))
	s.mux.Handle("POST /api/chats/{id}/shells/ask", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleShellAction("chat", "ask"))))
	// Shells left running by finished sessions (KON-DCEZV): reminders, Keep running, policy.
	s.mux.Handle("POST /api/tasks/{id}/shells/keep", s.requireBrowserAuth(s.byRef(ids.Task, s.handleKeepShells("task"))))
	s.mux.Handle("POST /api/chats/{id}/shells/keep", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleKeepShells("chat"))))
	s.mux.Handle("GET /api/shells/reminders", s.requireBrowserAuth(s.handleListShellReminders))
	s.mux.Handle("GET /api/settings/shells", s.requireBrowserAuth(s.handleGetShellPolicy))
	s.mux.Handle("PUT /api/settings/shells", s.requireBrowserAuth(s.handlePutShellPolicy))
	s.mux.Handle("GET /api/reviews", s.requireBrowserAuth(s.handleListReviews))
	s.mux.Handle("GET /api/audit", s.requireBrowserAuth(s.handleListAudit))

	// Human control (PRD §5.8a): interventions, holds, open requests, the fleet timeline.
	s.mux.Handle("POST /api/tasks/{id}/suspend", s.requireBrowserAuth(s.byRef(ids.Task, s.handleSuspendTask)))
	s.mux.Handle("POST /api/tasks/{id}/resume", s.requireBrowserAuth(s.byRef(ids.Task, s.handleResumeTask)))
	s.mux.Handle("POST /api/tasks/{id}/veto", s.requireBrowserAuth(s.byRef(ids.Task, s.handleVetoTask)))
	s.mux.Handle("POST /api/chats/{id}/suspend", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleSuspendChat)))
	s.mux.Handle("POST /api/chats/{id}/resume", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleResumeChat)))
	s.mux.Handle("POST /api/chats/{id}/stop", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleStopChat)))
	s.mux.Handle("POST /api/chats/{id}/veto", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleVetoChat)))
	s.mux.Handle("POST /api/workers/{id}/drain", s.requireBrowserAuth(s.byRef(ids.Machine, s.handleDrainWorker)))
	s.mux.Handle("POST /api/workers/{id}/suspend-all", s.requireBrowserAuth(s.byRef(ids.Machine, s.workerControl((*Hub).SuspendAll))))
	s.mux.Handle("POST /api/workers/{id}/resume-all", s.requireBrowserAuth(s.byRef(ids.Machine, s.workerControl((*Hub).ResumeAll))))
	s.mux.Handle("POST /api/workers/{id}/stop-all", s.requireBrowserAuth(s.byRef(ids.Machine, s.workerControl((*Hub).StopAll))))
	s.mux.Handle("GET /api/fleet/pause", s.requireBrowserAuth(s.handleGetFleetPause))
	s.mux.Handle("POST /api/fleet/pause", s.requireBrowserAuth(s.handlePauseFleet))
	s.mux.Handle("POST /api/fleet/resume", s.requireBrowserAuth(s.handleResumeFleet))
	s.mux.Handle("GET /api/holds", s.requireBrowserAuth(s.handleListHolds))
	s.mux.Handle("POST /api/holds", s.requireBrowserAuth(s.handleCreateHold))
	s.mux.Handle("POST /api/holds/{id}/release", s.requireBrowserAuth(s.byRef(ids.Hold, s.handleReleaseHold)))
	s.mux.Handle("GET /api/requests", s.requireBrowserAuth(s.handleListRequests))
	s.mux.Handle("POST /api/requests/{id}/resolve", s.requireBrowserAuth(s.byRef(ids.Request, s.handleResolveRequest)))
	s.mux.Handle("GET /api/overrides", s.requireBrowserAuth(s.handleListOverrides))
	s.mux.Handle("POST /api/overrides", s.requireBrowserAuth(s.handleCreateOverride))
	s.mux.Handle("POST /api/overrides/{id}/approve", s.requireBrowserAuth(s.byRef(ids.Override, s.decideOverrideHandler(true))))
	s.mux.Handle("POST /api/overrides/{id}/deny", s.requireBrowserAuth(s.byRef(ids.Override, s.decideOverrideHandler(false))))
	s.mux.Handle("GET /api/vetoes", s.requireBrowserAuth(s.handleListVetoes))
	s.mux.Handle("POST /api/vetoes/{id}/dismiss", s.requireBrowserAuth(s.byRef(ids.Veto, s.handleDismissVeto)))
	s.mux.Handle("GET /api/events", s.requireBrowserAuth(s.handleListFleetEvents))
	s.mux.Handle("GET /api/events/recent", s.requireBrowserAuth(s.handleRecentEvents))
	// Questions (PRD §5.8b): the fleet-wide queue, side conversations, delegation.
	s.registerQuestionRoutes()

	// Orchestrator Chat & Attachments REST API
	s.mux.Handle("GET /api/chat/sessions", s.requireBrowserAuth(s.handleListChatSessions))
	s.mux.Handle("POST /api/chat/sessions", s.requireBrowserAuth(s.handleCreateChatSession))
	s.mux.Handle("PATCH /api/chat/sessions/{id}", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleRenameChatSession)))
	s.mux.Handle("DELETE /api/chat/sessions/{id}", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleDeleteChatSession)))
	s.mux.Handle("GET /api/chat/sessions/{id}/messages", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleListChatMessages)))
	// Agent chat (subscription-backed; see hub_agentchat.go).
	s.mux.Handle("GET /api/chats", s.requireBrowserAuth(s.handleListChatSessions))
	s.mux.Handle("POST /api/chats", s.requireBrowserAuth(s.handleCreateChatSession))
	s.mux.Handle("POST /api/chats/orchestrator", s.requireBrowserAuth(s.handleEnsureOrchestratorChat))
	s.mux.Handle("GET /api/chats/{id}", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleGetChat)))
	s.mux.Handle("PATCH /api/chats/{id}", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleRenameChatSession)))
	s.mux.Handle("DELETE /api/chats/{id}", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleDeleteChatSession)))
	s.mux.Handle("GET /api/chats/{id}/events", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleChatEvents)))
	s.mux.Handle("GET /api/agents/accounts", s.requireBrowserAuth(s.handleListAgentAccounts))
	s.mux.Handle("POST /api/agents/accounts/refresh", s.requireBrowserAuth(s.handleRefreshAgentAccounts))
	s.mux.Handle("POST /api/attachments", s.requireBrowserAuth(s.handleUploadAttachment))

	// WebSocket endpoints
	s.mux.HandleFunc("GET /ws/worker", s.handleWorkerWS)
	s.mux.HandleFunc("GET /ws/client", s.handleClientWS)

	// Streamable HTTP / SSE MCP endpoint
	if s.cfg.EnableMCPHTTP {
		// Two MCP servers with separate session spaces: one for admin-scoped callers
		// (recovery key, CLI/browser device credentials, --no-auth) and one for
		// MCP-only device credentials, which may not redirect or replace the LLM API key.
		adminMCP := NewMCPHTTPHandler(serverControlPlane{FleetService: s.service, srv: s, admin: true}, s.version, s.orch)
		scopedMCP := NewMCPHTTPHandler(serverControlPlane{FleetService: s.service, srv: s, admin: false}, s.version, s.orch)
		s.mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok, admin := s.auth.MCPScope(r)
			if !ok {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			r = r.WithContext(withAuditActor(withIdentity(r.Context(), id), id.User, auditSourceMCP))
			if admin {
				adminMCP.ServeHTTP(w, r)
				return
			}
			scopedMCP.ServeHTTP(w, r)
		}))
	}

	// Embedded Angular PWA static assets & SPA fallback
	s.mux.Handle("/", SPAHandler())
}

func (s *Server) requireBrowserAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.auth.Authenticate(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		// MCP credentials are for /mcp only: they never reach the REST API or sockets.
		if id.Device != nil && id.Device.Kind == store.DeviceKindMCP {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "MCP credentials only work on /mcp"})
			return
		}
		// Control actions are audited against the authenticated identity.
		r = r.WithContext(withAuditActor(withIdentity(r.Context(), id), id.User, restAuditSource(r)))
		s.withQueryRefs(next).ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.version,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id, ok := s.auth.Authenticate(r)
	if ok && id.Device != nil && id.Device.Kind == store.DeviceKindMCP {
		id, ok = auth.Identity{}, false
	}
	s.cfgMu.RLock()
	prov := s.cfg.Orchestrator.Provider
	model := effectiveModel(s.cfg.Orchestrator)
	s.cfgMu.RUnlock()
	resp := map[string]any{
		"authenticated": ok,
		"auth_required": s.auth.AuthRequired(),
		"user":          id.User,
		"method":        id.Method,
		"provider":      prov,
		"model":         model,
	}
	if id.Device != nil {
		resp["device"] = s.viewDevice(*id.Device, id.Device.ID)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetFleet(w http.ResponseWriter, r *http.Request) {
	res, err := s.service.FleetStatus(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	tasks, _ := s.store.ListTasks(r.Context(), "", "", 100)
	spaces, _ := s.store.ListSpaces(r.Context(), "")
	taxonomy, _ := s.store.GetTaxonomy(r.Context())
	usage, _ := s.store.GetUsageSummary(r.Context())
	reviews, _ := s.store.ListReviews(r.Context())
	if tasks == nil {
		tasks = []store.Task{}
	}
	if spaces == nil {
		spaces = []store.Space{}
	}
	if reviews == nil {
		reviews = []store.Review{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workers":            res.Workers,
		"active_tasks_count": res.ActiveTasksCount,
		"tasks":              tasks,
		"spaces":             spaces,
		"taxonomy":           taxonomy,
		"usage":              usage,
		"controller_version": s.version,
		"pause":              res.Pause,
		"activity":           s.hub.activityList(),
		"reviews":            reviews,
	})
}

func (s *Server) handleListSpaces(w http.ResponseWriter, r *http.Request) {
	workerID := r.URL.Query().Get("worker_id")
	spaces, err := s.store.ListSpaces(r.Context(), workerID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if spaces == nil {
		spaces = []store.Space{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"spaces": spaces})
}

func (s *Server) handleCreateSpace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkerID string `json:"worker_id"`
		Name     string `json:"name"`
		Path     string `json:"path"`
		Branch   string `json:"branch"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.WorkerID = resolveRef(r.Context(), s.store, ids.Machine, body.WorkerID)
	body.Path = strings.TrimSpace(body.Path)
	body.Name = strings.TrimSpace(body.Name)
	if body.WorkerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}
	if body.Path == "" {
		body.Path = "~"
	}
	if body.Name == "" {
		if body.Path == "~" {
			body.Name = "~"
		} else {
			trimmed := strings.TrimRight(body.Path, "/\\")
			parts := strings.FieldsFunc(trimmed, func(r rune) bool { return r == '/' || r == '\\' })
			if len(parts) > 0 {
				body.Name = parts[len(parts)-1]
			} else {
				body.Name = body.Path
			}
		}
	}
	if _, err := s.store.GetWorker(r.Context(), body.WorkerID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "worker not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	spaceSID, err := s.store.NewSID(r.Context(), ids.Space)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	sp := store.Space{
		ID:        ids.UUID(),
		SID:       spaceSID,
		WorkerID:  body.WorkerID,
		Name:      body.Name,
		Path:      body.Path,
		Branch:    strings.TrimSpace(body.Branch),
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateSpace(r.Context(), sp); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.broadcastFleetUpdate("space", sp)
	writeJSON(w, http.StatusCreated, sp)
}

func (s *Server) handleDeleteSpace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteSpace(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "space not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.broadcastFleetUpdate("space_deleted", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func (s *Server) handleDeleteWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var name, sid string
	wk, getErr := s.store.GetWorker(r.Context(), id)
	if getErr == nil {
		name, sid = wk.Name, wk.SID
	}
	// The close code tells a konsertd on the SSH transport to drop the hub key itself
	// and stop; the attach loop ends with the record.
	s.hub.DisconnectWorker(id)
	s.attach.stop(id)
	if getErr == nil {
		s.forgetSSHMachine(wk)
	}
	if err := s.store.DeleteWorker(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "worker not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.broadcastFleetUpdate("worker_deleted", id)
	msg := "Removed worker"
	if name != "" {
		msg += " " + name
	}
	s.hub.recordAudit(r.Context(), store.AuditEvent{
		Action:    AuditWorkerRemove,
		WorkerID:  id,
		WorkerSID: sid,
		Message:   msg,
		MetaJSON:  auditMeta(map[string]any{"name": name}),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func (s *Server) handleFSList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	workerID := strings.TrimSpace(q.Get("worker_id"))
	if workerID == "" {
		// Pick first online worker if not explicitly specified
		workers, _ := s.store.ListWorkers(r.Context())
		for _, wk := range workers {
			if wk.Status == "online" && s.hub.IsWorkerConnected(wk.ID) {
				workerID = wk.ID
				break
			}
		}
	}
	if workerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no online worker selected"})
		return
	}
	dirsOnly := q.Get("dirs_only") != "false"
	rpcCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	env, err := s.hub.RequestWorker(rpcCtx, workerID, protocol.MsgFSList, protocol.FSListRequest{
		Path:     q.Get("path"),
		Prefix:   q.Get("prefix"),
		DirsOnly: dirsOnly,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	res, err := protocol.DecodePayload[protocol.FSListResult](env)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSSHHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := sshutil.DiscoverHosts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	workers, _ := s.store.ListWorkers(r.Context())
	type hostView struct {
		sshutil.SSHHost
		Added bool `json:"added"`
	}
	out := make([]hostView, 0, len(hosts))
	for _, h := range hosts {
		added := false
		for _, wk := range workers {
			if workerMatchesSSHHost(wk, h) {
				added = true
				break
			}
		}
		out = append(out, hostView{SSHHost: h, Added: added})
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": out})
}

// workerMatchesSSHHost reports whether a machine is the ~/.ssh/config host h. A machine
// the hub installed over SSH matches exactly its target; one started by hand matches by
// name, host group or HostName (several aliases can share a HostName, e.g. containers or
// WSL on other ports, so the SSH target is preferred whenever it is known).
func workerMatchesSSHHost(wk store.Worker, h sshutil.SSHHost) bool {
	if wk.SSHTarget != "" {
		return strings.EqualFold(wk.SSHTarget, h.Alias)
	}
	return strings.EqualFold(wk.Name, h.Alias) ||
		strings.EqualFold(wk.HostGroup, h.Alias) ||
		(h.HostName != "" && strings.EqualFold(wk.Name, h.HostName))
}

func (s *Server) handleSSHBootstrap(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target     string `json:"target"`
		Sandbox    bool   `json:"sandbox"`
		EnableMock bool   `json:"enable_mock"`
		// AcceptHostKey is the SHA256 host key fingerprint the operator confirmed after
		// a previous attempt returned host_key_confirmation_required.
		AcceptHostKey string `json:"accept_host_key"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if _, err := sshutil.ParseTarget(body.Target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// The hub attaches over SSH with its own key, which bootstrap allows (restricted to
	// `konsertd attach`): no machine credential.
	grant, err := s.hubKeyGrant(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	opts := sshutil.BootstrapOptions{
		Target:        body.Target,
		HubKey:        grant,
		Sandbox:       body.Sandbox,
		EnableMock:    body.EnableMock,
		AcceptHostKey: body.AcceptHostKey,
	}

	streamSSE := r.URL.Query().Get("stream") == "1" || strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	if streamSSE {
		flusher, ok := w.(http.Flusher)
		if ok {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			opts.OnStage = func(stage, label, detail string) {
				b, _ := json.Marshal(map[string]any{"type": "stage", "stage": stage, "label": label, "detail": detail}, protocol.JSONOptions)
				_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
				flusher.Flush()
			}
			res := s.launch.bootstrap(r.Context(), opts)
			s.afterSSHBootstrap(r.Context(), res)
			b, _ := json.Marshal(map[string]any{"type": "done", "result": res}, protocol.JSONOptions)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			flusher.Flush()
			return
		}
	}

	res := s.launch.bootstrap(r.Context(), opts)
	s.afterSSHBootstrap(r.Context(), res)
	switch {
	case res.OK:
		writeJSON(w, http.StatusOK, res)
	case res.HostKeyConfirmationRequired:
		writeJSON(w, http.StatusConflict, res)
	default:
		writeJSON(w, http.StatusBadGateway, res)
	}
}

func (s *Server) handleGetUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.store.GetUsageSummary(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func applySavedOrchestratorSettings(orch *config.OrchestratorConfig, saved map[string]string, box *secrets.Box, logger *slog.Logger) {
	if v := strings.TrimSpace(saved["llm_provider"]); v != "" {
		orch.Provider = v
	}
	if v := strings.TrimSpace(saved["llm_model"]); v != "" {
		orch.Model = v
	}
	if v, ok := saved["llm_base_url"]; ok {
		orch.BaseURL = strings.TrimSpace(v)
	}
	if v := strings.TrimSpace(saved["llm_api_key"]); v != "" {
		key, err := box.Open(v)
		if err != nil {
			logger.Error("cannot decrypt the saved LLM API key; re-enter it in Settings or restore the secret key file", "err", err)
		} else {
			orch.APIKey = key
		}
	}
}

type settingsResponse struct {
	LLMProvider           string `json:"llm_provider"`
	LLMModel              string `json:"llm_model"`
	LLMBaseURL            string `json:"llm_base_url"`
	LLMAPIKeySet          bool   `json:"llm_api_key_set"`
	DefaultAgent          string `json:"default_agent"`
	DefaultModel          string `json:"default_model"`
	LastUsedMachine       string `json:"last_used_machine"`
	LastUsedAgent         string `json:"last_used_agent"`
	LastUsedModel         string `json:"last_used_model"`
	DefaultPermissionMode string `json:"default_permission_mode"`
	TerminalFontSize      int    `json:"terminal_font_size"`
	NotifySound           bool   `json:"notify_sound"`
	NotifyDesktop         bool   `json:"notify_desktop"`
	// VetoAckMinutes is how long a veto waits for the agent's acknowledgement before it
	// escalates in Needs you.
	VetoAckMinutes int `json:"veto_ack_minutes"`
	// DeviceIdleDays is how long a signed-in device may go unused before it must pair
	// again (sliding expiry).
	DeviceIdleDays int `json:"device_idle_days"`
	// OrchestratorBackend is "agent": the orchestrator is an agent chat on a
	// logged-in agent CLI. The API-key fields above configure only the optional
	// "api" backend, so APIKeyRequired is always false.
	OrchestratorBackend string `json:"orchestrator_backend"`
	APIKeyRequired      bool   `json:"api_key_required"`
	OrchestratorChatID  string `json:"orchestrator_chat_id,omitempty"`
}

func (s *Server) buildSettingsResponse(ctx context.Context) settingsResponse {
	saved, _ := s.store.GetAllSettings(ctx)
	s.cfgMu.RLock()
	orch := s.cfg.Orchestrator
	s.cfgMu.RUnlock()

	perm := saved["default_permission_mode"]
	if perm == "" {
		perm = protocol.PermissionFullAccess
	}
	fontSize, _ := strconv.Atoi(saved["terminal_font_size"])
	if fontSize < 10 || fontSize > 24 {
		fontSize = 13
	}
	notifySound := saved["notify_sound"] != "false"
	notifyDesktop := saved["notify_desktop"] == "true"

	return settingsResponse{
		LLMProvider:           orch.Provider,
		LLMModel:              effectiveModel(orch),
		LLMBaseURL:            orch.BaseURL,
		LLMAPIKeySet:          orch.APIKey != "",
		DefaultAgent:          saved["default_agent"],
		DefaultModel:          saved["default_model"],
		LastUsedMachine:       saved["last_used_machine"],
		LastUsedAgent:         saved["last_used_agent"],
		LastUsedModel:         saved["last_used_model"],
		DefaultPermissionMode: perm,
		TerminalFontSize:      fontSize,
		NotifySound:           notifySound,
		NotifyDesktop:         notifyDesktop,
		VetoAckMinutes:        int(s.hub.vetoAckTimeout(ctx).Minutes()),
		DeviceIdleDays:        deviceIdleDays(saved),
		OrchestratorBackend:   store.ChatBackendAgent,
		OrchestratorChatID:    saved[orchestratorChatSetting],
	}
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildSettingsResponse(r.Context()))
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var body SettingsUpdate
	if !decodeJSON(w, r, &body) {
		return
	}
	// REST callers hold a full-access credential (a browser/CLI device, the recovery
	// key, or a trusted Tailscale identity; MCP-only credentials never get here).
	if err := s.applySettings(r.Context(), body, true); err != nil {
		writeSettingsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.buildSettingsResponse(r.Context()))
}

func (s *Server) resolveGitTarget(ctx context.Context, workerID, workdir, taskID, spaceID string) (string, string, error) {
	workerID = resolveRef(ctx, s.store, ids.Machine, workerID)
	taskID = resolveRef(ctx, s.store, ids.Task, taskID)
	spaceID = resolveRef(ctx, s.store, ids.Space, spaceID)
	if taskID != "" {
		t, err := s.store.GetTask(ctx, taskID)
		if err != nil {
			return "", "", err
		}
		if workerID == "" {
			workerID = t.WorkerID
		}
		if workdir == "" {
			if t.EffectiveDir != "" {
				workdir = t.EffectiveDir
			} else {
				workdir = t.Workdir
			}
		}
	}
	if spaceID != "" {
		sp, err := s.store.GetSpace(ctx, spaceID)
		if err != nil {
			return "", "", err
		}
		if workerID == "" {
			workerID = sp.WorkerID
		}
		if workdir == "" {
			workdir = sp.Path
		}
	}
	if workerID == "" || workdir == "" {
		return "", "", errors.New("worker_id and workdir (or task_id/space_id) are required")
	}
	return workerID, workdir, nil
}

func (s *Server) handleGitInspect(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	workerID, workdir, err := s.resolveGitTarget(r.Context(), q.Get("worker_id"), q.Get("workdir"), q.Get("task_id"), q.Get("space_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	rpcCtx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	env, err := s.hub.RequestWorker(rpcCtx, workerID, protocol.MsgGitInspect, protocol.GitInspectRequest{Workdir: workdir})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	res, err := protocol.DecodePayload[protocol.GitInspectResult](env)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGitCommit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkerID string `json:"worker_id"`
		Workdir  string `json:"workdir"`
		TaskID   string `json:"task_id"`
		SpaceID  string `json:"space_id"`
		Message  string `json:"message"`
		StageAll *bool  `json:"stage_all"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.TaskID = resolveRef(r.Context(), s.store, ids.Task, body.TaskID)
	body.SpaceID = resolveRef(r.Context(), s.store, ids.Space, body.SpaceID)
	workerID, workdir, err := s.resolveGitTarget(r.Context(), body.WorkerID, body.Workdir, body.TaskID, body.SpaceID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	stageAll := true
	if body.StageAll != nil {
		stageAll = *body.StageAll
	}
	rpcCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	env, err := s.hub.RequestWorker(rpcCtx, workerID, protocol.MsgGitCommit, protocol.GitCommitRequest{
		Workdir:  workdir,
		Message:  body.Message,
		StageAll: stageAll,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	res, err := protocol.DecodePayload[protocol.GitCommitResult](env)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if res.OK && body.TaskID != "" {
		ev := store.TaskEvent{
			TaskID:    body.TaskID,
			Kind:      "control",
			Message:   "Git commit " + res.CommitSHA + ": " + strings.TrimSpace(body.Message),
			CreatedAt: time.Now().UTC(),
		}
		if id, err := s.store.AppendEvent(r.Context(), ev); err == nil {
			ev.ID = id
		}
		s.hub.broadcastFleetUpdate("event", ev)
	}
	if res.OK {
		s.hub.recordAudit(r.Context(), store.AuditEvent{
			Action:   AuditGitCommit,
			WorkerID: workerID,
			TaskID:   body.TaskID,
			Message:  "Git commit " + res.CommitSHA + ": " + firstLine(body.Message),
			MetaJSON: auditMeta(map[string]any{"sha": res.CommitSHA, "workdir": workdir, "space_id": body.SpaceID}),
		})
	}
	writeJSON(w, http.StatusOK, res)
}

// firstLine returns the trimmed first line of s, capped at 200 bytes.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > 200 {
		s = strings.ToValidUTF8(s[:200], "") + "…"
	}
	return s
}

func (s *Server) handleGitWorktreeCleanup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkerID string `json:"worker_id"`
		Workdir  string `json:"workdir"`
		TaskID   string `json:"task_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	workerID, workdir, err := s.resolveGitTarget(r.Context(), body.WorkerID, body.Workdir, body.TaskID, "")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	rpcCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	env, err := s.hub.RequestWorker(rpcCtx, workerID, protocol.MsgGitWorktreeCleanup, protocol.GitWorktreeCleanupRequest{Workdir: workdir})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	res, err := protocol.DecodePayload[protocol.GitWorktreeCleanupResult](env)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
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
	if !decodeJSON(w, r, &req) {
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
	if !decodeJSON(w, r, &body) {
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

func (s *Server) handleUploadAttachment(w http.ResponseWriter, r *http.Request) {
	var (
		workerID   string
		sessionID  string
		filename   string
		dataBase64 string
		rawBytes   []byte
	)

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(worker.MaxAttachmentBytes + 1024*1024); err != nil {
			if isBodyTooLarge(err) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart upload: " + err.Error()})
			return
		}
		workerID = strings.TrimSpace(r.FormValue("worker_id"))
		sessionID = strings.TrimSpace(r.FormValue("session_id"))
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file field"})
			return
		}
		defer file.Close()
		limited := io.LimitReader(file, worker.MaxAttachmentBytes+1)
		rawBytes, err = io.ReadAll(limited)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if len(rawBytes) > worker.MaxAttachmentBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file exceeds 12 MiB limit"})
			return
		}
		filename = hdr.Filename
		dataBase64 = base64.StdEncoding.EncodeToString(rawBytes)
	} else {
		var body struct {
			WorkerID   string `json:"worker_id"`
			SessionID  string `json:"session_id"`
			Filename   string `json:"filename"`
			DataBase64 string `json:"data_base64"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		workerID = strings.TrimSpace(body.WorkerID)
		sessionID = strings.TrimSpace(body.SessionID)
		filename = strings.TrimSpace(body.Filename)
		dataBase64 = strings.TrimSpace(body.DataBase64)
		var err error
		rawBytes, err = base64.StdEncoding.DecodeString(dataBase64)
		if err != nil || len(rawBytes) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid or empty base64 file data"})
			return
		}
		if len(rawBytes) > worker.MaxAttachmentBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file exceeds 12 MiB limit"})
			return
		}
	}

	// Resolve target worker (explicit -> localhost -> first online worker).
	if workerID == "" {
		workers, _ := s.store.ListWorkers(r.Context())
		for _, wk := range workers {
			if wk.Status == "online" && s.hub.IsWorkerConnected(wk.ID) &&
				(strings.EqualFold(wk.Name, "localhost") || strings.EqualFold(wk.HostGroup, "localhost") || wk.Tags["local"] == "true") {
				workerID = wk.ID
				break
			}
		}
		if workerID == "" {
			for _, wk := range workers {
				if wk.Status == "online" && s.hub.IsWorkerConnected(wk.ID) {
					workerID = wk.ID
					break
				}
			}
		}
	}

	if workerID != "" && s.hub.IsWorkerConnected(workerID) {
		rpcCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		env, err := s.hub.RequestWorker(rpcCtx, workerID, protocol.MsgFileUpload, protocol.FileUploadRequest{
			SessionID:  sessionID,
			Filename:   filename,
			DataBase64: dataBase64,
		})
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		res, err := protocol.DecodePayload[protocol.FileUploadResult](env)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if !res.OK {
			writeJSON(w, http.StatusBadRequest, res)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"ok":          true,
			"worker_id":   workerID,
			"remote_path": res.RemotePath,
			"filename":    res.Filename,
			"size":        res.Size,
		})
		return
	}

	// Local controller fallback when no worker daemon is currently connected.
	savedPath, err := worker.SaveScratchAttachment("", sessionID, filename, rawBytes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok":          true,
		"worker_id":   "controller-local",
		"remote_path": savedPath,
		"filename":    worker.SanitizeAttachmentFilename(filename),
		"size":        int64(len(rawBytes)),
	})
}

func (s *Server) handleWorkerWS(w http.ResponseWriter, r *http.Request) {
	bound, ok := s.authenticateMachine(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid machine credential"})
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logger.Warn("accept worker ws failed", "err", err)
		return
	}
	ctx := withWorkerRemoteAddr(r.Context(), r.RemoteAddr)
	if bound.workerID != "" {
		ctx = withBoundMachine(ctx, bound)
	}
	s.hub.ServeWorkerConn(ctx, conn)
}

func (s *Server) handleClientWS(w http.ResponseWriter, r *http.Request) {
	id, ok := s.auth.Authenticate(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	if id.Device != nil && id.Device.Kind == store.DeviceKindMCP {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "MCP credentials only work on /mcp"})
		return
	}
	// Browser sockets carry ambient cookie credentials, so the Origin must be this
	// controller (request Host) or an explicitly configured origin. Non-browser
	// clients that send no Origin are unaffected.
	if allowed, _ := checkOrigin(r, s.originPatterns); !allowed {
		s.logger.Warn("rejected cross-origin /ws/client upgrade", "origin", r.Header.Get("Origin"), "host", r.Host)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin websocket rejected"})
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.originPatterns,
	})
	if err != nil {
		s.logger.Warn("accept client ws failed", "err", err)
		return
	}
	s.hub.ServeBrowserConn(withIdentity(r.Context(), id), conn, id.User)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Invalid UTF-8 (file names, command output) is replaced with U+FFFD rather than
	// failing the response.
	_ = json.MarshalWrite(w, v, protocol.JSONOptions)
}
