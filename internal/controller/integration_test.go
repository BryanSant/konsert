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
	"github.com/BryanSant/konsert/internal/store"
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
	loginReq.Header.Set("Content-Type", "application/json")
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

func TestRemoteFSListAndDeleteWorkerAPI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fs-del.db"))
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
	wCfg.WorkerID = "w-fs-1"
	wCfg.Name = "fs-node"
	wCfg.EnableMockDriver = true

	daemon := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = daemon.Run(ctx)
	}()
	waitForWorkerOnline(t, srv.Service(), "w-fs-1")

	// Prepare nested directories including a git repo inside a temp root.
	rootDir := t.TempDir()
	alphaRepo := filepath.Join(rootDir, "alpha-repo")
	betaDir := filepath.Join(rootDir, "beta-tools")
	if err := os.MkdirAll(filepath.Join(alphaRepo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(betaDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Browse full directory
	fsResp, err := http.Get(ts.URL + "/api/fs/list?worker_id=w-fs-1&path=" + rootDir)
	if err != nil || fsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/fs/list failed: resp=%v err=%v", fsResp, err)
	}
	var fsRes protocol.FSListResult
	if err := json.NewDecoder(fsResp.Body).Decode(&fsRes); err != nil {
		t.Fatal(err)
	}
	_ = fsResp.Body.Close()
	if len(fsRes.Entries) != 2 || fsRes.Entries[0].Name != "alpha-repo" || !fsRes.Entries[0].IsGitRepo {
		t.Fatalf("unexpected fs list result: %+v", fsRes)
	}

	// 2. Type-ahead partial path prefix match
	partialPath := filepath.Join(rootDir, "alp")
	taResp, err := http.Get(ts.URL + "/api/fs/list?worker_id=w-fs-1&path=" + partialPath)
	if err != nil || taResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/fs/list type-ahead failed: resp=%v err=%v", taResp, err)
	}
	var taRes protocol.FSListResult
	if err := json.NewDecoder(taResp.Body).Decode(&taRes); err != nil {
		t.Fatal(err)
	}
	_ = taResp.Body.Close()
	if len(taRes.Entries) != 1 || taRes.Entries[0].Name != "alpha-repo" {
		t.Fatalf("expected single alpha-repo match for partial path, got %+v", taRes)
	}

	// 3. SSH hosts endpoint returns valid JSON array
	sshResp, err := http.Get(ts.URL + "/api/ssh/hosts")
	if err != nil || sshResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/ssh/hosts failed: resp=%v err=%v", sshResp, err)
	}
	_ = sshResp.Body.Close()

	// 4. Delete worker via REST API
	cancel() // stop daemon reconnect loop before deleting so it doesn't immediately re-register
	<-done

	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/workers/w-fs-1", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil || delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/workers/w-fs-1 failed: resp=%v err=%v", delResp, err)
	}
	_ = delResp.Body.Close()

	// Confirm worker is gone from /api/fleet
	fleetResp, err := http.Get(ts.URL + "/api/fleet")
	if err != nil || fleetResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/fleet failed: %v", err)
	}
	var fleetOut struct {
		Workers []map[string]any `json:"workers"`
	}
	_ = json.NewDecoder(fleetResp.Body).Decode(&fleetOut)
	_ = fleetResp.Body.Close()
	if len(fleetOut.Workers) != 0 {
		t.Fatalf("expected 0 workers after delete, got %d", len(fleetOut.Workers))
	}
}

