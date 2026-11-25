package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// History retention (SYR-CSCWP): bounded compaction batches, incremental vacuum and the
// storage breakdown. Every batch is one short transaction touching at most limit rows,
// so the single connection is never held for long and a pass can stop anywhere.

// terminalTaskStatuses is the SQL list of finished task statuses.
var terminalTaskStatuses = sqlList([]string{protocol.TaskStatusCompleted, protocol.TaskStatusFailed, protocol.TaskStatusCanceled})

// busyChatStatuses are chat statuses retention never touches: mid-turn, blocked on the
// user, or suspended (held).
var busyChatStatuses = sqlList([]string{protocol.ChatStatusStarting, protocol.ChatStatusWorking, protocol.ChatStatusNeedsYou, protocol.ChatStatusSuspended})

var (
	taskKeepKinds = sqlList(store.TaskEventKeepKinds)
	chatKeepKinds = sqlList(store.ChatEventKeepKinds)
)

// sqlList renders constant identifiers as a SQL string list ('a', 'b').
func sqlList(vals []string) string {
	q := make([]string, len(vals))
	for i, v := range vals {
		q[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	return "(" + strings.Join(q, ", ") + ")"
}

// CompactHistory runs one bounded batch of a compaction step.
func (s *SQLiteStore) CompactHistory(ctx context.Context, step string, now time.Time, p store.RetentionPolicy, limit int) (store.CompactBatchResult, error) {
	if limit <= 0 {
		limit = 500
	}
	now = now.UTC()
	switch step {
	case store.CompactTasks:
		return s.compactTasks(ctx, now, now.Add(-p.Detail), limit)
	case store.CompactChats:
		return s.compactChats(ctx, now, now.Add(-p.Detail), limit)
	case store.CompactFleet:
		return s.compactFleet(ctx, now.Add(-p.Fleet), now.Add(-p.FleetSummary), limit)
	case store.CompactUsage:
		return s.compactUsage(ctx, now, p, limit)
	}
	return store.CompactBatchResult{}, fmt.Errorf("unknown compaction step %q", step)
}

// compactTasks drops the verbose events of finished tasks that ended before cutoff,
// keeping store.TaskEventKeepKinds, and marks each task compacted once it is done.
// Unfinished, pinned and held tasks are skipped.
func (s *SQLiteStore) compactTasks(ctx context.Context, now, cutoff time.Time, limit int) (store.CompactBatchResult, error) {
	var res store.CompactBatchResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id FROM tasks t
		WHERE t.compacted_at IS NULL AND t.completed_at IS NOT NULL AND t.completed_at < ?
		  AND t.status IN `+terminalTaskStatuses+` AND t.keep = 0
		  AND NOT EXISTS (SELECT 1 FROM holds h WHERE h.released_at IS NULL AND h.task_id = t.id)
		ORDER BY t.completed_at LIMIT ?
	`, formatTime(cutoff), min(limit, 50))
	if err != nil {
		return res, err
	}
	ids, err := scanStrings(rows)
	if err != nil {
		return res, err
	}
	if len(ids) == 0 {
		res.Done = true
		return res, nil
	}
	budget := int64(limit)
	for _, id := range ids {
		if budget <= 0 {
			break
		}
		r, err := tx.ExecContext(ctx, `
			DELETE FROM task_events WHERE id IN (
				SELECT id FROM task_events WHERE task_id = ? AND kind NOT IN `+taskKeepKinds+` ORDER BY id LIMIT ?)
		`, id, budget)
		if err != nil {
			return res, err
		}
		n, _ := r.RowsAffected()
		res.Rows += n
		full := n == budget // the budget ran out: the task may have more to drop
		budget -= n
		if !full {
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET compacted_at = ? WHERE id = ?`, formatTime(now), id); err != nil {
				return res, err
			}
			res.Items++
			budget--
		}
	}
	return res, tx.Commit()
}

