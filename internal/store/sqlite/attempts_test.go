package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestAttemptsUpsertListAndTicketCounter(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	workerID := ids.UUID()
	if err := st.UpsertWorker(ctx, store.Worker{
		ID: workerID, Name: "bocabox", HostGroup: "bocabox", OS: "linux", Arch: "amd64", Status: "online",
	}); err != nil {
		t.Fatal(err)
	}
	tk, err := st.CreateTicket(ctx, store.Ticket{Title: "Ship it", ProjectID: store.DefaultProjectID})
	if err != nil {
		t.Fatal(err)
	}
	taskID := ids.UUID()
	if err := st.CreateTask(ctx, store.Task{
		ID: taskID, WorkerID: workerID, Agent: "claude", Model: "opus",
		Workdir: "/work", Prompt: "ship", Status: "running", TicketID: tk.ID,
	}); err != nil {
		t.Fatal(err)
	}
	adhocID := ids.UUID()
	if err := st.CreateTask(ctx, store.Task{
		ID: adhocID, WorkerID: workerID, Agent: "claude", Workdir: "/work", Prompt: "adhoc", Status: "running",
	}); err != nil {
		t.Fatal(err)
	}

	queued := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	started := queued.Add(10 * time.Second)
	ended := started.Add(42 * time.Second)
	cost := 1.5
	exit0 := 0
	row, err := st.UpsertAttempt(ctx, store.Attempt{
		TicketID: tk.ID, TaskID: taskID, Attempt: 1, WorkerID: workerID,
		Agent: "claude", Model: "opus",
		QueuedAt: &queued, StartedAt: &started, EndedAt: &ended,
		Status: store.AttemptReview, ExitCode: &exit0, VerifyCmd: "go test",
		VerifyExit: &exit0, VerifyPassed: intPtr(4), VerifyFailed: intPtr(0),
		TokensIn: 120, TokensOut: 42, CostUSD: &cost, GitJSON: `{"api":"1 file, +1 -0"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.ID == "" || row.QueueWaitS == nil || *row.QueueWaitS != 10 || row.DurationS == nil || *row.DurationS != 42 {
		t.Fatalf("timings: wait=%v dur=%v id=%q", row.QueueWaitS, row.DurationS, row.ID)
	}
	if row.VerifyExit == nil || *row.VerifyExit != 0 || row.CostUSD == nil || *row.CostUSD != 1.5 {
		t.Fatalf("verify/cost: exit=%v cost=%v", row.VerifyExit, row.CostUSD)
	}
	gotTK, err := st.GetTicket(ctx, tk.ID)
	if err != nil || gotTK.Attempt != 1 {
		t.Fatalf("ticket attempt = %d err=%v", gotTK.Attempt, err)
	}

	// A blank rewrite keeps worker, tokens, and cost, and does not add a row.
	if _, err := st.UpsertAttempt(ctx, store.Attempt{
		TicketID: tk.ID, TaskID: taskID, Attempt: 1, Status: store.AttemptReview,
	}); err != nil {
		t.Fatal(err)
	}
	// A later lower number does not lower tickets.attempt.
	if _, err := st.UpsertAttempt(ctx, store.Attempt{
		TicketID: tk.ID, TaskID: taskID, Attempt: 0, Status: store.AttemptFailed,
		FailureCategory: protocol.ErrKindVerify,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListAttempts(ctx, tk.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Attempt != 1 || rows[1].Attempt != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	if rows[0].WorkerID != workerID || rows[0].TokensIn != 120 || rows[0].CostUSD == nil || *rows[0].CostUSD != 1.5 {
		t.Fatalf("merge wiped attempt 1: %+v", rows[0])
	}
	if rows[0].GitJSON == "" || rows[1].Status != store.AttemptFailed || rows[1].FailureCategory != protocol.ErrKindVerify {
		t.Fatalf("rows: %+v", rows)
	}
	gotTK, _ = st.GetTicket(ctx, tk.ID)
	if gotTK.Attempt != 2 {
		t.Fatalf("ticket attempt = %d, want 2", gotTK.Attempt)
	}
	// Writing attempt 1 again must not lower the counter.
	if _, err := st.UpsertAttempt(ctx, store.Attempt{
		TicketID: tk.ID, TaskID: taskID, Attempt: 1, Status: store.AttemptReview,
	}); err != nil {
		t.Fatal(err)
	}
	gotTK, _ = st.GetTicket(ctx, tk.ID)
	if gotTK.Attempt != 2 {
		t.Fatalf("ticket attempt lowered to %d", gotTK.Attempt)
	}

	// lost does not replace a finished row, and does not wipe tokens on an open one.
	if _, err := st.UpsertAttempt(ctx, store.Attempt{
		TicketID: tk.ID, TaskID: taskID, Attempt: 1, Status: store.AttemptFailed,
		FailureCategory: protocol.ErrKindLost, ExitCode: intPtr(-1),
	}); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.ListAttempts(ctx, tk.ID, "")
	if rows[0].Status != store.AttemptReview || rows[0].FailureCategory != "" || rows[0].TokensIn != 120 {
		t.Fatalf("lost clobbered finished row: %+v", rows[0])
	}
	openCost := 0.25
	if _, err := st.UpsertAttempt(ctx, store.Attempt{
		TaskID: adhocID, Attempt: 1, WorkerID: workerID, Status: "",
		TokensIn: 9, TokensOut: 3, CostUSD: &openCost,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertAttempt(ctx, store.Attempt{
		TaskID: adhocID, Attempt: 1, Status: store.AttemptFailed,
		FailureCategory: protocol.ErrKindLost, ExitCode: intPtr(-1), EndedAt: &ended,
	}); err != nil {
		t.Fatal(err)
	}
	adhoc, err := st.ListAttempts(ctx, "", adhocID)
	if err != nil || len(adhoc) != 1 {
		t.Fatalf("adhoc: %+v err=%v", adhoc, err)
	}
	if adhoc[0].TicketID != "" || adhoc[0].Status != store.AttemptFailed || adhoc[0].FailureCategory != protocol.ErrKindLost {
		t.Fatalf("adhoc lost: %+v", adhoc[0])
	}
	if adhoc[0].TokensIn != 9 || adhoc[0].CostUSD == nil || *adhoc[0].CostUSD != 0.25 || adhoc[0].WorkerID != workerID {
		t.Fatalf("lost wiped usage: %+v", adhoc[0])
	}
	// Unknown cost stays NULL.
	if _, err := st.UpsertAttempt(ctx, store.Attempt{TaskID: adhocID, Attempt: 2, Status: store.AttemptCompleted}); err != nil {
		t.Fatal(err)
	}
	adhoc, _ = st.ListAttempts(ctx, "", adhocID)
	if len(adhoc) != 2 || adhoc[1].CostUSD != nil || adhoc[1].TokensIn != 0 {
		t.Fatalf("nil cost: %+v", adhoc)
	}
	// Ticket list does not include the ad-hoc task.
	if ticketRows, _ := st.ListAttempts(ctx, tk.ID, ""); len(ticketRows) != 2 {
		t.Fatalf("ticket list = %d", len(ticketRows))
	}
	if _, err := st.ListAttempts(ctx, "", ""); !errors.Is(err, store.ErrInvalidWork) {
		t.Fatalf("missing id err = %v", err)
	}
}
