package controller

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/orchestrator"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FleetBackend abstracts local FleetService or a remote HTTP client for `konsert mcp` & `konsertd mcp`.
type FleetBackend interface {
	FleetStatus(ctx context.Context) (FleetStatusResult, error)
	DispatchTask(ctx context.Context, req DispatchRequest) (DispatchResult, error)
	ListTasks(ctx context.Context, statusFilter, workerID string, limit int) ([]TaskListItem, error)
	GetTaskResult(ctx context.Context, taskID string, wait bool, timeoutSeconds int) (TaskResult, error)
	SteerTask(ctx context.Context, taskID, inputText string, appendNewline bool) (ActionResult, error)
	CancelTask(ctx context.Context, taskID string) (ActionResult, error)
}

// ControlPlaneBackend extends FleetBackend with full taxonomy, facts, settings, memory, fan-out, and event streaming.
type ControlPlaneBackend interface {
	FleetBackend
	GetTaxonomy(ctx context.Context) (store.TaxonomySnapshot, error)
	ManageTaxonomy(ctx context.Context, req ManageTaxonomyInput) (any, error)
	FanOutFleet(ctx context.Context, req FanOutRequest) (FanOutResult, error)
	ListTaskEvents(ctx context.Context, taskID string, afterID int64) ([]store.TaskEvent, error)
	GetSettingsMap(ctx context.Context) (map[string]any, error)
	UpdateSettingsMap(ctx context.Context, updates map[string]string) (map[string]any, error)
	SaveMemory(ctx context.Context, scope, key, summary string) error
	RecallMemory(ctx context.Context, scope string, limit int) ([]store.MemoryEntry, error)
}

