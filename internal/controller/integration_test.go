package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/orchestrator"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store/sqlite"
	"github.com/BryanSant/konsert/internal/worker"
	"github.com/BryanSant/konsert/internal/worker/drivers"
	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func initTempGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	run("init")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")
	return repo
}

func waitForWorkerOnline(t *testing.T, svc *FleetService, workerID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fs, err := svc.FleetStatus(context.Background())
		if err == nil {
			for _, w := range fs.Workers {
				if w.ID == workerID && w.Status == "online" {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("worker %s did not come online in time", workerID)
}

func TestEndToEndMCPDispatchGitDiffAndAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "konsert.db")
	st, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.FleetToken = "fleet-tok-123"
	cfg.AdminToken = "admin-tok-456"
	cfg.MCPToken = "mcp-tok-789"
	cfg.EnableMCPHTTP = true

	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1. Auth checks: unauthorized REST and MCP rejected.
	resp, err := http.Get(ts.URL + "/api/fleet")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on unauth /api/fleet, got %v err=%v", resp, err)
	}
	_ = resp.Body.Close()

	// Browser login flow sets cookie.
	loginReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/login", strings.NewReader(`{"token":"admin-tok-456"}`))
	loginResp, err := http.DefaultClient.Do(loginReq)
	if err != nil || loginResp.StatusCode != http.StatusOK || len(loginResp.Cookies()) == 0 {
		t.Fatalf("expected 200 + session cookie on login, got status=%d err=%v", loginResp.StatusCode, err)
	}
	_ = loginResp.Body.Close()

	// 2. Start konsertd worker daemon with mock driver and fleet token.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/worker"
	wCfg := config.DefaultWorkerConfig()
	wCfg.ControllerURL = wsURL
	wCfg.FleetToken = "fleet-tok-123"
	wCfg.WorkerID = "worker-e2e-1"
	wCfg.Name = "e2e-node"
	wCfg.HostGroup = "homelab-1"
	wCfg.EnableMockDriver = true

	daemon := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
	daemon.HeartbeatInterval = 500 * time.Millisecond
	daemonDone := make(chan struct{})
	go func() {
		defer close(daemonDone)
		_ = daemon.Run(ctx)
	}()

	waitForWorkerOnline(t, srv.Service(), "worker-e2e-1")

	// 3. Connect an in-memory MCP client to Konsert's MCP server and dispatch a task on a git repo.
	mcpSrv := NewMCPServer(srv.Service(), "test")
	cTrans, sTrans := mcp.NewInMemoryTransports()
	mcpSrvSession, err := mcpSrv.Connect(ctx, sTrans, nil)
	if err != nil {
		t.Fatalf("mcp server connect failed: %v", err)
	}
	defer mcpSrvSession.Close()

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-orch", Version: "1.0"}, nil)
	mcpSession, err := mcpClient.Connect(ctx, cTrans, nil)
	if err != nil {
		t.Fatalf("mcp client connect failed: %v", err)
	}
	defer mcpSession.Close()

	// Call fleet_status via MCP
	fsRes, err := mcpSession.CallTool(ctx, &mcp.CallToolParams{Name: "fleet_status"})
	if err != nil || fsRes.IsError {
		t.Fatalf("MCP fleet_status failed: %v res=%+v", err, fsRes)
	}

	repo := initTempGitRepo(t)
	dispRes, err := mcpSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "dispatch_task",
		Arguments: map[string]any{
			"agent":        "mock",
			"repo_path":    repo,
			"branch":       "feat/mcp-e2e",
			"use_worktree": true,
			"prompt":       "TOUCH_FILE=hello.txt STREAM_LINES=3",
		},
	})
	if err != nil || dispRes.IsError {
		t.Fatalf("MCP dispatch_task failed: %v res=%+v", err, dispRes)
	}
	var dispOut DispatchResult
	if err := json.Unmarshal([]byte(dispRes.Content[0].(*mcp.TextContent).Text), &dispOut); err != nil {
		t.Fatalf("decode dispatch result: %v", err)
	}
	if dispOut.TaskID == "" || dispOut.WorkerID != "worker-e2e-1" {
		t.Fatalf("unexpected dispatch result: %+v", dispOut)
	}

	// Await task completion via MCP get_task_result(wait=true)
	resCall, err := mcpSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_task_result",
		Arguments: map[string]any{
			"task_id":         dispOut.TaskID,
			"wait":            true,
			"timeout_seconds": 15,
		},
	})
	if err != nil || resCall.IsError {
		t.Fatalf("MCP get_task_result failed: %v res=%+v", err, resCall)
	}
	var taskRes TaskResult
	if err := json.Unmarshal([]byte(resCall.Content[0].(*mcp.TextContent).Text), &taskRes); err != nil {
		t.Fatalf("decode task result: %v", err)
	}
	if taskRes.Status != protocol.TaskStatusCompleted || taskRes.ExitCode == nil || *taskRes.ExitCode != 0 {
		t.Fatalf("unexpected task result: %+v", taskRes)
	}
	if !strings.Contains(taskRes.Summary, "Mock agent finished cleanly") {
		t.Fatalf("unexpected summary: %q", taskRes.Summary)
	}
	if taskRes.GitSummary == nil || len(taskRes.GitSummary.DirtyFiles) == 0 {
		t.Fatalf("expected git_summary with dirty hello.txt, got %+v", taskRes.GitSummary)
	}

	cancel()
	<-daemonDone
}

