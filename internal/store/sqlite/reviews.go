package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/store"
)

// Review decisions and Expedite flags (KON-BM8JR).

const reviewCols = `id, sid, task_id, chat_id, decision, note, decided_by, decided_at,
	github_status, github_message, github_url, follow_up_task_id, expedite, expedited_at,
	created_at, updated_at`

func scanReview(sc interface{ Scan(...any) error }) (store.Review, error) {
	var (
		r                store.Review
		decided, expAt   sql.NullString
		created, updated string
		expedite         int
	)
	if err := sc.Scan(&r.ID, &r.SID, &r.TaskID, &r.ChatID, &r.Decision, &r.Note, &r.DecidedBy, &decided,
		&r.GitHubStatus, &r.GitHubMessage, &r.GitHubURL, &r.FollowUpTaskID, &expedite, &expAt,
		&created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Review{}, store.ErrNotFound
		}
		return store.Review{}, err
	}
	r.DecidedAt = scanNullTime(decided)
	r.Expedite = expedite != 0
	r.ExpeditedAt = scanNullTime(expAt)
	r.CreatedAt = parseTime(created)
	r.UpdatedAt = parseTime(updated)
	return r, nil
}

// GetReview returns the review of a task (taskID) or chat (chatID).
func (s *SQLiteStore) GetReview(ctx context.Context, taskID, chatID string) (store.Review, error) {
	switch {
	case taskID != "":
		return scanReview(s.db.QueryRowContext(ctx, `SELECT `+reviewCols+` FROM reviews WHERE task_id = ?`, taskID))
	case chatID != "":
		return scanReview(s.db.QueryRowContext(ctx, `SELECT `+reviewCols+` FROM reviews WHERE chat_id = ?`, chatID))
	}
	return store.Review{}, store.ErrNotFound
}

// SaveReview inserts the session's review or updates the existing one.
func (s *SQLiteStore) SaveReview(ctx context.Context, r store.Review) (store.Review, error) {
	if (r.TaskID == "") == (r.ChatID == "") {
		return r, errors.New("a review names exactly one task or chat")
	}
	now := time.Now().UTC()
	if cur, err := s.GetReview(ctx, r.TaskID, r.ChatID); err == nil {
		r.ID, r.SID, r.CreatedAt = cur.ID, cur.SID, cur.CreatedAt
	} else if !errors.Is(err, store.ErrNotFound) {
		return r, err
	} else {
		r.ID, r.SID = "", ""
		if err := s.assignIDs(ctx, ids.Review, &r.ID, &r.SID); err != nil {
			return r, err
		}
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	expedite := 0
	if r.Expedite {
		expedite = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO reviews (`+reviewCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			decision = excluded.decision, note = excluded.note, decided_by = excluded.decided_by,
			decided_at = excluded.decided_at, github_status = excluded.github_status,
			github_message = excluded.github_message, github_url = excluded.github_url,
			follow_up_task_id = excluded.follow_up_task_id, expedite = excluded.expedite,
			expedited_at = excluded.expedited_at, updated_at = excluded.updated_at
	`, r.ID, r.SID, r.TaskID, r.ChatID, r.Decision, r.Note, r.DecidedBy, nullTime(r.DecidedAt),
		r.GitHubStatus, r.GitHubMessage, r.GitHubURL, r.FollowUpTaskID, expedite, nullTime(r.ExpeditedAt),
		formatTime(r.CreatedAt), formatTime(r.UpdatedAt))
	return r, err
}

// ListReviews returns every review, oldest first.
func (s *SQLiteStore) ListReviews(ctx context.Context) ([]store.Review, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+reviewCols+` FROM reviews ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Review
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
