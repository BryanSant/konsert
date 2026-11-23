package sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/store"
)

func seedAudit(t *testing.T, st *SQLiteStore, e store.AuditEvent) int64 {
	t.Helper()
	id, err := st.AppendAudit(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAuditListFiltersAndPaging(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		seedAudit(t, st, store.AuditEvent{
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			User:      []string{"alice", "bob", "token"}[i%3],
			Action:    []string{"task.steer", "task.cancel"}[i%2],
			WorkerID:  fmt.Sprintf("w%d", i%2),
			TaskID:    fmt.Sprintf("t%d", i%5),
			Message:   fmt.Sprintf("event %d", i),
		})
	}

	all, err := st.ListAudit(ctx, store.AuditFilter{Limit: 500})
	if err != nil || len(all) != 30 {
		t.Fatalf("list all = %d, %v", len(all), err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID >= all[i-1].ID {
			t.Fatalf("not newest first: %d then %d", all[i-1].ID, all[i].ID)
		}
	}

	cases := []struct {
		name string
		f    store.AuditFilter
		want int
	}{
		{"user", store.AuditFilter{User: "alice"}, 10},
		{"worker", store.AuditFilter{WorkerID: "w1"}, 15},
		{"task", store.AuditFilter{TaskID: "t3"}, 6},
		{"action", store.AuditFilter{Action: "task.cancel"}, 15},
		{"combined", store.AuditFilter{User: "alice", Action: "task.steer"}, 5},
		{"since", store.AuditFilter{Since: base.Add(20 * time.Minute)}, 10},
		{"until", store.AuditFilter{Until: base.Add(9 * time.Minute)}, 10},
		{"window", store.AuditFilter{Since: base.Add(10 * time.Minute), Until: base.Add(14 * time.Minute)}, 5},
		{"after", store.AuditFilter{AfterID: all[5].ID}, 5},
	}
	for _, c := range cases {
		got, err := st.ListAudit(ctx, c.f)
		if err != nil || len(got) != c.want {
			t.Errorf("%s: got %d rows (want %d), err=%v", c.name, len(got), c.want, err)
		}
	}

	// Paging with BeforeID walks every row exactly once.
	seen := map[int64]bool{}
	var before int64
	for page := 0; page < 10; page++ {
		got, err := st.ListAudit(ctx, store.AuditFilter{Limit: 12, BeforeID: before})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			break
		}
		for _, e := range got {
			if seen[e.ID] {
				t.Fatalf("row %d returned twice", e.ID)
			}
			seen[e.ID] = true
		}
		before = got[len(got)-1].ID
	}
	if len(seen) != 30 {
		t.Fatalf("paging saw %d rows, want 30", len(seen))
	}

	// Limits are clamped.
	got, _ := st.ListAudit(ctx, store.AuditFilter{})
	if len(got) != 30 {
		t.Fatalf("default limit returned %d", len(got))
	}
}

func TestAuditPruneByAgeAndCount(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	now := time.Now().UTC()
	// Bypass AppendAudit's inline pruning for the old rows.
	for i := 0; i < 5; i++ {
		if _, err := st.db.ExecContext(ctx,
			`INSERT INTO audit_log (created_at, actor, action) VALUES (?, 'old', 'task.steer')`,
			formatTime(now.Add(-AuditRetention-time.Duration(i+1)*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := st.db.ExecContext(ctx,
			`INSERT INTO audit_log (created_at, actor, action) VALUES (?, 'new', 'task.steer')`,
			formatTime(now.Add(-time.Duration(i)*time.Minute))); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.PruneAudit(ctx, now.Add(-AuditRetention), 0)
	if err != nil || n != 5 {
		t.Fatalf("age prune deleted %d, err=%v", n, err)
	}
	n, err = st.PruneAudit(ctx, time.Time{}, 4)
	if err != nil || n != 6 {
		t.Fatalf("count prune deleted %d, err=%v", n, err)
	}
	left, _ := st.ListAudit(ctx, store.AuditFilter{Limit: 100})
	if len(left) != 4 {
		t.Fatalf("%d rows left, want 4", len(left))
	}
	// Pruning below the cap is a no-op.
	if n, err := st.PruneAudit(ctx, time.Time{}, 4); err != nil || n != 0 {
		t.Fatalf("idempotent prune deleted %d, err=%v", n, err)
	}
}

func TestAuditAppendPrunesExpiredRowsInline(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO audit_log (created_at, actor, action) VALUES (?, 'old', 'task.steer')`,
		formatTime(time.Now().UTC().Add(-AuditRetention-time.Hour))); err != nil {
		t.Fatal(err)
	}
	// The first AppendAudit after Open runs a retention pass.
	seedAudit(t, st, store.AuditEvent{User: "alice", Action: "task.cancel"})
	rows, _ := st.ListAudit(ctx, store.AuditFilter{})
	if len(rows) != 1 || rows[0].User != "alice" {
		t.Fatalf("expired row survived the inline prune: %+v", rows)
	}
	// An empty user is stored as "unknown" rather than blank.
	seedAudit(t, st, store.AuditEvent{Action: "task.cancel"})
	rows, _ = st.ListAudit(ctx, store.AuditFilter{User: "unknown"})
	if len(rows) != 1 {
		t.Fatalf("blank user not normalized: %+v", rows)
	}
}

func TestAuditQueriesUseIndexes(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	for _, q := range []string{
		`SELECT id, created_at, actor, source, action, worker_id, task_id, message, meta_json FROM audit_log WHERE actor = ? ORDER BY id DESC LIMIT 10`,
		`SELECT id, created_at, actor, source, action, worker_id, task_id, message, meta_json FROM audit_log WHERE worker_id = ? ORDER BY id DESC LIMIT 10`,
		`SELECT id, created_at, actor, source, action, worker_id, task_id, message, meta_json FROM audit_log WHERE task_id = ? ORDER BY id DESC LIMIT 10`,
		`SELECT id, created_at, actor, source, action, worker_id, task_id, message, meta_json FROM audit_log WHERE action = ? ORDER BY id DESC LIMIT 10`,
	} {
		var detail string
		rows, err := st.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, "x")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, notused int
			var d string
			if err := rows.Scan(&id, &parent, &notused, &d); err != nil {
				t.Fatal(err)
			}
			detail += d + " | "
		}
		_ = rows.Close()
		if !strings.Contains(detail, "INDEX idx_audit_log_") || strings.Contains(detail, "TEMP B-TREE") {
			t.Errorf("%s: plan %s", q, detail)
		}
	}
}