// compactChats drops the bulky events (all but store.ChatEventKeepKinds) older than
// cutoff of chats that are not busy, pinned, held or waiting on an open question or
// request. Each chat is walked from compacted_seq, so only events retention has not
// seen yet are read.
func (s *SQLiteStore) compactChats(ctx context.Context, now, cutoff time.Time, limit int) (store.CompactBatchResult, error) {
	var res store.CompactBatchResult
	cut := formatTime(cutoff)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.compacted_seq FROM chat_sessions c
		WHERE c.compact_next_at IS NOT NULL AND c.compact_next_at < ? AND c.keep = 0
		  AND c.status NOT IN `+busyChatStatuses+`
		  AND NOT EXISTS (SELECT 1 FROM holds h WHERE h.released_at IS NULL AND h.chat_id = c.id)
		  AND NOT EXISTS (SELECT 1 FROM questions q WHERE q.chat_id = c.id AND q.status IN ('open', 'delegated'))
		  AND NOT EXISTS (SELECT 1 FROM user_requests r WHERE r.chat_id = c.id AND r.status = 'open')
		ORDER BY c.compact_next_at LIMIT ?
	`, cut, min(limit, 20))
	if err != nil {
		return res, err
	}
	type cand struct {
		id string
		lo int64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.lo); err != nil {
			_ = rows.Close()
			return res, err
		}
		cands = append(cands, c)
	}
	if err := rows.Close(); err != nil {
		return res, err
	}
	if len(cands) == 0 {
		res.Done = true
		return res, nil
	}
	budget := int64(limit)
	for _, c := range cands {
		if budget <= 0 {
			break
		}
		// upper: the first event at or after the cutoff; the walk stops there.
		upper := int64(math.MaxInt64)
		var u sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT seq FROM chat_events WHERE chat_id = ? AND seq > ? AND created_at >= ? ORDER BY seq LIMIT 1
		`, c.id, c.lo, cut).Scan(&u); err != nil && err != sql.ErrNoRows {
			return res, err
		}
		if u.Valid {
			upper = u.Int64
		}
		var last sql.NullInt64
		var n int64
		if err := tx.QueryRowContext(ctx, `
			SELECT MAX(seq), COUNT(1) FROM (
				SELECT seq FROM chat_events WHERE chat_id = ? AND seq > ? AND seq < ? AND kind NOT IN `+chatKeepKinds+`
				ORDER BY seq LIMIT ?)
		`, c.id, c.lo, upper, budget).Scan(&last, &n); err != nil {
			return res, err
		}
		if n > 0 {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM chat_events WHERE chat_id = ? AND seq > ? AND seq <= ? AND kind NOT IN `+chatKeepKinds,
				c.id, c.lo, last.Int64); err != nil {
				return res, err
			}
		}
		res.Rows += n
		full := n == budget
		budget -= n
		// Where the next walk starts: after the last dropped event when the budget ran
		// out, else at the cutoff (or the end of the chat).
		var through int64
		switch {
		case full:
			through = last.Int64
		case u.Valid:
			through = upper - 1
		default:
			var m sql.NullInt64
			if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM chat_events WHERE chat_id = ?`, c.id).Scan(&m); err != nil {
				return res, err
			}
			through = max(m.Int64, c.lo)
		}
		set := `compacted_seq = ?1, compact_next_at = (SELECT created_at FROM chat_events WHERE chat_id = ?2 AND seq > ?1 ORDER BY seq LIMIT 1)`
		args := []any{through, c.id}
		if n > 0 {
			set += `, compacted_at = ?3, compacted_before = ?4`
			args = append(args, formatTime(now), cut)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET `+set+` WHERE id = ?2`, args...); err != nil {
			return res, err
		}
		if !full {
			res.Items++
		}
		budget--
	}
	return res, tx.Commit()
}

// compactFleet deletes timeline events older than fullBefore that are not human actions
// or failures, and every event older than summaryBefore. Events of pinned or
// unfinished work are kept.
func (s *SQLiteStore) compactFleet(ctx context.Context, fullBefore, summaryBefore time.Time, limit int) (store.CompactBatchResult, error) {
	var res store.CompactBatchResult
	r, err := s.db.ExecContext(ctx, `
		DELETE FROM fleet_events WHERE id IN (
			SELECT f.id FROM fleet_events f
			WHERE f.created_at < ?1 AND (f.created_at < ?2 OR NOT `+store.FleetEventKeepSQL+`)
			  AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = f.task_id AND (t.keep = 1 OR t.status NOT IN `+terminalTaskStatuses+`))
			  AND NOT EXISTS (SELECT 1 FROM chat_sessions c WHERE c.id = f.chat_id AND c.keep = 1)
			ORDER BY f.created_at LIMIT ?3)
	`, formatTime(fullBefore), formatTime(summaryBefore), limit)
	if err != nil {
		return res, err
	}
	res.Rows, _ = r.RowsAffected()
	res.Done = res.Rows < int64(limit)
	return res, nil
}

// compactUsage folds per-call usage older than the raw period (whole UTC days) into
// usage_daily and deletes the folded calls in the same transaction, then prunes daily
// totals past their period. Adding and deleting together keeps it idempotent across
// restarts: a call is counted exactly once.
func (s *SQLiteStore) compactUsage(ctx context.Context, now time.Time, p store.RetentionPolicy, limit int) (store.CompactBatchResult, error) {
	var res store.CompactBatchResult
	rawBefore := formatTime(now.Add(-p.UsageRaw).Truncate(24 * time.Hour))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()
	batch := `SELECT id FROM usage_events WHERE created_at < ?1 ORDER BY created_at, id LIMIT ?2`
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_daily (day, provider, model, account_id, input_tokens, output_tokens, requests)
		SELECT substr(created_at, 1, 10), provider, model, account_id, SUM(input_tokens), SUM(output_tokens), COUNT(1)
		FROM usage_events WHERE id IN (`+batch+`)
		GROUP BY substr(created_at, 1, 10), provider, model, account_id
		ON CONFLICT (day, provider, model, account_id) DO UPDATE SET
			input_tokens  = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			requests      = requests + excluded.requests
	`, rawBefore, limit); err != nil {
		return res, err
	}
	r, err := tx.ExecContext(ctx, `DELETE FROM usage_events WHERE id IN (`+batch+`)`, rawBefore, limit)
	if err != nil {
		return res, err
	}
	res.Rows, _ = r.RowsAffected()
	if budget := int64(limit) - res.Rows; budget > 0 {
		// Every old call is folded: prune daily totals past their period.
		dailyBefore := now.Add(-p.UsageDaily).UTC().Format(time.DateOnly)
		r, err := tx.ExecContext(ctx, `
			DELETE FROM usage_daily WHERE (day, provider, model, account_id) IN (
				SELECT day, provider, model, account_id FROM usage_daily WHERE day < ? LIMIT ?)
		`, dailyBefore, budget)
		if err != nil {
			return res, err
		}
		n, _ := r.RowsAffected()
		res.Rows += n
		res.Done = n < budget
	}
	return res, tx.Commit()
}

