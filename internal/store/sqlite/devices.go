package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/store"
)

// Device credentials and pairing codes (KON-W0P8B, KON-V5JZB, KON-A29CT). Timestamps
// are fixed-width UTC text (formatTime), so they compare correctly as strings in SQL.

const deviceCols = `id, sid, name, kind, secret_hash, created_at, created_by, last_used_at,
	last_ip, last_user_agent, expires_at, revoked_at`

func scanDevice(sc interface{ Scan(...any) error }) (store.Device, error) {
	var (
		d                      store.Device
		created, used, expires string
		revoked                sql.NullString
	)
	if err := sc.Scan(&d.ID, &d.SID, &d.Name, &d.Kind, &d.SecretHash, &created, &d.CreatedBy, &used,
		&d.LastIP, &d.LastUserAgent, &expires, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Device{}, store.ErrNotFound
		}
		return store.Device{}, err
	}
	d.CreatedAt = parseTime(created)
	d.LastUsedAt = parseTime(used)
	d.ExpiresAt = parseTime(expires)
	d.RevokedAt = scanNullTime(revoked)
	return d, nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *SQLiteStore) prepareDevice(ctx context.Context, d *store.Device) error {
	if d.SecretHash == "" {
		return errors.New("device needs a secret hash")
	}
	if !store.ValidDeviceKind(d.Kind) {
		return errors.New("invalid device kind " + d.Kind)
	}
	if err := s.assignIDs(ctx, ids.Device, &d.ID, &d.SID); err != nil {
		return err
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	if d.LastUsedAt.IsZero() {
		d.LastUsedAt = d.CreatedAt
	}
	return nil
}

func insertDevice(ctx context.Context, ex execer, d store.Device) error {
	_, err := ex.ExecContext(ctx, `INSERT INTO devices (`+deviceCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.SID, d.Name, d.Kind, d.SecretHash, formatTime(d.CreatedAt), d.CreatedBy, formatTime(d.LastUsedAt),
		d.LastIP, d.LastUserAgent, formatTime(d.ExpiresAt), nullTime(d.RevokedAt))
	return err
}

// CreateDevice inserts a device credential.
func (s *SQLiteStore) CreateDevice(ctx context.Context, d store.Device) (store.Device, error) {
	if err := s.prepareDevice(ctx, &d); err != nil {
		return d, err
	}
	return d, insertDevice(ctx, s.db, d)
}

// GetDevice returns a device by id.
func (s *SQLiteStore) GetDevice(ctx context.Context, id string) (store.Device, error) {
	return scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, id))
}

// GetDeviceBySecretHash returns the device holding a credential, revoked or not.
func (s *SQLiteStore) GetDeviceBySecretHash(ctx context.Context, hash string) (store.Device, error) {
	if hash == "" {
		return store.Device{}, store.ErrNotFound
	}
	return scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE secret_hash = ?`, hash))
}

// ListDevices returns the devices that are not revoked, most recently used first.
func (s *SQLiteStore) ListDevices(ctx context.Context) ([]store.Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE revoked_at IS NULL
		ORDER BY last_used_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// TouchDevice records a use of an active device.
func (s *SQLiteStore) TouchDevice(ctx context.Context, id string, at, expiresAt time.Time, ip, userAgent string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_used_at = ?, expires_at = ?, last_ip = ?, last_user_agent = ?
		WHERE id = ? AND revoked_at IS NULL`, formatTime(at), formatTime(expiresAt), ip, userAgent, id)
	return err
}