func TestTaxonomyAPIAndUnifiedSessionCascade(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "tax-session.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Connect a localhost-named worker with mock driver
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/worker"
	wCfg := config.DefaultWorkerConfig()
	wCfg.ControllerURL = wsURL
	wCfg.WorkerID = "w-local-1"
	wCfg.Name = "localhost"
	wCfg.EnableMockDriver = true

	daemon := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = daemon.Run(ctx)
	}()
	waitForWorkerOnline(t, srv.Service(), "w-local-1")

	// 1. Verify GET /api/taxonomy starts with built-in Default Space & Default Project
	taxResp, err := http.Get(ts.URL + "/api/taxonomy")
	if err != nil || taxResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/taxonomy failed: resp=%v err=%v", taxResp, err)
	}
	var tax struct {
		Workspaces []map[string]any `json:"workspaces"`
		Projects   []map[string]any `json:"projects"`
	}
	_ = json.NewDecoder(taxResp.Body).Decode(&tax)
	_ = taxResp.Body.Close()
	if len(tax.Workspaces) != 1 || len(tax.Projects) != 1 {
		t.Fatalf("expected Default workspace and project, got %+v", tax)
	}

	// 2. Create Workspace -> Project -> 2 Repos -> Worktree
	wsResp, err := http.Post(ts.URL+"/api/workspaces", "application/json", strings.NewReader(`{"name":"Engineering"}`))
	if err != nil || wsResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/workspaces failed: %v", err)
	}
	var wsObj struct{ ID string `json:"id"` }
	_ = json.NewDecoder(wsResp.Body).Decode(&wsObj)
	_ = wsResp.Body.Close()

	projResp, err := http.Post(ts.URL+"/api/projects", "application/json", strings.NewReader(`{"workspace_id":"`+wsObj.ID+`","name":"Platform"}`))
	if err != nil || projResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/projects failed: %v", err)
	}
	var projObj struct{ ID string `json:"id"` }
	_ = json.NewDecoder(projResp.Body).Decode(&projObj)
	_ = projResp.Body.Close()

	repoDir := t.TempDir()
	repoBody, _ := json.Marshal(map[string]string{
		"project_id": projObj.ID,
		"worker_id":  "w-local-1",
		"name":       "platform-core",
		"path":       repoDir,
		"branch":     "main",
	})
	repoResp, err := http.Post(ts.URL+"/api/repos", "application/json", bytes.NewReader(repoBody))
	if err != nil || repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/repos failed: %v", err)
	}
	var repoObj struct{ ID string `json:"id"` }
	_ = json.NewDecoder(repoResp.Body).Decode(&repoObj)
	_ = repoResp.Body.Close()

	// 3. Session defaults cascade: localhost machine + smart first-run guess
	defResp, err := http.Get(ts.URL + "/api/sessions/defaults")
	if err != nil || defResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/sessions/defaults failed: %v", err)
	}
	var defs SessionDefaultsResult
	_ = json.NewDecoder(defResp.Body).Decode(&defs)
	_ = defResp.Body.Close()
	if defs.MachineID != "w-local-1" || defs.MachineSource != "localhost" || defs.Agent == "" || defs.Model == "" {
		t.Fatalf("unexpected initial session defaults: %+v", defs)
	}

	// 4. Start a unified Chat Session bound to repo_id with explicit model -> persists as last_used
	sessBody, _ := json.Marshal(map[string]any{
		"session_kind": "chat",
		"origin":       "direct",
		"repo_id":      repoObj.ID,
		"agent":        "mock",
		"model":        "claude-sonnet-4-5",
		"mode":         "oneshot",
		"prompt":       "STREAM_LINES=1",
	})
	sessResp, err := http.Post(ts.URL+"/api/sessions", "application/json", bytes.NewReader(sessBody))
	if err != nil || sessResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/sessions failed: resp=%v err=%v", sessResp, err)
	}
	var disp DispatchResult
	_ = json.NewDecoder(sessResp.Body).Decode(&disp)
	_ = sessResp.Body.Close()
	if disp.WorkspaceID != wsObj.ID || disp.ProjectID != projObj.ID || disp.RepoID != repoObj.ID || disp.Model != "claude-sonnet-4-5" {
		t.Fatalf("unexpected unified session result: %+v", disp)
	}

	// 5. Verify last-used cascade now picks mock + claude-sonnet-4-5
	defResp2, err := http.Get(ts.URL + "/api/sessions/defaults")
	if err != nil || defResp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/sessions/defaults (2) failed: %v", err)
	}
	var defs2 SessionDefaultsResult
	_ = json.NewDecoder(defResp2.Body).Decode(&defs2)
	_ = defResp2.Body.Close()
	if defs2.Agent != "mock" || defs2.AgentSource != "last_used" || defs2.Model != "claude-sonnet-4-5" || defs2.ModelSource != "last_used" {
		t.Fatalf("expected last_used cascade after session start, got %+v", defs2)
	}

	// 6. Configured default overrides last_used
	putReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", strings.NewReader(`{"default_agent":"mock","default_model":"gemini-2.5-pro"}`))
	putReq.Header.Set("Content-Type", "application/json")
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil || putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/settings default agent/model failed: %v", err)
	}
	_ = putResp.Body.Close()

	defResp3, _ := http.Get(ts.URL + "/api/sessions/defaults")
	var defs3 SessionDefaultsResult
	_ = json.NewDecoder(defResp3.Body).Decode(&defs3)
	_ = defResp3.Body.Close()
	if defs3.Agent != "mock" || defs3.AgentSource != "default" || defs3.Model != "gemini-2.5-pro" || defs3.ModelSource != "default" {
		t.Fatalf("expected configured default to override last_used, got %+v", defs3)
	}

	// Deleting built-in default workspace via REST returns 400
	delDefReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/workspaces/space-default", nil)
	delDefResp, err := http.DefaultClient.Do(delDefReq)
	if err != nil || delDefResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 deleting default workspace, got %v err=%v", delDefResp, err)
	}
	_ = delDefResp.Body.Close()

	cancel()
	<-done
}

