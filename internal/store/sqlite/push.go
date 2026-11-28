package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

// UpsertPushSubscription implements store.Store: a re-subscription of the same endpoint
// updates its keys, device and prefs.
func (s *SQLiteStore) UpsertPushSubscription(ctx context.Context, p store.PushSubscription) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if p.PrefsJSON == "" {
		p.PrefsJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO push_subscriptions (endpoint, device_id, p256dh, auth, prefs_json, user_agent, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET device_id = excluded.device_id, p256dh = excluded.p256dh, auth = excluded.auth,
			prefs_json = excluded.prefs_json, user_agent = excluded.user_agent, last_error = ''`,
		p.Endpoint, p.DeviceID, p.P256DH, p.Auth, p.PrefsJSON, p.UserAgent, formatTime(p.CreatedAt))
	return err
}

// ListPushSubscriptions implements store.Store (deviceID "": all).
func (s *SQLiteStore) ListPushSubscriptions(ctx context.Context, deviceID string) ([]store.PushSubscription, error) {
	q := `SELECT endpoint, device_id, p256dh, auth, prefs_json, user_agent, created_at, last_sent_at, last_error FROM push_subscriptions`
	var args []any
	if deviceID != "" {
		q += ` WHERE device_id = ?`
		args = append(args, deviceID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.PushSubscription
	for rows.Next() {
		var p store.PushSubscription
		var created string
		var sent sql.NullString
		if err := rows.Scan(&p.Endpoint, &p.DeviceID, &p.P256DH, &p.Auth, &p.PrefsJSON, &p.UserAgent, &created, &sent, &p.LastError); err != nil {
			return nil, err
		}
		p.CreatedAt, p.LastSentAt = parseTime(created), parseTimePtr(sent)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePushSubscription implements store.Store.
func (s *SQLiteStore) DeletePushSubscription(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
	return err
}

// DeletePushSubscriptionsForDevice implements store.Store.
func (s *SQLiteStore) DeletePushSubscriptionsForDevice(ctx context.Context, deviceID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE device_id = ?`, deviceID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RecordPushResult implements store.Store.
func (s *SQLiteStore) RecordPushResult(ctx context.Context, endpoint string, at time.Time, errText string) error {
	if errText == "" {
		_, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions SET last_sent_at = ?, last_error = '' WHERE endpoint = ?`, formatTime(at), endpoint)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions SET last_error = ? WHERE endpoint = ?`, errText, endpoint)
	return err
}
