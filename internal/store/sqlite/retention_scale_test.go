package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/store"
)

// TestRetentionScale is a manual scale check, skipped unless KONSERT_RETENTION_SCALE_DIR
// names a directory for the database (it is large: use /var/tmp, not a RAM-backed /tmp).
// It builds KONSERT_RETENTION_SCALE_SESSIONS (default 2000) finished chat sessions dated
// 60 days ago with a realistic event mix, runs one full compaction pass the way the hub
// does (500-row batches, then 1024-page incremental vacuum steps) and reports the file
// size before and after, the pass duration and the longest single write transaction.
func TestRetentionScale(t *testing.T) {
	dir := os.Getenv("KONSERT_RETENTION_SCALE_DIR")
	if dir == "" {
		t.Skip("set KONSERT_RETENTION_SCALE_DIR to run the retention scale check")
	}
	sessions := 2000
	if v, err := strconv.Atoi(os.Getenv("KONSERT_RETENTION_SCALE_SESSIONS")); err == nil && v > 0 {
		sessions = v
	}
	ctx := context.Background()
	path := filepath.Join(dir, "retention-scale.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// One turn: the prompt, status changes, reasoning, a plan, tool calls with inputs and
	// outputs (the bulk), the reply, usage, rate limit and the turn end.
	toolOut := strings.Repeat("ok  \tgithub.com/example/pkg\t0.42s\n", 40) // ~1.4 KB
	thought := strings.Repeat("Considering the next step carefully. ", 20)
	reply := strings.Repeat("Here is what I changed and why. ", 15)
	type ev struct{ kind, json string }
	turn := []ev{
		{"user_message", `{"kind":"user_message","text":"Please fix the failing tests in the scheduler package"}`},
		{"status", `{"kind":"status","status":"working"}`},
		{"thought_delta", `{"kind":"thought_delta","text":"` + thought + `"}`},
		{"plan", `{"kind":"plan","plan":[{"text":"Run tests","status":"completed"},{"text":"Fix","status":"in_progress"}]}`},
	}
	for i := range 6 {
		turn = append(turn, ev{"tool_call", fmt.Sprintf(`{"kind":"tool_call","tool_call":{"id":"tc%d","name":"Bash","title":"Run: go test ./...","status":"completed","input":"{\"command\":\"go test ./internal/scheduler/...\"}","output":%q}}`, i, toolOut)})
	}
	turn = append(turn,
		ev{"message", `{"kind":"message","text":"` + reply + `"}`},
		ev{"usage", `{"kind":"usage","usage":{"input_tokens":12000,"output_tokens":800}}`},
		ev{"rate_limit", `{"kind":"rate_limit","rate_limit":{"window":"five_hour","used_percent":42}}`},
		ev{"status", `{"kind":"status","status":"done"}`},
		ev{"turn_end", `{"kind":"turn_end","stop_reason":"end_turn"}`},
	)
	turnsPerSession := 12 // 15 events a turn: 180 events, ~10 KB of tool output a turn
	old := time.Now().UTC().Add(-60 * 24 * time.Hour)
	ts := formatTime(old)

	gen := time.Now()
	for s := range sessions {
		tx, err := st.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		id := ids.UUID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_sessions (id, sid, title, provider, model, status, compact_next_at, created_at, updated_at)
			VALUES (?, ?, 'scale', 'claude', 'm', 'done', ?, ?, ?)`, id, fmt.Sprintf("c-s%06d", s), ts, ts, ts); err != nil {
			t.Fatal(err)
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO chat_events (chat_id, seq, turn_id, kind, json, created_at) VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			t.Fatal(err)
		}
		seq := 0
		for tn := range turnsPerSession {
			for _, e := range turn {
				seq++
				if _, err := stmt.ExecContext(ctx, id, seq, fmt.Sprintf("t%d", tn), e.kind, e.json, ts); err != nil {
					t.Fatal(err)
				}
			}
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CheckpointWAL(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := st.StorageStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var events int64
	_ = st.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM chat_events`).Scan(&events)
	t.Logf("generated %d sessions, %d chat events in %v; file %.1f MB, WAL %.1f MB",
		sessions, events, time.Since(gen).Round(time.Millisecond), mb(before.FileBytes), mb(before.WALBytes))

	start := time.Now()
	var longest time.Duration
	var longestWhat string
	var batches int
	var rows int64
	for _, step := range store.CompactSteps {
		for {
			t0 := time.Now()
			res, err := st.CompactHistory(ctx, step, time.Now(), store.DefaultRetention, 500)
			if d := time.Since(t0); d > longest {
				longest, longestWhat = d, step+" batch"
			}
			if err != nil {
				t.Fatal(err)
			}
			batches++
			rows += res.Rows
			if res.Done {
				break
			}
		}
	}
	compacted := time.Since(start)
	var freed int64
	vacuumSteps := 0
	for {
		t0 := time.Now()
		f, left, err := st.IncrementalVacuum(ctx, 1024)
		if d := time.Since(t0); d > longest {
			longest, longestWhat = d, "vacuum step"
		}
		if err != nil {
			t.Fatal(err)
		}
		vacuumSteps++
		freed += f
		if left == 0 || f == 0 {
			break
		}
	}
	t0 := time.Now()
	if err := st.CheckpointWAL(ctx); err != nil {
		t.Fatal(err)
	}
	checkpoint := time.Since(t0)
	total := time.Since(start)
	after, err := st.StorageStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM chat_events`).Scan(&events)
	t.Logf("compaction: %d rows in %d batches in %v; vacuum %d steps freed %.1f MB; checkpoint %v; pass total %v",
		rows, batches, compacted.Round(time.Millisecond), vacuumSteps, mb(freed), checkpoint.Round(time.Millisecond), total.Round(time.Millisecond))
	t.Logf("longest single write transaction: %v (%s)", longest.Round(time.Microsecond), longestWhat)
	t.Logf("after: file %.1f MB, WAL %.1f MB, %d chat events left", mb(after.FileBytes), mb(after.WALBytes), events)
	for _, k := range after.Breakdown {
		t.Logf("  %-30s %8.1f MB", k.Label, mb(k.Bytes))
	}
}

func mb(n int64) float64 { return float64(n) / (1 << 20) }