type capturingLLMProvider struct {
	lastSystem string
	lastCount  int
}

func (p *capturingLLMProvider) Name() string { return "capture-llm" }

func (p *capturingLLMProvider) Complete(_ context.Context, _, system string, msgs []orchestrator.Message, _ []orchestrator.ToolDef, onDelta func(string)) (orchestrator.Message, error) {
	p.lastSystem = system
	p.lastCount = len(msgs)
	text := "Compact snapshot received."
	if onDelta != nil {
		onDelta(text)
	}
	return orchestrator.Message{Role: "assistant", Content: text}, nil
}

func TestMachineFactsCachingDaemonFirstToolsAndOrchestratorMemory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "facts-mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Start worker daemon that ships real MachineFacts on worker.hello
	rootDir := t.TempDir()
	wtDir := filepath.Join(rootDir, "app1", ".konsert-worktrees", "epic-123-cleanup")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/worker"
	wCfg := config.DefaultWorkerConfig()
	wCfg.ControllerURL = wsURL
	wCfg.WorkerID = "w-facts-1"
	wCfg.Name = "foo1"
	wCfg.EnableMockDriver = true
	wCfg.AllowedRoots = []string{rootDir}

	daemon := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = daemon.Run(ctx)
	}()
	waitForWorkerOnline(t, srv.Service(), "w-facts-1")

	// 1. Verify cached MachineFacts via GET /api/workers/{id}/facts
	fResp, err := http.Get(ts.URL + "/api/workers/w-facts-1/facts")
	if err != nil || fResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/workers/w-facts-1/facts failed: resp=%v err=%v", fResp, err)
	}
	var fOut struct {
		WorkerID string                 `json:"worker_id"`
		Facts    *protocol.MachineFacts `json:"facts"`
	}
	if err := json.NewDecoder(fResp.Body).Decode(&fOut); err != nil {
		t.Fatal(err)
	}
	_ = fResp.Body.Close()
	if fOut.Facts == nil || fOut.Facts.CPUCores <= 0 || fOut.Facts.OSName == "" || fOut.Facts.Virtualization == "" {
		t.Fatalf("expected populated cached machine facts, got %+v", fOut.Facts)
	}

	// 2. Deterministic daemon-first tool: scan_temp_worktrees finds epic-123 worktree with 0 agent LLM calls
	scanRes, err := srv.Service().ExecuteTool(ctx, "scan_temp_worktrees", json.RawMessage(`{"query":"epic-123"}`))
	if err != nil {
		t.Fatalf("scan_temp_worktrees failed: %v", err)
	}
	scanBytes, _ := json.Marshal(scanRes)
	if !strings.Contains(string(scanBytes), "epic-123-cleanup") {
		t.Fatalf("expected deterministic scan to find epic-123-cleanup, got %s", string(scanBytes))
	}

	// 3. Deterministic save_memory + Orchestrator context-window compaction checkpointing
	if _, err := srv.Service().ExecuteTool(ctx, "save_memory", json.RawMessage(`{"scope":"fleet","key":"gcp-box","summary":"foo1 has gcloud logged in"}`)); err != nil {
		t.Fatalf("save_memory failed: %v", err)
	}

	// Seed 30 old chat messages so loadBoundedHistory compacts older turns into SQLite thread_checkpoint
	_ = st.CreateChatSession(ctx, store.ChatSession{ID: "sess-compact", Title: "Long Thread"})
	for i := 0; i < 30; i++ {
		_, _ = st.AppendChatMessage(ctx, store.ChatMessage{
			SessionID: "sess-compact",
			Role:      "user",
			Content:   "Earlier fleet instruction about app1",
		})
	}

	capProv := &capturingLLMProvider{}
	srv.Orchestrator().SetProvider(capProv)
	if err := srv.Orchestrator().HandleUserMessage(ctx, "sess-compact", "What do we know about foo1?", orchestrator.StreamCallbacks{}); err != nil {
		t.Fatalf("HandleUserMessage failed: %v", err)
	}
	if capProv.lastCount > 25 {
		t.Fatalf("expected bounded history <= 25 messages, got %d", capProv.lastCount)
	}
	if !strings.Contains(capProv.lastSystem, "foo1") || !strings.Contains(capProv.lastSystem, "foo1 has gcloud logged in") || !strings.Contains(capProv.lastSystem, "Thread Checkpoint") {
		t.Fatalf("expected compact fleet snapshot + memory + thread checkpoint in system prompt, got:\n%s", capProv.lastSystem)
	}

	cancel()
	<-done
}

