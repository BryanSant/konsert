package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

// ListMachineLabels implements store.Store: machine ID to its labels, in the order added.
func (s *SQLiteStore) ListMachineLabels(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT worker_id, label FROM machine_labels ORDER BY created_at, label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var w, l string
		if err := rows.Scan(&w, &l); err != nil {
			return nil, err
		}
		out[w] = append(out[w], l)
	}
	return out, rows.Err()
}

// AddMachineLabel implements store.Store. Adding a label the machine has (in any case)
// changes nothing.
func (s *SQLiteStore) AddMachineLabel(ctx context.Context, workerID, label string) error {
	var have int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM machine_labels WHERE worker_id = ? AND label = ? COLLATE NOCASE`, workerID, label).Scan(&have); err != nil {
		return err
	}
	if have > 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO machine_labels (worker_id, label, created_at) VALUES (?, ?, ?)`,
		workerID, label, formatTime(time.Now().UTC()))
	if err != nil && strings.Contains(err.Error(), "FOREIGN KEY") {
		return store.ErrNotFound
	}
	return err
}

// RemoveMachineLabel implements store.Store (case-insensitive).
func (s *SQLiteStore) RemoveMachineLabel(ctx context.Context, workerID, label string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM machine_labels WHERE worker_id = ? AND label = ? COLLATE NOCASE`, workerID, label)
	return err
}

// ListMachineGroups implements store.Store, by name.
func (s *SQLiteStore) ListMachineGroups(ctx context.Context) ([]store.MachineGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_by, created_at FROM machine_groups ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	var out []store.MachineGroup
	for rows.Next() {
		var g store.MachineGroup
		var created string
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedBy, &created); err != nil {
			rows.Close()
			return nil, err
		}
		g.CreatedAt = parseTime(created)
		g.WorkerIDs = []string{}
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	members, err := s.groupMembers(ctx, "")
	if err != nil {
		return nil, err
	}
	for i := range out {
		if m := members[out[i].ID]; m != nil {
			out[i].WorkerIDs = m
		}
	}
	return out, nil
}

func (s *SQLiteStore) groupMembers(ctx context.Context, groupID string) (map[string][]string, error) {
	q := `SELECT group_id, worker_id FROM machine_group_members`
	var args []any
	if groupID != "" {
		q += ` WHERE group_id = ?`
		args = append(args, groupID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var g, w string
		if err := rows.Scan(&g, &w); err != nil {
			return nil, err
		}
		out[g] = append(out[g], w)
	}
	return out, rows.Err()
}

// GetMachineGroup implements store.Store.
func (s *SQLiteStore) GetMachineGroup(ctx context.Context, id string) (store.MachineGroup, error) {
	var g store.MachineGroup
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, created_by, created_at FROM machine_groups WHERE id = ?`, id).
		Scan(&g.ID, &g.Name, &g.CreatedBy, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return g, store.ErrNotFound
	}
	if err != nil {
		return g, err
	}
	g.CreatedAt = parseTime(created)
	members, err := s.groupMembers(ctx, id)
	if err != nil {
		return g, err
	}
	g.WorkerIDs = members[id]
	if g.WorkerIDs == nil {
		g.WorkerIDs = []string{}
	}
	return g, nil
}

