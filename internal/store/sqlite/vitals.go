package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/BryanSant/konsert/internal/store"
)

// Machine vitals and heat (KON-WY5F2): raw 15-second buckets (machine_vitals, 12 h),
// hourly and daily roll-ups (machine_vitals_rollup, 30 / 365 days). See 001_schema.sql.

// UpsertVitals stores raw samples in one transaction, merging into buckets already stored
// (a partial bucket flushed at shutdown): means weighted by sample count, the larger heat
// max and its cause. SQLite evaluates every SET expression against the old row, so the
// order below is safe. Samples of machines that no longer exist are skipped.
func (s *SQLiteStore) UpsertVitals(ctx context.Context, samples []store.VitalsSample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO machine_vitals (worker_id, t, n, cpu, load, mem, mem_total, swap, gpu_n, gpu, heat, heat_max, cause)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM workers WHERE id = ?)
		ON CONFLICT (worker_id, t) DO UPDATE SET
			cpu       = (cpu * n + excluded.cpu * excluded.n) / (n + excluded.n),
			load      = (load * n + excluded.load * excluded.n) / (n + excluded.n),
			mem       = (mem * n + excluded.mem * excluded.n) / (n + excluded.n),
			swap      = (swap * n + excluded.swap * excluded.n) / (n + excluded.n),
			mem_total = excluded.mem_total,
			gpu       = CASE WHEN gpu_n + excluded.gpu_n = 0 THEN NULL
			            ELSE (COALESCE(gpu, 0) * gpu_n + COALESCE(excluded.gpu, 0) * excluded.gpu_n) / (gpu_n + excluded.gpu_n) END,
			gpu_n     = gpu_n + excluded.gpu_n,
			heat      = (heat * n + excluded.heat * excluded.n) / (n + excluded.n),
			cause     = CASE WHEN excluded.heat_max > heat_max THEN excluded.cause ELSE cause END,
			heat_max  = MAX(heat_max, excluded.heat_max),
			n         = n + excluded.n
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	bucket := int64(store.VitalsBucket / time.Second)
	for _, v := range samples {
		n := max(v.N, 1)
		var gpu any
		gpuN := 0
		if v.GPU != nil {
			gpu, gpuN = *v.GPU, max(v.GPUN, 1)
		}
		t := v.At.Unix() / bucket * bucket
		if _, err := stmt.ExecContext(ctx, v.WorkerID, t, n, v.CPU, v.Load, v.Mem, int64(v.MemTotal), v.Swap,
			gpuN, gpu, v.Heat, v.HeatMax, v.Cause, v.WorkerID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListVitals returns every machine's raw samples in [since, until), oldest first.
func (s *SQLiteStore) ListVitals(ctx context.Context, since, until time.Time) ([]store.VitalsSample, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT worker_id, t, n, cpu, load, mem, mem_total, swap, gpu_n, gpu, heat, heat_max, cause
		FROM machine_vitals WHERE t >= ? AND t < ? ORDER BY t, worker_id
	`, since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.VitalsSample
	for rows.Next() {
		var v store.VitalsSample
		var t, memTotal int64
		var gpu sql.NullFloat64
		if err := rows.Scan(&v.WorkerID, &t, &v.N, &v.CPU, &v.Load, &v.Mem, &memTotal, &v.Swap, &v.GPUN, &gpu, &v.Heat, &v.HeatMax, &v.Cause); err != nil {
			return nil, err
		}
		v.At, v.MemTotal = time.Unix(t, 0).UTC(), uint64(max(memTotal, 0))
		if gpu.Valid {
			g := gpu.Float64
			v.GPU = &g
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListVitalsRollups returns every machine's roll-ups of period starting in [since, until),
// oldest first.
func (s *SQLiteStore) ListVitalsRollups(ctx context.Context, period time.Duration, since, until time.Time) ([]store.VitalsRollup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT worker_id, t, n, cpu_min, cpu_avg, cpu_max, load_min, load_avg, load_max,
		       mem_min, mem_avg, mem_max, mem_total, swap_min, swap_avg, swap_max,
		       gpu_n, gpu_min, gpu_avg, gpu_max, heat_min, heat_avg, heat_max, cause
		FROM machine_vitals_rollup WHERE period = ? AND t >= ? AND t < ? ORDER BY t, worker_id
	`, int64(period/time.Second), since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.VitalsRollup
	for rows.Next() {
		r := store.VitalsRollup{Period: period}
		var t, memTotal int64
		var gmin, gavg, gmax sql.NullFloat64
		if err := rows.Scan(&r.WorkerID, &t, &r.N, &r.CPU.Min, &r.CPU.Avg, &r.CPU.Max, &r.Load.Min, &r.Load.Avg, &r.Load.Max,
			&r.Mem.Min, &r.Mem.Avg, &r.Mem.Max, &memTotal, &r.Swap.Min, &r.Swap.Avg, &r.Swap.Max,
			&r.GPUN, &gmin, &gavg, &gmax, &r.Heat.Min, &r.Heat.Avg, &r.Heat.Max, &r.Cause); err != nil {
			return nil, err
		}
		r.At, r.MemTotal = time.Unix(t, 0).UTC(), uint64(max(memTotal, 0))
		if gavg.Valid {
			r.GPU = &store.VitalsStat{Min: gmin.Float64, Avg: gavg.Float64, Max: gmax.Float64}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// rollupColumns is the column list of machine_vitals_rollup inserts.
const rollupColumns = `worker_id, period, t, n, cpu_min, cpu_avg, cpu_max, load_min, load_avg, load_max,
	mem_min, mem_avg, mem_max, mem_total, swap_min, swap_avg, swap_max,
	gpu_n, gpu_min, gpu_avg, gpu_max, heat_min, heat_avg, heat_max, cause`

// RollupVitals recomputes hourly roll-ups from the raw tier and daily roll-ups from the
// hourly tier, then prunes every tier, in one transaction. Only complete periods are
// rolled up (the current hour and day stay in the finer tier until they end), and raw
// rows are pruned on an hour boundary, so the oldest raw hour is always complete: an
// hourly row is only ever recomputed from all of its samples.
func (s *SQLiteStore) RollupVitals(ctx context.Context, now time.Time) (store.VitalsRollupResult, error) {
	var res store.VitalsRollupResult
	hour := int64(store.VitalsHourly / time.Second)
	day := int64(store.VitalsDaily / time.Second)
	hourStart := now.Unix() / hour * hour
	dayStart := now.Unix() / day * day

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	// The earliest raw sample before the current hour: every complete hour from there on
	// is recomputed, and every complete day it touches.
	var minRaw sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MIN(t) FROM machine_vitals WHERE t < ?`, hourStart).Scan(&minRaw); err != nil {
		return res, err
	}
	if minRaw.Valid {
		r, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO machine_vitals_rollup (`+rollupColumns+`)
			SELECT worker_id, ?1, (t / ?1) * ?1, SUM(n),
				MIN(cpu), SUM(cpu * n) / SUM(n), MAX(cpu),
				MIN(load), SUM(load * n) / SUM(n), MAX(load),
				MIN(mem), SUM(mem * n) / SUM(n), MAX(mem), MAX(mem_total),
				MIN(swap), SUM(swap * n) / SUM(n), MAX(swap),
				SUM(gpu_n), MIN(gpu), SUM(COALESCE(gpu, 0) * gpu_n) / NULLIF(SUM(gpu_n), 0), MAX(gpu),
				MIN(heat), SUM(heat * n) / SUM(n), MAX(heat_max),
				(SELECT c.cause FROM machine_vitals c
				 WHERE c.worker_id = r.worker_id AND c.t >= (r.t / ?1) * ?1 AND c.t < (r.t / ?1) * ?1 + ?1
				 ORDER BY c.heat_max DESC, c.t LIMIT 1)
			FROM machine_vitals r
			WHERE t < ?2
			GROUP BY worker_id, t / ?1
		`, hour, hourStart)
		if err != nil {
			return res, err
		}
		n, _ := r.RowsAffected()
		res.Hourly = int(n)

		firstDay := minRaw.Int64 / day * day
		if firstDay < dayStart {
			r, err := tx.ExecContext(ctx, `
				INSERT OR REPLACE INTO machine_vitals_rollup (`+rollupColumns+`)
				SELECT worker_id, ?1, (t / ?1) * ?1, SUM(n),
					MIN(cpu_min), SUM(cpu_avg * n) / SUM(n), MAX(cpu_max),
					MIN(load_min), SUM(load_avg * n) / SUM(n), MAX(load_max),
					MIN(mem_min), SUM(mem_avg * n) / SUM(n), MAX(mem_max), MAX(mem_total),
					MIN(swap_min), SUM(swap_avg * n) / SUM(n), MAX(swap_max),
					SUM(gpu_n), MIN(gpu_min), SUM(COALESCE(gpu_avg, 0) * gpu_n) / NULLIF(SUM(gpu_n), 0), MAX(gpu_max),
					MIN(heat_min), SUM(heat_avg * n) / SUM(n), MAX(heat_max),
					(SELECT c.cause FROM machine_vitals_rollup c
					 WHERE c.worker_id = r.worker_id AND c.period = ?2 AND c.t >= (r.t / ?1) * ?1 AND c.t < (r.t / ?1) * ?1 + ?1
					 ORDER BY c.heat_max DESC, c.t LIMIT 1)
				FROM machine_vitals_rollup r
				WHERE period = ?2 AND t >= ?3 AND t < ?4
				GROUP BY worker_id, t / ?1
			`, day, hour, firstDay, dayStart)
			if err != nil {
				return res, err
			}
			n, _ := r.RowsAffected()
			res.Daily = int(n)
		}
	}

	for _, p := range []struct {
		query string
		args  []any
	}{
		{`DELETE FROM machine_vitals WHERE t < ?`, []any{hourStart - int64(store.VitalsRawKeep/time.Second)}},
		{`DELETE FROM machine_vitals_rollup WHERE period = ? AND t < ?`, []any{hour, hourStart - int64(store.VitalsHourlyKeep/time.Second)}},
		{`DELETE FROM machine_vitals_rollup WHERE period = ? AND t < ?`, []any{day, dayStart - int64(store.VitalsDailyKeep/time.Second)}},
	} {
		r, err := tx.ExecContext(ctx, p.query, p.args...)
		if err != nil {
			return res, err
		}
		n, _ := r.RowsAffected()
		res.Pruned += n
	}
	return res, tx.Commit()
}