func TestFanOutQuotasAndExternalSessionTaxonomyMapping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "wave3.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Worker 1 has an epic-123 temp worktree; Worker 2 is clean (no-op).
	root1 := t.TempDir()
	root2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root1, "app1", ".konsert-worktrees", "epic-123-wt"), 0o755); err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/worker"
	startWorker := func(id, name, root string) chan struct{} {
		wCfg := config.DefaultWorkerConfig()
		wCfg.ControllerURL = wsURL
		wCfg.WorkerID = id
		wCfg.Name = name
		wCfg.EnableMockDriver = true
		wCfg.AllowedRoots = []string{root}
		d := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = d.Run(ctx)
		}()
		waitForWorkerOnline(t, srv.Service(), id)
		return done
	}
	done1 := startWorker("w-match", "box-a", root1)
	done2 := startWorker("w-noop", "box-b", root2)

	// 1. Fleet-wide fan-out deterministically short-circuits w-noop and matches w-match (0 agent LLM calls)
	fanBody := `{"kind":"worktree_cleanup","query":"epic-123","apply":false}`
	fResp, err := http.Post(ts.URL+"/api/fleet/fanout", "application/json", strings.NewReader(fanBody))
	if err != nil || fResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/fleet/fanout failed: resp=%v err=%v", fResp, err)
	}
	var fanOut FanOutResult
	if err := json.NewDecoder(fResp.Body).Decode(&fanOut); err != nil {
		t.Fatal(err)
	}
	_ = fResp.Body.Close()
	if fanOut.TotalMachines != 2 || fanOut.NoOpMachines != 1 || fanOut.ActedMachines != 1 || fanOut.TotalMatches != 1 {
		t.Fatalf("unexpected fanout result: %+v", fanOut)
	}

	// 2. Subscription-first quotas upsert & retrieval via /api/usage
	qBody := `{"quotas":[{"provider":"codex","label":"Codex","window_label":"5h","used_percent":21},{"provider":"agy","label":"Antigravity","window_label":"8h","used_percent":15}]}`
	qResp, err := http.Post(ts.URL+"/api/usage/quotas", "application/json", strings.NewReader(qBody))
	if err != nil || qResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/usage/quotas failed: resp=%v err=%v", qResp, err)
	}
	var usageOut store.UsageSummary
	if err := json.NewDecoder(qResp.Body).Decode(&usageOut); err != nil {
		t.Fatal(err)
	}
	_ = qResp.Body.Close()
	if len(usageOut.Quotas) != 2 {
		t.Fatalf("expected 2 subscription quotas in usage summary, got %+v", usageOut.Quotas)
	}

	// 3. External SSH-launched session maps automatically to Space > Project > Repo (or Default fallback)
	_ = st.CreateWorkspace(ctx, store.Workspace{ID: "ws-eng", Name: "Eng"})
	_ = st.CreateProject(ctx, store.Project{ID: "proj-app1", WorkspaceID: "ws-eng", Name: "App1"})
	app1Dir := filepath.Join(root1, "app1")
	_ = st.CreateRepo(ctx, store.Repo{ID: "repo-app1", ProjectID: "proj-app1", WorkerID: "w-match", Name: "app1", Path: app1Dir})

	srv.Hub().handleExternalSession(ctx, "w-match", protocol.ExternalSessionPayload{
		ExternalID:   "ssh-agy-42",
		Agent:        "agy",
		Cwd:          filepath.Join(app1Dir, "cmd"),
		Branch:       "feat/deploy",
		Status:       "working",
		ChangedFiles: []string{"cmd/main.go"},
		Source:       "hook",
	})
	mapped, err := st.GetTask(ctx, "ext-w-match-ssh-agy-42")
	if err != nil || mapped.WorkspaceID != "ws-eng" || mapped.ProjectID != "proj-app1" || mapped.RepoID != "repo-app1" || mapped.Status != protocol.TaskStatusRunning {
		t.Fatalf("expected external session mapped to Eng/App1/repo-app1, got %+v err=%v", mapped, err)
	}

	// Unmanaged path falls back cleanly to Default Space & Default Project
	srv.Hub().handleExternalSession(ctx, "w-match", protocol.ExternalSessionPayload{
		ExternalID: "ssh-claude-99",
		Agent:      "claude",
		Cwd:        "/tmp/ad-hoc-scratch",
		Status:     "needs_you",
		Source:     "passive",
	})
	fallback, err := st.GetTask(ctx, "ext-w-match-ssh-claude-99")
	if err != nil || fallback.WorkspaceID != store.DefaultWorkspaceID || fallback.ProjectID != store.DefaultProjectID || fallback.Status != protocol.TaskStatusWaitingInput {
		t.Fatalf("expected unmanaged external session to fall back to Default Space/Project, got %+v err=%v", fallback, err)
	}

	cancel()
	<-done1
	<-done2
}

