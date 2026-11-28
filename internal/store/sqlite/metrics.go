package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

// UpsertMetricSamples stores samples. A row already there for the same subject, period
// and start (a minute the hub wrote before a restart) is merged: counts add up,
// averages are re-weighted, maxima kept.
func (s *SQLiteStore) UpsertMetricSamples(ctx context.Context, samples []store.MetricSample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO metric_samples (subject, period, t, worker_id, n, cpu_avg, cpu_max, rss_avg, rss_max, procs_max)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM workers WHERE id = ?)
		ON CONFLICT(subject, period, t) DO UPDATE SET
			cpu_avg   = (metric_samples.cpu_avg * metric_samples.n + excluded.cpu_avg * excluded.n) / (metric_samples.n + excluded.n),
			rss_avg   = (metric_samples.rss_avg * metric_samples.n + excluded.rss_avg * excluded.n) / (metric_samples.n + excluded.n),
			n         = metric_samples.n + excluded.n,
			cpu_max   = MAX(metric_samples.cpu_max, excluded.cpu_max),
			rss_max   = MAX(metric_samples.rss_max, excluded.rss_max),
			procs_max = MAX(metric_samples.procs_max, excluded.procs_max)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range samples {
		if m.N <= 0 {
			continue
		}
		if _, err := stmt.ExecContext(ctx, m.Subject, m.Period, m.At.Unix(), m.WorkerID, m.N,
			m.CPUAvg, m.CPUMax, m.RSSAvg, m.RSSMax, m.ProcsMax, m.WorkerID); err != nil {
			return fmt.Errorf("store metric sample %s: %w", m.Subject, err)
		}
	}
	return tx.Commit()
}

// ListMetricSamples returns the rows f selects, oldest first.
func (s *SQLiteStore) ListMetricSamples(ctx context.Context, f store.MetricFilter) ([]store.MetricSample, error) {
	period := f.Period
	if period == 0 {
		period = store.MetricMinute
	}
	conds := []string{"period = ?"}
	args := []any{period}
	if len(f.Subjects) > 0 {
		conds = append(conds, "subject IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Subjects)), ",")+")")
		for _, sub := range f.Subjects {
			args = append(args, sub)
		}
	}
	if f.WorkerID != "" {
		conds = append(conds, "worker_id = ?")
		args = append(args, f.WorkerID)
	}
	if !f.Since.IsZero() {
		conds = append(conds, "t >= ?")
		args = append(args, f.Since.Unix())
	}
	if !f.Until.IsZero() {
		conds = append(conds, "t < ?")
		args = append(args, f.Until.Unix())
	}
	limit := f.Limit
	if limit <= 0 || limit > 100000 {
		limit = 100000
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT subject, period, t, worker_id, n, cpu_avg, cpu_max, rss_avg, rss_max, procs_max
		FROM metric_samples WHERE `+strings.Join(conds, " AND ")+` ORDER BY t, subject LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.MetricSample
	for rows.Next() {
		var m store.MetricSample
		var t int64
		if err := rows.Scan(&m.Subject, &m.Period, &t, &m.WorkerID, &m.N, &m.CPUAvg, &m.CPUMax, &m.RSSAvg, &m.RSSMax, &m.ProcsMax); err != nil {
			return nil, err
		}
		m.At = time.Unix(t, 0).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// RollupMetrics recomputes the hourly rows of the complete hours whose minutes are all
// still kept (so an hour at the retention edge is never recomputed from a partial
// minute set), then prunes minutes older than minuteKeep and hours older than
// hourlyKeep. Running it again rewrites the same values.
func (s *SQLiteStore) RollupMetrics(ctx context.Context, now time.Time, minuteKeep, hourlyKeep time.Duration) (store.MetricRollupResult, error) {
	var res store.MetricRollupResult
	hourEnd := now.UTC().Truncate(time.Hour).Unix()
	minuteCut := now.Add(-minuteKeep).UTC()
	hourStart := minuteCut.Truncate(time.Hour)
	if hourStart.Before(minuteCut) {
		hourStart = hourStart.Add(time.Hour)
	}
	r, err := s.db.ExecContext(ctx, `
		INSERT INTO metric_samples (subject, period, t, worker_id, n, cpu_avg, cpu_max, rss_avg, rss_max, procs_max)
		SELECT subject, 3600, (t / 3600) * 3600, MAX(worker_id), SUM(n),
		       SUM(cpu_avg * n) / SUM(n), MAX(cpu_max), SUM(rss_avg * n) / SUM(n), MAX(rss_max), MAX(procs_max)
		FROM metric_samples
		WHERE period = 60 AND t >= ? AND t < ?
		GROUP BY subject, t / 3600
		ON CONFLICT(subject, period, t) DO UPDATE SET
			n = excluded.n, cpu_avg = excluded.cpu_avg, cpu_max = excluded.cpu_max,
			rss_avg = excluded.rss_avg, rss_max = excluded.rss_max, procs_max = excluded.procs_max`,
		hourStart.Unix(), hourEnd)
	if err != nil {
		return res, fmt.Errorf("roll up metrics: %w", err)
	}
	res.Hourly, _ = r.RowsAffected()
	for _, p := range []struct {
		period int
		before time.Time
	}{{store.MetricMinute, minuteCut}, {store.MetricHour, now.Add(-hourlyKeep)}} {
		r, err := s.db.ExecContext(ctx, `DELETE FROM metric_samples WHERE period = ? AND t < ?`, p.period, p.before.Unix())
		if err != nil {
			return res, fmt.Errorf("prune metrics: %w", err)
		}
		n, _ := r.RowsAffected()
		res.Pruned += n
	}
	return res, nil
}

// RaiseTaskPeaks raises a task's recorded peaks (never lowers them).
func (s *SQLiteStore) RaiseTaskPeaks(ctx context.Context, taskID string, rssMB uint64, cpuPct float64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET peak_rss_mb = MAX(peak_rss_mb, ?), peak_cpu_pct = MAX(peak_cpu_pct, ?) WHERE id = ?`,
		rssMB, cpuPct, taskID)
	return err
}
