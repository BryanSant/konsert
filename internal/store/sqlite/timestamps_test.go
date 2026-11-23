package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

func openTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "konsert.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestFormatTimeFixedWidthAndSortable(t *testing.T) {
	base := time.Date(2026, 11, 22, 12, 0, 5, 0, time.UTC)
	whole := formatTime(base)
	half := formatTime(base.Add(500 * time.Millisecond))
	if len(whole) != 30 || len(half) != 30 {
		t.Fatalf("expected fixed width 30, got %q (%d) %q (%d)", whole, len(whole), half, len(half))
	}
	if !(whole < half) {
		t.Fatalf("expected %q < %q as text", whole, half)
	}
	// Non-UTC input is normalized to UTC.
	if got := formatTime(base.In(time.FixedZone("x", 3600))); got != whole {
		t.Fatalf("non-UTC not normalized: %q vs %q", got, whole)
	}
	// Reads accept both new and legacy formats.
	for _, v := range []string{whole, base.Format(time.RFC3339Nano), base.Format(time.RFC3339), "2026-11-22T12:00:05.5Z"} {
		if got := parseTime(v); got.IsZero() || got.Truncate(time.Second) != base {
			t.Fatalf("parseTime(%q) = %v", v, got)
		}
	}
}

func TestSameSecondTimestampOrdering(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	base := time.Date(2026, 11, 22, 12, 0, 5, 0, time.UTC)
	// Created in an order where the legacy RFC3339Nano encoding would sort wrongly:
	// "...05Z" vs "...05.5Z" ('.' < 'Z').
	for i, off := range []time.Duration{0, 500 * time.Millisecond, 100 * time.Millisecond} {
		if err := st.CreateTask(ctx, store.Task{
			ID: fmt.Sprintf("t%d", i), Agent: "claude", Workdir: "/x", Prompt: "p",
			CreatedAt: base.Add(off),
		}); err != nil {
			t.Fatal(err)
		}
	}
	tasks, err := st.ListTasks(ctx, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tk := range tasks {
		ids = append(ids, tk.ID)
	}
	if strings.Join(ids, ",") != "t1,t2,t0" {
		t.Fatalf("expected newest-first t1,t2,t0; got %v", ids)
	}

	// Stale check within the same second: last seen at :05.000, cutoff at :05.500.
	if err := st.UpsertWorker(ctx, store.Worker{ID: "w", Name: "w", HostGroup: "g", OS: "linux", Arch: "amd64", Status: "online", LastSeenAt: base}); err != nil {
		t.Fatal(err)
	}
	stale, err := st.MarkStaleWorkersOffline(ctx, base.Add(500*time.Millisecond))
	if err != nil || len(stale) != 1 {
		t.Fatalf("expected worker reaped within same second, got %v err=%v", stale, err)
	}
}

func TestMarkStaleWorkersOfflineSemantics(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	now := time.Now().UTC()
	for _, w := range []store.Worker{
		{ID: "fresh", Status: "online", LastSeenAt: now},
		{ID: "stale", Status: "online", LastSeenAt: now.Add(-time.Hour)},
		{ID: "already-off", Status: "offline", LastSeenAt: now.Add(-time.Hour)},
	} {
		w.Name, w.HostGroup, w.OS, w.Arch = w.ID, w.ID, "linux", "amd64"
		if err := st.UpsertWorker(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	stale, err := st.MarkStaleWorkersOffline(ctx, now.Add(-time.Minute))
	if err != nil || len(stale) != 1 || stale[0] != "stale" {
		t.Fatalf("expected only [stale], got %v err=%v", stale, err)
	}
	stale, err = st.MarkStaleWorkersOffline(ctx, now.Add(-time.Minute))
	if err != nil || len(stale) != 0 {
		t.Fatalf("second reap should be empty, got %v err=%v", stale, err)
	}
}

// TestMarkStaleWorkersOfflineRaceWithHeartbeat races a heartbeat (which only
// refreshes last_seen_at, as a real heartbeat does) against the reaper. With the
// old SELECT-then-UPDATE implementation a heartbeat landing between the two
// statements made the reaper report a worker it did not actually take offline.
// The invariant: an ID is returned if and only if that worker ended up offline.
func TestMarkStaleWorkersOfflineRaceWithHeartbeat(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	base := time.Now().UTC()
	if err := st.UpsertWorker(ctx, store.Worker{ID: "w", Name: "w", HostGroup: "g", OS: "linux", Arch: "amd64", Status: "online", LastSeenAt: base.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cutoff := base.Add(-time.Minute)
	for i := 0; i < 300; i++ {
		if _, err := st.db.ExecContext(ctx, `UPDATE workers SET status='online', last_seen_at=? WHERE id='w'`, formatTime(base.Add(-time.Hour))); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var reaped []string
		var reapErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			reaped, reapErr = st.MarkStaleWorkersOffline(ctx, cutoff)
		}()
		go func() {
			defer wg.Done()
			_, _ = st.db.ExecContext(ctx, `UPDATE workers SET last_seen_at=? WHERE id='w'`, formatTime(base))
		}()
		wg.Wait()
		if reapErr != nil {
			t.Fatal(reapErr)
		}
		w, err := st.GetWorker(ctx, "w")
		if err != nil {
			t.Fatal(err)
		}
		if (len(reaped) == 1) != (w.Status == "offline") {
			t.Fatalf("iteration %d: reaped=%v but final status=%s (live worker reported as reaped)", i, reaped, w.Status)
		}
	}
}

// applyMigrationsUpTo applies embedded migrations whose name sorts <= last,
// emulating a database created by an older binary.
func applyMigrationsUpTo(t *testing.T, db *sql.DB, last string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		if n > last {
			break
		}
		b, err := fs.ReadFile(migrationsFS, "migrations/"+n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", n, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES (?, ?)`, n, "2025-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrationUpgradeFrom007NormalizesTimestamps(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	applyMigrationsUpTo(t, raw, "007_quotas_and_external_sessions.sql")

	// Legacy rows written with time.RFC3339Nano.
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	legacy := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339Nano) }
	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO workers (id,name,host_group,os,arch,agents_json,tags_json,status,last_seen_at,facts_updated_at) VALUES ('w1','w1','g','linux','amd64','[]','{}','online',?,?)`, []any{legacy(0), legacy(120 * time.Millisecond)}},
		{`INSERT INTO tasks (id,worker_id,agent,workdir,prompt,status,created_at,started_at,completed_at) VALUES ('a','w1','claude','/x','p','completed',?,?,?)`, []any{legacy(0), legacy(time.Second), legacy(2*time.Second + 5*time.Millisecond)}},
		{`INSERT INTO tasks (id,worker_id,agent,workdir,prompt,status,created_at) VALUES ('b','w1','claude','/x','p','queued',?)`, []any{legacy(500 * time.Millisecond)}},
		{`INSERT INTO chat_sessions (id,title,provider,model,created_at,updated_at) VALUES ('s1','t','openai','m',?,?)`, []any{legacy(0), legacy(0)}},
		{`INSERT INTO chat_sessions (id,title,provider,model,created_at,updated_at) VALUES ('s2','t','openai','m',?,?)`, []any{legacy(0), legacy(250 * time.Millisecond)}},
		{`INSERT INTO chat_messages (session_id,role,content,created_at) VALUES ('s1','user','hi',?)`, []any{legacy(123456789 * time.Nanosecond)}},
		{`INSERT INTO subscription_quotas (id,provider,label,reset_at,updated_at) VALUES ('q','p','l',NULL,?)`, []any{legacy(0)}},
		{`INSERT INTO settings (key,value,updated_at) VALUES ('k','not-a-time',?)`, []any{"garbage"}},
	}
	for _, s := range stmts {
		if _, err := raw.ExecContext(ctx, s.q, s.args...); err != nil {
			t.Fatalf("%s: %v", s.q, err)
		}
	}
	_ = raw.Close()

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (upgrade): %v", err)
	}
	defer st.Close()

	check := func(q, want string) {
		t.Helper()
		var got sql.NullString
		if err := st.db.QueryRowContext(ctx, q).Scan(&got); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got.String != want {
			t.Fatalf("%s = %q, want %q", q, got.String, want)
		}
	}
	check(`SELECT last_seen_at FROM workers WHERE id='w1'`, "2026-03-04T05:06:07.000000000Z")
	check(`SELECT facts_updated_at FROM workers WHERE id='w1'`, "2026-03-04T05:06:07.120000000Z")
	check(`SELECT created_at FROM tasks WHERE id='a'`, "2026-03-04T05:06:07.000000000Z")
	check(`SELECT started_at FROM tasks WHERE id='a'`, "2026-03-04T05:06:08.000000000Z")
	check(`SELECT completed_at FROM tasks WHERE id='a'`, "2026-03-04T05:06:09.005000000Z")
	check(`SELECT created_at FROM tasks WHERE id='b'`, "2026-03-04T05:06:07.500000000Z")
	check(`SELECT created_at FROM chat_messages WHERE session_id='s1'`, "2026-03-04T05:06:07.123456789Z")
	check(`SELECT reset_at FROM subscription_quotas WHERE id='q'`, "")
	check(`SELECT updated_at FROM settings WHERE key='k'`, "garbage") // non-timestamps untouched
	check(`SELECT created_at FROM workspaces WHERE id='space-default'`, "1970-01-01T00:00:00.000000000Z")

	// Existing data is readable and now orders correctly.
	tasks, err := st.ListTasks(ctx, "", "", 10)
	if err != nil || len(tasks) != 2 || tasks[0].ID != "b" || tasks[1].ID != "a" {
		t.Fatalf("expected [b a] after upgrade, got %+v err=%v", tasks, err)
	}
	if tasks[1].CompletedAt == nil || !tasks[1].CompletedAt.Equal(base.Add(2*time.Second+5*time.Millisecond)) {
		t.Fatalf("completed_at mis-parsed: %v", tasks[1].CompletedAt)
	}
	sessions, err := st.ListChatSessions(ctx, 10)
	if err != nil || len(sessions) != 2 || sessions[0].ID != "s2" {
		t.Fatalf("expected s2 first, got %+v err=%v", sessions, err)
	}
	if err := st.CreateTask(ctx, store.Task{ID: "c", Agent: "claude", Workdir: "/x", Prompt: "p", Status: protocol.TaskStatusQueued, CreatedAt: base.Add(600 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	tasks, _ = st.ListTasks(ctx, "", "", 10)
	if len(tasks) != 3 || tasks[0].ID != "c" || tasks[1].ID != "b" {
		t.Fatalf("new rows must interleave correctly with migrated ones, got %v", tasks)
	}
}
