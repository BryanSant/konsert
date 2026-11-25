package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

// --- override requests ------------------------------------------------------------------

const overrideCols = `id, sid, chat_id, scope_kind, worker_id, task_id, target_chat_id, hold_id, extra_slots,
	duration_seconds, reason, consequences, status, created_at, decided_at, decided_by, decision_note,
	approved_at, expires_at, ended_at, use_count, first_used_at, last_used_at`

func scanOverride(sc interface{ Scan(...any) error }) (store.Override, error) {
	var (
		o                                                      store.Override
		cAt                                                    string
		decided, approved, expires, ended, firstUsed, lastUsed sql.NullString
	)
	if err := sc.Scan(&o.ID, &o.SID, &o.ChatID, &o.ScopeKind, &o.WorkerID, &o.TaskID, &o.TargetChatID, &o.HoldID, &o.ExtraSlots,
		&o.DurationSeconds, &o.Reason, &o.Consequences, &o.Status, &cAt, &decided, &o.DecidedBy, &o.DecisionNote,
		&approved, &expires, &ended, &o.UseCount, &firstUsed, &lastUsed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Override{}, store.ErrNotFound
		}
		return store.Override{}, err
	}
	o.CreatedAt = parseTime(cAt)
	o.DecidedAt = scanNullTime(decided)
	o.ApprovedAt = scanNullTime(approved)
	o.ExpiresAt = scanNullTime(expires)
	o.EndedAt = scanNullTime(ended)
	o.FirstUsedAt = scanNullTime(firstUsed)
	o.LastUsedAt = scanNullTime(lastUsed)
	return o, nil
}

