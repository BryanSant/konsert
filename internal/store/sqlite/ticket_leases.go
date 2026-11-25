package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

// ClaimTicket atomically transitions a dispatchable ticket from ready -> running and
// attaches a lease (expires_at = now + ttl, default 10m) in a single SQLite transaction.
// A ticket is dispatchable only if:
//   - status = 'ready', non-container kind, and not currently leased;
//   - it has at least one repo binding;
//   - every blocked_by ticket is done (or review when the project's autonomy has optimistic_chaining=true);
//   - no active hold applies to workerID or to any task linked to the ticket (unless lifted by an active approved override).
func (s *SQLiteStore) ClaimTicket(ctx context.Context, ticketID, workerID, taskID string, now time.Time, ttl time.Duration) (store.Ticket, bool, error) {
	if strings.TrimSpace(ticketID) == "" || strings.TrimSpace(workerID) == "" {
		return store.Ticket{}, false, fmt.Errorf("%w: ticket_id and worker_id are required", store.ErrInvalidWork)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if ttl <= 0 {
		ttl = store.DefaultTicketLeaseTTL
	}
	nowStr := formatTime(now)
	expiresStr := formatTime(now.Add(ttl))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Ticket{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE tickets
		SET status = 'running',
		    lease_worker_id = ?,
		    lease_task_id = ?,
		    lease_expires_at = ?,
		    lease_heartbeat_at = ?,
		    updated_at = ?
		WHERE id = ?
		  AND status = 'ready'
		  AND kind NOT IN ('initiative', 'epic')
		  AND lease_worker_id = ''
		  AND EXISTS (
		      SELECT 1 FROM ticket_repo_bindings b
		      WHERE b.ticket_id = tickets.id
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM ticket_edges e
		      JOIN tickets blk ON blk.id = e.blocked_by_id
		      JOIN projects p ON p.id = tickets.project_id
		      WHERE e.ticket_id = tickets.id
		        AND blk.status != 'done'
		        AND NOT (
		            blk.status = 'review'
		            AND COALESCE(json_extract(p.autonomy_json, '$.optimistic_chaining'), 0) = 1
		        )
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM holds h
		      WHERE h.released_at IS NULL
		        AND (
		            (h.scope_kind = 'machine' AND h.worker_id = ? AND h.resource_json NOT LIKE '%"kind":"restarts"%')
		            OR (h.scope_kind = 'session' AND h.task_id != '' AND h.task_id IN (SELECT id FROM tasks WHERE ticket_id = tickets.id))
		        )
		        AND NOT EXISTS (
		            SELECT 1 FROM override_requests o
		            WHERE o.hold_id = h.id
		              AND o.status = 'approved'
		              AND o.ended_at IS NULL
		              AND (o.expires_at IS NULL OR o.expires_at > ?)
		        )
		  )
	`, workerID, taskID, expiresStr, nowStr, nowStr, ticketID, workerID, nowStr)
	if err != nil {
		return store.Ticket{}, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.Ticket{}, false, nil
	}
	tk, err := scanTicket(tx.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE id = ?`, ticketID))
	if err != nil {
		return store.Ticket{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return store.Ticket{}, false, err
	}
	return tk, true, nil
}

// ReleaseTicket clears a ticket's lease and optionally transitions it to nextStatus.
// Returns the updated ticket and whether the ticket's lease or status changed.
func (s *SQLiteStore) ReleaseTicket(ctx context.Context, ticketID, nextStatus string) (store.Ticket, bool, error) {
	nextStatus = strings.ToLower(strings.TrimSpace(nextStatus))
	if nextStatus != "" && !store.ValidTicketStatus(nextStatus) {
		return store.Ticket{}, false, fmt.Errorf("%w: unknown status %q", store.ErrInvalidWork, nextStatus)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Ticket{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	cur, err := scanTicket(tx.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE id = ?`, ticketID))
	if err != nil {
		return store.Ticket{}, false, err
	}
	targetStatus := cur.Status
	if nextStatus != "" {
		targetStatus = nextStatus
	}
	changed := cur.Lease != nil || cur.Status != targetStatus
	if changed {
		nowStr := formatTime(time.Now().UTC())
		if _, err := tx.ExecContext(ctx, `
			UPDATE tickets
			SET status = ?,
			    lease_worker_id = '',
			    lease_task_id = '',
			    lease_expires_at = NULL,
			    lease_heartbeat_at = NULL,
			    updated_at = ?
			WHERE id = ?
		`, targetStatus, nowStr, ticketID); err != nil {
			return store.Ticket{}, false, err
		}
		cur, err = scanTicket(tx.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE id = ?`, ticketID))
		if err != nil {
			return store.Ticket{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return store.Ticket{}, false, err
	}
	return cur, changed, nil
}

// RenewLeases extends lease_expires_at and lease_heartbeat_at for active leases held by workerID.
func (s *SQLiteStore) RenewLeases(ctx context.Context, workerID string, ticketIDs []string, now time.Time, ttl time.Duration) (int, error) {
	if strings.TrimSpace(workerID) == "" || len(ticketIDs) == 0 {
		return 0, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if ttl <= 0 {
		ttl = store.DefaultTicketLeaseTTL
	}
	nowStr := formatTime(now)
	expiresStr := formatTime(now.Add(ttl))

	placeholders := make([]string, len(ticketIDs))
	args := make([]any, 0, len(ticketIDs)+3)
	args = append(args, expiresStr, nowStr, workerID)
	for i, id := range ticketIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE tickets
		SET lease_expires_at = ?,
		    lease_heartbeat_at = ?
		WHERE lease_worker_id = ?
		  AND status IN ('running', 'needs_you')
		  AND id IN (`+strings.Join(placeholders, ",")+`)
	`, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ExpireLeases transitions every active leased ticket whose lease_expires_at <= now
// according to its orphan_policy: 'requeue' -> 'ready', 'fail' -> 'failed'.
func (s *SQLiteStore) ExpireLeases(ctx context.Context, now time.Time) ([]store.ExpiredTicketLease, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	nowStr := formatTime(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT `+ticketColumns+`
		FROM tickets
		WHERE lease_expires_at IS NOT NULL
		  AND lease_expires_at <= ?
		  AND status IN ('running', 'needs_you')
		ORDER BY lease_expires_at ASC
	`, nowStr)
	if err != nil {
		return nil, err
	}
	var candidates []store.Ticket
	for rows.Next() {
		tk, err := scanTicket(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		candidates = append(candidates, tk)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return []store.ExpiredTicketLease{}, nil
	}

	out := make([]store.ExpiredTicketLease, 0, len(candidates))
	for _, tk := range candidates {
		nextStatus := store.TicketStatusReady
		if tk.OrphanPolicy == store.TicketOrphanPolicyFail {
			nextStatus = store.TicketStatusFailed
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tickets
			SET status = ?,
			    lease_worker_id = '',
			    lease_task_id = '',
			    lease_expires_at = NULL,
			    lease_heartbeat_at = NULL,
			    updated_at = ?
			WHERE id = ?
		`, nextStatus, nowStr, tk.ID); err != nil {
			return nil, err
		}
		updated, err := scanTicket(tx.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE id = ?`, tk.ID))
		if err != nil {
			return nil, err
		}
		var wid, tid string
		if tk.Lease != nil {
			wid = tk.Lease.WorkerID
			tid = tk.Lease.TaskID
		}
		out = append(out, store.ExpiredTicketLease{
			Ticket:      updated,
			PriorStatus: tk.Status,
			WorkerID:    wid,
			TaskID:      tid,
		})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListDispatchableTickets returns tickets that are ready, non-container, unleased,
// bound to at least one repo, unblocked (respecting the project's optimistic_chaining),
// and not covered by an active session hold, ordered by priority (P0 first) then created_at.
func (s *SQLiteStore) ListDispatchableTickets(ctx context.Context, projectID string, limit int) ([]store.Ticket, error) {
	nowStr := formatTime(time.Now().UTC())
	conds := []string{
		"t.status = 'ready'",
		"t.kind NOT IN ('initiative', 'epic')",
		"t.lease_worker_id = ''",
		"EXISTS (SELECT 1 FROM ticket_repo_bindings b WHERE b.ticket_id = t.id)",
		`NOT EXISTS (
			SELECT 1 FROM ticket_edges e
			JOIN tickets blk ON blk.id = e.blocked_by_id
			WHERE e.ticket_id = t.id
			  AND blk.status != 'done'
			  AND NOT (
			      blk.status = 'review'
			      AND COALESCE(json_extract(p.autonomy_json, '$.optimistic_chaining'), 0) = 1
			  )
		)`,
		`NOT EXISTS (
			SELECT 1 FROM holds h
			JOIN tasks tk ON tk.id = h.task_id
			WHERE h.released_at IS NULL
			  AND h.scope_kind = 'session'
			  AND tk.ticket_id = t.id
			  AND NOT EXISTS (
			      SELECT 1 FROM override_requests o
			      WHERE o.hold_id = h.id
			        AND o.status = 'approved'
			        AND o.ended_at IS NULL
			        AND (o.expires_at IS NULL OR o.expires_at > ?)
			  )
		)`,
	}
	args := []any{nowStr}
	if strings.TrimSpace(projectID) != "" {
		conds = append(conds, "t.project_id = ?")
		args = append(args, projectID)
	}
	q := `SELECT ` + ticketColumnsAliased + `
		FROM tickets t
		JOIN projects p ON p.id = t.project_id
		WHERE ` + strings.Join(conds, " AND ") + `
		ORDER BY t.priority ASC, t.created_at ASC`
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []store.Ticket{}
	for rows.Next() {
		tk, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tk)
	}
	return out, rows.Err()
}

// ListLeasedTicketsByWorker returns active leased tickets currently held by workerID.
func (s *SQLiteStore) ListLeasedTicketsByWorker(ctx context.Context, workerID string) ([]store.Ticket, error) {
	if strings.TrimSpace(workerID) == "" {
		return []store.Ticket{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+ticketColumns+`
		FROM tickets
		WHERE lease_worker_id = ?
		  AND status IN ('running', 'needs_you')
		ORDER BY created_at ASC
	`, workerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []store.Ticket{}
	for rows.Next() {
		tk, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tk)
	}
	return out, rows.Err()
}
