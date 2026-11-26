package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

const attemptSelect = `id, ticket_id, task_id, chat_id, attempt, worker_id, account_id, agent, model,
	queued_at, started_at, ended_at, queue_wait_s, duration_s, exit_code, status, failure_category,
	verify_cmd, verify_exit, verify_passed, verify_failed, tokens_in, tokens_out, cost_usd, git_json, substitution`

// UpsertAttempt inserts or updates the row for (ticket, attempt) or, when the
// attempt has no ticket, (task, attempt). A zero Attempt assigns the next number.
func (s *SQLiteStore) UpsertAttempt(ctx context.Context, a store.Attempt) (store.Attempt, error) {
	a.TaskID = strings.TrimSpace(a.TaskID)
	a.TicketID = strings.TrimSpace(a.TicketID)
	a.ChatID = strings.TrimSpace(a.ChatID)
	if a.TaskID == "" {
		return store.Attempt{}, fmt.Errorf("%w: task_id is required", store.ErrInvalidWork)
	}
	if !store.ValidAttemptStatus(a.Status) {
		return store.Attempt{}, fmt.Errorf("%w: unknown attempt status %q", store.ErrInvalidWork, a.Status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Attempt{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var out store.Attempt
	for try := 0; try < 2; try++ {
		out, err = upsertAttemptTx(ctx, tx, a)
		if err == nil || !isUniqueConstraint(err) || try == 1 {
			break
		}
	}
	if err != nil {
		return store.Attempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.Attempt{}, err
	}
	return out, nil
}

func upsertAttemptTx(ctx context.Context, tx *sql.Tx, a store.Attempt) (store.Attempt, error) {
	if a.Attempt < 1 {
		n, err := nextAttemptNumber(ctx, tx, a.TicketID, a.TaskID)
		if err != nil {
			return store.Attempt{}, err
		}
		a.Attempt = n
	}
	prev, found, err := findAttemptTx(ctx, tx, a.TicketID, a.TaskID, a.Attempt)
	if err != nil {
		return store.Attempt{}, err
	}
	if found {
		// A lease expiry must not wipe an attempt that already finished.
		if a.Status == store.AttemptFailed && a.FailureCategory == protocol.ErrKindLost &&
			prev.Status != "" && prev.Status != store.AttemptNeedsYou {
			return prev, nil
		}
		merged := mergeAttempt(prev, a)
		if err := updateAttemptTx(ctx, tx, merged); err != nil {
			return store.Attempt{}, err
		}
		if err := raiseTicketAttempt(ctx, tx, merged.TicketID, merged.Attempt); err != nil {
			return store.Attempt{}, err
		}
		return loadAttemptTx(ctx, tx, merged.ID)
	}
	if a.ID == "" {
		a.ID = ids.UUID()
	}
	fillAttemptTimings(&a)
	if err := insertAttemptTx(ctx, tx, a); err != nil {
		return store.Attempt{}, err
	}
	if err := raiseTicketAttempt(ctx, tx, a.TicketID, a.Attempt); err != nil {
		return store.Attempt{}, err
	}
	return loadAttemptTx(ctx, tx, a.ID)
}

func nextAttemptNumber(ctx context.Context, tx *sql.Tx, ticketID, taskID string) (int, error) {
	var max sql.NullInt64
	var err error
	if ticketID != "" {
		err = tx.QueryRowContext(ctx, `SELECT MAX(attempt) FROM attempts WHERE ticket_id = ?`, ticketID).Scan(&max)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT MAX(attempt) FROM attempts WHERE task_id = ? AND ticket_id IS NULL`, taskID).Scan(&max)
	}
	if err != nil {
		return 0, err
	}
	n := int(max.Int64) + 1
	if n < 1 {
		n = 1
	}
	return n, nil
}

func findAttemptTx(ctx context.Context, tx *sql.Tx, ticketID, taskID string, n int) (store.Attempt, bool, error) {
	q := `SELECT ` + attemptSelect + ` FROM attempts WHERE task_id = ? AND ticket_id IS NULL AND attempt = ?`
	args := []any{taskID, n}
	if ticketID != "" {
		q = `SELECT ` + attemptSelect + ` FROM attempts WHERE ticket_id = ? AND attempt = ?`
		args = []any{ticketID, n}
	}
	row, err := scanAttempt(tx.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Attempt{}, false, nil
	}
	if err != nil {
		return store.Attempt{}, false, err
	}
	return row, true, nil
}

func loadAttemptTx(ctx context.Context, tx *sql.Tx, id string) (store.Attempt, error) {
	return scanAttempt(tx.QueryRowContext(ctx, `SELECT `+attemptSelect+` FROM attempts WHERE id = ?`, id))
}

func insertAttemptTx(ctx context.Context, tx *sql.Tx, a store.Attempt) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO attempts (
			id, ticket_id, task_id, chat_id, attempt,
			worker_id, account_id, agent, model,
			queued_at, started_at, ended_at,
			queue_wait_s, duration_s, exit_code,
			status, failure_category,
			verify_cmd, verify_exit, verify_passed, verify_failed,
			tokens_in, tokens_out, cost_usd, git_json, substitution
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, a.ID, nullString(a.TicketID), a.TaskID, nullString(a.ChatID), a.Attempt,
		a.WorkerID, a.AccountID, a.Agent, a.Model,
		optTime(a.QueuedAt), optTime(a.StartedAt), optTime(a.EndedAt),
		nullInt64(a.QueueWaitS), nullInt64(a.DurationS), nullInt(a.ExitCode),
		a.Status, a.FailureCategory,
		a.VerifyCmd, nullInt(a.VerifyExit), nullInt(a.VerifyPassed), nullInt(a.VerifyFailed),
		a.TokensIn, a.TokensOut, nullFloat(a.CostUSD), a.GitJSON, a.Substitution)
	return err
}

func updateAttemptTx(ctx context.Context, tx *sql.Tx, a store.Attempt) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE attempts SET
			task_id = ?, chat_id = ?, worker_id = ?, account_id = ?, agent = ?, model = ?,
			queued_at = ?, started_at = ?, ended_at = ?,
			queue_wait_s = ?, duration_s = ?, exit_code = ?,
			status = ?, failure_category = ?,
			verify_cmd = ?, verify_exit = ?, verify_passed = ?, verify_failed = ?,
			tokens_in = ?, tokens_out = ?, cost_usd = ?, git_json = ?, substitution = ?
		WHERE id = ?
	`, a.TaskID, nullString(a.ChatID), a.WorkerID, a.AccountID, a.Agent, a.Model,
		optTime(a.QueuedAt), optTime(a.StartedAt), optTime(a.EndedAt),
		nullInt64(a.QueueWaitS), nullInt64(a.DurationS), nullInt(a.ExitCode),
		a.Status, a.FailureCategory,
		a.VerifyCmd, nullInt(a.VerifyExit), nullInt(a.VerifyPassed), nullInt(a.VerifyFailed),
		a.TokensIn, a.TokensOut, nullFloat(a.CostUSD), a.GitJSON, a.Substitution, a.ID)
	return err
}

func raiseTicketAttempt(ctx context.Context, tx *sql.Tx, ticketID string, n int) error {
	if ticketID == "" || n < 1 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE tickets SET attempt = ?, updated_at = ? WHERE id = ? AND attempt < ?
	`, n, formatTime(time.Now().UTC()), ticketID, n)
	return err
}

// mergeAttempt keeps a prior row's identity and any field the new write leaves blank,
// so a late open-row insert cannot wipe a finished outcome and a lost update cannot
// wipe tokens that were already recorded.
func mergeAttempt(prev, next store.Attempt) store.Attempt {
	out := prev
	if out.TaskID == "" {
		out.TaskID = next.TaskID
	}
	if next.ChatID != "" {
		out.ChatID = next.ChatID
	}
	if next.WorkerID != "" {
		out.WorkerID = next.WorkerID
	}
	if next.AccountID != "" {
		out.AccountID = next.AccountID
	}
	if next.Agent != "" {
		out.Agent = next.Agent
	}
	if next.Model != "" {
		out.Model = next.Model
	}
	if out.QueuedAt == nil {
		out.QueuedAt = next.QueuedAt
	}
	if out.StartedAt == nil {
		out.StartedAt = next.StartedAt
	}
	if next.EndedAt != nil {
		out.EndedAt = next.EndedAt
	}
	if next.Status != "" {
		out.Status = next.Status
	}
	if next.FailureCategory != "" {
		out.FailureCategory = next.FailureCategory
	}
	if next.ExitCode != nil {
		out.ExitCode = next.ExitCode
	}
	if next.VerifyExit != nil || next.VerifyCmd != "" {
		out.VerifyCmd = next.VerifyCmd
		out.VerifyExit = next.VerifyExit
		out.VerifyPassed = next.VerifyPassed
		out.VerifyFailed = next.VerifyFailed
	}
	if next.TokensIn > 0 || next.TokensOut > 0 || next.CostUSD != nil {
		out.TokensIn = next.TokensIn
		out.TokensOut = next.TokensOut
		out.CostUSD = next.CostUSD
	}
	if next.GitJSON != "" && next.GitJSON != "{}" {
		out.GitJSON = next.GitJSON
	}
	if next.Substitution != "" {
		out.Substitution = next.Substitution
	}
	if next.DurationS != nil {
		out.DurationS = next.DurationS
	}
	if next.QueueWaitS != nil {
		out.QueueWaitS = next.QueueWaitS
	}
	fillAttemptTimings(&out)
	return out
}

func fillAttemptTimings(a *store.Attempt) {
	if a.QueueWaitS == nil && a.QueuedAt != nil && a.StartedAt != nil && !a.QueuedAt.IsZero() && !a.StartedAt.IsZero() {
		w := int64(a.StartedAt.Sub(*a.QueuedAt).Seconds())
		if w < 0 {
			w = 0
		}
		a.QueueWaitS = &w
	}
	if a.DurationS == nil && a.StartedAt != nil && a.EndedAt != nil && !a.StartedAt.IsZero() && !a.EndedAt.IsZero() {
		d := int64(a.EndedAt.Sub(*a.StartedAt).Seconds())
		if d < 0 {
			d = 0
		}
		a.DurationS = &d
	}
}

// ListAttempts returns the ticket's attempts when ticketID is set, otherwise the task's.
func (s *SQLiteStore) ListAttempts(ctx context.Context, ticketID, taskID string) ([]store.Attempt, error) {
	ticketID = strings.TrimSpace(ticketID)
	taskID = strings.TrimSpace(taskID)
	var (
		q   string
		arg string
	)
	switch {
	case ticketID != "":
		q = `SELECT ` + attemptSelect + ` FROM attempts WHERE ticket_id = ? ORDER BY attempt ASC, id ASC`
		arg = ticketID
	case taskID != "":
		q = `SELECT ` + attemptSelect + ` FROM attempts WHERE task_id = ? ORDER BY attempt ASC, id ASC`
		arg = taskID
	default:
		return nil, fmt.Errorf("%w: ticket_id or task_id is required", store.ErrInvalidWork)
	}
	rows, err := s.db.QueryContext(ctx, q, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.Attempt{}
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type attemptScanner interface {
	Scan(dest ...any) error
}

func scanAttempt(sc attemptScanner) (store.Attempt, error) {
	var a store.Attempt
	var ticketID, chatID, queued, started, ended sql.NullString
	var queueWait, duration, exitCode sql.NullInt64
	var verifyExit, verifyPassed, verifyFailed sql.NullInt64
	var cost sql.NullFloat64
	err := sc.Scan(
		&a.ID, &ticketID, &a.TaskID, &chatID, &a.Attempt, &a.WorkerID, &a.AccountID, &a.Agent, &a.Model,
		&queued, &started, &ended, &queueWait, &duration, &exitCode, &a.Status, &a.FailureCategory,
		&a.VerifyCmd, &verifyExit, &verifyPassed, &verifyFailed, &a.TokensIn, &a.TokensOut, &cost, &a.GitJSON,
		&a.Substitution,
	)
	if err != nil {
		return store.Attempt{}, err
	}
	if ticketID.Valid {
		a.TicketID = ticketID.String
	}
	if chatID.Valid {
		a.ChatID = chatID.String
	}
	a.QueuedAt = parseTimePtr(queued)
	a.StartedAt = parseTimePtr(started)
	a.EndedAt = parseTimePtr(ended)
	a.QueueWaitS = nullInt64Ptr(queueWait)
	a.DurationS = nullInt64Ptr(duration)
	a.ExitCode = nullIntPtr(exitCode)
	a.VerifyExit = nullIntPtr(verifyExit)
	a.VerifyPassed = nullIntPtr(verifyPassed)
	a.VerifyFailed = nullIntPtr(verifyFailed)
	if cost.Valid {
		v := cost.Float64
		a.CostUSD = &v
	}
	return a, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func optTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(*t)
}

func nullInt(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}

func nullInt64(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}

func nullFloat(n *float64) any {
	if n == nil {
		return nil
	}
	return *n
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func nullIntPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}
