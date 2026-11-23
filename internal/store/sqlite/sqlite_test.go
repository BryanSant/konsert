package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

func TestSQLiteStoreCRUDAndWAL(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "konsert.db")

	st, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer st.Close()

	// Workers
	now := time.Now().UTC()
	w1 := store.Worker{
		ID:             "w1",
		Name:           "fedora-podman-1",
		HostGroup:      "fedora-main",
		OS:             "linux",
		Arch:           "amd64",
		Isolation:      "container",
		PermissionMode: store.PermissionFullAccess,
		Agents:         []string{"claude", "agy"},
		Tags:           map[string]string{"env": "podman"},
		MaxConcurrent:  4,
		Status:         "online",
		LastSeenAt:     now.Add(-60 * time.Second),
	}
	if err := st.UpsertWorker(ctx, w1); err != nil {
		t.Fatalf("UpsertWorker failed: %v", err)
	}
	gotW, err := st.GetWorker(ctx, "w1")
	if err != nil || gotW.Name != "fedora-podman-1" || len(gotW.Agents) != 2 || gotW.Tags["env"] != "podman" {
		t.Fatalf("GetWorker mismatch: %+v err=%v", gotW, err)
	}

	stale, err := st.MarkStaleWorkersOffline(ctx, now.Add(-30*time.Second))
	if err != nil || len(stale) != 1 || stale[0] != "w1" {
		t.Fatalf("expected w1 marked offline, got stale=%v err=%v", stale, err)
	}
	gotW, _ = st.GetWorker(ctx, "w1")
	if gotW.Status != "offline" {
		t.Fatalf("expected status offline, got %s", gotW.Status)
	}

	// Spaces & task space_id linkage
	sp := store.Space{
		ID:       "sp-1",
		WorkerID: "w1",
		Name:     "konsert",
		Path:     "/repo",
		Branch:   "main",
	}
	if err := st.CreateSpace(ctx, sp); err != nil {
		t.Fatalf("CreateSpace failed: %v", err)
	}
	gotSp, err := st.GetSpace(ctx, "sp-1")
	if err != nil || gotSp.Name != "konsert" || gotSp.Path != "/repo" || gotSp.WorkerID != "w1" {
		t.Fatalf("GetSpace mismatch: %+v err=%v", gotSp, err)
	}
	allSp, err := st.ListSpaces(ctx, "w1")
	if err != nil || len(allSp) != 1 {
		t.Fatalf("ListSpaces mismatch: %+v err=%v", allSp, err)
	}

	// Tasks & Events
	task := store.Task{
		ID:             "t1",
		WorkerID:       "w1",
		SpaceID:        "sp-1",
		Agent:          "claude",
		Workdir:        "/repo",
		Branch:         "feat/x",
		Mode:           protocol.TaskModeOneshot,
		PermissionMode: store.PermissionFullAccess,
		Prompt:         "implement feature x",
		Status:         protocol.TaskStatusQueued,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}
	if err := st.UpdateTaskAssignment(ctx, "t1", "w1", "/repo/.konsert-worktrees/t1"); err != nil {
		t.Fatalf("UpdateTaskAssignment failed: %v", err)
	}
	if err := st.UpdateTaskStatus(ctx, "t1", protocol.TaskStatusRunning, "", "", nil); err != nil {
		t.Fatalf("UpdateTaskStatus running failed: %v", err)
	}
	ec := 0
	if err := st.UpdateTaskStatus(ctx, "t1", protocol.TaskStatusCompleted, "done!", `{"diff_stat":"1 file changed"}`, &ec); err != nil {
		t.Fatalf("UpdateTaskStatus completed failed: %v", err)
	}
	gotT, err := st.GetTask(ctx, "t1")
	if err != nil || gotT.Status != protocol.TaskStatusCompleted || gotT.SpaceID != "sp-1" || gotT.ExitCode == nil || *gotT.ExitCode != 0 || gotT.StartedAt == nil || gotT.CompletedAt == nil {
		t.Fatalf("GetTask mismatch: %+v err=%v", gotT, err)
	}

	if err := st.DeleteSpace(ctx, "sp-1"); err != nil {
		t.Fatalf("DeleteSpace failed: %v", err)
	}
	if _, err := st.GetSpace(ctx, "sp-1"); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound after DeleteSpace, got %v", err)
	}

	evID, err := st.AppendEvent(ctx, store.TaskEvent{TaskID: "t1", Kind: "milestone", Message: "started"})
	if err != nil || evID <= 0 {
		t.Fatalf("AppendEvent failed: id=%d err=%v", evID, err)
	}
	evs, err := st.ListEvents(ctx, "t1", 0, 10)
	if err != nil || len(evs) != 1 || evs[0].Message != "started" {
		t.Fatalf("ListEvents mismatch: %+v err=%v", evs, err)
	}

	// Chat sessions & messages
	cs := store.ChatSession{ID: "s1", Title: "Default", Provider: "openai", Model: "gpt-4o"}
	if err := st.CreateChatSession(ctx, cs); err != nil {
		t.Fatalf("CreateChatSession failed: %v", err)
	}
	if _, err := st.AppendChatMessage(ctx, store.ChatMessage{SessionID: "s1", Role: "user", Content: "check fleet"}); err != nil {
		t.Fatalf("AppendChatMessage failed: %v", err)
	}
	msgs, err := st.ListChatMessages(ctx, "s1")
	if err != nil || len(msgs) != 1 || msgs[0].Content != "check fleet" {
		t.Fatalf("ListChatMessages mismatch: %+v err=%v", msgs, err)
	}

	// DeleteWorker cascades spaces and unlinks tasks without deleting task history.
	if err := st.CreateSpace(ctx, store.Space{ID: "sp-del", WorkerID: "w1", Name: "repo", Path: "/repo"}); err != nil {
		t.Fatalf("CreateSpace for delete test failed: %v", err)
	}
	if err := st.DeleteWorker(ctx, "w1"); err != nil {
		t.Fatalf("DeleteWorker failed: %v", err)
	}
	if _, err := st.GetWorker(ctx, "w1"); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound after DeleteWorker, got %v", err)
	}
	if _, err := st.GetSpace(ctx, "sp-del"); err != store.ErrNotFound {
		t.Fatalf("expected worker space deleted after DeleteWorker, got %v", err)
	}
	gotTAfter, err := st.GetTask(ctx, "t1")
	if err != nil || gotTAfter.WorkerID != "" {
		t.Fatalf("expected task preserved with empty worker_id, got %+v err=%v", gotTAfter, err)
	}
}