func TestFullControlMCPToolsAndProgressNotifications(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "mcp-full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	cfg.EnableMCPHTTP = true
	srv := NewServer(cfg, st, nil, "test")
	srv.Orchestrator().SetProvider(&capturingLLMProvider{})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Connect via in-memory transport (covers stdio MCP parity)
	mcpSrv := NewMCPServer(srv.Service(), "test", srv.Orchestrator())
	cTrans, sTrans := mcp.NewInMemoryTransports()
	srvSess, err := mcpSrv.Connect(ctx, sTrans, nil)
	if err != nil {
		t.Fatalf("mcp server connect failed: %v", err)
	}
	defer srvSess.Close()

	var progressMsgs []string
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-mcp-client", Version: "1.0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			if req != nil && req.Params != nil {
				progressMsgs = append(progressMsgs, req.Params.Message)
			}
		},
	})
	clientSess, err := mcpClient.Connect(ctx, cTrans, nil)
	if err != nil {
		t.Fatalf("mcp client connect failed: %v", err)
	}
	defer clientSess.Close()

	// 1. manage_taxonomy + get_taxonomy
	if res, err := clientSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "manage_taxonomy",
		Arguments: map[string]any{"action": "create", "kind": "workspace", "name": "MCP-Space"},
	}); err != nil || res.IsError {
		t.Fatalf("manage_taxonomy failed: %v res=%+v", err, res)
	}
	taxRes, err := clientSess.CallTool(ctx, &mcp.CallToolParams{Name: "get_taxonomy"})
	if err != nil || taxRes.IsError || !strings.Contains(taxRes.Content[0].(*mcp.TextContent).Text, "MCP-Space") {
		t.Fatalf("get_taxonomy missing MCP-Space: %v res=%+v", err, taxRes)
	}

	// 2. update_settings + get_settings (never exposes api keys)
	if res, err := clientSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "update_settings",
		Arguments: map[string]any{"updates": map[string]string{"default_agent": "codex", "default_model": "o4-mini"}},
	}); err != nil || res.IsError {
		t.Fatalf("update_settings failed: %v res=%+v", err, res)
	}
	setRes, err := clientSess.CallTool(ctx, &mcp.CallToolParams{Name: "get_settings"})
	if err != nil || setRes.IsError || !strings.Contains(setRes.Content[0].(*mcp.TextContent).Text, "codex") {
		t.Fatalf("get_settings failed: %v res=%+v", err, setRes)
	}

	// 3. save_memory + recall_memory + ask_orchestrator
	if res, err := clientSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "save_memory",
		Arguments: map[string]any{"scope": "fleet", "key": "mcp-note", "summary": "verified via MCP"},
	}); err != nil || res.IsError {
		t.Fatalf("save_memory failed: %v res=%+v", err, res)
	}
	orchRes, err := clientSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ask_orchestrator",
		Arguments: map[string]any{"prompt": "Summarize fleet status"},
	});
	if err != nil || orchRes.IsError || !strings.Contains(orchRes.Content[0].(*mcp.TextContent).Text, "Compact snapshot received.") {
		t.Fatalf("ask_orchestrator failed: %v res=%+v", err, orchRes)
	}

	// 4. Verify Streamable HTTP (/mcp) endpoint responds cleanly to RemoteFleetClient + MCP HTTP client
	httpClient := mcp.NewClient(&mcp.Implementation{Name: "http-mcp-client", Version: "1.0"}, nil)
	httpSess, err := httpClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("Streamable HTTP /mcp connect failed: %v", err)
	}
	defer httpSess.Close()
	factsRes, err := httpSess.CallTool(ctx, &mcp.CallToolParams{Name: "inspect_machine_facts"})
	if err != nil || factsRes.IsError {
		t.Fatalf("HTTP /mcp inspect_machine_facts failed: %v res=%+v", err, factsRes)
	}
}