func TestBrowserPtySubscribeInteractiveAndUnsubscribe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "konsert.db")
	st, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	wCfg := config.DefaultWorkerConfig()
	wCfg.ControllerURL = wsBase + "/ws/worker"
	wCfg.WorkerID = "worker-pty-1"
	wCfg.EnableMockDriver = true

	daemon := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
	daemonDone := make(chan struct{})
	go func() {
		defer close(daemonDone)
		_ = daemon.Run(ctx)
	}()
	waitForWorkerOnline(t, srv.Service(), "worker-pty-1")

	// Dispatch interactive task
	workdir := t.TempDir()
	disp, err := srv.Service().DispatchTask(ctx, DispatchRequest{
		Agent:    "mock",
		RepoPath: workdir,
		Prompt:   "WAIT_INPUT",
		Mode:     protocol.TaskModeInteractive,
	})
	if err != nil {
		t.Fatalf("DispatchTask failed: %v", err)
	}

	// Wait briefly so output fills ring buffer before browser subscribes.
	time.Sleep(300 * time.Millisecond)

	// Connect browser WebSocket
	bConn, _, err := websocket.Dial(ctx, wsBase+"/ws/client", nil)
	if err != nil {
		t.Fatalf("dial /ws/client failed: %v", err)
	}
	defer bConn.Close(websocket.StatusNormalClosure, "done")

	// Subscribe to task PTY
	subBytes, _ := protocol.NewEnvelope(protocol.MsgPtySubscribe, "", protocol.PtySubscribePayload{TaskID: disp.TaskID})
	if err := bConn.Write(ctx, websocket.MessageText, subBytes); err != nil {
		t.Fatalf("write pty.subscribe failed: %v", err)
	}

	// Expect binary 0x01 snapshot frame
	gotSnapshot := false
	readDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(readDeadline) && !gotSnapshot {
		rCtx, rCancel := context.WithTimeout(ctx, 2*time.Second)
		mt, data, err := bConn.Read(rCtx)
		rCancel()
		if err != nil {
			t.Fatalf("browser read failed: %v", err)
		}
		if mt == websocket.MessageBinary {
			ft, tid, payload, err := protocol.DecodeFrame(data)
			if err == nil && ft == protocol.FramePtySnapshot && tid == disp.TaskID && bytes.Contains(payload, []byte("konsert-mock")) {
				gotSnapshot = true
			}
		}
	}
	if !gotSnapshot {
		t.Fatal("did not receive binary 0x01 PTY snapshot")
	}

	// Send interactive 0x03 stdin frame ("exit\n")
	stdinFrame, err := protocol.EncodeFrame(protocol.FramePtyStdin, disp.TaskID, []byte("exit\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := bConn.Write(ctx, websocket.MessageBinary, stdinFrame); err != nil {
		t.Fatalf("write stdin frame failed: %v", err)
	}

	// Verify task completes cleanly
	res, err := srv.Service().GetTaskResult(ctx, disp.TaskID, true, 10)
	if err != nil || res.Status != protocol.TaskStatusCompleted {
		t.Fatalf("expected completed interactive task, got %+v err=%v", res, err)
	}

	// Unsubscribe cleanly
	unsubBytes, _ := protocol.NewEnvelope(protocol.MsgPtyUnsubscribe, "", protocol.PtyUnsubscribePayload{TaskID: disp.TaskID})
	_ = bConn.Write(ctx, websocket.MessageText, unsubBytes)

	cancel()
	<-daemonDone
}

