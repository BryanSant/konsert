package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/store"
)

// Machine credentials and join codes (KON-RK9YD).

const machineCredCols = `id, sid, worker_id, source, created_at, last_used_at, revoked_at, revoked_reason`

func scanMachineCredential(sc interface{ Scan(...any) error }, extra ...any) (store.MachineCredential, error) {
	var (
		c             store.MachineCredential
		created       string
		used, revoked sql.NullString
	)
	dest := append([]any{&c.ID, &c.SID, &c.WorkerID, &c.Source, &created, &used, &revoked, &c.RevokedReason}, extra...)
	if err := sc.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.MachineCredential{}, store.ErrNotFound
		}
		return store.MachineCredential{}, err
	}
	c.CreatedAt = parseTime(created)
	c.LastUsedAt = scanNullTime(used)
	c.RevokedAt = scanNullTime(revoked)
	return c, nil
}

// CreateMachineCredential stores c, minting its id and mc- sid when empty.
func (s *SQLiteStore) CreateMachineCredential(ctx context.Context, c store.MachineCredential, secretHash string) (store.MachineCredential, error) {
	if c.WorkerID == "" || secretHash == "" {
		return c, errors.New("a machine credential needs a machine and a secret hash")
	}
	if err := s.assignIDs(ctx, ids.MachineCredential, &c.ID, &c.SID); err != nil {
		return c, err
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	c.LastUsedAt, c.RevokedAt, c.RevokedReason = nil, nil, ""
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO machine_credentials (id, sid, worker_id, secret_hash, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, c.SID, c.WorkerID, secretHash, c.Source, formatTime(c.CreatedAt))
	return c, err
}

// GetMachineCredential returns the credential with id and its secret hash.
func (s *SQLiteStore) GetMachineCredential(ctx context.Context, id string) (store.MachineCredential, string, error) {
	var hash string
	c, err := scanMachineCredential(s.db.QueryRowContext(ctx,
		`SELECT `+machineCredCols+`, secret_hash FROM machine_credentials WHERE id = ?`, id), &hash)
	return c, hash, err
}

// ListMachineCredentials lists workerID's credentials (all when ""), newest first.
func (s *SQLiteStore) ListMachineCredentials(ctx context.Context, workerID string) ([]store.MachineCredential, error) {
	q := `SELECT ` + machineCredCols + ` FROM machine_credentials`
	var args []any
	if workerID != "" {
		q += ` WHERE worker_id = ?`
		args = append(args, workerID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.MachineCredential
	for rows.Next() {
		c, err := scanMachineCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TouchMachineCredential records a successful authentication at at.
func (s *SQLiteStore) TouchMachineCredential(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machine_credentials SET last_used_at = ? WHERE id = ?`, formatTime(at), id)
	return err
}

// RevokeMachineCredentials revokes workerID's active credentials except exceptID.
func (s *SQLiteStore) RevokeMachineCredentials(ctx context.Context, workerID, exceptID, reason string, at time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE machine_credentials SET revoked_at = ?, revoked_reason = ?
		WHERE worker_id = ? AND revoked_at IS NULL AND id != ?`,
		formatTime(at), reason, workerID, exceptID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// RevokeMachineCredential revokes one credential.
func (s *SQLiteStore) RevokeMachineCredential(ctx context.Context, id, reason string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE machine_credentials SET revoked_at = ?, revoked_reason = ?
		WHERE id = ? AND revoked_at IS NULL`, formatTime(at), reason, id)
	return err
}

const joinCodeCols = `id, created_by, created_at, expires_at, used_at, worker_id`

func scanJoinCode(sc interface{ Scan(...any) error }) (store.MachineJoinCode, error) {
	var (
		c                store.MachineJoinCode
		created, expires string
		used             sql.NullString
	)
	if err := sc.Scan(&c.ID, &c.CreatedBy, &created, &expires, &used, &c.WorkerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.MachineJoinCode{}, store.ErrNotFound
		}
		return store.MachineJoinCode{}, err
	}
	c.CreatedAt = parseTime(created)
	c.ExpiresAt = parseTime(expires)
	c.UsedAt = scanNullTime(used)
	return c, nil
}

// joinCodeKeep is how long an expired join code is kept (for its status) before it is
// deleted.
const joinCodeKeep = 24 * time.Hour

// CreateMachineJoinCode stores c with the code's hash and prunes long-expired codes.
func (s *SQLiteStore) CreateMachineJoinCode(ctx context.Context, c store.MachineJoinCode, codeHash string) (store.MachineJoinCode, error) {
	if codeHash == "" || c.ExpiresAt.IsZero() {
		return c, errors.New("a join code needs a hash and an expiry")
	}
	if c.ID == "" {
		c.ID = ids.UUID()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	c.UsedAt, c.WorkerID = nil, ""
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM machine_join_codes WHERE expires_at < ?`,
		formatTime(c.CreatedAt.Add(-joinCodeKeep))); err != nil {
		return c, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO machine_join_codes (id, code_hash, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		c.ID, codeHash, c.CreatedBy, formatTime(c.CreatedAt), formatTime(c.ExpiresAt)); err != nil {
		return c, err
	}
	return c, tx.Commit()
}

// GetMachineJoinCode returns the join code with id.
func (s *SQLiteStore) GetMachineJoinCode(ctx context.Context, id string) (store.MachineJoinCode, error) {
	return scanJoinCode(s.db.QueryRowContext(ctx, `SELECT `+joinCodeCols+` FROM machine_join_codes WHERE id = ?`, id))
}

// RedeemMachineJoinCode marks the unused, unexpired code with codeHash as used by
// workerID in one statement, so a code can be redeemed only once.
func (s *SQLiteStore) RedeemMachineJoinCode(ctx context.Context, codeHash, workerID string, at time.Time) (store.MachineJoinCode, error) {
	return scanJoinCode(s.db.QueryRowContext(ctx, `
		UPDATE machine_join_codes SET used_at = ?, worker_id = ?
		WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?
		RETURNING `+joinCodeCols,
		formatTime(at), workerID, codeHash, formatTime(at)))
}

// CountActiveMachineJoinCodes counts unused codes that have not expired at at.
func (s *SQLiteStore) CountActiveMachineJoinCodes(ctx context.Context, at time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM machine_join_codes WHERE used_at IS NULL AND expires_at > ?`, formatTime(at)).Scan(&n)
	return n, err
}