type ManageTaxonomyInput struct {
	Action      string `json:"action" jsonschema:"create or delete"`
	Kind        string `json:"kind" jsonschema:"workspace, project, repo, or worktree"`
	ID          string `json:"id,omitempty" jsonschema:"ID to delete when action=delete"`
	Name        string `json:"name,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	RepoID      string `json:"repo_id,omitempty"`
	WorkerID    string `json:"worker_id,omitempty"`
	Path        string `json:"path,omitempty"`
	Branch      string `json:"branch,omitempty"`
}

// Implement ControlPlaneBackend on *FleetService.
func (s *FleetService) GetTaxonomy(ctx context.Context) (store.TaxonomySnapshot, error) {
	return s.store.GetTaxonomy(ctx)
}

func (s *FleetService) ManageTaxonomy(ctx context.Context, req ManageTaxonomyInput) (any, error) {
	action := strings.ToLower(strings.TrimSpace(req.Action))
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	switch action + ":" + kind {
	case "create:workspace":
		id, err := s.store.NewID(ctx, ids.Workspace)
		if err != nil {
			return nil, err
		}
		ws := store.Workspace{ID: id, Name: strings.TrimSpace(req.Name), CreatedAt: time.Now().UTC()}
		if ws.Name == "" {
			return nil, fmt.Errorf("name is required")
		}
		if err := s.store.CreateWorkspace(ctx, ws); err != nil {
			return nil, err
		}
		s.hub.broadcastFleetUpdate("workspace", ws)
		return ws, nil
	case "delete:workspace":
		if err := s.store.DeleteWorkspace(ctx, req.ID); err != nil {
			return nil, err
		}
		s.hub.broadcastFleetUpdate("workspace_deleted", req.ID)
		return map[string]any{"ok": true, "id": req.ID}, nil
	case "create:project":
		id, err := s.store.NewID(ctx, ids.Project)
		if err != nil {
			return nil, err
		}
		p := store.Project{ID: id, WorkspaceID: req.WorkspaceID, Name: strings.TrimSpace(req.Name), CreatedAt: time.Now().UTC()}
		if p.Name == "" {
			return nil, fmt.Errorf("name is required")
		}
		if p.WorkspaceID == "" {
			p.WorkspaceID = store.DefaultWorkspaceID
		}
		if err := s.store.CreateProject(ctx, p); err != nil {
			return nil, err
		}
		s.hub.broadcastFleetUpdate("project", p)
		return p, nil
	case "delete:project":
		if err := s.store.DeleteProject(ctx, req.ID); err != nil {
			return nil, err
		}
		s.hub.broadcastFleetUpdate("project_deleted", req.ID)
		return map[string]any{"ok": true, "id": req.ID}, nil
	case "create:repo":
		id, err := s.store.NewID(ctx, ids.Repo)
		if err != nil {
			return nil, err
		}
		r := store.Repo{ID: id, ProjectID: req.ProjectID, WorkerID: req.WorkerID, Name: strings.TrimSpace(req.Name), Path: strings.TrimSpace(req.Path), CreatedAt: time.Now().UTC()}
		if r.ProjectID == "" {
			r.ProjectID = store.DefaultProjectID
		}
		if r.WorkerID == "" || r.Path == "" {
			return nil, fmt.Errorf("worker_id and path are required")
		}
		if r.Name == "" {
			r.Name = derivePathLeaf(r.Path)
		}
		if err := s.store.CreateRepo(ctx, r); err != nil {
			return nil, err
		}
		s.hub.broadcastFleetUpdate("repo", r)
		return r, nil
	case "delete:repo":
		if err := s.store.DeleteRepo(ctx, req.ID); err != nil {
			return nil, err
		}
		s.hub.broadcastFleetUpdate("repo_deleted", req.ID)
		return map[string]any{"ok": true, "id": req.ID}, nil
	case "create:worktree":
		id, err := s.store.NewID(ctx, ids.Worktree)
		if err != nil {
			return nil, err
		}
		wt := store.Worktree{ID: id, RepoID: req.RepoID, Branch: req.Branch, Path: req.Path, CreatedAt: time.Now().UTC()}
		if wt.RepoID == "" {
			return nil, fmt.Errorf("repo_id is required")
		}
		if err := s.store.CreateWorktree(ctx, wt); err != nil {
			return nil, err
		}
		s.hub.broadcastFleetUpdate("worktree", wt)
		return wt, nil
	case "delete:worktree":
		if err := s.store.DeleteWorktree(ctx, req.ID); err != nil {
			return nil, err
		}
		s.hub.broadcastFleetUpdate("worktree_deleted", req.ID)
		return map[string]any{"ok": true, "id": req.ID}, nil
	default:
		return nil, fmt.Errorf("unsupported action/kind %q/%q", req.Action, req.Kind)
	}
}

func (s *FleetService) ListTaskEvents(ctx context.Context, taskID string, afterID int64) ([]store.TaskEvent, error) {
	return s.store.ListEvents(ctx, taskID, afterID, 50)
}

func (s *FleetService) GetSettingsMap(ctx context.Context) (map[string]any, error) {
	saved, err := s.store.GetAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	// Never expose raw llm_api_key over MCP!
	out := map[string]any{
		"llm_provider":            saved["llm_provider"],
		"llm_model":               saved["llm_model"],
		"llm_base_url":            saved["llm_base_url"],
		"llm_api_key_set":         saved["llm_api_key"] != "",
		"default_agent":           saved["default_agent"],
		"default_model":           saved["default_model"],
		"default_permission_mode": saved["default_permission_mode"],
	}
	return out, nil
}

// UpdateSettingsMap persists UI/dispatch preferences only. LLM settings (provider,
// model, base URL, key) are deliberately not writable here: they must go through
// Server.applySettings (REST or the controller's /mcp endpoint), which validates them,
// enforces the admin scope, updates the live config, and hot-reloads the orchestrator.
func (s *FleetService) UpdateSettingsMap(ctx context.Context, updates map[string]string) (map[string]any, error) {
	allowed := map[string]bool{
		"default_agent":           true,
		"default_model":           true,
		"default_permission_mode": true,
		"terminal_font_size":      true,
		"notify_sound":            true,
		"notify_desktop":          true,
	}
	filtered := map[string]string{}
	for k, v := range updates {
		if allowed[k] {
			filtered[k] = strings.TrimSpace(v)
		} else if strings.HasPrefix(k, "llm_") {
			return nil, fmt.Errorf("setting %q cannot be changed through this backend; use the controller settings API", k)
		}
	}
	if err := s.store.SetSettings(ctx, filtered); err != nil {
		return nil, err
	}
	recordSettingsAudit(ctx, s.hub, filtered)
	return s.GetSettingsMap(ctx)
}

func (s *FleetService) SaveMemory(ctx context.Context, scope, key, summary string) error {
	return s.store.UpsertMemory(ctx, store.MemoryEntry{Scope: scope, Key: key, Summary: summary})
}

func (s *FleetService) RecallMemory(ctx context.Context, scope string, limit int) ([]store.MemoryEntry, error) {
	return s.store.ListMemory(ctx, scope, limit)
}

type emptyInput struct{}

type listTasksInput struct {
	StatusFilter string `json:"status_filter,omitempty" jsonschema:"Filter by task status (queued, running, waiting_input, completed, failed, canceled)"`
	WorkerID     string `json:"worker_id,omitempty" jsonschema:"Filter by worker ID"`
	Limit        int    `json:"limit,omitzero" jsonschema:"Maximum tasks to return (default 50)"`
}

type getTaskResultInput struct {
	TaskID         string `json:"task_id" jsonschema:"Task ID to inspect"`
	Wait           bool   `json:"wait,omitzero" jsonschema:"Block until task reaches a terminal state while emitting concise milestone progress notifications (default false)"`
	TimeoutSeconds int    `json:"timeout_seconds,omitzero" jsonschema:"Maximum seconds to wait when wait=true (default 300)"`
}

type steerTaskInput struct {
	TaskID        string `json:"task_id" jsonschema:"Running or waiting_input task ID"`
	InputText     string `json:"input_text" jsonschema:"Text to send to the agent PTY stdin"`
	AppendNewline *bool  `json:"append_newline,omitzero" jsonschema:"Append trailing newline (default true)"`
}

type cancelTaskInput struct {
	TaskID string `json:"task_id" jsonschema:"Task ID to cancel"`
}

type inspectFactsInput struct {
	WorkerID string `json:"worker_id,omitempty" jsonschema:"Optional worker ID or name; omit for all machines"`
}

type updateSettingsInput struct {
	Updates map[string]string `json:"updates" jsonschema:"Map of Konsert settings keys to values (e.g. default_agent, default_model, default_permission_mode)"`
}

type askOrchestratorInput struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"Optional Orchestrator session ID (creates/reuses 'mcp-orchestrator' if omitted)"`
	Prompt    string `json:"prompt" jsonschema:"Question or fleet instruction for Konsert Orchestrator"`
}