func TestControllerRestartTaskReadoption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "konsert-restart.db")
	st1, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true

	// Start Controller #1 on a fixed local port so we can restart Controller #2 on the exact same address.
	srv1 := NewServer(cfg, st1, nil, "test-1")
	ts1 := httptest.NewServer(srv1)
	addr := ts1.Listener.Addr().String()

	wCfg := config.DefaultWorkerConfig()
	wCfg.ControllerURL = "ws://" + addr + "/ws/worker"
	wCfg.WorkerID = "worker-restart-1"
	wCfg.EnableMockDriver = true

	daemon := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
	daemonDone := make(chan struct{})
	go func() {
		defer close(daemonDone)
		_ = daemon.Run(ctx)
	}()
	waitForWorkerOnline(t, srv1.Service(), "worker-restart-1")

	// Launch a long-ish interactive task so it stays alive while Controller #1 shuts down.
	disp, err := srv1.Service().DispatchTask(ctx, DispatchRequest{
		Agent:    "mock",
		RepoPath: t.TempDir(),
		Prompt:   "WAIT_INPUT",
		Mode:     protocol.TaskModeInteractive,
	})
	if err != nil {
		t.Fatalf("DispatchTask failed: %v", err)
	}
	time.Sleep(250 * time.Millisecond)

	// Shut down Controller #1 (closing worker WS cleanly) and close DB #1.
	srv1.Hub().Close()
	ts1.Close()
	_ = st1.Close()

	// Start Controller #2 on the exact same address & SQLite DB.
	st2, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	srv2 := NewServer(cfg, st2, nil, "test-2")
	httpSrv2 := &http.Server{Handler: srv2}
	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("rebind %s failed: %v", addr, err)
	}
	defer ln2.Close()
	go func() { _ = httpSrv2.Serve(ln2) }()
	defer httpSrv2.Close()

	// Wait for worker-restart-1 to reconnect and re-adopt the task!
	waitForWorkerOnline(t, srv2.Service(), "worker-restart-1")

	// Steer the re-adopted task via Controller #2 to prove end-to-end re-adoption works.
	if _, err := srv2.Service().SteerTask(ctx, disp.TaskID, "exit", true); err != nil {
		t.Fatalf("SteerTask after restart failed: %v", err)
	}
	res, err := srv2.Service().GetTaskResult(ctx, disp.TaskID, true, 10)
	if err != nil || res.Status != protocol.TaskStatusCompleted {
		t.Fatalf("expected re-adopted task to complete, got %+v err=%v", res, err)
	}

	cancel()
	<-daemonDone
}

type mockLLMProvider struct {
	step int
}

func (m *mockLLMProvider) Name() string { return "mock-llm" }

