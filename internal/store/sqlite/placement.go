package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

// CreatePlacementRule implements store.Store.
func (s *SQLiteStore) CreatePlacementRule(ctx context.Context, r store.PlacementRule) (store.PlacementRule, error) {
	if r.ID == "" {
		r.ID = ids.UUID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	var expires any
	if r.ExpiresAt != nil {
		expires = formatTime(*r.ExpiresAt)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO placement_rules
		(id, scope_kind, scope_value, effect, target_kind, target, provenance, evidence, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.ScopeKind, r.ScopeValue, r.Effect, r.TargetKind, r.Target, r.Provenance, r.Evidence, r.CreatedBy,
		formatTime(r.CreatedAt), expires)
	return r, err
}

// ListPlacementRules implements store.Store: every rule, or those active at activeAt
// when it is set, oldest first.
func (s *SQLiteStore) ListPlacementRules(ctx context.Context, activeAt time.Time) ([]store.PlacementRule, error) {
	q := `SELECT id, scope_kind, scope_value, effect, target_kind, target, provenance, evidence, created_by, created_at, expires_at, ended_at
		FROM placement_rules`
	var args []any
	if !activeAt.IsZero() {
		q += ` WHERE ended_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`
		args = append(args, formatTime(activeAt))
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at, id LIMIT 1000`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.PlacementRule
	for rows.Next() {
		var r store.PlacementRule
		var created string
		var expires, ended sql.NullString
		if err := rows.Scan(&r.ID, &r.ScopeKind, &r.ScopeValue, &r.Effect, &r.TargetKind, &r.Target, &r.Provenance,
			&r.Evidence, &r.CreatedBy, &created, &expires, &ended); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTime(created)
		r.ExpiresAt, r.EndedAt = parseTimePtr(expires), parseTimePtr(ended)
		out = append(out, r)
	}
	return out, rows.Err()
}

// EndPlacementRule implements store.Store.
func (s *SQLiteStore) EndPlacementRule(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE placement_rules SET ended_at = ? WHERE id = ? AND ended_at IS NULL`, formatTime(at), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// AppendPlacementLog implements store.Store.
func (s *SQLiteStore) AppendPlacementLog(ctx context.Context, e store.PlacementLogEntry) error {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO placement_log (at, subject, work_key, decider, worker_id, detail_json) VALUES (?, ?, ?, ?, ?, ?)`,
		formatTime(e.At), e.Subject, e.WorkKey, e.Decider, e.WorkerID, e.DetailJSON)
	return err
}

// ListPlacementLog implements store.Store: newest first.
func (s *SQLiteStore) ListPlacementLog(ctx context.Context, f store.PlacementLogFilter) ([]store.PlacementLogEntry, error) {
	q := `SELECT id, at, subject, work_key, decider, worker_id, detail_json FROM placement_log WHERE 1=1`
	var args []any
	if f.Subject != "" {
		q += ` AND subject = ?`
		args = append(args, f.Subject)
	}
	if !f.Since.IsZero() {
		q += ` AND at >= ?`
		args = append(args, formatTime(f.Since))
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY at DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.PlacementLogEntry
	for rows.Next() {
		var e store.PlacementLogEntry
		var at string
		if err := rows.Scan(&e.ID, &at, &e.Subject, &e.WorkKey, &e.Decider, &e.WorkerID, &e.DetailJSON); err != nil {
			return nil, err
		}
		e.At = parseTime(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PrunePlacementLog implements store.Store.
func (s *SQLiteStore) PrunePlacementLog(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM placement_log WHERE at < ?`, formatTime(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