type saveMemoryInput struct {
	Scope   string `json:"scope,omitempty" jsonschema:"Memory scope (default 'fleet')"`
	Key     string `json:"key" jsonschema:"Unique memory key"`
	Summary string `json:"summary" jsonschema:"Concise summary to persist"`
}

type recallMemoryInput struct {
	Scope string `json:"scope,omitempty" jsonschema:"Optional scope filter"`
	Limit int    `json:"limit,omitzero"`
}

func jsonToolResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v, protocol.JSONOptions)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(b)},
		},
	}, v, nil
}

// NewMCPServer constructs a full-control MCP server exposing Konsert's fleet, taxonomy,
// facts, configuration, memory, fan-out, and Orchestrator tools with concise progress notifications.
func NewMCPServer(backend FleetBackend, version string, orch ...*orchestrator.Service) *mcp.Server {
	if version == "" {
		version = "1.0.0"
	}
	var orchSvc *orchestrator.Service
	if len(orch) > 0 {
		orchSvc = orch[0]
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "konsert",
		Version: version,
	}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_status",
		Description: "Inspect registered Konsert workers, their online status, cached machine facts, isolation tier, permission mode, available coding agents, and active task counts.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		res, err := backend.FleetStatus(ctx)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "inspect_machine_facts",
		Description: "Inspect cached OS/patch, CPU/RAM/swap/disk, virtualization, tool versions (e.g. postgres 18), and safe auth_ready flags (e.g. gcloud) across the fleet.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in inspectFactsInput) (*mcp.CallToolResult, any, error) {
		fs, err := backend.FleetStatus(ctx)
		if err != nil {
			return nil, nil, err
		}
		type row struct {
			WorkerID   string                 `json:"worker_id"`
			WorkerName string                 `json:"worker_name"`
			Status     string                 `json:"status"`
			Facts      *protocol.MachineFacts `json:"facts,omitzero"`
		}
		out := []row{}
		for _, w := range fs.Workers {
			if in.WorkerID != "" && w.ID != in.WorkerID && !strings.EqualFold(w.Name, in.WorkerID) {
				continue
			}
			out = append(out, row{WorkerID: w.ID, WorkerName: w.Name, Status: w.Status, Facts: w.Facts})
		}
		return jsonToolResult(map[string]any{"machines": out})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dispatch_task",
		Description: "Dispatch a coding agent session/task to a matching worker node in the Konsert fleet (supports fact-based selectors like required_tools, required_auth, os, name).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in DispatchRequest) (*mcp.CallToolResult, any, error) {
		res, err := backend.DispatchTask(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_tasks",
		Description: "List active and recent fleet tasks/sessions with concise status, elapsed time, and last milestone.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listTasksInput) (*mcp.CallToolResult, any, error) {
		res, err := backend.ListTasks(ctx, in.StatusFilter, in.WorkerID, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(map[string]any{"tasks": res})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_task_result",
		Description: "Retrieve concise structured task results (exit code, final summary, git diff stats, milestones). Set wait=true to await completion while streaming concise milestone progress notifications (no raw PTY bytes).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in getTaskResultInput) (*mcp.CallToolResult, any, error) {
		// Stream concise structured milestone notifications while waiting when client supplied a progress token.
		var stopProgress context.CancelFunc
		if in.Wait && req != nil && req.Session != nil {
			if token := req.Params.GetProgressToken(); token != nil {
				if cp, ok := backend.(ControlPlaneBackend); ok {
					pCtx, pCancel := context.WithCancel(ctx)
					stopProgress = pCancel
					go func() {
						var lastEventID int64
						var step float64
						ticker := time.NewTicker(400 * time.Millisecond)
						defer ticker.Stop()
						for {
							select {
							case <-pCtx.Done():
								return
							case <-ticker.C:
								evs, err := cp.ListTaskEvents(pCtx, in.TaskID, lastEventID)
								if err != nil {
									continue
								}
								for _, ev := range evs {
									lastEventID = ev.ID
									step++
									_ = req.Session.NotifyProgress(pCtx, &mcp.ProgressNotificationParams{
										ProgressToken: token,
										Progress:      step,
										Message:       fmt.Sprintf("[%s] %s", ev.Kind, ev.Message),
									})
								}
							}
						}
					}()
				}
			}
		}
		if stopProgress != nil {
			defer stopProgress()
		}

		res, err := backend.GetTaskResult(ctx, in.TaskID, in.Wait, in.TimeoutSeconds)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "steer_task",
		Description: "Send steering input or follow-up prompt text to a running or waiting_input session.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in steerTaskInput) (*mcp.CallToolResult, any, error) {
		appendNL := true
		if in.AppendNewline != nil {
			appendNL = *in.AppendNewline
		}
		res, err := backend.SteerTask(ctx, in.TaskID, in.InputText, appendNL)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "cancel_task",
		Description: "Cancel a queued or running session and cleanly terminate its remote process group.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cancelTaskInput) (*mcp.CallToolResult, any, error) {
		res, err := backend.CancelTask(ctx, in.TaskID)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	// Read-only fleet audit log (KON-WP2A5).
	if ab, ok := backend.(AuditBackend); ok {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "list_audit",
			Description: "Read the fleet-wide operator audit log (Take Control, steer, cancel, git commit, worker removal, settings and wake-lock changes), newest first. Filter by worker_id, task_id, user, action or time; page with before_id = next_cursor.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListAuditInput) (*mcp.CallToolResult, any, error) {
			res, err := ab.ListAudit(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})
	}

	// Full control-plane tools (taxonomy, fan-out, settings, memory, Orchestrator)
	if cp, ok := backend.(ControlPlaneBackend); ok {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "get_taxonomy",
			Description: "Query Konsert's Space > Project > Repo > Worktree taxonomy hierarchy.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
			res, err := cp.GetTaxonomy(ctx)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "manage_taxonomy",
			Description: "Create or delete a Workspace (Space), Project, Repo, or Worktree in Konsert's taxonomy.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in ManageTaxonomyInput) (*mcp.CallToolResult, any, error) {
			res, err := cp.ManageTaxonomy(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "fan_out_fleet",
			Description: "Run a parallel operation across all matching online machines (short-circuiting no-op hosts deterministically via daemon at zero token cost).",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in FanOutRequest) (*mcp.CallToolResult, any, error) {
			res, err := cp.FanOutFleet(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "get_settings",
			Description: "Read Konsert controller settings and session defaults (API keys are never exposed).",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
			res, err := cp.GetSettingsMap(ctx)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "update_settings",
			Description: "Configure Konsert settings (default_agent, default_model, default_permission_mode, llm_provider, llm_model, etc.).",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in updateSettingsInput) (*mcp.CallToolResult, any, error) {
			res, err := cp.UpdateSettingsMap(ctx, in.Updates)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "save_memory",
			Description: "Persist a concise note in Konsert's SQLite Orchestrator memory store.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in saveMemoryInput) (*mcp.CallToolResult, any, error) {
			if err := cp.SaveMemory(ctx, in.Scope, in.Key, in.Summary); err != nil {
				return nil, nil, err
			}
			return jsonToolResult(map[string]any{"ok": true, "scope": in.Scope, "key": in.Key})
		})

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "recall_memory",
			Description: "Query persisted fleet/project/session memory entries from Konsert's SQLite store.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in recallMemoryInput) (*mcp.CallToolResult, any, error) {
			entries, err := cp.RecallMemory(ctx, in.Scope, in.Limit)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(map[string]any{"entries": entries})
		})
	}

	if orchSvc != nil {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "ask_orchestrator",
			Description: "Invoke or query Konsert's logical-fleet Orchestrator programmatically and return its concise response.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in askOrchestratorInput) (*mcp.CallToolResult, any, error) {
			sid := strings.TrimSpace(in.SessionID)
			if sid == "" {
				sid = "mcp-orchestrator"
			}
			var buf strings.Builder
			err := orchSvc.HandleUserMessage(ctx, sid, in.Prompt, orchestrator.StreamCallbacks{
				OnDelta: func(_, d string) { buf.WriteString(d) },
			})
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(map[string]any{"session_id": sid, "response": buf.String()})
		})
	}

	return srv
}