func (m *mockLLMProvider) Complete(_ context.Context, _, _ string, _ []orchestrator.Message, _ []orchestrator.ToolDef, onDelta func(string)) (orchestrator.Message, error) {
	m.step++
	if m.step == 1 {
		return orchestrator.Message{
			Role: "assistant",
			ToolCalls: []orchestrator.ToolCall{
				{ID: "call_1", Name: "fleet_status", Arguments: json.RawMessage(`{}`)},
			},
		}, nil
	}
	text := "Checked the fleet: ready for tasks."
	if onDelta != nil {
		onDelta(text)
	}
	return orchestrator.Message{Role: "assistant", Content: text}, nil
}

func TestOrchestratorChatToolLoop(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	cfg.Orchestrator.APIKey = "test-key"
	srv := NewServer(cfg, st, nil, "test")
	srv.Orchestrator().SetProvider(&mockLLMProvider{})

	var deltas []string
	var toolStatuses []string
	var doneCalled bool

	err = srv.Orchestrator().HandleUserMessage(ctx, "sess-1", "Check the fleet", orchestrator.StreamCallbacks{
		OnDelta: func(_, d string) { deltas = append(deltas, d) },
		OnTool:  func(p protocol.ChatToolPayload) { toolStatuses = append(toolStatuses, p.Name+":"+p.Status) },
		OnDone:  func(_ string, _ int64) { doneCalled = true },
	})
	if err != nil {
		t.Fatalf("HandleUserMessage failed: %v", err)
	}
	if !doneCalled || len(deltas) == 0 || len(toolStatuses) != 2 {
		t.Fatalf("unexpected callbacks: done=%v deltas=%v tools=%v", doneCalled, deltas, toolStatuses)
	}
	msgs, err := st.ListChatMessages(ctx, "sess-1")
	if err != nil || len(msgs) != 4 { // user, assistant(tool_call), tool(result), assistant(final)
		t.Fatalf("expected 4 persisted chat messages, got %d err=%v", len(msgs), err)
	}
}

func TestSpacesRESTAndTaskAssociation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "spaces.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/worker"
	wCfg := config.DefaultWorkerConfig()
	wCfg.ControllerURL = wsURL
	wCfg.WorkerID = "w-space-1"
	wCfg.Name = "space-node"
	wCfg.EnableMockDriver = true

	daemon := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = daemon.Run(ctx)
	}()
	waitForWorkerOnline(t, srv.Service(), "w-space-1")

	workdir := t.TempDir()
	// POST /api/spaces
	createBody, _ := json.Marshal(map[string]string{
		"worker_id": "w-space-1",
		"name":      "demo-space",
		"path":      workdir,
	})
	resp, err := http.Post(ts.URL+"/api/spaces", "application/json", bytes.NewReader(createBody))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/spaces failed: resp=%v err=%v", resp, err)
	}
	var created struct {
		ID       string `json:"id"`
		WorkerID string `json:"worker_id"`
		Name     string `json:"name"`
		Path     string `json:"path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || created.ID == "" {
		t.Fatalf("decode space failed: %+v err=%v", created, err)
	}
	_ = resp.Body.Close()

	// Dispatch task using only space_id (resolves worker_id and repo_path automatically!)
	disp, err := srv.Service().DispatchTask(ctx, DispatchRequest{
		SpaceID: created.ID,
		Agent:   "mock",
		Prompt:  "STREAM_LINES=1",
		Mode:    protocol.TaskModeOneshot,
	})
	if err != nil {
		t.Fatalf("DispatchTask via SpaceID failed: %v", err)
	}
	if disp.SpaceID != created.ID || disp.WorkerID != "w-space-1" {
		t.Fatalf("unexpected dispatch result: %+v", disp)
	}
	res, err := srv.Service().GetTaskResult(ctx, disp.TaskID, true, 10)
	if err != nil || res.Status != protocol.TaskStatusCompleted {
		t.Fatalf("expected completed task, got %+v err=%v", res, err)
	}

	// GET /api/spaces
	listResp, err := http.Get(ts.URL + "/api/spaces?worker_id=w-space-1")
	if err != nil || listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/spaces failed: %v", err)
	}
	var listOut struct {
		Spaces []map[string]any `json:"spaces"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&listOut)
	_ = listResp.Body.Close()
	if len(listOut.Spaces) != 1 {
		t.Fatalf("expected 1 space, got %d", len(listOut.Spaces))
	}

	// DELETE /api/spaces/{id}
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/spaces/"+created.ID, nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil || delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/spaces failed: %v", err)
	}
	_ = delResp.Body.Close()

	cancel()
	<-done
}

