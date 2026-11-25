package sqlite

import (
	"context"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

const (
	// AuditRetention is how long audit rows are kept.
	AuditRetention = 90 * 24 * time.Hour
	// AuditMaxRows caps the audit table regardless of age.
	AuditMaxRows = 100_000
	// auditPruneEvery is how many inserts pass between retention passes.
	auditPruneEvery = 256

	defaultAuditLimit = 100
	maxAuditLimit     = 500
)

// AppendAudit records one audit event. Retention is applied cheaply in-line: on
// the first insert after Open and then every auditPruneEvery inserts.
func (s *SQLiteStore) AppendAudit(ctx context.Context, e store.AuditEvent) (int64, error) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.User == "" {
		e.User = "unknown"
	}
	// The sids shown to users are recorded with the event (looked up unless given), so the
	// log stays readable after the worker or task is deleted.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_log (created_at, actor, source, action, worker_id, worker_sid, task_id, task_sid, message, meta_json, actor_kind, cause_json)
		VALUES (?, ?, ?, ?,
			?, COALESCE(NULLIF(?, ''), (SELECT sid FROM workers WHERE id = ?), ''),
			?, COALESCE(NULLIF(?, ''), (SELECT sid FROM tasks WHERE id = ?), ''),
			?, ?, ?, ?)
	`, formatTime(e.CreatedAt), e.User, e.Source, e.Action,
		e.WorkerID, e.WorkerSID, e.WorkerID,
		e.TaskID, e.TaskSID, e.TaskID,
		e.Message, e.MetaJSON, actorKind(e.Cause), causeJSON(e.Cause))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if n := s.auditInserts.Add(1); n%auditPruneEvery == 1 {
		_, _ = s.PruneAudit(ctx, time.Now().UTC().Add(-AuditRetention), AuditMaxRows)
	}
	return id, nil
}

// ListAudit returns audit events matching f, newest first.
func (s *SQLiteStore) ListAudit(ctx context.Context, f store.AuditFilter) ([]store.AuditEvent, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	if limit > maxAuditLimit {
		limit = maxAuditLimit
	}
	var where []string
	var args []any
	add := func(cond string, v any) {
		where = append(where, cond)
		args = append(args, v)
	}
	if f.WorkerID != "" {
		add("worker_id = ?", f.WorkerID)
	}
	if f.TaskID != "" {
		add("task_id = ?", f.TaskID)
	}
	if f.User != "" {
		add("actor = ?", f.User)
	}
	if f.Action != "" {
		add("action = ?", f.Action)
	}
	if !f.Since.IsZero() {
		add("created_at >= ?", formatTime(f.Since))
	}
	if !f.Until.IsZero() {
		add("created_at <= ?", formatTime(f.Until))
	}
	if f.BeforeID > 0 {
		add("id < ?", f.BeforeID)
	}
	if f.AfterID > 0 {
		add("id > ?", f.AfterID)
	}
	q := `SELECT id, created_at, actor, source, action, worker_id, worker_sid, task_id, task_sid, message, meta_json, cause_json FROM audit_log`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.AuditEvent
	for rows.Next() {
		var e store.AuditEvent
		var ts, cause string
		if err := rows.Scan(&e.ID, &ts, &e.User, &e.Source, &e.Action, &e.WorkerID, &e.WorkerSID, &e.TaskID, &e.TaskSID, &e.Message, &e.MetaJSON, &cause); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(ts)
		e.Cause = parseCause(cause)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneAudit deletes audit rows older than olderThan and, when maxRows > 0, every
// row but the newest maxRows. Both deletes walk an index (created_at / rowid).
func (s *SQLiteStore) PruneAudit(ctx context.Context, olderThan time.Time, maxRows int) (int64, error) {
	var total int64
	if !olderThan.IsZero() {
		res, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE created_at < ?`, formatTime(olderThan))
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if maxRows > 0 {
		res, err := s.db.ExecContext(ctx, `
			DELETE FROM audit_log WHERE id <= (
				SELECT id FROM audit_log ORDER BY id DESC LIMIT 1 OFFSET ?
			)`, maxRows)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
