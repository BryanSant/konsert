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

// Human control storage: causes, drain, the fleet timeline, holds and open requests.

// causeJSON encodes a cause ("" for nil or empty).
func causeJSON(c *store.Cause) string {
	if c == nil || c.IsZero() {
		return ""
	}
	b, err := json.Marshal(c, json.Deterministic(true))
	if err != nil {
		return ""
	}
	return string(b)
}

// parseCause decodes a stored cause (nil when empty or invalid).
func parseCause(s string) *store.Cause {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var c store.Cause
	if err := json.Unmarshal([]byte(s), &c); err != nil || c.IsZero() {
		return nil
	}
	return &c
}

func actorKind(c *store.Cause) string {
	if c == nil {
		return ""
	}
	return c.Actor
}

func resourceJSON(r *store.Resource) string {
	if r == nil || (r.Kind == "" && r.Name == "" && r.Port == 0) {
		return ""
	}
	b, err := json.Marshal(r, json.Deterministic(true))
	if err != nil {
		return ""
	}
	return string(b)
}

func parseResource(s string) *store.Resource {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var r store.Resource
	if err := json.Unmarshal([]byte(s), &r); err != nil || (r.Kind == "" && r.Name == "" && r.Port == 0) {
		return nil
	}
	return &r
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func scanNullTime(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parseTime(v.String)
	return &t
}

// SetWorkerDrained marks a machine drained (or not).
func (s *SQLiteStore) SetWorkerDrained(ctx context.Context, id string, drained bool, at time.Time) error {
	var when any
	if drained {
		if at.IsZero() {
			at = time.Now().UTC()
		}
		when = formatTime(at)
	}
	return s.execOne(ctx, `UPDATE workers SET drained = ?, drained_at = ? WHERE id = ?`, boolInt(drained), when, id)
}

// --- fleet events -------------------------------------------------------------------

// AppendFleetEvent records one fleet timeline event.
func (s *SQLiteStore) AppendFleetEvent(ctx context.Context, e store.FleetEvent) (int64, error) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.Cause.Actor == "" {
		e.Cause.Actor = store.ActorUnknown
	}
	if e.Cause.Confidence == "" {
		e.Cause.Confidence = store.ConfidenceUnknown
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO fleet_events (created_at, kind, action, actor_kind, actor_source, actor_id, confidence,
			request_id, command_id, hold_id, worker_id, task_id, chat_id, from_status, to_status, summary, meta_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, formatTime(e.CreatedAt), e.Kind, e.Action, e.Cause.Actor, e.Cause.Source, e.Cause.ActorID, e.Cause.Confidence,
		e.Cause.RequestID, e.Cause.CommandID, e.Cause.HoldID, e.WorkerID, e.TaskID, e.ChatID,
		e.FromStatus, e.ToStatus, e.Summary, e.MetaJSON)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListFleetEvents returns fleet events matching f, newest first.
func (s *SQLiteStore) ListFleetEvents(ctx context.Context, f store.FleetEventFilter) ([]store.FleetEvent, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	var where []string
	var args []any
	add := func(cond string, v ...any) {
		where = append(where, cond)
		args = append(args, v...)
	}
	if f.WorkerID != "" {
		add("worker_id = ?", f.WorkerID)
	}
	if f.TaskID != "" {
		add("task_id = ?", f.TaskID)
	}
	if f.ChatID != "" {
		add("chat_id = ?", f.ChatID)
	}
	if f.Kind != "" {
		add("kind = ?", f.Kind)
	}
	if f.Actor != "" {
		add("actor_kind = ?", f.Actor)
	}
	if f.Notable {
		// Routine transitions (turns starting and ending, placement) are not notable unless
		// a user caused them or the work failed.
		add("(kind != ? OR actor_kind = ? OR to_status IN ('failed', 'canceled', 'cancelled', 'suspended'))",
			store.EventKindStatus, store.ActorUser)
	}
	if !f.Since.IsZero() {
		add("created_at >= ?", formatTime(f.Since))
	}
	if f.AfterID > 0 {
		add("id > ?", f.AfterID)
	}
	if f.BeforeID > 0 {
		add("id < ?", f.BeforeID)
	}
	q := `SELECT id, created_at, kind, action, actor_kind, actor_source, actor_id, confidence,
		request_id, command_id, hold_id, worker_id, task_id, chat_id, from_status, to_status, summary, meta_json
		FROM fleet_events`
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
	var out []store.FleetEvent
	for rows.Next() {
		var e store.FleetEvent
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.Kind, &e.Action, &e.Cause.Actor, &e.Cause.Source, &e.Cause.ActorID, &e.Cause.Confidence,
			&e.Cause.RequestID, &e.Cause.CommandID, &e.Cause.HoldID, &e.WorkerID, &e.TaskID, &e.ChatID,
			&e.FromStatus, &e.ToStatus, &e.Summary, &e.MetaJSON); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxFleetEventID returns the newest fleet event id (0 when there is none).
func (s *SQLiteStore) MaxFleetEventID(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(id) FROM fleet_events`).Scan(&n)
	return n.Int64, err
}

// --- holds ----------------------------------------------------------------------------

const holdCols = `id, sid, scope_kind, worker_id, task_id, chat_id, resource_json, summary, cause_json,
	created_at, released_at, released_by, release_note`

func scanHold(sc interface{ Scan(...any) error }) (store.Hold, error) {
	var (
		h               store.Hold
		res, cause, cAt string
		released        sql.NullString
	)
	if err := sc.Scan(&h.ID, &h.SID, &h.ScopeKind, &h.WorkerID, &h.TaskID, &h.ChatID, &res, &h.Summary, &cause,
		&cAt, &released, &h.ReleasedBy, &h.ReleaseNote); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Hold{}, store.ErrNotFound
		}
		return store.Hold{}, err
	}
	h.Resource = parseResource(res)
	if c := parseCause(cause); c != nil {
		h.Cause = *c
	}
	h.CreatedAt = parseTime(cAt)
	h.ReleasedAt = scanNullTime(released)
	return h, nil
}

// CreateHold inserts a hold (id and sid minted when empty) and returns it.
func (s *SQLiteStore) CreateHold(ctx context.Context, h store.Hold) (store.Hold, error) {
	if err := s.assignIDs(ctx, ids.Hold, &h.ID, &h.SID); err != nil {
		return h, err
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO holds (`+holdCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.ID, h.SID, h.ScopeKind, h.WorkerID, h.TaskID, h.ChatID, resourceJSON(h.Resource), h.Summary, causeJSON(&h.Cause),
		formatTime(h.CreatedAt), nullTime(h.ReleasedAt), h.ReleasedBy, h.ReleaseNote)
	return h, err
}

// GetHold returns a hold by id.
func (s *SQLiteStore) GetHold(ctx context.Context, id string) (store.Hold, error) {
	return scanHold(s.db.QueryRowContext(ctx, `SELECT `+holdCols+` FROM holds WHERE id = ?`, id))
}

// ListHolds returns holds matching f, newest first.
func (s *SQLiteStore) ListHolds(ctx context.Context, f store.HoldFilter) ([]store.Hold, error) {
	var where []string
	var args []any
	if f.ActiveOnly {
		where = append(where, "released_at IS NULL")
	}
	if f.WorkerID != "" {
		where = append(where, "worker_id = ?")
		args = append(args, f.WorkerID)
	}
	if f.TaskID != "" {
		where = append(where, "task_id = ?")
		args = append(args, f.TaskID)
	}
	if f.ChatID != "" {
		where = append(where, "chat_id = ?")
		args = append(args, f.ChatID)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + holdCols + ` FROM holds`
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
	var out []store.Hold
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ReleaseHold releases an active hold.
func (s *SQLiteStore) ReleaseHold(ctx context.Context, id, by, note string, at time.Time) (bool, error) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE holds SET released_at = ?, released_by = ?, release_note = ? WHERE id = ? AND released_at IS NULL
	`, formatTime(at), by, note, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// --- user requests --------------------------------------------------------------------

const requestCols = `id, sid, chat_id, task_id, worker_id, resource_json, description, status,
	created_at, resolved_at, resolved_by, resolution, command_id`

func scanRequest(sc interface{ Scan(...any) error }) (store.UserRequest, error) {
	var (
		r        store.UserRequest
		res, cAt string
		resolved sql.NullString
	)
	if err := sc.Scan(&r.ID, &r.SID, &r.ChatID, &r.TaskID, &r.WorkerID, &res, &r.Description, &r.Status,
		&cAt, &resolved, &r.ResolvedBy, &r.Resolution, &r.CommandID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.UserRequest{}, store.ErrNotFound
		}
		return store.UserRequest{}, err
	}
	r.Resource = parseResource(res)
	r.CreatedAt = parseTime(cAt)
	r.ResolvedAt = scanNullTime(resolved)
	return r, nil
}

// CreateUserRequest inserts an open request (id and sid minted when empty).
func (s *SQLiteStore) CreateUserRequest(ctx context.Context, r store.UserRequest) (store.UserRequest, error) {
	if err := s.assignIDs(ctx, ids.Request, &r.ID, &r.SID); err != nil {
		return r, err
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.Status == "" {
		r.Status = store.RequestOpen
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_requests (`+requestCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.SID, r.ChatID, r.TaskID, r.WorkerID, resourceJSON(r.Resource), r.Description, r.Status,
		formatTime(r.CreatedAt), nullTime(r.ResolvedAt), r.ResolvedBy, r.Resolution, r.CommandID)
	return r, err
}

// GetUserRequest returns a request by id.
func (s *SQLiteStore) GetUserRequest(ctx context.Context, id string) (store.UserRequest, error) {
	return scanRequest(s.db.QueryRowContext(ctx, `SELECT `+requestCols+` FROM user_requests WHERE id = ?`, id))
}

// ListUserRequests returns requests matching f, newest first.
func (s *SQLiteStore) ListUserRequests(ctx context.Context, f store.UserRequestFilter) ([]store.UserRequest, error) {
	var where []string
	var args []any
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.WorkerID != "" {
		where = append(where, "worker_id = ?")
		args = append(args, f.WorkerID)
	}
	if f.ChatID != "" {
		where = append(where, "chat_id = ?")
		args = append(args, f.ChatID)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + requestCols + ` FROM user_requests`
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
	var out []store.UserRequest
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveUserRequest closes an open request.
func (s *SQLiteStore) ResolveUserRequest(ctx context.Context, id, status, by, resolution, commandID string, at time.Time) (bool, error) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_requests SET status = ?, resolved_at = ?, resolved_by = ?, resolution = ?, command_id = ?
		WHERE id = ? AND status = ?
	`, status, formatTime(at), by, resolution, commandID, id, store.RequestOpen)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
