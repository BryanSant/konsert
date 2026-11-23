package sqlite

import (
	"context"
	"time"

	"github.com/BryanSant/konsert/internal/store"
)

// UpsertHeatSample stores one worker's 1-minute heat bucket, merging into a bucket already
// stored for that minute (a partial minute flushed at shutdown or before a reconnect): max
// of maxes, cause of the larger max, and the mean weighted by sample count. SQLite
// evaluates every SET expression against the old row, so the order below is safe.
func (s *SQLiteStore) UpsertHeatSample(ctx context.Context, h store.HeatSample) error {
	n := h.Samples
	if n < 1 {
		n = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO worker_heat_samples (worker_id, bucket_start, score_max, score_avg, samples, cause)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (worker_id, bucket_start) DO UPDATE SET
			score_avg = (score_avg * samples + excluded.score_avg * excluded.samples) / (samples + excluded.samples),
			cause     = CASE WHEN excluded.score_max > score_max THEN excluded.cause ELSE cause END,
			score_max = MAX(score_max, excluded.score_max),
			samples   = samples + excluded.samples
	`, h.WorkerID, h.BucketStart.UTC().Truncate(time.Minute).Unix(), h.ScoreMax, h.ScoreAvg, n, h.Cause)
	return err
}

// ListHeatSamples returns every worker's heat buckets starting in [since, until), oldest
// first (walks idx_worker_heat_samples_bucket).
func (s *SQLiteStore) ListHeatSamples(ctx context.Context, since, until time.Time) ([]store.HeatSample, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT worker_id, bucket_start, score_max, score_avg, samples, cause
		FROM worker_heat_samples
		WHERE bucket_start >= ? AND bucket_start < ?
		ORDER BY bucket_start, worker_id
	`, since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.HeatSample
	for rows.Next() {
		var h store.HeatSample
		var start int64
		if err := rows.Scan(&h.WorkerID, &start, &h.ScoreMax, &h.ScoreAvg, &h.Samples, &h.Cause); err != nil {
			return nil, err
		}
		h.BucketStart = time.Unix(start, 0).UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}

// PruneHeatSamples deletes heat buckets starting before olderThan.
func (s *SQLiteStore) PruneHeatSamples(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM worker_heat_samples WHERE bucket_start < ?`, olderThan.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
