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
	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/store/sqlite"
)

func TestListTicketAttemptsREST(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "attempts.db"))
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

	workerID := ids.UUID()
	if err := st.UpsertWorker(ctx, store.Worker{
		ID: workerID, Name: "bocabox", HostGroup: "bocabox", OS: "linux", Arch: "amd64", Status: "online",
	}); err != nil {
		t.Fatal(err)
	}
	tk, err := st.CreateTicket(ctx, store.Ticket{Title: "Attempts", ProjectID: store.DefaultProjectID, Status: store.TicketStatusReady})
	if err != nil {
		t.Fatal(err)
	}
	taskID := ids.UUID()
	if err := st.CreateTask(ctx, store.Task{
		ID: taskID, WorkerID: workerID, Agent: "claude", Model: "opus",
		Workdir: "/work", Prompt: "go", Status: protocol.TaskStatusCompleted, TicketID: tk.ID,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	dur := int64(42)
	if _, err := st.UpsertAttempt(ctx, store.Attempt{
		TicketID: tk.ID, TaskID: taskID, Attempt: 1, WorkerID: workerID,
		Agent: "claude", Model: "opus", Status: store.AttemptFailed,
		FailureCategory: protocol.ErrKindVerify, DurationS: &dur,
		VerifyCmd: "go test", VerifyExit: intPtr(1), VerifyFailed: intPtr(2),
		EndedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertAttempt(ctx, store.Attempt{
		TicketID: tk.ID, TaskID: taskID, Attempt: 2, WorkerID: workerID,
		Agent: "claude", Model: "opus", Status: store.AttemptReview,
		DurationS: &dur, VerifyExit: intPtr(0), EndedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/api/tickets/" + tk.ID + "/attempts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body struct {
		Attempts []store.Attempt `json:"attempts"`
	}
	if err := json.UnmarshalRead(resp.Body, &body, protocol.JSONOptions); err != nil {
		t.Fatal(err)
	}
	if len(body.Attempts) != 2 {
		t.Fatalf("attempts: %+v", body.Attempts)
	}
	if body.Attempts[0].Attempt != 1 || body.Attempts[0].Status != store.AttemptFailed || body.Attempts[0].FailureCategory != protocol.ErrKindVerify {
		t.Fatalf("attempt 1: %+v", body.Attempts[0])
	}
	if body.Attempts[0].Machine != "bocabox" || body.Attempts[1].Status != store.AttemptReview || body.Attempts[1].VerifyExit == nil || *body.Attempts[1].VerifyExit != 0 {
		t.Fatalf("attempts: %+v", body.Attempts)
	}

	miss, err := http.Get(ts.URL + "/api/tickets/" + ids.UUID() + "/attempts")
	if err != nil {
		t.Fatal(err)
	}
	defer miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound {
		t.Fatalf("missing ticket status %d", miss.StatusCode)
	}

	res, err := srv.service.Inspect(ctx, InspectRequest{Target: tk.ID, Kind: protocol.InspectKindDigest})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "attempt  machine  agent/model  duration  status  verify") ||
		!strings.Contains(res.Text, "bocabox") || !strings.Contains(res.Text, "claude/opus") ||
		!strings.Contains(res.Text, "failed/verify") || !strings.Contains(res.Text, "fail 2") ||
		!strings.Contains(res.Text, "pass") {
		t.Fatalf("inspect:\n%s", res.Text)
	}
}

func intPtr(n int) *int { return &n }
