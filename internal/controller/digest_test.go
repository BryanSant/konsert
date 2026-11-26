package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/store/sqlite"
)

func TestTaskDigestStoredAndExposed(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "digest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer srv.Close()

	th := &testHub{h: srv.hub, st: st, svc: srv.service, ts: ts}
	fw := th.dialWorker(t, protocol.WorkerInfo{ID: "w-digest", Name: "bocabox", Agents: []string{"claude"}}, nil)
	defer fw.close()

	// Wait until worker.hello has registered the worker row (tasks.worker_id FK).
	regDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(regDeadline) {
		if _, err := st.GetWorker(ctx, "w-digest"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	task := store.Task{
		ID:       "t-digest-1",
		WorkerID: "w-digest",
		Agent:    "claude",
		Model:    "opus",
		Workdir:  "/work",
		Mode:     protocol.TaskModeOneshot,
		Origin:   protocol.TaskOriginCommander,
		Prompt:   "rotate tokens",
		Status:   protocol.TaskStatusRunning,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	code := 0
	wantDigest := &protocol.RunDigest{
		TicketSID: "SYR-X17JQ",
		Status:    protocol.DigestStatusReview,
		Machine:   "bocabox",
		Agent:     "claude",
		Model:     "opus",
		DurationS: 42,
		Git:       map[string]string{"api": "2 files, +74 -9"},
		Summary:   "Rotation implemented; tests pass.",
	}
	fw.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{
		TaskID:   task.ID,
		Status:   protocol.TaskStatusCompleted,
		ExitCode: &code,
		Summary:  "raw free-text summary that should be replaced by digest text",
		Digest:   wantDigest,
	})

	// Wait until the hub persists the terminal status and digest.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := st.GetTask(ctx, task.ID)
		if err == nil && got.Status == protocol.TaskStatusCompleted && got.DigestJSON != "" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	stored, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DigestJSON == "" {
		t.Fatalf("expected digest_json persisted on task")
	}

	// Verify GetTaskResult (used by MCP get_task_result) exposes Digest and replaces Summary.
	res, err := srv.service.GetTaskResult(ctx, task.ID, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest == nil || res.Digest.TicketSID != "SYR-X17JQ" || res.Digest.Git["api"] != "2 files, +74 -9" {
		t.Fatalf("unexpected GetTaskResult.Digest: %+v", res.Digest)
	}
	wantText := wantDigest.Text()
	if res.Summary != wantText {
		t.Fatalf("GetTaskResult.Summary:\n got: %q\nwant: %q", res.Summary, wantText)
	}

	// Verify GET /api/tasks/{id} exposes the same Digest and digest-text Summary.
	resp, err := http.Get(ts.URL + "/api/tasks/" + task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/tasks/%s status = %d", task.ID, resp.StatusCode)
	}
	var apiRes TaskResult
	if err := json.UnmarshalRead(resp.Body, &apiRes, protocol.JSONOptions); err != nil {
		t.Fatal(err)
	}
	if apiRes.Digest == nil || apiRes.Digest.TicketSID != "SYR-X17JQ" {
		t.Fatalf("unexpected API Digest: %+v", apiRes.Digest)
	}
	if !strings.Contains(apiRes.Summary, "ticket: SYR-X17JQ") || apiRes.Summary != wantText {
		t.Fatalf("unexpected API Summary: %q", apiRes.Summary)
	}

	rows, err := st.ListAttempts(ctx, "", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != store.AttemptReview || rows[0].TicketID != "" ||
		rows[0].Agent != "claude" || rows[0].Model != "opus" ||
		rows[0].DurationS == nil || *rows[0].DurationS != 42 || !strings.Contains(rows[0].GitJSON, "api") {
		t.Fatalf("adhoc attempt: %+v", rows)
	}
	ins, err := srv.service.Inspect(ctx, InspectRequest{Target: task.ID, Kind: protocol.InspectKindDigest})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ins.Text, "summary: Rotation implemented") || !strings.Contains(ins.Text, "bocabox") ||
		!strings.Contains(ins.Text, "claude/opus") || !strings.Contains(ins.Text, "42s") || !strings.Contains(ins.Text, "review") {
		t.Fatalf("inspect digest:\n%s", ins.Text)
	}
	if res.Summary != wantText {
		t.Fatalf("GetTaskResult.Summary gained the attempts table: %q", res.Summary)
	}
}
