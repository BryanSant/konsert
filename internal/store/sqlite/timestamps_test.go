package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
