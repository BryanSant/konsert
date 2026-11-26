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

	"github.com/BryanSant/syre/internal/commander"
	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FleetBackend abstracts local FleetService or a remote HTTP client for `syre mcp` & `syred mcp`.
type FleetBackend interface {
	FleetStatus(ctx context.Context) (FleetStatusResult, error)
	DispatchTask(ctx context.Context, req DispatchRequest) (DispatchResult, error)
	ListTasks(ctx context.Context, statusFilter, workerID string, limit int) ([]TaskListItem, error)
	GetTaskResult(ctx context.Context, taskID string, wait bool, timeoutSeconds int) (TaskResult, error)
	SteerTask(ctx context.Context, taskID, inputText string, appendNewline bool) (ActionResult, error)
	CancelTask(ctx context.Context, taskID string) (ActionResult, error)
}

// ControlPlaneBackend extends FleetBackend with facts, settings, memory, fan-out, and event streaming.
type ControlPlaneBackend interface {
	FleetBackend
	FanOutFleet(ctx context.Context, req FanOutRequest) (FanOutResult, error)
	ListTaskEvents(ctx context.Context, taskID string, afterID int64) ([]store.TaskEvent, error)
	GetSettingsMap(ctx context.Context) (map[string]any, error)
	UpdateSettingsMap(ctx context.Context, updates map[string]string) (map[string]any, error)
	SaveMemory(ctx context.Context, scope, key, summary string) error
	RecallMemory(ctx context.Context, scope string, limit int) ([]store.MemoryEntry, error)
}

// PulseBackend exposes the latest board pulse as an MCP resource ("pulse") and registers
// external MCP servers for push notifications (notifications/message and resources/updated).
type PulseBackend interface {
	LatestPulse(ctx context.Context) (string, error)
	RegisterPulseMCPServer(srv *mcp.Server)
}

func (s *FleetService) ListTaskEvents(ctx context.Context, taskID string, afterID int64) ([]store.TaskEvent, error) {
	return s.store.ListEvents(ctx, resolveRef(ctx, s.store, ids.Task, taskID), afterID, 50)
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
		"commander_policy_writes": firstNonEmpty(saved[commanderPolicyWritesSetting], policyWritesTighten),
	}
	return out, nil
}

// UpdateSettingsMap persists UI/dispatch preferences only. LLM settings (provider,
// model, base URL, key) are deliberately not writable here: they must go through
// Server.applySettings (REST or the controller's /mcp endpoint), which validates them,
// enforces the admin scope, updates the live config, and hot-reloads the commander.
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

