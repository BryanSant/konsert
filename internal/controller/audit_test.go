package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/BryanSant/konsert/internal/store/sqlite"
	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	auditAdminTok = "admin-tok"
	auditMCPTok   = "mcp-tok"
	auditTSUser   = "alice@example.com"
)

// auditEnv is an authenticated controller (admin + MCP tokens, trusted Tailscale
// headers) plus a side listener that lets fake workers connect without the fleet
// token, so each control path can be exercised under a distinct identity.
type auditEnv struct {
	srv    *Server
	api    *httptest.Server
	th     *testHub
	cookie *http.Cookie
}

func newAuditEnv(t *testing.T) *auditEnv {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = false
	cfg.AdminToken = auditAdminTok
	cfg.MCPToken = auditMCPTok
	cfg.EnableMCPHTTP = true
	cfg.TrustTailscaleHeaders = true
	srv := NewServer(cfg, st, nil, "test")
	api := httptest.NewServer(srv)
	wmux := http.NewServeMux()
	wmux.HandleFunc("/ws/worker", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		srv.Hub().ServeWorkerConn(r.Context(), conn)
	})
	th := &testHub{h: srv.Hub(), st: st, svc: srv.Service(), ts: httptest.NewServer(wmux)}
	t.Cleanup(func() {
		api.Close()
		th.stop()
	})
	env := &auditEnv{srv: srv, api: api, th: th}

	// Browser session login: the cookie identity is "operator".
	resp := doReq(t, http.MethodPost, api.URL+"/api/auth/login", strings.NewReader(`{"token":"`+auditAdminTok+`"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Value != "" {
			env.cookie = c
		}
	}
	if env.cookie == nil {
		t.Fatal("login set no session cookie")
	}
	return env
}

// Identities used by the tests.
type auditCred int

const (
	credCookie auditCred = iota
	credTailscale
	credBearer
)

func (e *auditEnv) do(t *testing.T, cred auditCred, method, path, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.api.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Konsert-Request", "1")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	switch cred {
	case credCookie:
		req.AddCookie(e.cookie)
	case credTailscale:
		req.Header.Set("Tailscale-User-Login", auditTSUser)
	case credBearer:
		req.Header.Set("Authorization", "Bearer "+auditAdminTok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func (e *auditEnv) list(t *testing.T, query string) AuditListResponse {
	t.Helper()
	code, raw := e.do(t, credBearer, http.MethodGet, "/api/audit?"+query, "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/audit?%s: %d %s", query, code, raw)
	}
	var out AuditListResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// latest returns the newest audit event for action, failing if there is none.
func (e *auditEnv) latest(t *testing.T, action string) store.AuditEvent {
	t.Helper()
	out := e.list(t, "limit=1&action="+url.QueryEscape(action))
	if len(out.Events) != 1 {
		t.Fatalf("no audit event for %s", action)
	}
	return out.Events[0]
}

func expectAudit(t *testing.T, got store.AuditEvent, action, user, source, workerID, taskID string) {
	t.Helper()
	if got.Action != action || got.User != user || got.Source != source || got.WorkerID != workerID || got.TaskID != taskID {
		t.Fatalf("audit = %+v; want action=%s user=%s source=%s worker=%s task=%s", got, action, user, source, workerID, taskID)
	}
	if got.CreatedAt.IsZero() || time.Since(got.CreatedAt) > time.Minute {
		t.Fatalf("audit timestamp %v", got.CreatedAt)
	}
}

// seedTask creates a running task on workerID and routes it to the connected worker.
func (e *auditEnv) seedTask(t *testing.T, id, workerID string) {
	t.Helper()
	if err := e.th.st.CreateTask(context.Background(), store.Task{
		ID: id, WorkerID: workerID, Agent: "mock", Workdir: "/r", Status: protocol.TaskStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	e.th.h.mu.Lock()
	e.th.h.taskWorker[id] = workerID
	e.th.h.mu.Unlock()
}

func callTool(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("mcp %s: %v %+v", name, err, res)
	}
	return res
}

func TestAuditTaskControlActionsRecordUser(t *testing.T) {
	e := newAuditEnv(t)
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	e.seedTask(t, "t1", "w1")
	e.seedTask(t, "t2", "w1")
	e.seedTask(t, "t3", "w1")
	e.seedTask(t, "t4", "w1")

	// Take Control / release from the PWA toggle.
	if code, raw := e.do(t, credTailscale, http.MethodPost, "/api/tasks/t1/control", `{"enabled":true}`); code != http.StatusOK {
		t.Fatalf("take control: %d %s", code, raw)
	}
	expectAudit(t, e.latest(t, AuditTakeControl), AuditTakeControl, auditTSUser, "rest", "w1", "t1")
	if code, _ := e.do(t, credCookie, http.MethodPost, "/api/tasks/t1/control", `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("release control: %d", code)
	}
	expectAudit(t, e.latest(t, AuditReleaseControl), AuditReleaseControl, "operator", "rest", "w1", "t1")
	if code, _ := e.do(t, credCookie, http.MethodPost, "/api/tasks/t1/control", `{}`); code != http.StatusBadRequest {
		t.Fatalf("control without enabled: %d", code)
	}
	if code, _ := e.do(t, credCookie, http.MethodPost, "/api/tasks/nope/control", `{"enabled":true}`); code != http.StatusNotFound {
		t.Fatalf("control on unknown task: %d", code)
	}
	if code, _ := e.do(t, -1, http.MethodPost, "/api/tasks/t1/control", `{"enabled":true}`); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated control: %d", code)
	}

	// Steer over REST (browser session): the text itself is never recorded.
	const secret = "hunter2-do-not-log"
	if code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t1/steer", `{"input_text":"`+secret+`"}`); code != http.StatusOK {
		t.Fatalf("steer: %d %s", code, raw)
	}
	w.expect(protocol.MsgTaskSteer, 3*time.Second)
	got := e.latest(t, AuditSteer)
	expectAudit(t, got, AuditSteer, "operator", "rest", "w1", "t1")
	if strings.Contains(got.Message+got.MetaJSON, secret) {
		t.Fatalf("steer text leaked into the audit log: %+v", got)
	}

	// The konsert CLI (REST with the admin bearer token, labelled "cli").
	cli := NewRemoteFleetClient(e.api.URL, auditAdminTok)
	cli.Source = "cli"
	if _, err := cli.SteerTask(context.Background(), "t1", "y", false); err != nil {
		t.Fatal(err)
	}
	expectAudit(t, e.latest(t, AuditSteer), AuditSteer, "token", "cli", "w1", "t1")
	if _, err := cli.CancelTask(context.Background(), "t2"); err != nil {
		t.Fatal(err)
	}
	expectAudit(t, e.latest(t, AuditCancel), AuditCancel, "token", "cli", "w1", "t2")

	// REST cancel with a Tailscale identity.
	if code, _ := e.do(t, credTailscale, http.MethodPost, "/api/tasks/t3/cancel", ""); code != http.StatusOK {
		t.Fatalf("cancel: %d", code)
	}
	expectAudit(t, e.latest(t, AuditCancel), AuditCancel, auditTSUser, "rest", "w1", "t3")

	// MCP (scoped MCP token): attributed to "mcp".
	sess := connectMCPHTTP(t, e.api.URL, auditMCPTok)
	callTool(t, sess, "steer_task", map[string]any{"task_id": "t1", "input_text": secret})
	expectAudit(t, e.latest(t, AuditSteer), AuditSteer, "mcp", "mcp", "w1", "t1")
	callTool(t, sess, "cancel_task", map[string]any{"task_id": "t4"})
	expectAudit(t, e.latest(t, AuditCancel), AuditCancel, "mcp", "mcp", "w1", "t4")

	// MCP's read-only audit tool sees the same log.
	res := callTool(t, sess, "list_audit", map[string]any{"task_id": "t1", "action": AuditSteer})
	text := res.Content[0].(*mcp.TextContent).Text
	var viaMCP AuditListResponse
	if err := json.Unmarshal([]byte(text), &viaMCP); err != nil || len(viaMCP.Events) != 3 {
		t.Fatalf("list_audit = %s (%v)", text, err)
	}

	// The Orchestrator's tool calls are attributed to "orchestrator".
	e.seedTask(t, "t5", "w1")
	args, _ := json.Marshal(map[string]any{"task_id": "t5"})
	if _, err := e.srv.Service().ExecuteTool(context.Background(), "cancel_task", args); err != nil {
		t.Fatal(err)
	}
	expectAudit(t, e.latest(t, AuditCancel), AuditCancel, "orchestrator", "orchestrator", "w1", "t5")

	// Per-task control events are still written to the task timeline.
	evs, _ := e.th.st.ListEvents(context.Background(), "t1", 0, 100)
	var control int
	for _, ev := range evs {
		if ev.Kind == "control" {
			control++
		}
	}
	if control < 3 {
		t.Fatalf("task timeline has %d control events, want >= 3", control)
	}
}

