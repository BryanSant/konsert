package sqlite

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

const directiveCols = `id, sid, kind, target, params_json, until, provenance, actor, reason, created_at, ended_at, ended_by`

func scanDirective(sc interface{ Scan(...any) error }) (store.Directive, error) {
	var (
		d            store.Directive
		paramsJSON   string
		cAt          string
		until, ended sql.NullString
	)
	if err := sc.Scan(&d.ID, &d.SID, &d.Kind, &d.Target, &paramsJSON, &until, &d.Provenance, &d.Actor, &d.Reason, &cAt, &ended, &d.EndedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Directive{}, store.ErrNotFound
		}
		return store.Directive{}, err
	}
	if paramsJSON != "" && paramsJSON != "{}" {
		if err := json.Unmarshal([]byte(paramsJSON), &d.Params); err != nil {
			return store.Directive{}, err
		}
	}
	d.CreatedAt = parseTime(cAt)
	d.Until = scanNullTime(until)
	d.EndedAt = scanNullTime(ended)
	return d, nil
}

// CreateDirective inserts a directive (id and sid minted when empty).
func (s *SQLiteStore) CreateDirective(ctx context.Context, d store.Directive) (store.Directive, error) {
	if err := s.assignIDs(ctx, ids.Directive, &d.ID, &d.SID); err != nil {
		return d, err
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	b, err := json.Marshal(d.Params)
	if err != nil {
		return d, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO directives (`+directiveCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.SID, d.Kind, d.Target, string(b), nullTime(d.Until), d.Provenance, d.Actor, d.Reason, formatTime(d.CreatedAt), nullTime(d.EndedAt), d.EndedBy)
	return d, err
}

// GetDirective returns a directive by id.
func (s *SQLiteStore) GetDirective(ctx context.Context, id string) (store.Directive, error) {
	return scanDirective(s.db.QueryRowContext(ctx, `SELECT `+directiveCols+` FROM directives WHERE id = ?`, id))
}

// ListDirectives returns directives matching f, newest first.
func (s *SQLiteStore) ListDirectives(ctx context.Context, f store.DirectiveFilter) ([]store.Directive, error) {
	var where []string
	var args []any
	if f.Target != "" {
		where = append(where, "target = ?")
		args = append(args, f.Target)
	}
	if f.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, f.Kind)
	}
	switch {
	case f.ActiveOnly:
		now := f.Now
		if now.IsZero() {
			now = time.Now().UTC()
		}
		where = append(where, "ended_at IS NULL AND (until IS NULL OR until > ?)")
		args = append(args, formatTime(now))
	case f.OpenOnly:
		where = append(where, "ended_at IS NULL")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + directiveCols + ` FROM directives`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Directive
	for rows.Next() {
		d, err := scanDirective(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// EndDirective stamps ended_at when the row is still open.
func (s *SQLiteStore) EndDirective(ctx context.Context, id, by string, at time.Time) (bool, error) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE directives SET ended_at = ?, ended_by = ? WHERE id = ? AND ended_at IS NULL`,
		formatTime(at), by, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