type askCommanderInput struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"Optional Commander session ID"`
	Prompt    string `json:"prompt" jsonschema:"Question or instruction for Syre Commander"`
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

// NewMCPServer constructs the external /mcp (and stdio) MCP server exposing Syre's
// consolidated fleet tool surface. Undoing a user's action (resume/undrain) is refused
// on /mcp and must be done in the UI or confirmed in a Commander chat.
func NewMCPServer(backend FleetBackend, version string, orch ...*commander.Service) *mcp.Server {
	var orchSvc *commander.Service
	if len(orch) > 0 {
		orchSvc = orch[0]
	}
	srv := newMCPServer(backend, version, orchSvc, nil)
	if pb, ok := backend.(PulseBackend); ok {
		pb.RegisterPulseMCPServer(srv)
	}
	return srv
}

// newMCPServer builds the consolidated fleet tool server. confirm is nil for external
// /mcp (refuses resume/undrain) and set to chatConfirm for Commander chats.
func newMCPServer(backend FleetBackend, version string, orchSvc *commander.Service, confirm confirmFunc) *mcp.Server {
	if version == "" {
		version = "1.0.0"
	}

	var opts *mcp.ServerOptions
	if _, ok := backend.(PulseBackend); ok {
		opts = &mcp.ServerOptions{
			SubscribeHandler:   func(context.Context, *mcp.SubscribeRequest) error { return nil },
			UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
		}
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "syre",
		Version: version,
	}, opts)

	if pb, ok := backend.(PulseBackend); ok {
		srv.AddResource(&mcp.Resource{
			URI:         PulseResourceURI,
			Name:        "pulse",
			Title:       "Board pulse",
			Description: "Latest compact Syre board pulse (fleet status, board counts, and actionable triggers).",
			MIMEType:    "text/plain",
		}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			text, err := pb.LatestPulse(ctx)
			if err != nil {
				return nil, err
			}
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{{
					URI:      req.Params.URI,
					MIMEType: "text/plain",
					Text:     text,
				}},
			}, nil
		})
	}

	if sb, ok := backend.(SurfaceBackend); ok {
		registerSurfaceTools(srv, sb, confirm)
	}

	if pb, ok := backend.(PolicyBackend); ok {
		registerSetPolicyTool(srv, pb)
	}

	if ib, ok := backend.(InspectBackend); ok {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "inspect",
			Description: inspectToolDescription,
			InputSchema: inspectSchema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in InspectRequest) (*mcp.CallToolResult, any, error) {
			res, err := ib.Inspect(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})
	}

	if orchSvc != nil {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "ask_commander",
			Description: "Invoke or query Syre Commander programmatically and return its concise response.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in askCommanderInput) (*mcp.CallToolResult, any, error) {
			sid := strings.TrimSpace(in.SessionID)
			if sid == "" {
				sid = "mcp-commander"
			}
			var buf strings.Builder
			err := orchSvc.HandleUserMessage(ctx, sid, in.Prompt, commander.StreamCallbacks{
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
func NewMCPHTTPHandler(backend FleetBackend, version string, orch ...*commander.Service) http.Handler {
	srv := NewMCPServer(backend, version, orch...)
	return mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return srv
	}, nil)
}

// RunMCPStdio runs the MCP server over stdio until EOF or context cancellation.
func RunMCPStdio(ctx context.Context, backend FleetBackend, version string, orch ...*commander.Service) error {
	srv := NewMCPServer(backend, version, orch...)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// RemoteFleetClient implements ControlPlaneBackend over the syre REST API (for `syre mcp`, `syred mcp`, & CLI commands).
type RemoteFleetClient struct {
	BaseURL string
	Token   string
	Client  *http.Client
	// Source optionally labels this client ("cli" or "mcp") in the hub's audit
	// log via the X-Syre-Client header. Identity still comes from Token.
	Source string
	// UserAgent, when set, identifies this client (shown in Settings → Devices).
	UserAgent string
}

func NewRemoteFleetClient(baseURL, token string) *RemoteFleetClient {
	return &RemoteFleetClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		Client:  &http.Client{},
	}
}

// APIError is a non-2xx response from the syre REST API.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	// Message is the server's {"error": ...} text, or the raw body if it was not JSON.
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("syre API %s %s failed (HTTP %d): %s", e.Method, e.Path, e.StatusCode, e.Message)
}

// DoJSON sends a JSON request to the hub's REST API and decodes the JSON reply into
// out (when non-nil). A non-2xx reply is an *APIError.
func (c *RemoteFleetClient) DoJSON(ctx context.Context, method, path string, reqBody any, out any) error {
	return c.doJSON(ctx, method, path, reqBody, out)
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
		// The hub's CSRF guard requires a JSON body type or X-Syre-Request on
		// state-changing /api requests; send both.
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Syre-Request", "1")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.Source != "" {
		req.Header.Set(clientKindHeader, c.Source)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
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
	return c.doJSON(ctx, http.MethodPost, "/api/commander/memory", map[string]string{
		"scope":   scope,
		"key":     key,
		"summary": summary,
	}, nil)
}

func (c *RemoteFleetClient) SetPolicy(ctx context.Context, in SetPolicyInput) (SetPolicyResult, error) {
	var res SetPolicyResult
	err := c.doJSON(ctx, http.MethodPost, "/api/policy", in, &res)
	return res, err
}

func (c *RemoteFleetClient) RecallMemory(ctx context.Context, scope string, limit int) ([]store.MemoryEntry, error) {
	var resp struct {
		Entries []store.MemoryEntry `json:"entries"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/commander/memory?scope="+scope, nil, &resp)
	return resp.Entries, err
}