// RenameDevice renames an active device.
func (s *SQLiteStore) RenameDevice(ctx context.Context, id, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET name = ? WHERE id = ? AND revoked_at IS NULL`, name, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// RevokeDevice revokes an active device.
func (s *SQLiteStore) RevokeDevice(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, formatTime(at), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

const pairingCols = `id, sid, purpose, code_hash, poll_hash, kind, device_name, created_by, created_ip,
	created_at, expires_at, approved_at, approved_by, denied_at, redeemed_at, device_id`

func scanPairingCode(sc interface{ Scan(...any) error }) (store.PairingCode, error) {
	var (
		p                          store.PairingCode
		created, expires           string
		approved, denied, redeemed sql.NullString
	)
	if err := sc.Scan(&p.ID, &p.SID, &p.Purpose, &p.CodeHash, &p.PollHash, &p.Kind, &p.DeviceName, &p.CreatedBy,
		&p.CreatedIP, &created, &expires, &approved, &p.ApprovedBy, &denied, &redeemed, &p.DeviceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.PairingCode{}, store.ErrNotFound
		}
		return store.PairingCode{}, err
	}
	p.CreatedAt = parseTime(created)
	p.ExpiresAt = parseTime(expires)
	p.ApprovedAt = scanNullTime(approved)
	p.DeniedAt = scanNullTime(denied)
	p.RedeemedAt = scanNullTime(redeemed)
	return p, nil
}

// CreatePairingCode inserts a pairing or login code.
func (s *SQLiteStore) CreatePairingCode(ctx context.Context, p store.PairingCode) (store.PairingCode, error) {
	if p.CodeHash == "" || p.ExpiresAt.IsZero() {
		return p, errors.New("pairing code needs a hash and an expiry")
	}
	if p.Purpose != store.PairingPurposePair && p.Purpose != store.PairingPurposeLogin {
		return p, errors.New("invalid pairing code purpose " + p.Purpose)
	}
	if err := s.assignIDs(ctx, ids.PairingCode, &p.ID, &p.SID); err != nil {
		return p, err
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO pairing_codes (`+pairingCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.SID, p.Purpose, p.CodeHash, p.PollHash, p.Kind, p.DeviceName, p.CreatedBy, p.CreatedIP,
		formatTime(p.CreatedAt), formatTime(p.ExpiresAt), nullTime(p.ApprovedAt), p.ApprovedBy,
		nullTime(p.DeniedAt), nullTime(p.RedeemedAt), p.DeviceID)
	return p, err
}

// GetPairingCodeByHash returns the code with the given code hash.
func (s *SQLiteStore) GetPairingCodeByHash(ctx context.Context, codeHash string) (store.PairingCode, error) {
	if codeHash == "" {
		return store.PairingCode{}, store.ErrNotFound
	}
	return scanPairingCode(s.db.QueryRowContext(ctx, `SELECT `+pairingCols+` FROM pairing_codes WHERE code_hash = ?`, codeHash))
}

// GetPairingCodeByPollHash returns the login code with the given poll-secret hash.
func (s *SQLiteStore) GetPairingCodeByPollHash(ctx context.Context, pollHash string) (store.PairingCode, error) {
	if pollHash == "" {
		return store.PairingCode{}, store.ErrNotFound
	}
	return scanPairingCode(s.db.QueryRowContext(ctx, `SELECT `+pairingCols+` FROM pairing_codes WHERE poll_hash = ?`, pollHash))
}

// DecidePairingCode approves or denies a pending, unexpired login code.
func (s *SQLiteStore) DecidePairingCode(ctx context.Context, id string, approve bool, by string, at time.Time) (bool, error) {
	col := "denied_at"
	if approve {
		col = "approved_at"
	}
	res, err := s.db.ExecContext(ctx, `UPDATE pairing_codes SET `+col+` = ?, approved_by = ?
		WHERE id = ? AND purpose = ? AND approved_at IS NULL AND denied_at IS NULL AND redeemed_at IS NULL
		AND expires_at > ?`, formatTime(at), by, id, store.PairingPurposeLogin, formatTime(at))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RedeemPairingCode marks a code used and creates its device in one transaction.
func (s *SQLiteStore) RedeemPairingCode(ctx context.Context, codeID string, d store.Device, at time.Time) (store.Device, bool, error) {
	if err := s.prepareDevice(ctx, &d); err != nil {
		return d, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return d, false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE pairing_codes SET redeemed_at = ?, device_id = ?
		WHERE id = ? AND redeemed_at IS NULL AND denied_at IS NULL AND expires_at > ?
		AND (purpose = ? OR approved_at IS NOT NULL)`,
		formatTime(at), d.ID, codeID, formatTime(at), store.PairingPurposePair)
	if err != nil {
		return d, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return d, false, err
	}
	if err := insertDevice(ctx, tx, d); err != nil {
		return d, false, err
	}
	if err := tx.Commit(); err != nil {
		return d, false, err
	}
	return d, true, nil
}

// PruneAuth deletes stale pairing codes and dead devices.
func (s *SQLiteStore) PruneAuth(ctx context.Context, cutoff time.Time) error {
	c := formatTime(cutoff)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pairing_codes WHERE expires_at < ?`, c); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE revoked_at < ?`, c)
	return err
}