// IncrementalVacuum frees up to pages free pages and returns the bytes it returned to
// the file system and how many free pages remain.
func (s *SQLiteStore) IncrementalVacuum(ctx context.Context, pages int) (int64, int64, error) {
	if pages <= 0 {
		pages = 256
	}
	var before, after, pageSize int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&before); err != nil {
		return 0, 0, err
	}
	if before == 0 {
		return 0, 0, nil
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, pages)); err != nil {
		return 0, 0, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&after); err != nil {
		return 0, 0, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, 0, err
	}
	return max(before-after, 0) * pageSize, after, nil
}

// CheckpointWAL checkpoints and truncates the WAL, so a vacuumed file shrinks on disk.
func (s *SQLiteStore) CheckpointWAL(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// SetTaskKeep pins or unpins a task.
func (s *SQLiteStore) SetTaskKeep(ctx context.Context, id string, keep bool) error {
	return s.execOne(ctx, `UPDATE tasks SET keep = ? WHERE id = ?`, boolInt(keep), id)
}

// SetChatKeep pins or unpins a chat.
func (s *SQLiteStore) SetChatKeep(ctx context.Context, id string, keep bool) error {
	return s.execOne(ctx, `UPDATE chat_sessions SET keep = ? WHERE id = ?`, boolInt(keep), id)
}

// storageChatEvents is the interim kind of the chat_events table, split afterwards
// between conversations and session detail (splitChatEvents).
const storageChatEvents = "chat_events"

// chatEventRowBytes is the typical chat_events row size the estimate falls back to.
const chatEventRowBytes = 700

// storageKindOf maps a table to its breakdown kind.
var storageKindOf = map[string]string{
	"chat_messages":         store.StorageConversations,
	"chat_events":           storageChatEvents,
	"task_events":           store.StorageTaskEvents,
	"fleet_events":          store.StorageFleet,
	"audit_log":             store.StorageFleet,
	"usage_events":          store.StorageUsage,
	"usage_daily":           store.StorageUsage,
	"machine_vitals":        store.StorageMachine,
	"machine_vitals_rollup": store.StorageMachine,
}

// storageKinds lists the breakdown in display order with labels and the typical row
// size the estimate falls back to (bytes, indexes included).
var storageKinds = []struct {
	kind, label string
	rowBytes    int64
}{
	{store.StorageConversations, "Conversations", 900},
	{store.StorageSessionDetail, "Session detail", chatEventRowBytes},
	{store.StorageTaskEvents, "Task events", 250},
	{store.StorageFleet, "Fleet timeline and audit log", 350},
	{store.StorageUsage, "Usage", 90},
	{store.StorageMachine, "Machine history", 120},
	{store.StorageOther, "Other", 0},
}

// StorageStats measures the files and the space each kind of history uses: exact page
// usage from dbstat when the SQLite build has it, else row counts times typical sizes.
func (s *SQLiteStore) StorageStats(ctx context.Context) (store.StorageStats, error) {
	st := store.StorageStats{MeasuredAt: time.Now().UTC()}
	var pageCount, free, av int64
	for q, dst := range map[string]*int64{
		`PRAGMA page_size`: &st.PageSize, `PRAGMA page_count`: &pageCount,
		`PRAGMA freelist_count`: &free, `PRAGMA auto_vacuum`: &av,
	} {
		if err := s.db.QueryRowContext(ctx, q).Scan(dst); err != nil {
			return st, err
		}
	}
	st.FreeBytes = free * st.PageSize
	st.AutoVacuum = map[int64]string{0: "none", 1: "full", 2: "incremental"}[av]
	if rows, err := s.db.QueryContext(ctx, `PRAGMA database_list`); err == nil {
		for rows.Next() {
			var seq int
			var name, file string
			if rows.Scan(&seq, &name, &file) == nil && name == "main" {
				st.Path = file
			}
		}
		_ = rows.Close()
	}
	if st.Path != "" {
		if fi, err := os.Stat(st.Path); err == nil {
			st.FileBytes = fi.Size()
		}
		if fi, err := os.Stat(st.Path + "-wal"); err == nil {
			st.WALBytes = fi.Size()
		}
	}
	if st.FileBytes == 0 {
		st.FileBytes = pageCount * st.PageSize
	}
	used := (pageCount - free) * st.PageSize

	bytes := map[string]int64{}
	rowsBy := map[string]int64{}
	tableOf := map[string]string{}
	if rows, err := s.db.QueryContext(ctx, `SELECT name, tbl_name FROM sqlite_schema WHERE type IN ('table', 'index')`); err == nil {
		for rows.Next() {
			var name, tbl string
			if rows.Scan(&name, &tbl) == nil {
				tableOf[name] = tbl
			}
		}
		_ = rows.Close()
	}
	st.Method = "dbstat"
	rows, err := s.db.QueryContext(ctx, `SELECT name, pgsize, ncell FROM dbstat WHERE aggregate = TRUE`)
	if err == nil {
		for rows.Next() {
			var name string
			var size, ncell int64
			if err := rows.Scan(&name, &size, &ncell); err != nil {
				break
			}
			tbl := tableOf[name]
			if tbl == "" {
				tbl = name
			}
			kind, ok := storageKindOf[tbl]
			if !ok {
				kind = store.StorageOther
			}
			bytes[kind] += size
			if name == tbl && ok {
				rowsBy[kind] += ncell
			}
		}
		err = rows.Err()
		_ = rows.Close()
	}
	if err != nil {
		// No dbstat: estimate from row counts.
		st.Method = "estimate"
		clear(bytes)
		clear(rowsBy)
		for tbl, kind := range storageKindOf {
			var n int64
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM `+tbl).Scan(&n); err != nil {
				return st, err
			}
			rowsBy[kind] += n
		}
		var known int64
		for _, k := range storageKinds {
			bytes[k.kind] = rowsBy[k.kind] * k.rowBytes
			known += bytes[k.kind]
		}
		bytes[storageChatEvents] = rowsBy[storageChatEvents] * chatEventRowBytes
		known += bytes[storageChatEvents]
		if used > known {
			bytes[store.StorageOther] = used - known
		}
	}
	if err := s.splitChatEvents(ctx, bytes, rowsBy); err != nil {
		return st, err
	}
	for _, k := range storageKinds {
		st.Breakdown = append(st.Breakdown, store.StorageKind{Kind: k.kind, Label: k.label, Bytes: bytes[k.kind], Rows: rowsBy[k.kind]})
	}
	return st, nil
}

// splitChatEvents divides the chat_events table's measured space between conversations
// (the kinds compaction keeps) and session detail (the rest). dbstat measures pages per
// table, not per kind, so the split is proportional to SUM(length(json)) of each group;
// row counts are exact.
func (s *SQLiteStore) splitChatEvents(ctx context.Context, bytes, rowsBy map[string]int64) error {
	total := bytes[storageChatEvents]
	delete(bytes, storageChatEvents)
	delete(rowsBy, storageChatEvents)
	rows, err := s.db.QueryContext(ctx, `SELECT kind IN `+chatKeepKinds+` AS kept, COUNT(1), COALESCE(SUM(length(json)), 0)
		FROM chat_events GROUP BY kept`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var keptRows, keptLen, otherRows, otherLen int64
	for rows.Next() {
		var kept bool
		var n, l int64
		if err := rows.Scan(&kept, &n, &l); err != nil {
			return err
		}
		if kept {
			keptRows, keptLen = n, l
		} else {
			otherRows, otherLen = n, l
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	keptBytes := splitShare(total, keptLen, otherLen, keptRows, otherRows)
	bytes[store.StorageConversations] += keptBytes
	bytes[store.StorageSessionDetail] += total - keptBytes
	rowsBy[store.StorageConversations] += keptRows
	rowsBy[store.StorageSessionDetail] += otherRows
	return nil
}

// splitShare is the part of total belonging to a group with content length a and ra
// rows against the other group's b and rb: proportional to length, or to rows when
// neither group has content.
func splitShare(total, a, b, ra, rb int64) int64 {
	switch {
	case a+b > 0:
		return int64(float64(total) * float64(a) / float64(a+b))
	case ra+rb > 0:
		return int64(float64(total) * float64(ra) / float64(ra+rb))
	}
	return 0
}

// scanStrings reads a single-column result and closes rows.
func scanStrings(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