func TestAuditGitCommitWorkerRemovalAndWakeLock(t *testing.T) {
	e := newAuditEnv(t)
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	e.seedTask(t, "t1", "w1")

	// The fake worker answers the git.commit RPC.
	go func() {
		env := w.expect(protocol.MsgGitCommit, 5*time.Second)
		raw, _ := protocol.NewEnvelope(protocol.MsgGitCommitResult, env.ID, protocol.GitCommitResult{OK: true, CommitSHA: "abc1234"})
		_ = w.conn.Write(w.ctx, websocket.MessageText, raw)
	}()
	code, raw := e.do(t, credTailscale, http.MethodPost, "/api/git/commit", `{"task_id":"t1","message":"fix: thing\n\nlong body"}`)
	if code != http.StatusOK {
		t.Fatalf("git commit: %d %s", code, raw)
	}
	got := e.latest(t, AuditGitCommit)
	expectAudit(t, got, AuditGitCommit, auditTSUser, "rest", "w1", "t1")
	if got.Message != "Git commit abc1234: fix: thing" {
		t.Fatalf("git commit message = %q", got.Message)
	}

	// Host wake locks.
	if code, raw := e.do(t, credCookie, http.MethodPost, "/api/workers/w1/wakelock", `{"enabled":true,"reason":"long build"}`); code != http.StatusOK {
		t.Fatalf("host wakelock: %d %s", code, raw)
	}
	got = e.latest(t, AuditWakeLockHost)
	expectAudit(t, got, AuditWakeLockHost, "operator", "rest", "w1", "")
	if !strings.Contains(got.Message, "long build") {
		t.Fatalf("wakelock message = %q", got.Message)
	}
	if code, _ := e.do(t, credBearer, http.MethodPost, "/api/wakelock", `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("fleet wakelock: %d", code)
	}
	expectAudit(t, e.latest(t, AuditWakeLockFleet), AuditWakeLockFleet, "token", "rest", "", "")

	// Worker removal.
	if err := e.th.st.UpsertWorker(context.Background(), store.Worker{ID: "w2", Name: "beta", HostGroup: "g", OS: "linux", Arch: "amd64", Status: "offline", LastSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.do(t, credTailscale, http.MethodDelete, "/api/workers/w2", ""); code != http.StatusOK {
		t.Fatalf("delete worker: %d", code)
	}
	got = e.latest(t, AuditWorkerRemove)
	expectAudit(t, got, AuditWorkerRemove, auditTSUser, "rest", "w2", "")
	if got.Message != "Removed worker beta" {
		t.Fatalf("worker removal message = %q", got.Message)
	}
	// A failed removal records nothing new.
	if code, _ := e.do(t, credTailscale, http.MethodDelete, "/api/workers/w2", ""); code != http.StatusNotFound {
		t.Fatalf("second delete: %d", code)
	}
	if out := e.list(t, "action="+AuditWorkerRemove); len(out.Events) != 1 {
		t.Fatalf("failed removal was audited: %+v", out.Events)
	}
}

func TestAuditSettingsRecordKeysNeverSecrets(t *testing.T) {
	e := newAuditEnv(t)
	const restSecret = "sk-rest-super-secret"
	const mcpSecret = "sk-mcp-super-secret"

	code, raw := e.do(t, credCookie, http.MethodPut, "/api/settings", `{"llm_api_key":"`+restSecret+`","llm_model":"gpt-4.1","terminal_font_size":14}`)
	if code != http.StatusOK {
		t.Fatalf("PUT settings: %d %s", code, raw)
	}
	got := e.latest(t, AuditSettingsUpdate)
	expectAudit(t, got, AuditSettingsUpdate, "operator", "rest", "", "")
	var meta struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal([]byte(got.MetaJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if strings.Join(meta.Keys, ",") != "llm_api_key,llm_model,terminal_font_size" {
		t.Fatalf("settings keys = %v", meta.Keys)
	}

	// The admin token over MCP may replace the key; still keys only.
	admin := connectMCPHTTP(t, e.api.URL, auditAdminTok)
	callTool(t, admin, "update_settings", map[string]any{"updates": map[string]string{"llm_api_key": mcpSecret}})
	expectAudit(t, e.latest(t, AuditSettingsUpdate), AuditSettingsUpdate, "mcp", "mcp", "", "")

	if code, _ := e.do(t, credBearer, http.MethodPut, "/api/settings", `{"clear_api_key":true}`); code != http.StatusOK {
		t.Fatalf("clear key: %d", code)
	}
	expectAudit(t, e.latest(t, AuditSettingsUpdate), AuditSettingsUpdate, "token", "rest", "", "")

	// A rejected change records nothing.
	before := len(e.list(t, "action="+AuditSettingsUpdate).Events)
	if code, _ := e.do(t, credBearer, http.MethodPut, "/api/settings", `{"llm_provider":"antropic"}`); code != http.StatusBadRequest {
		t.Fatalf("invalid provider: %d", code)
	}
	if after := len(e.list(t, "action="+AuditSettingsUpdate).Events); after != before {
		t.Fatalf("rejected settings change was audited (%d -> %d)", before, after)
	}

	// No secret value anywhere in the stored log.
	all, err := e.th.st.ListAudit(context.Background(), store.AuditFilter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range all {
		blob := fmt.Sprintf("%+v", ev)
		if strings.Contains(blob, restSecret) || strings.Contains(blob, mcpSecret) {
			t.Fatalf("secret recorded in audit log: %s", blob)
		}
	}
}

func TestAuditAPIFiltersPagingAndAuth(t *testing.T) {
	e := newAuditEnv(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		if _, err := e.th.st.AppendAudit(ctx, store.AuditEvent{
			CreatedAt: base.Add(time.Duration(i) * time.Hour),
			User:      []string{"alice", "bob"}[i%2],
			Action:    AuditSteer,
			WorkerID:  []string{"w1", "w2", "w3"}[i%3],
			TaskID:    fmt.Sprintf("t%d", i%4),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Paging: limit=10 yields next_cursor until the last partial page.
	seen := map[int64]bool{}
	cursor := ""
	pages := 0
	for {
		q := "limit=10"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		out := e.list(t, q)
		pages++
		for _, ev := range out.Events {
			if seen[ev.ID] {
				t.Fatalf("row %d repeated across pages", ev.ID)
			}
			seen[ev.ID] = true
		}
		if out.NextCursor == 0 {
			break
		}
		cursor = fmt.Sprint(out.NextCursor)
		if pages > 5 {
			t.Fatal("paging did not terminate")
		}
	}
	if pages != 3 || len(seen) != 25 {
		t.Fatalf("pages=%d rows=%d", pages, len(seen))
	}

	for q, want := range map[string]int{
		"user=alice":            13,
		"worker_id=w2":          8,
		"task_id=t0":            7,
		"user=bob&worker_id=w1": 4,
		"action=task.cancel":    0,
		"since=" + url.QueryEscape(base.Add(20*time.Hour).Format(time.RFC3339)):                                                                          5,
		"until=" + fmt.Sprint(base.Add(4*time.Hour).Unix()):                                                                                              5,
		"since=" + url.QueryEscape(base.Add(2*time.Hour).Format(time.RFC3339)) + "&until=" + url.QueryEscape(base.Add(3*time.Hour).Format(time.RFC3339)): 2,
	} {
		if got := len(e.list(t, q).Events); got != want {
			t.Errorf("%s: %d events, want %d", q, got, want)
		}
	}
	newest := e.list(t, "limit=1").Events[0].ID
	if got := e.list(t, fmt.Sprintf("after_id=%d", newest-3)).Events; len(got) != 3 {
		t.Errorf("after_id returned %d events", len(got))
	}

	for _, q := range []string{"limit=abc", "limit=0", "since=yesterday", "before_id=-1", "after_id=x"} {
		if code, _ := e.do(t, credBearer, http.MethodGet, "/api/audit?"+q, ""); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, code)
		}
	}
	if code, _ := e.do(t, -1, http.MethodGet, "/api/audit", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/audit: %d", code)
	}
	// The MCP token is not a browser credential.
	req, _ := http.NewRequest(http.MethodGet, e.api.URL+"/api/audit", nil)
	req.Header.Set("Authorization", "Bearer "+auditMCPTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("MCP token on /api/audit: %d", resp.StatusCode)
	}
}

func TestAuditEventsBroadcastToBrowsers(t *testing.T) {
	th := newTestHub(t)
	browser := th.dialBrowser(t)
	waitFor(t, 3*time.Second, "browser registered", func() bool { return th.clientCount() == 1 })

	ctx := withAuditActor(context.Background(), "carol", "rest")
	th.h.recordAudit(ctx, store.AuditEvent{Action: AuditTakeControl, WorkerID: "w1", TaskID: "t1", Message: "Took interactive control"})

	for {
		env := browser.expect(protocol.MsgFleetUpdate, 3*time.Second)
		var p struct {
			Kind  string           `json:"kind"`
			Audit store.AuditEvent `json:"audit"`
		}
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Kind != "audit" {
			continue
		}
		if p.Audit.ID == 0 || p.Audit.User != "carol" || p.Audit.Action != AuditTakeControl || p.Audit.TaskID != "t1" {
			t.Fatalf("broadcast audit = %+v", p.Audit)
		}
		return
	}
}

func TestAuditActorDefaults(t *testing.T) {
	if a := auditActorFrom(context.Background()); a.User != "system" {
		t.Fatalf("default actor = %+v", a)
	}
	ctx := withAuditActor(context.Background(), "alice", "rest")
	if a := auditActorFrom(withDefaultAuditActor(ctx, "orchestrator", "orchestrator")); a.User != "alice" {
		t.Fatalf("withDefaultAuditActor overrode an existing actor: %+v", a)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(clientKindHeader, "evil")
	if got := restAuditSource(r); got != "rest" {
		t.Fatalf("unknown client kind mapped to %q", got)
	}
}