// CreateMachineGroup implements store.Store. A name in use (in any case) is ErrConflict.
func (s *SQLiteStore) CreateMachineGroup(ctx context.Context, g store.MachineGroup) (store.MachineGroup, error) {
	if g.ID == "" {
		g.ID = ids.UUID()
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return g, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO machine_groups (id, name, created_by, created_at) VALUES (?, ?, ?, ?)`,
		g.ID, g.Name, g.CreatedBy, formatTime(g.CreatedAt)); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return g, store.ErrConflict
		}
		return g, err
	}
	if err := setGroupMembers(ctx, tx, g.ID, g.WorkerIDs); err != nil {
		return g, err
	}
	return g, tx.Commit()
}

// UpdateMachineGroup implements store.Store: renames the group and replaces its members.
func (s *SQLiteStore) UpdateMachineGroup(ctx context.Context, g store.MachineGroup) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE machine_groups SET name = ? WHERE id = ?`, g.Name, g.ID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return store.ErrConflict
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM machine_group_members WHERE group_id = ?`, g.ID); err != nil {
		return err
	}
	if err := setGroupMembers(ctx, tx, g.ID, g.WorkerIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func setGroupMembers(ctx context.Context, tx *sql.Tx, groupID string, workerIDs []string) error {
	for _, w := range workerIDs {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO machine_group_members (group_id, worker_id) VALUES (?, ?)`, groupID, w); err != nil {
			if strings.Contains(err.Error(), "FOREIGN KEY") {
				return store.ErrNotFound
			}
			return err
		}
	}
	return nil
}

// DeleteMachineGroup implements store.Store.
func (s *SQLiteStore) DeleteMachineGroup(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM machine_groups WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetChatScope implements store.Store.
func (s *SQLiteStore) GetChatScope(ctx context.Context, chatID string) (store.MachineTarget, error) {
	return s.getScope(ctx, `SELECT scope_json FROM chat_scopes WHERE chat_id = ?`, chatID)
}

// SetChatScope implements store.Store. All machines removes the stored scope.
func (s *SQLiteStore) SetChatScope(ctx context.Context, chatID string, t store.MachineTarget) error {
	return s.setScope(ctx, "chat_scopes", "chat_id", chatID, t)
}

// GetTicketScope implements store.Store.
func (s *SQLiteStore) GetTicketScope(ctx context.Context, ticketID string) (store.MachineTarget, error) {
	return s.getScope(ctx, `SELECT scope_json FROM ticket_scopes WHERE ticket_id = ?`, ticketID)
}

// SetTicketScope implements store.Store. All machines removes the stored Run on.
func (s *SQLiteStore) SetTicketScope(ctx context.Context, ticketID string, t store.MachineTarget) error {
	return s.setScope(ctx, "ticket_scopes", "ticket_id", ticketID, t)
}

func (s *SQLiteStore) getScope(ctx context.Context, q, id string) (store.MachineTarget, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, q, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return store.MachineTarget{Kind: store.TargetAll}, nil
	}
	if err != nil {
		return store.MachineTarget{}, err
	}
	var t store.MachineTarget
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return store.MachineTarget{}, err
	}
	return t, nil
}

// setScope stores t for id in table (keyed by col); all machines deletes the row.
func (s *SQLiteStore) setScope(ctx context.Context, table, col, id string, t store.MachineTarget) error {
	if t.IsAll() {
		_, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+col+` = ?`, id)
		return err
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+table+` (`+col+`, scope_json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(`+col+`) DO UPDATE SET scope_json = excluded.scope_json, updated_at = excluded.updated_at`,
		id, string(raw), formatTime(time.Now().UTC()))
	if err != nil && strings.Contains(err.Error(), "FOREIGN KEY") {
		return store.ErrNotFound
	}
	return err
}

// GetChatWorkFolder implements store.Store.
func (s *SQLiteStore) GetChatWorkFolder(ctx context.Context, chatID string) (string, error) {
	var f string
	err := s.db.QueryRowContext(ctx, `SELECT folder FROM chat_work_folders WHERE chat_id = ?`, chatID).Scan(&f)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return f, err
}

// SetChatWorkFolder implements store.Store; "" goes back to the fleet default.
func (s *SQLiteStore) SetChatWorkFolder(ctx context.Context, chatID, folder string) error {
	if folder == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM chat_work_folders WHERE chat_id = ?`, chatID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_work_folders (chat_id, folder, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET folder = excluded.folder, updated_at = excluded.updated_at`,
		chatID, folder, formatTime(time.Now().UTC()))
	if err != nil && strings.Contains(err.Error(), "FOREIGN KEY") {
		return store.ErrNotFound
	}
	return err
}
