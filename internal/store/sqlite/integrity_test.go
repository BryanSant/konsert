package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/store"
)

func mustNoDanglingTaskRefs(t *testing.T, st *SQLiteStore) {
	t.Helper()
	var n int
	err := st.db.QueryRow(`
		SELECT COUNT(1) FROM tasks
		WHERE (space_id != '' AND space_id NOT IN (SELECT id FROM spaces))
		   OR (repo_id != '' AND repo_id NOT IN (SELECT id FROM repos))
		   OR (worktree_id != '' AND worktree_id NOT IN (SELECT id FROM worktrees))
		   OR (worker_id IS NOT NULL AND worker_id NOT IN (SELECT id FROM workers))
	`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d task(s) have dangling references", n)
	}
}

func TestDeleteWorkerClearsTaskReferences(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	if err := st.UpsertWorker(ctx, store.Worker{ID: "w1", Name: "w1", HostGroup: "g", OS: "linux", Arch: "amd64", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSpace(ctx, store.Space{ID: "sp1", WorkerID: "w1", Name: "s", Path: "/s"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRepo(ctx, store.Repo{ID: "r1", ProjectID: store.DefaultProjectID, WorkerID: "w1", Name: "r", Path: "/r"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorktree(ctx, store.Worktree{ID: "wt1", RepoID: "r1", Path: "/r/wt"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, store.Task{ID: "t1", WorkerID: "w1", SpaceID: "sp1", RepoID: "r1", WorktreeID: "wt1", Agent: "claude", Workdir: "/r", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	// A task that references the worktree without a repo_id.
	if err := st.CreateTask(ctx, store.Task{ID: "t2", WorktreeID: "wt1", Agent: "claude", Workdir: "/r", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteWorker(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	mustNoDanglingTaskRefs(t, st)
	got, err := st.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkerID != "" || got.SpaceID != "" || got.RepoID != "" || got.WorktreeID != "" {
		t.Fatalf("task refs not cleared: %+v", got)
	}
}

func TestDeleteSpaceClearsTaskReference(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	if err := st.UpsertWorker(ctx, store.Worker{ID: "w1", Name: "w1", HostGroup: "g", OS: "linux", Arch: "amd64", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSpace(ctx, store.Space{ID: "sp1", WorkerID: "w1", Name: "s", Path: "/s"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, store.Task{ID: "t1", SpaceID: "sp1", Agent: "claude", Workdir: "/s", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSpace(ctx, "sp1"); err != nil {
		t.Fatal(err)
	}
	mustNoDanglingTaskRefs(t, st)
	if err := st.DeleteSpace(ctx, "sp1"); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDeduplicateWorkersRepointsTasksFromDuplicateSpaces(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dedup.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, w := range []store.Worker{
		{ID: "old", Status: "offline", LastSeenAt: now.Add(-time.Hour)},
		{ID: "new", Status: "online", LastSeenAt: now},
	} {
		w.Name, w.HostGroup, w.OS, w.Arch, w.Isolation = "mac", "mac", "darwin", "arm64", "host"
		if err := st.UpsertWorker(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	// The same folder saved on both duplicate rows; the task points at the copy
	// that will be removed after both collapse onto the winner.
	if err := st.CreateSpace(ctx, store.Space{ID: "sp-new", WorkerID: "new", Name: "jk", Path: "/jk", CreatedAt: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSpace(ctx, store.Space{ID: "sp-old", WorkerID: "old", Name: "jk", Path: "/jk", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, store.Task{ID: "t", WorkerID: "old", SpaceID: "sp-old", Agent: "claude", Workdir: "/jk", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}

	removed, err := st.DeduplicateWorkers(ctx)
	if err != nil || len(removed) != 1 || removed[0] != "old" {
		t.Fatalf("dedup removed=%v err=%v", removed, err)
	}
	mustNoDanglingTaskRefs(t, st)
	spaces, _ := st.ListSpaces(ctx, "")
	if len(spaces) != 1 {
		t.Fatalf("expected 1 surviving space, got %+v", spaces)
	}
	got, _ := st.GetTask(ctx, "t")
	if got.WorkerID != "new" || got.SpaceID != spaces[0].ID {
		t.Fatalf("task not repointed: %+v (surviving space %s)", got, spaces[0].ID)
	}
	_ = st.Close()
}

func TestAppendChatMessageIsAtomic(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	if err := st.CreateChatSession(ctx, store.ChatSession{ID: "s1", Title: "t", Provider: "openai", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	// Happy path: message stored and session bumped.
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := st.AppendChatMessage(ctx, store.ChatMessage{SessionID: "s1", Role: "user", Content: "hi", CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	cs, _ := st.GetChatSession(ctx, "s1")
	if !cs.UpdatedAt.Equal(at) {
		t.Fatalf("session updated_at not bumped: %v", cs.UpdatedAt)
	}

	// Make the session bump fail: the message insert must roll back with it.
	if _, err := st.db.ExecContext(ctx, `
		CREATE TRIGGER fail_bump BEFORE UPDATE OF updated_at ON chat_sessions
		BEGIN SELECT RAISE(ABORT, 'boom'); END
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendChatMessage(ctx, store.ChatMessage{SessionID: "s1", Role: "user", Content: "lost"}); err == nil {
		t.Fatal("expected error when the session update fails")
	}
	msgs, err := st.ListChatMessages(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "hi" {
		t.Fatalf("insert was not rolled back: %+v", msgs)
	}
}