func TestChatSessionRenameDeleteAndAttachments(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "chat-attach.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Connect worker daemon so POST /api/attachments routes over worker RPC to OS scratch dir.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/worker"
	wCfg := config.DefaultWorkerConfig()
	wCfg.ControllerURL = wsURL
	wCfg.WorkerID = "w-att-1"
	wCfg.Name = "localhost"
	wCfg.EnableMockDriver = true

	daemon := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = daemon.Run(ctx)
	}()
	waitForWorkerOnline(t, srv.Service(), "w-att-1")

	// 1. Create -> Rename -> Delete chat session
	createResp, err := http.Post(ts.URL+"/api/chat/sessions", "application/json", strings.NewReader(`{"title":"Initial"}`))
	if err != nil || createResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/chat/sessions failed: resp=%v err=%v", createResp, err)
	}
	var cs store.ChatSession
	_ = json.NewDecoder(createResp.Body).Decode(&cs)
	_ = createResp.Body.Close()

	patchReq, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/chat/sessions/"+cs.ID, strings.NewReader(`{"title":"Renamed Thread"}`))
	patchReq.Header.Set("Content-Type", "application/json")
	patchResp, err := http.DefaultClient.Do(patchReq)
	if err != nil || patchResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/chat/sessions failed: resp=%v err=%v", patchResp, err)
	}
	var renamed store.ChatSession
	_ = json.NewDecoder(patchResp.Body).Decode(&renamed)
	_ = patchResp.Body.Close()
	if renamed.Title != "Renamed Thread" {
		t.Fatalf("expected Renamed Thread, got %q", renamed.Title)
	}

	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/chat/sessions/"+cs.ID, nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil || delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/chat/sessions failed: resp=%v err=%v", delResp, err)
	}
	_ = delResp.Body.Close()

	// 2. Upload local attachment -> forwarded via MsgFileUpload RPC to konsertd OS scratch dir
	attachBody := `{"worker_id":"w-att-1","session_id":"sess-1","filename":"trace.log","data_base64":"aGVsbG8ga29uc2VydAo="}`
	attResp, err := http.Post(ts.URL+"/api/attachments", "application/json", strings.NewReader(attachBody))
	if err != nil || attResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/attachments failed: resp=%v err=%v", attResp, err)
	}
	var attOut struct {
		OK         bool   `json:"ok"`
		WorkerID   string `json:"worker_id"`
		RemotePath string `json:"remote_path"`
		Filename   string `json:"filename"`
		Size       int64  `json:"size"`
	}
	_ = json.NewDecoder(attResp.Body).Decode(&attOut)
	_ = attResp.Body.Close()
	if !attOut.OK || attOut.WorkerID != "w-att-1" || attOut.RemotePath == "" || attOut.Size != 14 {
		t.Fatalf("unexpected attachment upload response: %+v", attOut)
	}
	content, err := os.ReadFile(attOut.RemotePath)
	if err != nil || string(content) != "hello konsert\n" {
		t.Fatalf("expected uploaded file at %q with content, got %q err=%v", attOut.RemotePath, string(content), err)
	}
	_ = os.Remove(attOut.RemotePath)

	cancel()
	<-done
}

