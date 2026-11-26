package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

func syncedAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return formatTime(t)
}

func scanExternalRefs(rows *sql.Rows) ([]store.ExternalRef, error) {
	defer rows.Close()
	out := []store.ExternalRef{}
	for rows.Next() {
		var r store.ExternalRef
		var synced string
		if err := rows.Scan(&r.TicketID, &r.Tracker, &r.ExternalID, &r.URL, &r.Version, &synced); err != nil {
			return nil, err
		}
		if synced != "" {
			r.SyncedAt = parseTime(synced)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) TicketByExternalRef(ctx context.Context, tracker, externalID string) (store.Ticket, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE id = (
		SELECT ticket_id FROM external_refs WHERE tracker = ? AND external_id = ? LIMIT 1)`,
		strings.TrimSpace(tracker), strings.TrimSpace(externalID))
	return scanTicket(row)
}

func (s *SQLiteStore) ListExternalRefsByTracker(ctx context.Context, tracker string) ([]store.ExternalRef, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ticket_id, tracker, external_id, url, version, synced_at
		FROM external_refs WHERE tracker = ? ORDER BY external_id ASC`, strings.TrimSpace(tracker))
	if err != nil {
		return nil, err
	}
	return scanExternalRefs(rows)
}

const trackerColumns = `id, project_id, kind, config_json, cursor, enabled, last_pull_at, last_push_at, last_error, created_at`

func scanTracker(sc interface{ Scan(...any) error }) (store.Tracker, error) {
	var t store.Tracker
	var enabled int
	var pull, push, created string
	if err := sc.Scan(&t.ID, &t.ProjectID, &t.Kind, &t.ConfigJSON, &t.Cursor, &enabled, &pull, &push, &t.LastError, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Tracker{}, store.ErrNotFound
		}
		return store.Tracker{}, err
	}
	t.Enabled = enabled != 0
	if pull != "" {
		t.LastPullAt = parseTime(pull)
	}
	if push != "" {
		t.LastPushAt = parseTime(push)
	}
	t.CreatedAt = parseTime(created)
	return t, nil
}

func (s *SQLiteStore) CreateTracker(ctx context.Context, t store.Tracker) error {
	t.ID, t.Kind = strings.TrimSpace(t.ID), strings.TrimSpace(t.Kind)
	if t.ID == "" || t.ProjectID == "" || t.Kind == "" {
		return fmt.Errorf("%w: tracker id, project_id and kind are required", store.ErrInvalidWork)
	}
	if t.ConfigJSON == "" {
		t.ConfigJSON = "{}"
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO trackers (`+trackerColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.Kind, t.ConfigJSON, t.Cursor, boolInt(t.Enabled), syncedAt(t.LastPullAt), syncedAt(t.LastPushAt), t.LastError, formatTime(t.CreatedAt))
	return err
}

func (s *SQLiteStore) GetTracker(ctx context.Context, id string) (store.Tracker, error) {
	return scanTracker(s.db.QueryRowContext(ctx, `SELECT `+trackerColumns+` FROM trackers WHERE id = ?`, id))
}

func (s *SQLiteStore) ListTrackers(ctx context.Context, projectID string) ([]store.Tracker, error) {
	q, args := `SELECT `+trackerColumns+` FROM trackers`, []any{}
	if projectID != "" {
		q += ` WHERE project_id = ?`
		args = append(args, projectID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.Tracker{}
	for rows.Next() {
		t, err := scanTracker(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) UpdateTracker(ctx context.Context, t store.Tracker) error {
	res, err := s.db.ExecContext(ctx, `UPDATE trackers SET kind = ?, config_json = ?, cursor = ?, enabled = ?,
		last_pull_at = ?, last_push_at = ?, last_error = ? WHERE id = ?`,
		t.Kind, t.ConfigJSON, t.Cursor, boolInt(t.Enabled), syncedAt(t.LastPullAt), syncedAt(t.LastPushAt), t.LastError, t.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) DeleteTracker(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM external_refs WHERE tracker = ?`, id); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM trackers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}
