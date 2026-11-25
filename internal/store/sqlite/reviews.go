package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/store"
)

// Review decisions, delivery outcomes, PR tracking and Expedite flags (KON-H50YW).

const reviewCols = `id, sid, task_id, chat_id, decision, note, decided_by, decided_at,
	delivery_mode, delivery_status, delivery_message, merge_method, merge_sha,
	pr_number, pr_url, pr_state, pr_checks, pr_auto_merge, compare_url, pr_remote_url, pr_last_comment_at,
	follow_up_task_id, expedite, expedited_at, created_at, updated_at`

func scanReview(sc interface{ Scan(...any) error }) (store.Review, error) {
	var (
		r                           store.Review
		decided, expAt, lastComment sql.NullString
		created, updated            string
		expedite, prAutoMerge       int
	)
	if err := sc.Scan(&r.ID, &r.SID, &r.TaskID, &r.ChatID, &r.Decision, &r.Note, &r.DecidedBy, &decided,
		&r.DeliveryMode, &r.DeliveryStatus, &r.DeliveryMessage, &r.MergeMethod, &r.MergeSHA,
		&r.PRNumber, &r.PRURL, &r.PRState, &r.PRChecks, &prAutoMerge, &r.CompareURL, &r.PRRemoteURL, &lastComment,
		&r.FollowUpTaskID, &expedite, &expAt, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Review{}, store.ErrNotFound
		}
		return store.Review{}, err
	}
	r.DecidedAt = scanNullTime(decided)
	r.PRAutoMerge = prAutoMerge != 0
	r.PRLastCommentAt = scanNullTime(lastComment)
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
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO reviews (`+reviewCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			decision = excluded.decision, note = excluded.note, decided_by = excluded.decided_by,
			decided_at = excluded.decided_at,
			delivery_mode = excluded.delivery_mode, delivery_status = excluded.delivery_status,
			delivery_message = excluded.delivery_message, merge_method = excluded.merge_method,
			merge_sha = excluded.merge_sha, pr_number = excluded.pr_number, pr_url = excluded.pr_url,
			pr_state = excluded.pr_state, pr_checks = excluded.pr_checks, pr_auto_merge = excluded.pr_auto_merge,
			compare_url = excluded.compare_url, pr_remote_url = excluded.pr_remote_url,
			pr_last_comment_at = excluded.pr_last_comment_at,
			follow_up_task_id = excluded.follow_up_task_id, expedite = excluded.expedite,
			expedited_at = excluded.expedited_at, updated_at = excluded.updated_at
	`, r.ID, r.SID, r.TaskID, r.ChatID, r.Decision, r.Note, r.DecidedBy, nullTime(r.DecidedAt),
		r.DeliveryMode, r.DeliveryStatus, r.DeliveryMessage, r.MergeMethod, r.MergeSHA,
		r.PRNumber, r.PRURL, r.PRState, r.PRChecks, boolToInt(r.PRAutoMerge), r.CompareURL, r.PRRemoteURL, nullTime(r.PRLastCommentAt),
		r.FollowUpTaskID, boolToInt(r.Expedite), nullTime(r.ExpeditedAt),
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

// MarkReviewCommentSeen records a PR/MR review comment as processed for taskID.
// Returns true if inserted (new), false if it was already recorded.
func (s *SQLiteStore) MarkReviewCommentSeen(ctx context.Context, taskID, commentID string, at time.Time) (bool, error) {
	if taskID == "" || commentID == "" {
		return false, nil
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO review_seen_comments (task_id, comment_id, seen_at)
		VALUES (?, ?, ?)
	`, taskID, commentID, formatTime(at.UTC()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