func TestWorkerDedupAndCleanRemoval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "dedup-remove.db"))
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

	// 1. Start first konsertd instance for machine "devbox" with ID "w-devbox-1"
	wCfg1 := config.DefaultWorkerConfig()
	wCfg1.ControllerURL = wsURL
	wCfg1.WorkerID = "w-devbox-1"
	wCfg1.Name = "devbox"
	wCfg1.HostGroup = "devbox"
	wCfg1.Isolation = "host"
	wCfg1.EnableMockDriver = true

	d1Ctx, d1Cancel := context.WithCancel(ctx)
	d1 := worker.NewDaemon(wCfg1, drivers.DefaultRegistry, nil)
	d1Done := make(chan struct{})
	go func() {
		defer close(d1Done)
		_ = d1.Run(d1Ctx)
	}()
	waitForWorkerOnline(t, srv.Service(), "w-devbox-1")

	// 2. Rerun konsertd on the same machine with a different ephemeral ID -> must reconcile, NOT add a redundant worker!
	wCfg2 := config.DefaultWorkerConfig()
	wCfg2.ControllerURL = wsURL
	wCfg2.WorkerID = "w-devbox-ephemeral-2"
	wCfg2.Name = "devbox"
	wCfg2.HostGroup = "devbox"
	wCfg2.Isolation = "host"
	wCfg2.EnableMockDriver = true

	d2 := worker.NewDaemon(wCfg2, drivers.DefaultRegistry, nil)
	d2Done := make(chan struct{})
	go func() {
		defer close(d2Done)
		_ = d2.Run(ctx)
	}()

	// Wait for d1 to be cleanly superseded and exit!
	select {
	case <-d1Done:
	case <-time.After(5 * time.Second):
		d1Cancel()
		t.Fatal("expected superseded first konsertd to exit cleanly")
	}
	d1Cancel()

	// Verify only 1 canonical worker exists in fleet (no redundant duplicate!)
	fs, err := srv.Service().FleetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.Workers) != 1 || fs.Workers[0].ID != "w-devbox-1" || fs.Workers[0].Status != "online" {
		t.Fatalf("expected 1 reconciled online worker w-devbox-1, got %+v", fs.Workers)
	}

	// 3. DELETE /api/workers/w-devbox-1 while d2 is actively running -> d2 must exit cleanly and NOT reconnect!
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/workers/w-devbox-1", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil || delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/workers/w-devbox-1 failed: resp=%v err=%v", delResp, err)
	}
	_ = delResp.Body.Close()

	select {
	case <-d2Done:
	case <-time.After(5 * time.Second):
		t.Fatal("expected removed konsertd to stop instead of reconnecting")
	}

	fsAfter, err := srv.Service().FleetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(fsAfter.Workers) != 0 {
		t.Fatalf("expected 0 workers after removal, got %+v", fsAfter.Workers)
	}
}