// CreateOverride inserts an override request (id and sid minted when empty; pending
// by default).
func (s *SQLiteStore) CreateOverride(ctx context.Context, o store.Override) (store.Override, error) {
	if err := s.assignIDs(ctx, ids.Override, &o.ID, &o.SID); err != nil {
		return o, err
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	if o.Status == "" {
		o.Status = store.OverridePending
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO override_requests (`+overrideCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.SID, o.ChatID, o.ScopeKind, o.WorkerID, o.TaskID, o.TargetChatID, o.HoldID, o.ExtraSlots,
		o.DurationSeconds, o.Reason, o.Consequences, o.Status, formatTime(o.CreatedAt), nullTime(o.DecidedAt), o.DecidedBy, o.DecisionNote,
		nullTime(o.ApprovedAt), nullTime(o.ExpiresAt), nullTime(o.EndedAt), o.UseCount, nullTime(o.FirstUsedAt), nullTime(o.LastUsedAt))
	return o, err
}

// GetOverride returns an override by id.
func (s *SQLiteStore) GetOverride(ctx context.Context, id string) (store.Override, error) {
	return scanOverride(s.db.QueryRowContext(ctx, `SELECT `+overrideCols+` FROM override_requests WHERE id = ?`, id))
}

// ListOverrides returns overrides matching f, newest first.
func (s *SQLiteStore) ListOverrides(ctx context.Context, f store.OverrideFilter) ([]store.Override, error) {
	var where []string
	var args []any
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.ChatID != "" {
		where = append(where, "chat_id = ?")
		args = append(args, f.ChatID)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + overrideCols + ` FROM override_requests`
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
	var out []store.Override
	for rows.Next() {
		o, err := scanOverride(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// DecideOverride approves or denies a pending override.
func (s *SQLiteStore) DecideOverride(ctx context.Context, id, status, by, note string, at time.Time, expiresAt *time.Time) (bool, error) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var approved any
	if status == store.OverrideApproved {
		approved = formatTime(at)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE override_requests SET status = ?, decided_at = ?, decided_by = ?, decision_note = ?, approved_at = ?, expires_at = ?
		WHERE id = ? AND status = ?
	`, status, formatTime(at), by, note, approved, nullTime(expiresAt), id, store.OverridePending)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ExpireOverride ends an approved override.
func (s *SQLiteStore) ExpireOverride(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE override_requests SET status = ?, ended_at = ? WHERE id = ? AND status = ?`,
		store.OverrideExpired, formatTime(at), id, store.OverrideApproved)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkOverrideUsed counts one use of an override.
func (s *SQLiteStore) MarkOverrideUsed(ctx context.Context, id string, at time.Time) error {
	ts := formatTime(at)
	_, err := s.db.ExecContext(ctx, `
		UPDATE override_requests SET use_count = use_count + 1, first_used_at = COALESCE(first_used_at, ?), last_used_at = ?
		WHERE id = ?
	`, ts, ts, id)
	return err
}

// --- vetoes -----------------------------------------------------------------------------

const vetoCols = `id, sid, reason, note, summary, worker_id, task_id, chat_id, fleet_event_id, created_at,
	delivered_at, delivered_chat_id, delivered_turn_id, acked_at, acked_chat_id, escalated_at, dismissed_at, dismissed_by`

func scanVeto(sc interface{ Scan(...any) error }) (store.Veto, error) {
	var (
		v                                      store.Veto
		cAt                                    string
		delivered, acked, escalated, dismissed sql.NullString
	)
	if err := sc.Scan(&v.ID, &v.SID, &v.Reason, &v.Note, &v.Summary, &v.WorkerID, &v.TaskID, &v.ChatID, &v.FleetEventID, &cAt,
		&delivered, &v.DeliveredChatID, &v.DeliveredTurnID, &acked, &v.AckedChatID, &escalated, &dismissed, &v.DismissedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Veto{}, store.ErrNotFound
		}
		return store.Veto{}, err
	}
	v.CreatedAt = parseTime(cAt)
	v.DeliveredAt = scanNullTime(delivered)
	v.AckedAt = scanNullTime(acked)
	v.EscalatedAt = scanNullTime(escalated)
	v.DismissedAt = scanNullTime(dismissed)
	return v, nil
}

// CreateVeto inserts a veto (id and sid minted when empty).
func (s *SQLiteStore) CreateVeto(ctx context.Context, v store.Veto) (store.Veto, error) {
	if err := s.assignIDs(ctx, ids.Veto, &v.ID, &v.SID); err != nil {
		return v, err
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO vetoes (`+vetoCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.SID, v.Reason, v.Note, v.Summary, v.WorkerID, v.TaskID, v.ChatID, v.FleetEventID, formatTime(v.CreatedAt),
		nullTime(v.DeliveredAt), v.DeliveredChatID, v.DeliveredTurnID, nullTime(v.AckedAt), v.AckedChatID,
		nullTime(v.EscalatedAt), nullTime(v.DismissedAt), v.DismissedBy)
	return v, err
}

// GetVeto returns a veto by id.
func (s *SQLiteStore) GetVeto(ctx context.Context, id string) (store.Veto, error) {
	return scanVeto(s.db.QueryRowContext(ctx, `SELECT `+vetoCols+` FROM vetoes WHERE id = ?`, id))
}

func (s *SQLiteStore) queryVetoes(ctx context.Context, q string, args ...any) ([]store.Veto, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Veto
	for rows.Next() {
		v, err := scanVeto(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListVetoes returns vetoes matching f, newest first.
func (s *SQLiteStore) ListVetoes(ctx context.Context, f store.VetoFilter) ([]store.Veto, error) {
	var where []string
	var args []any
	if f.OpenOnly {
		where = append(where, "acked_at IS NULL AND dismissed_at IS NULL")
	}
	if !f.Since.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, formatTime(f.Since))
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + vetoCols + ` FROM vetoes`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	return s.queryVetoes(ctx, q, args...)
}

// MarkVetoesDelivered records the delivery of the open vetoes of fleetEventIDs.
func (s *SQLiteStore) MarkVetoesDelivered(ctx context.Context, fleetEventIDs []int64, chatID, turnID string, at time.Time) ([]store.Veto, error) {
	if len(fleetEventIDs) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(fleetEventIDs)), ",")
	args := []any{formatTime(at), chatID, turnID}
	sel := make([]any, 0, len(fleetEventIDs))
	for _, id := range fleetEventIDs {
		sel = append(sel, id)
	}
	args = append(args, sel...)
	if _, err := s.db.ExecContext(ctx, `
		UPDATE vetoes SET delivered_at = ?, delivered_chat_id = ?, delivered_turn_id = ?
		WHERE fleet_event_id IN (`+ph+`) AND acked_at IS NULL AND dismissed_at IS NULL
	`, args...); err != nil {
		return nil, err
	}
	return s.queryVetoes(ctx, `SELECT `+vetoCols+` FROM vetoes WHERE fleet_event_id IN (`+ph+`) AND acked_at IS NULL AND dismissed_at IS NULL`, sel...)
}

// AckVetoes acknowledges the open vetoes delivered in chatID's turn turnID.
func (s *SQLiteStore) AckVetoes(ctx context.Context, chatID, turnID string, at time.Time) ([]store.Veto, error) {
	if chatID == "" || turnID == "" {
		return nil, nil
	}
	open, err := s.queryVetoes(ctx, `SELECT `+vetoCols+` FROM vetoes
		WHERE delivered_chat_id = ? AND delivered_turn_id = ? AND acked_at IS NULL AND dismissed_at IS NULL`, chatID, turnID)
	if err != nil || len(open) == 0 {
		return nil, err
	}
	var out []store.Veto
	for _, v := range open {
		res, err := s.db.ExecContext(ctx, `UPDATE vetoes SET acked_at = ?, acked_chat_id = ? WHERE id = ? AND acked_at IS NULL AND dismissed_at IS NULL`,
			formatTime(at), chatID, v.ID)
		if err != nil {
			return out, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			t := at
			v.AckedAt, v.AckedChatID = &t, chatID
			out = append(out, v)
		}
	}
	return out, nil
}

// EscalateVeto marks an open veto escalated.
func (s *SQLiteStore) EscalateVeto(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE vetoes SET escalated_at = ?
		WHERE id = ? AND escalated_at IS NULL AND acked_at IS NULL AND dismissed_at IS NULL`, formatTime(at), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DismissVeto closes an open veto without acknowledgement.
func (s *SQLiteStore) DismissVeto(ctx context.Context, id, by string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE vetoes SET dismissed_at = ?, dismissed_by = ?
		WHERE id = ? AND acked_at IS NULL AND dismissed_at IS NULL`, formatTime(at), by, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