func TestSettingsPersistenceAndLiveGitRPC(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "settings-git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1. Settings PUT & GET (API key must be masked as bool, never echoed)
	putBody := `{"llm_provider":"anthropic","llm_model":"claude-sonnet-4-20250514","llm_api_key":"sk-ant-secret","terminal_font_size":14,"notify_desktop":true}`
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", strings.NewReader(putBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/settings failed: resp=%v err=%v", resp, err)
	}
	var rawBuf bytes.Buffer
	_, _ = rawBuf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(rawBuf.String(), "sk-ant-secret") {
		t.Fatalf("API key leaked in settings response: %s", rawBuf.String())
	}
	var gotSettings settingsResponse
	if err := json.Unmarshal(rawBuf.Bytes(), &gotSettings); err != nil {
		t.Fatal(err)
	}
	if gotSettings.LLMProvider != "anthropic" || !gotSettings.LLMAPIKeySet || gotSettings.TerminalFontSize != 14 || !gotSettings.NotifyDesktop {
		t.Fatalf("unexpected settings response: %+v", gotSettings)
	}

	// 2. Live Git Inspect & Commit over Worker WS RPC
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/worker"
	wCfg := config.DefaultWorkerConfig()
	wCfg.ControllerURL = wsURL
	wCfg.WorkerID = "w-git-1"
	wCfg.EnableMockDriver = true

	daemon := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = daemon.Run(ctx)
	}()
	waitForWorkerOnline(t, srv.Service(), "w-git-1")

	repo := initTempGitRepo(t)
	// Configure user inside temp repo for commit
	for _, kv := range [][2]string{{"user.name", "Test"}, {"user.email", "test@example.com"}} {
		cmd := exec.Command("git", "-C", repo, "config", kv[0], kv[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config failed: %v %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("init\nchanged line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	inspResp, err := http.Get(ts.URL + "/api/git/inspect?worker_id=w-git-1&workdir=" + repo)
	if err != nil || inspResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/git/inspect failed: resp=%v err=%v", inspResp, err)
	}
	var insp protocol.GitInspectResult
	if err := json.NewDecoder(inspResp.Body).Decode(&insp); err != nil {
		t.Fatal(err)
	}
	_ = inspResp.Body.Close()
	if !insp.IsGitRepo || len(insp.Files) != 1 || insp.Files[0].Path != "README.md" || insp.Added < 1 {
		t.Fatalf("unexpected git inspect result: %+v", insp)
	}

	commitPayload, _ := json.Marshal(map[string]any{
		"worker_id": "w-git-1",
		"workdir":   repo,
		"message":   "update readme via RPC",
		"stage_all": true,
	})
	cResp, err := http.Post(ts.URL+"/api/git/commit", "application/json", bytes.NewReader(commitPayload))
	if err != nil || cResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/git/commit failed: resp=%v err=%v", cResp, err)
	}
	var cRes protocol.GitCommitResult
	_ = json.NewDecoder(cResp.Body).Decode(&cRes)
	_ = cResp.Body.Close()
	if !cRes.OK || cRes.CommitSHA == "" {
		t.Fatalf("expected successful commit RPC, got %+v", cRes)
	}

	cancel()
	<-done
}
