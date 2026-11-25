package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// SetTaskStartSpec persists the selector and full task.start payload for a task.
func (s *SQLiteStore) SetTaskStartSpec(ctx context.Context, taskID string, spec store.TaskStartSpec) error {
	pinned := 0
	if spec.WorkerPinned {
		pinned = 1
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET selector_json = ?, start_payload_json = ?, worker_pinned = ? WHERE id = ?
	`, spec.SelectorJSON, spec.PayloadJSON, pinned, taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetTaskStartSpec returns the persisted selector and start payload for a task.
// Tasks created before migration 009 return an empty spec.
func (s *SQLiteStore) GetTaskStartSpec(ctx context.Context, taskID string) (store.TaskStartSpec, error) {
	var (
		spec   store.TaskStartSpec
		pinned int
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT selector_json, start_payload_json, worker_pinned FROM tasks WHERE id = ?
	`, taskID).Scan(&spec.SelectorJSON, &spec.PayloadJSON, &pinned)
	if errors.Is(err, sql.ErrNoRows) {
		return store.TaskStartSpec{}, store.ErrNotFound
	}
	if err != nil {
		return store.TaskStartSpec{}, err
	}
	spec.WorkerPinned = pinned != 0
	return spec, nil
}

// ClaimQueuedTask atomically moves a queued task to assigned on workerID.
// It reports false when another scheduler run already claimed the task.
func (s *SQLiteStore) ClaimQueuedTask(ctx context.Context, taskID, workerID string, cause store.Cause) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status = ?, worker_id = ?, cause_json = ? WHERE id = ? AND status = ?
	`, protocol.TaskStatusAssigned, workerID, causeJSON(&cause), taskID, protocol.TaskStatusQueued)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RequeueTask returns a non-terminal task to queued. The worker assignment is
// cleared unless keepWorker is set (the worker was explicitly pinned).
// It reports false when the task is missing or already terminal.
func (s *SQLiteStore) RequeueTask(ctx context.Context, taskID, summary string, keepWorker bool, cause store.Cause) (bool, error) {
	keep := 0
	if keepWorker {
		keep = 1
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET status = ?,
		    worker_id = CASE WHEN ? = 1 THEN worker_id ELSE NULL END,
		    summary = CASE WHEN ? != '' THEN ? ELSE summary END,
		    cause_json = ?, suspended_by = ''
		WHERE id = ? AND status NOT IN (?, ?, ?)
	`, protocol.TaskStatusQueued, keep, summary, summary, causeJSON(&cause), taskID,
		protocol.TaskStatusCompleted, protocol.TaskStatusFailed, protocol.TaskStatusCanceled)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