// NewMCPHTTPHandler wraps NewMCPServer in the official StreamableHTTPHandler.
func NewMCPHTTPHandler(backend FleetBackend, version string, orch ...*orchestrator.Service) http.Handler {
	srv := NewMCPServer(backend, version, orch...)
	return mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return srv
	}, nil)
}

// RunMCPStdio runs the MCP server over stdio until EOF or context cancellation.
func RunMCPStdio(ctx context.Context, backend FleetBackend, version string, orch ...*orchestrator.Service) error {
	srv := NewMCPServer(backend, version, orch...)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// RemoteFleetClient implements ControlPlaneBackend over the konsert REST API (for `konsert mcp`, `konsertd mcp`, & CLI commands).
type RemoteFleetClient struct {
	BaseURL string
	Token   string
	Client  *http.Client
	// Source optionally labels this client ("cli" or "mcp") in the hub's audit
	// log via the X-Konsert-Client header. Identity still comes from Token.
	Source string
}

func NewRemoteFleetClient(baseURL, token string) *RemoteFleetClient {
	return &RemoteFleetClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		Client:  &http.Client{},
	}
}

// APIError is a non-2xx response from the konsert REST API.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	// Message is the server's {"error": ...} text, or the raw body if it was not JSON.
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("konsert API %s %s failed (HTTP %d): %s", e.Method, e.Path, e.StatusCode, e.Message)
}

