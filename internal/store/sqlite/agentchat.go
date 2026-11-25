package sqlite

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

const (
	defaultChatEventLimit = 1000
	maxChatEventLimit     = 5000
)

// AppendChatEvent stores one agent chat event. A duplicate (chat_id, seq) is
// ignored and reported as not inserted. The session's updated_at is bumped so
// the chat list keeps its most-recent-first order.
func (s *SQLiteStore) AppendChatEvent(ctx context.Context, e store.ChatEvent) (bool, error) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	ts := formatTime(e.CreatedAt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := spoolGuard(ctx, tx); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO chat_events (chat_id, seq, turn_id, kind, json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, e.ChatID, e.Seq, e.TurnID, e.Kind, e.JSON, ts)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	// compact_next_at marks the oldest event retention has not looked at yet.
	if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET updated_at = ?1, compact_next_at = COALESCE(compact_next_at, ?1) WHERE id = ?2`, ts, e.ChatID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListChatEvents returns a chat's events with seq > afterSeq in seq order.
func (s *SQLiteStore) ListChatEvents(ctx context.Context, chatID string, afterSeq int64, limit int) ([]store.ChatEvent, error) {
	if limit <= 0 {
		limit = defaultChatEventLimit
	}
	if limit > maxChatEventLimit {
		limit = maxChatEventLimit
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT chat_id, seq, turn_id, kind, json, created_at
		FROM chat_events WHERE chat_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?
	`, chatID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ChatEvent
	for rows.Next() {
		var e store.ChatEvent
		var ts string
		if err := rows.Scan(&e.ChatID, &e.Seq, &e.TurnID, &e.Kind, &e.JSON, &ts); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxChatEventSeq returns the highest stored seq of a chat (0 when it has none).
func (s *SQLiteStore) MaxChatEventSeq(ctx context.Context, chatID string) (int64, error) {
	var n sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM chat_events WHERE chat_id = ?`, chatID).Scan(&n); err != nil {
		return 0, err
	}
	return n.Int64, nil
}

// CoalesceChatTurn folds a finished turn's streamed deltas so storage stays small:
//
//   - message deltas are deleted when the turn has an EventMessage; otherwise they
//     are folded into one EventMessage stored at the first delta's seq;
//   - thought deltas are folded into a single thought_delta at the first one's seq.
func (s *SQLiteStore) CoalesceChatTurn(ctx context.Context, chatID, turnID string) error {
	if turnID == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var messages int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM chat_events WHERE chat_id = ? AND turn_id = ? AND kind = ?`,
		chatID, turnID, protocol.EventMessage).Scan(&messages); err != nil {
		return err
	}
	if messages > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_events WHERE chat_id = ? AND turn_id = ? AND kind = ?`,
			chatID, turnID, protocol.EventMessageDelta); err != nil {
			return err
		}
	} else if err := foldDeltas(ctx, tx, chatID, turnID, protocol.EventMessageDelta, protocol.EventMessage); err != nil {
		return err
	}
	if err := foldDeltas(ctx, tx, chatID, turnID, protocol.EventThoughtDelta, protocol.EventThoughtDelta); err != nil {
		return err
	}
	return tx.Commit()
}

// foldDeltas concatenates the text of a turn's deltaKind events into the first
// one (rewritten as kind intoKind) and deletes the rest.
func foldDeltas(ctx context.Context, tx *sql.Tx, chatID, turnID, deltaKind, intoKind string) error {
	rows, err := tx.QueryContext(ctx, `SELECT seq, json FROM chat_events
		WHERE chat_id = ? AND turn_id = ? AND kind = ? ORDER BY seq ASC`, chatID, turnID, deltaKind)
	if err != nil {
		return err
	}
	type row struct {
		seq int64
		raw string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.seq, &r.raw); err != nil {
			_ = rows.Close()
			return err
		}
		all = append(all, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(all) == 0 || (len(all) == 1 && deltaKind == intoKind) {
		return nil
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(all[0].raw), &first); err != nil {
		return err
	}
	var text strings.Builder
	for _, r := range all {
		var ev struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(r.raw), &ev) == nil {
			text.WriteString(ev.Text)
		}
	}
	first["kind"] = intoKind
	first["text"] = text.String()
	b, err := json.Marshal(first)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_events SET kind = ?, json = ? WHERE chat_id = ? AND seq = ?`,
		intoKind, string(b), chatID, all[0].seq); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM chat_events WHERE chat_id = ? AND turn_id = ? AND kind = ? AND seq > ?`,
		chatID, turnID, deltaKind, all[0].seq)
	return err
}

// UpsertAgentAccount inserts or replaces one worker account.
func (s *SQLiteStore) UpsertAgentAccount(ctx context.Context, a store.AgentAccountRecord) error {
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_accounts (worker_id, account_id, json, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(worker_id, account_id) DO UPDATE SET json = excluded.json, updated_at = excluded.updated_at
	`, a.WorkerID, a.AccountID, a.JSON, formatTime(a.UpdatedAt))
	return err
}

// ListAgentAccounts lists one worker's accounts, or all of them when workerID is "".
func (s *SQLiteStore) ListAgentAccounts(ctx context.Context, workerID string) ([]store.AgentAccountRecord, error) {
	q := `SELECT worker_id, account_id, json, updated_at FROM agent_accounts`
	var args []any
	if workerID != "" {
		q += ` WHERE worker_id = ?`
		args = append(args, workerID)
	}
	q += ` ORDER BY worker_id, account_id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.AgentAccountRecord
	for rows.Next() {
		var a store.AgentAccountRecord
		var ts string
		if err := rows.Scan(&a.WorkerID, &a.AccountID, &a.JSON, &ts); err != nil {
			return nil, err
		}
		a.UpdatedAt = parseTime(ts)
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAgentAccounts removes every account of a worker.
func (s *SQLiteStore) DeleteAgentAccounts(ctx context.Context, workerID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_accounts WHERE worker_id = ?`, workerID)
	return err
}
