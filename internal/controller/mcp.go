package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FleetBackend abstracts local FleetService or a remote HTTP client for `konsert mcp`.
type FleetBackend interface {
	FleetStatus(ctx context.Context) (FleetStatusResult, error)
	DispatchTask(ctx context.Context, req DispatchRequest) (DispatchResult, error)
	ListTasks(ctx context.Context, statusFilter, workerID string, limit int) ([]TaskListItem, error)
	GetTaskResult(ctx context.Context, taskID string, wait bool, timeoutSeconds int) (TaskResult, error)
	SteerTask(ctx context.Context, taskID, inputText string, appendNewline bool) (ActionResult, error)
	CancelTask(ctx context.Context, taskID string) (ActionResult, error)
}

type emptyInput struct{}

type listTasksInput struct {
	StatusFilter string `json:"status_filter,omitempty" jsonschema:"Filter by task status (queued, running, waiting_input, completed, failed, canceled)"`
	WorkerID     string `json:"worker_id,omitempty" jsonschema:"Filter by worker ID"`
	Limit        int    `json:"limit,omitempty" jsonschema:"Maximum tasks to return (default 50)"`
}

type getTaskResultInput struct {
	TaskID         string `json:"task_id" jsonschema:"Task ID to inspect"`
	Wait           bool   `json:"wait,omitempty" jsonschema:"Block until task reaches a terminal state (default false)"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Maximum seconds to wait when wait=true (default 300)"`
}

type steerTaskInput struct {
	TaskID        string `json:"task_id" jsonschema:"Running or waiting_input task ID"`
	InputText     string `json:"input_text" jsonschema:"Text to send to the agent PTY stdin"`
	AppendNewline *bool  `json:"append_newline,omitempty" jsonschema:"Append trailing newline (default true)"`
}

type cancelTaskInput struct {
	TaskID string `json:"task_id" jsonschema:"Task ID to cancel"`
}

func jsonToolResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(b)},
		},
	}, v, nil
}

// NewMCPServer constructs a configured MCP server exposing Konsert's 6 fleet tools.
func NewMCPServer(backend FleetBackend, version string) *mcp.Server {
	if version == "" {
		version = "1.0.0"
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "konsert",
		Version: version,
	}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_status",
		Description: "Inspect registered Konsert workers, their online status, isolation tier, permission mode, available coding agents, and active task counts.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		res, err := backend.FleetStatus(ctx)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dispatch_task",
		Description: "Dispatch a coding agent task to a matching worker node in the Konsert fleet.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in DispatchRequest) (*mcp.CallToolResult, any, error) {
		res, err := backend.DispatchTask(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_tasks",
		Description: "List active and recent fleet tasks with concise status, elapsed time, and last milestone.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listTasksInput) (*mcp.CallToolResult, any, error) {
		res, err := backend.ListTasks(ctx, in.StatusFilter, in.WorkerID, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(map[string]any{"tasks": res})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_task_result",
		Description: "Retrieve concise structured task results (exit code, final summary, git diff stats, milestones). Set wait=true to await completion without polling.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getTaskResultInput) (*mcp.CallToolResult, any, error) {
		res, err := backend.GetTaskResult(ctx, in.TaskID, in.Wait, in.TimeoutSeconds)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "steer_task",
		Description: "Send steering input or follow-up prompt text to a running or waiting_input task.",
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
		Description: "Cancel a queued or running task and cleanly terminate its remote process group.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cancelTaskInput) (*mcp.CallToolResult, any, error) {
		res, err := backend.CancelTask(ctx, in.TaskID)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})

	return srv
}

// NewMCPHTTPHandler wraps NewMCPServer in the official StreamableHTTPHandler.
func NewMCPHTTPHandler(backend FleetBackend, version string) http.Handler {
	srv := NewMCPServer(backend, version)
	return mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return srv
	}, nil)
}

// RunMCPStdio runs the MCP server over stdio until EOF or context cancellation.
func RunMCPStdio(ctx context.Context, backend FleetBackend, version string) error {
	srv := NewMCPServer(backend, version)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// RemoteFleetClient implements FleetBackend over the konsert REST API (for `konsert mcp` & CLI commands).
type RemoteFleetClient struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func NewRemoteFleetClient(baseURL, token string) *RemoteFleetClient {
	return &RemoteFleetClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		Client:  &http.Client{},
	}
}

func (c *RemoteFleetClient) doJSON(ctx context.Context, method, path string, reqBody any, out any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bodyReader)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
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
		return fmt.Errorf("konsert API %s %s failed (HTTP %d): %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
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
	q := fmt.Sprintf("/api/tasks/%s?wait=%t&timeout=%d", taskID, wait, timeoutSeconds)
	var res TaskResult
	err := c.doJSON(ctx, http.MethodGet, q, nil, &res)
	return res, err
}

func (c *RemoteFleetClient) SteerTask(ctx context.Context, taskID, inputText string, appendNewline bool) (ActionResult, error) {
	var res ActionResult
	err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/tasks/%s/steer", taskID), map[string]any{
		"input_text":     inputText,
		"append_newline": appendNewline,
	}, &res)
	return res, err
}

func (c *RemoteFleetClient) CancelTask(ctx context.Context, taskID string) (ActionResult, error) {
	var res ActionResult
	err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/tasks/%s/cancel", taskID), nil, &res)
	return res, err
}