func (c *RemoteFleetClient) doJSON(ctx context.Context, method, path string, reqBody any, out any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody, protocol.JSONOptions)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if reqBody != nil || (method != http.MethodGet && method != http.MethodHead) {
		// The hub's CSRF guard requires a JSON body type or X-Konsert-Request on
		// state-changing /api requests; send both.
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Konsert-Request", "1")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.Source != "" {
		req.Header.Set(clientKindHeader, c.Source)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(raw))
		var eb struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &eb) == nil && eb.Error != "" {
			msg = eb.Error
		}
		return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Message: msg}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *RemoteFleetClient) FleetStatus(ctx context.Context) (FleetStatusResult, error) {
	var res FleetStatusResult
	err := c.doJSON(ctx, http.MethodGet, "/api/fleet", nil, &res)
	return res, err
}

func (c *RemoteFleetClient) DispatchTask(ctx context.Context, req DispatchRequest) (DispatchResult, error) {
	var res DispatchResult
	err := c.doJSON(ctx, http.MethodPost, "/api/tasks", req, &res)
	return res, err
}

func (c *RemoteFleetClient) ListTasks(ctx context.Context, statusFilter, workerID string, limit int) ([]TaskListItem, error) {
	q := fmt.Sprintf("/api/tasks?status=%s&worker_id=%s&limit=%d", statusFilter, workerID, limit)
	var resp struct {
		Tasks []TaskListItem `json:"tasks"`
	}
	err := c.doJSON(ctx, http.MethodGet, q, nil, &resp)
	return resp.Tasks, err
}

