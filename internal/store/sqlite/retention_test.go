package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// retentionNow is the fixed clock of the retention tests.
var retentionNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func daysAgo(d float64) time.Time {
	return retentionNow.Add(-time.Duration(d * float64(24*time.Hour)))
}

// compactAll runs step to completion with batches of limit, checking every batch stays
// within its bound, and returns the total rows and items.
func compactAll(t *testing.T, st *SQLiteStore, step string, p store.RetentionPolicy, limit int) (rows int64, items int) {
	t.Helper()
	for i := 0; ; i++ {
		if i > 10000 {
			t.Fatalf("%s: compaction did not finish", step)
		}
		res, err := st.CompactHistory(context.Background(), step, retentionNow, p, limit)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if res.Rows > int64(limit) {
			t.Fatalf("%s: batch deleted %d rows, limit %d", step, res.Rows, limit)
		}
		rows += res.Rows
		items += res.Items
		if res.Done {
			return rows, items
		}
	}
}

func taskKinds(t *testing.T, st *SQLiteStore, id string) []string {
	t.Helper()
	evs, err := st.ListEvents(context.Background(), id, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func chatKinds(t *testing.T, st *SQLiteStore, id string) []string {
	t.Helper()
	evs, err := st.ListChatEvents(context.Background(), id, 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

var verboseTaskKinds = []string{"milestone", "tool_call", "file_edit", "tool_call", "usage", "error", "control", "exit"}

func seedTask(t *testing.T, st *SQLiteStore, status string, completed *time.Time, events []string) string {
	t.Helper()
	ctx := context.Background()
	tk := store.Task{ID: ids.UUID(), Agent: "claude", Workdir: "/w", Prompt: "p", Status: status, CreatedAt: daysAgo(100), CompletedAt: completed}
	if err := st.CreateTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	id := tk.ID
	for i, k := range events {
		if _, err := st.AppendEvent(ctx, store.TaskEvent{TaskID: id, Kind: k, Message: fmt.Sprintf("%s %d", k, i), CreatedAt: daysAgo(99)}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestCompactTasksKeepsMilestonesAndSkipsLiveWork(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	p := store.DefaultRetention
	old, recent := daysAgo(40), daysAgo(20)
	edgeIn, edgeOut := daysAgo(30).Add(-time.Second), daysAgo(30).Add(time.Second)

	finishedOld := seedTask(t, st, protocol.TaskStatusCompleted, &old, verboseTaskKinds)
	failedOld := seedTask(t, st, protocol.TaskStatusFailed, &old, verboseTaskKinds)
	finishedRecent := seedTask(t, st, protocol.TaskStatusCompleted, &recent, verboseTaskKinds)
	running := seedTask(t, st, protocol.TaskStatusRunning, nil, verboseTaskKinds)
	queued := seedTask(t, st, protocol.TaskStatusQueued, nil, verboseTaskKinds)
	// Resumed after finishing long ago: completed_at is old but the task is live again.
	resumed := seedTask(t, st, protocol.TaskStatusRunning, &old, verboseTaskKinds)
	pinned := seedTask(t, st, protocol.TaskStatusCompleted, &old, verboseTaskKinds)
	if err := st.SetTaskKeep(ctx, pinned, true); err != nil {
		t.Fatal(err)
	}
	held := seedTask(t, st, protocol.TaskStatusCanceled, &old, verboseTaskKinds)
	if _, err := st.CreateHold(ctx, store.Hold{ScopeKind: store.HoldScopeSession, TaskID: held, Summary: "held"}); err != nil {
		t.Fatal(err)
	}
	pastBoundary := seedTask(t, st, protocol.TaskStatusCompleted, &edgeIn, verboseTaskKinds)
	beforeBoundary := seedTask(t, st, protocol.TaskStatusCompleted, &edgeOut, verboseTaskKinds)

	rows, items := compactAll(t, st, store.CompactTasks, p, 2)
	wantKept := []string{"milestone", "error", "control", "exit"}
	for _, id := range []string{finishedOld, failedOld, pastBoundary} {
		if got := taskKinds(t, st, id); !slices.Equal(got, wantKept) {
			t.Errorf("compacted task kinds = %v, want %v", got, wantKept)
		}
		tk, _ := st.GetTask(ctx, id)
		if tk.CompactedAt == nil || !tk.CompactedAt.Equal(retentionNow) {
			t.Errorf("compacted_at = %v, want %v", tk.CompactedAt, retentionNow)
		}
	}
	for name, id := range map[string]string{"recent": finishedRecent, "running": running, "queued": queued,
		"resumed": resumed, "pinned": pinned, "held": held, "inside 30 days": beforeBoundary} {
		if got := taskKinds(t, st, id); !slices.Equal(got, verboseTaskKinds) {
			t.Errorf("%s task lost events: %v", name, got)
		}
		if tk, _ := st.GetTask(ctx, id); tk.CompactedAt != nil {
			t.Errorf("%s task marked compacted", name)
		}
	}
	if rows != 12 || items != 3 {
		t.Fatalf("rows=%d items=%d, want 12 and 3", rows, items)
	}
	// A second pass finds nothing.
	if rows, items := compactAll(t, st, store.CompactTasks, p, 2); rows != 0 || items != 0 {
		t.Fatalf("second pass rows=%d items=%d", rows, items)
	}
	// Running a compacted task again clears the marker, so it is compacted again later.
	if _, err := st.UpdateTaskStatus(ctx, finishedOld, protocol.TaskStatusRunning, "", "", nil, store.Cause{}); err != nil {
		t.Fatal(err)
	}
	if tk, _ := st.GetTask(ctx, finishedOld); tk.CompactedAt != nil {
		t.Fatal("rerun task keeps compacted_at")
	}
}

func appendChat(t *testing.T, st *SQLiteStore, chatID string, seq *int64, at time.Time, kinds ...string) {
	t.Helper()
	for _, k := range kinds {
		*seq++
		raw := fmt.Sprintf(`{"chat_id":%q,"seq":%d,"kind":%q,"text":"payload %s"}`, chatID, *seq, k, strings.Repeat("x", 64))
		if _, err := st.AppendChatEvent(context.Background(), store.ChatEvent{ChatID: chatID, Seq: *seq, TurnID: "t", Kind: k, JSON: raw, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
}

var turnKinds = []string{"user_message", "status", "thought_delta", "tool_call", "tool_call", "plan", "usage", "message", "turn_end"}

func seedChat(t *testing.T, st *SQLiteStore, status string) (string, *int64) {
	t.Helper()
	cs := store.ChatSession{ID: ids.UUID(), Title: "c", Provider: "claude", Model: "m", Status: status, CreatedAt: daysAgo(90)}
	if err := st.CreateChatSession(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	var seq int64
	return cs.ID, &seq
}

func TestCompactChatsKeepsTranscript(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	p := store.DefaultRetention

	idle, seq := seedChat(t, st, protocol.ChatStatusIdle)
	appendChat(t, st, idle, seq, daysAgo(60), turnKinds...)
	appendChat(t, st, idle, seq, daysAgo(45), turnKinds...)
	appendChat(t, st, idle, seq, daysAgo(5), turnKinds...)

	busy, bseq := seedChat(t, st, protocol.ChatStatusWorking)
	appendChat(t, st, busy, bseq, daysAgo(60), turnKinds...)
	pinned, pseq := seedChat(t, st, protocol.ChatStatusDone)
	appendChat(t, st, pinned, pseq, daysAgo(60), turnKinds...)
	if err := st.SetChatKeep(ctx, pinned, true); err != nil {
		t.Fatal(err)
	}
	asking, aseq := seedChat(t, st, protocol.ChatStatusIdle)
	appendChat(t, st, asking, aseq, daysAgo(60), turnKinds...)
	if _, err := st.CreateQuestion(ctx, store.Question{ChatID: asking, Kind: "question", Source: "ask_user", Prompt: "?", Status: "open"}); err != nil {
		t.Fatal(err)
	}

	rows, _ := compactAll(t, st, store.CompactChats, p, 3)
	transcript := []string{"user_message", "message", "turn_end"}
	want := slices.Concat(transcript, transcript, turnKinds)
	if got := chatKinds(t, st, idle); !slices.Equal(got, want) {
		t.Fatalf("idle chat kinds = %v\nwant %v", got, want)
	}
	if rows != 12 {
		t.Fatalf("rows = %d, want 12", rows)
	}
	cs, _ := st.GetChatSession(ctx, idle)
	if cs.CompactedAt == nil || !cs.CompactedAt.Equal(retentionNow) || cs.CompactedBefore == nil || !cs.CompactedBefore.Equal(daysAgo(30)) {
		t.Fatalf("markers = %v / %v", cs.CompactedAt, cs.CompactedBefore)
	}
	for name, id := range map[string]string{"busy": busy, "pinned": pinned, "open question": asking} {
		if got := chatKinds(t, st, id); !slices.Equal(got, turnKinds) {
			t.Errorf("%s chat lost events: %v", name, got)
		}
		if cs, _ := st.GetChatSession(ctx, id); cs.CompactedAt != nil {
			t.Errorf("%s chat marked compacted", name)
		}
	}
	// Nothing more until the recent turn ages past the cutoff.
	if rows, _ := compactAll(t, st, store.CompactChats, p, 3); rows != 0 {
		t.Fatalf("second pass dropped %d rows", rows)
	}
	later := p
	later.Detail = 2 * 24 * time.Hour
	if rows, _ := compactAll(t, st, store.CompactChats, later, 100); rows != 6 {
		t.Fatalf("aged turn: dropped %d rows, want 6", rows)
	}
	if got := chatKinds(t, st, idle); !slices.Equal(got, slices.Concat(transcript, transcript, transcript)) {
		t.Fatalf("after aging kinds = %v", got)
	}
}

func TestCompactFleetTiers(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	p := store.DefaultRetention
	running := seedTask(t, st, protocol.TaskStatusRunning, nil, nil)
	pinnedChat, _ := seedChat(t, st, protocol.ChatStatusIdle)
	_ = st.SetChatKeep(ctx, pinnedChat, true)

	add := func(name string, age float64, e store.FleetEvent) {
		e.CreatedAt = daysAgo(age)
		e.Summary = name
		if _, err := st.AppendFleetEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	sys := store.Cause{Actor: store.ActorSystem}
	usr := store.Cause{Actor: store.ActorUser}
	add("recent routine", 10, store.FleetEvent{Kind: store.EventKindStatus, ToStatus: "running", Cause: sys})
	add("old routine", 100, store.FleetEvent{Kind: store.EventKindStatus, ToStatus: "running", Cause: sys})
	add("old notice", 100, store.FleetEvent{Kind: store.EventKindNotice, Cause: sys})
	add("old user action", 100, store.FleetEvent{Kind: store.EventKindAction, Action: "cancel", Cause: usr})
	add("old failure", 100, store.FleetEvent{Kind: store.EventKindStatus, ToStatus: "failed", Cause: sys})
	add("ancient user action", 400, store.FleetEvent{Kind: store.EventKindAction, Action: "cancel", Cause: usr})
	add("old event of running task", 100, store.FleetEvent{Kind: store.EventKindStatus, TaskID: running, Cause: sys})
	add("ancient event of pinned chat", 400, store.FleetEvent{Kind: store.EventKindStatus, ChatID: pinnedChat, Cause: sys})

	if rows, _ := compactAll(t, st, store.CompactFleet, p, 1); rows != 3 {
		t.Fatalf("rows = %d, want 3", rows)
	}
	evs, _ := st.ListFleetEvents(ctx, store.FleetEventFilter{Limit: 100})
	var got []string
	for _, e := range evs {
		got = append(got, e.Summary)
	}
	slices.Sort(got)
	want := []string{"ancient event of pinned chat", "old event of running task", "old failure", "old user action", "recent routine"}
	if !slices.Equal(got, want) {
		t.Fatalf("kept %v\nwant %v", got, want)
	}
}

func usageTotals(t *testing.T, st *SQLiteStore) (in, out, req int64) {
	t.Helper()
	sum, err := st.GetUsageSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sum.TotalInputTokens, sum.TotalOutputTokens, sum.TotalRequests
}

func TestCompactUsageRollupIsExactAndIdempotentAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "syre.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	p := store.DefaultRetention
	type key struct{ day, provider, model, account string }
	want := map[key][3]int64{}
	n := 0
	for _, age := range []float64{400, 45.2, 45.7, 40, 31.5, 29, 3} {
		for i := range 7 {
			u := store.UsageEvent{Provider: []string{"anthropic", "openai"}[i%2], Model: "m" + fmt.Sprint(i%3), AccountID: "acct" + fmt.Sprint(i%2),
				InputTokens: int64(100 + i), OutputTokens: int64(10 * i), CreatedAt: daysAgo(age)}
			if err := st.RecordUsage(ctx, u); err != nil {
				t.Fatal(err)
			}
			n++
			if age > 30 && age < 365 {
				k := key{u.CreatedAt.Format(time.DateOnly), u.Provider, u.Model, u.AccountID}
				w := want[k]
				want[k] = [3]int64{w[0] + u.InputTokens, w[1] + u.OutputTokens, w[2] + 1}
			}
		}
	}
	in0, out0, req0 := usageTotals(t, st)

	// Fold a few batches, "restart", then finish: totals stay exact.
	for range 2 {
		if _, err := st.CompactHistory(ctx, store.CompactUsage, retentionNow, p, 4); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()
	if st, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	compactAll(t, st, store.CompactUsage, p, 4)
	compactAll(t, st, store.CompactUsage, p, 4) // and again: nothing changes

	in1, out1, req1 := usageTotals(t, st)
	// Only the 400-day-old calls (7, past the daily retention) are gone.
	var gone [3]int64
	for i := range 7 {
		gone[0] += int64(100 + i)
		gone[1] += int64(10 * i)
	}
	if in1 != in0-gone[0] || out1 != out0-gone[1] || req1 != req0-7 {
		t.Fatalf("totals %d/%d/%d, want %d/%d/%d", in1, out1, req1, in0-gone[0], out0-gone[1], req0-7)
	}
	rows, err := st.db.QueryContext(ctx, `SELECT day, provider, model, account_id, input_tokens, output_tokens, requests FROM usage_daily`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[key][3]int64{}
	for rows.Next() {
		var k key
		var v [3]int64
		if err := rows.Scan(&k.day, &k.provider, &k.model, &k.account, &v[0], &v[1], &v[2]); err != nil {
			t.Fatal(err)
		}
		got[k] = v
	}
	_ = rows.Close()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("daily totals\n got %v\nwant %v", got, want)
	}
	// Calls inside the raw period (29 and 3 days old, and the part of day 31.5 ... the
	// cutoff is a whole UTC day) stay per call.
	var raw int
	_ = st.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM usage_events`).Scan(&raw)
	if raw != 14 {
		t.Fatalf("per-call rows left = %d, want 14 (of %d)", raw, n)
	}
}

func TestIncrementalVacuumShrinksFile(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	stats, err := st.StorageStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.AutoVacuum != "incremental" {
		t.Fatalf("auto_vacuum = %s, want incremental", stats.AutoVacuum)
	}
	id, seq := seedChat(t, st, protocol.ChatStatusIdle)
	big := strings.Repeat("y", 4000)
	for i := range 2000 {
		*seq++
		k := "tool_call"
		if i%10 == 0 {
			k = "message"
		}
		if _, err := st.AppendChatEvent(ctx, store.ChatEvent{ChatID: id, Seq: *seq, Kind: k, JSON: `{"text":"` + big + `"}`, CreatedAt: daysAgo(60)}); err != nil {
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
	if before.Method != "dbstat" {
		t.Fatalf("method = %s", before.Method)
	}
	var sessionBytes int64
	for _, k := range before.Breakdown {
		if k.Kind == store.StorageSessionDetail {
			sessionBytes = k.Bytes
		}
	}
	if sessionBytes < 4_000_000 {
		t.Fatalf("session detail uses %d bytes, want > 4 MB: %+v", sessionBytes, before.Breakdown)
	}
	compactAll(t, st, store.CompactChats, store.DefaultRetention, 500)
	for {
		_, left, err := st.IncrementalVacuum(ctx, 64)
		if err != nil {
			t.Fatal(err)
		}
		if left == 0 {
			break
		}
	}
	if err := st.CheckpointWAL(ctx); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(before.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > before.FileBytes/4 {
		t.Fatalf("file %d bytes after compaction, was %d", fi.Size(), before.FileBytes)
	}
}

// TestStorageStatsSplitsConversationsFromSessionDetail: agent-chat transcript events
// (the kinds compaction keeps) count as conversations with the commander transcripts;
// tool calls and other streamed events are session detail. The split follows content
// length, rows are exact.
func TestStorageStatsSplitsConversationsFromSessionDetail(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id, seq := seedChat(t, st, protocol.ChatStatusIdle)
	reply := strings.Repeat("r", 1000)
	tool := strings.Repeat("t", 3000)
	for i := range 400 {
		*seq++
		k, body := "tool_call", tool
		if i%4 == 0 {
			k, body = "message", reply
		}
		if _, err := st.AppendChatEvent(ctx, store.ChatEvent{ChatID: id, Seq: *seq, Kind: k, JSON: `{"text":"` + body + `"}`, CreatedAt: daysAgo(1)}); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := st.StorageStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]store.StorageKind{}
	var labels []string
	for _, k := range stats.Breakdown {
		by[k.Kind] = k
		labels = append(labels, k.Label)
	}
	if _, ok := by["transcripts"]; ok {
		t.Fatalf("old transcripts kind still reported: %v", labels)
	}
	conv, detail := by[store.StorageConversations], by[store.StorageSessionDetail]
	if conv.Label != "Conversations" || detail.Label != "Session detail" {
		t.Fatalf("labels = %v", labels)
	}
	if conv.Rows != 100 || detail.Rows != 300 {
		t.Fatalf("rows: conversations %d, session detail %d; want 100 and 300", conv.Rows, detail.Rows)
	}
	// 100 x ~1 KB of replies against 300 x ~3 KB of tool output: about 1:9.
	if conv.Bytes <= 0 || detail.Bytes < 7*conv.Bytes || detail.Bytes > 11*conv.Bytes {
		t.Fatalf("bytes: conversations %d, session detail %d; want about 1:9", conv.Bytes, detail.Bytes)
	}
}

func TestSplitShare(t *testing.T) {
	for _, c := range []struct{ total, a, b, ra, rb, want int64 }{
		{1000, 1, 3, 9, 9, 250},
		{1000, 0, 0, 1, 1, 500},
		{1000, 0, 0, 0, 0, 0},
		{0, 5, 5, 1, 1, 0},
	} {
		if got := splitShare(c.total, c.a, c.b, c.ra, c.rb); got != c.want {
			t.Errorf("splitShare(%v) = %d, want %d", c, got, c.want)
		}
	}
}