func (c *RemoteFleetClient) GetTaskResult(ctx context.Context, taskID string, wait bool, timeoutSeconds int) (TaskResult, error) {
	q := fmt.Sprintf("/api/tasks/%s?wait=%t&timeout=%d", url.PathEscape(taskID), wait, timeoutSeconds)
	var res TaskResult
	err := c.doJSON(ctx, http.MethodGet, q, nil, &res)
	return res, err
}

func (c *RemoteFleetClient) SteerTask(ctx context.Context, taskID, inputText string, appendNewline bool) (ActionResult, error) {
	var res ActionResult
	err := c.doJSON(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(taskID)+"/steer", map[string]any{
		"input_text":     inputText,
		"append_newline": appendNewline,
	}, &res)
	return res, err
}

func (c *RemoteFleetClient) CancelTask(ctx context.Context, taskID string) (ActionResult, error) {
	var res ActionResult
	err := c.doJSON(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(taskID)+"/cancel", nil, &res)
	return res, err
}

func (c *RemoteFleetClient) GetTaxonomy(ctx context.Context) (store.TaxonomySnapshot, error) {
	var res store.TaxonomySnapshot
	err := c.doJSON(ctx, http.MethodGet, "/api/taxonomy", nil, &res)
	return res, err
}

func (c *RemoteFleetClient) ManageTaxonomy(ctx context.Context, req ManageTaxonomyInput) (any, error) {
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	action := strings.ToLower(strings.TrimSpace(req.Action))
	var path string
	switch kind {
	case "workspace":
		path = "/api/workspaces"
	case "project":
		path = "/api/projects"
	case "repo":
		path = "/api/repos"
	case "worktree":
		path = "/api/worktrees"
	default:
		return nil, fmt.Errorf("unknown taxonomy kind %q", req.Kind)
	}
	var out any
	if action == "delete" {
		err := c.doJSON(ctx, http.MethodDelete, path+"/"+req.ID, nil, &out)
		return out, err
	}
	err := c.doJSON(ctx, http.MethodPost, path, req, &out)
	return out, err
}

func (c *RemoteFleetClient) FanOutFleet(ctx context.Context, req FanOutRequest) (FanOutResult, error) {
	var res FanOutResult
	err := c.doJSON(ctx, http.MethodPost, "/api/fleet/fanout", req, &res)
	return res, err
}

// ListAudit implements AuditBackend over GET /api/audit.
func (c *RemoteFleetClient) ListAudit(ctx context.Context, in ListAuditInput) (AuditListResponse, error) {
	var resp AuditListResponse
	err := c.doJSON(ctx, http.MethodGet, "/api/audit?"+url.Values(in.query()).Encode(), nil, &resp)
	return resp, err
}

func (c *RemoteFleetClient) ListTaskEvents(ctx context.Context, taskID string, afterID int64) ([]store.TaskEvent, error) {
	var resp struct {
		Events []store.TaskEvent `json:"events"`
	}
	err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/tasks/%s/events?after_id=%d", url.PathEscape(taskID), afterID), nil, &resp)
	return resp.Events, err
}

func (c *RemoteFleetClient) GetSettingsMap(ctx context.Context) (map[string]any, error) {
	var res map[string]any
	err := c.doJSON(ctx, http.MethodGet, "/api/settings", nil, &res)
	return res, err
}

func (c *RemoteFleetClient) UpdateSettingsMap(ctx context.Context, updates map[string]string) (map[string]any, error) {
	var res map[string]any
	err := c.doJSON(ctx, http.MethodPut, "/api/settings", updates, &res)
	return res, err
}

func (c *RemoteFleetClient) SaveMemory(ctx context.Context, scope, key, summary string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/orchestrator/memory", map[string]string{
		"scope":   scope,
		"key":     key,
		"summary": summary,
	}, nil)
}

func (c *RemoteFleetClient) RecallMemory(ctx context.Context, scope string, limit int) ([]store.MemoryEntry, error) {
	var resp struct {
		Entries []store.MemoryEntry `json:"entries"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/orchestrator/memory?scope="+scope, nil, &resp)
	return resp.Entries, err
}
