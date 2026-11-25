package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

// Applied spool seqs per worker (SYR-VZR9F). See store/spool.go.

// GetSpoolState returns workerID's applied spool state.
func (s *SQLiteStore) GetSpoolState(ctx context.Context, workerID string) (store.SpoolState, error) {
	st := store.SpoolState{WorkerID: workerID}
	var applied int64
	var at string
	err := s.db.QueryRowContext(ctx, `SELECT epoch, applied_seq, updated_at FROM worker_spool WHERE worker_id = ?`, workerID).Scan(&st.Epoch, &applied, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.AppliedSeq, st.UpdatedAt = uint64(max(applied, 0)), parseTime(at)
	return st, nil
}

// ResetSpoolEpoch starts epoch for workerID with nothing applied.
func (s *SQLiteStore) ResetSpoolEpoch(ctx context.Context, workerID, epoch string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO worker_spool (worker_id, epoch, applied_seq, updated_at)
		SELECT ?1, ?2, 0, ?3 WHERE EXISTS (SELECT 1 FROM workers WHERE id = ?1)
		ON CONFLICT (worker_id) DO UPDATE SET epoch = excluded.epoch, applied_seq = 0, updated_at = excluded.updated_at
	`, workerID, epoch, formatTime(time.Now()))
	return err
}

// AdvanceSpoolApplied raises workerID's applied seq to seq.
func (s *SQLiteStore) AdvanceSpoolApplied(ctx context.Context, workerID string, seq uint64) error {
	_, err := advanceSpool(ctx, s.db, workerID, seq)
	return err
}

// advanceSpool raises workerID's applied seq to seq and reports whether it did (false:
// it was already at least seq, or the worker does not exist).
func advanceSpool(ctx context.Context, db execer, workerID string, seq uint64) (bool, error) {
	if workerID == "" || seq == 0 {
		return false, nil
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO worker_spool (worker_id, applied_seq, updated_at)
		SELECT ?1, ?2, ?3 WHERE EXISTS (SELECT 1 FROM workers WHERE id = ?1)
		ON CONFLICT (worker_id) DO UPDATE SET applied_seq = excluded.applied_seq, updated_at = excluded.updated_at
		WHERE worker_spool.applied_seq < excluded.applied_seq
	`, workerID, int64(seq), formatTime(time.Now()))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// spoolGuard applies the ctx's spool mark inside tx: the first guarded write of a record
// advances the worker's applied seq; a record already applied gets ErrSpoolDuplicate.
// Without a mark it does nothing.
func spoolGuard(ctx context.Context, tx *sql.Tx) error {
	m := store.SpoolMarkFrom(ctx)
	if m == nil || m.Claimed() {
		return nil
	}
	advanced, err := advanceSpool(ctx, tx, m.WorkerID, m.Seq)
	if err != nil {
		return err
	}
	if !advanced {
		var applied int64
		if err := tx.QueryRowContext(ctx, `SELECT applied_seq FROM worker_spool WHERE worker_id = ?`, m.WorkerID).Scan(&applied); err == nil && uint64(applied) >= m.Seq {
			return store.ErrSpoolDuplicate
		}
	}
	m.Claim()
	return nil
}

// guardedExec runs one statement, in a transaction guarded by the ctx's spool mark when
// there is one.
func (s *SQLiteStore) guardedExec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if store.SpoolMarkFrom(ctx) == nil {
		return s.db.ExecContext(ctx, query, args...)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := spoolGuard(ctx, tx); err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return res, tx.Commit()
}

// StoreLateVitals stores vitals sampled in the past at their original time (see
// store.SpoolStore).
func (s *SQLiteStore) StoreLateVitals(ctx context.Context, samples []store.VitalsSample, now time.Time) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := spoolGuard(ctx, tx); err != nil {
		return err
	}
	hour := int64(store.VitalsHourly / time.Second)
	day := int64(store.VitalsDaily / time.Second)
	hourStart := now.Unix() / hour * hour
	dayStart := now.Unix() / day * day
	// The raw tier keeps whole hours from rawFrom on (RollupVitals prunes on hour
	// boundaries), so a sample there is rolled up with the rest of its hour later.
	rawFrom := hourStart - int64(store.VitalsRawKeep/time.Second)
	hourlyFrom := hourStart - int64(store.VitalsHourlyKeep/time.Second)
	var raw []store.VitalsSample
	type dayKey struct {
		worker string
		day    int64
	}
	days := map[dayKey]bool{}
	for _, v := range samples {
		t := v.At.Unix()
		switch {
		case t >= rawFrom:
			raw = append(raw, v)
		case t >= hourlyFrom:
			if err := mergeRollup(ctx, tx, v, hour); err != nil {
				return err
			}
			if d := t / day * day; d < dayStart {
				days[dayKey{v.WorkerID, d}] = true
			}
		default:
			if err := mergeRollup(ctx, tx, v, day); err != nil {
				return err
			}
		}
		if _, err := advanceSpool(ctx, tx, v.WorkerID, v.SpoolSeq); err != nil {
			return err
		}
	}
	if err := upsertRawVitals(ctx, tx, raw); err != nil {
		return err
	}
	for k := range days {
		if err := recomputeDay(ctx, tx, k.worker, k.day); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// mergeRollup merges one sample into the roll-up row of period holding it: min, max and
// sample-weighted means.
func mergeRollup(ctx context.Context, tx *sql.Tx, v store.VitalsSample, period int64) error {
	n := max(v.N, 1)
	var gpu any
	gpuN := 0
	if v.GPU != nil {
		gpu, gpuN = *v.GPU, max(v.GPUN, 1)
	}
	t := v.At.Unix() / period * period
	_, err := tx.ExecContext(ctx, `
		INSERT INTO machine_vitals_rollup (`+rollupColumns+`)
		SELECT ?1, ?2, ?3, ?4, ?5, ?5, ?5, ?6, ?6, ?6, ?7, ?7, ?7, ?8, ?9, ?9, ?9, ?10, ?11, ?11, ?11, ?12, ?12, ?13, ?14
		WHERE EXISTS (SELECT 1 FROM workers WHERE id = ?1)
		ON CONFLICT (worker_id, period, t) DO UPDATE SET
			cpu_min   = MIN(cpu_min, excluded.cpu_min),
			cpu_avg   = (cpu_avg * n + excluded.cpu_avg * excluded.n) / (n + excluded.n),
			cpu_max   = MAX(cpu_max, excluded.cpu_max),
			load_min  = MIN(load_min, excluded.load_min),
			load_avg  = (load_avg * n + excluded.load_avg * excluded.n) / (n + excluded.n),
			load_max  = MAX(load_max, excluded.load_max),
			mem_min   = MIN(mem_min, excluded.mem_min),
			mem_avg   = (mem_avg * n + excluded.mem_avg * excluded.n) / (n + excluded.n),
			mem_max   = MAX(mem_max, excluded.mem_max),
			mem_total = MAX(mem_total, excluded.mem_total),
			swap_min  = MIN(swap_min, excluded.swap_min),
			swap_avg  = (swap_avg * n + excluded.swap_avg * excluded.n) / (n + excluded.n),
			swap_max  = MAX(swap_max, excluded.swap_max),
			gpu_min   = CASE WHEN excluded.gpu_n = 0 THEN gpu_min WHEN gpu_n = 0 THEN excluded.gpu_min ELSE MIN(gpu_min, excluded.gpu_min) END,
			gpu_max   = CASE WHEN excluded.gpu_n = 0 THEN gpu_max WHEN gpu_n = 0 THEN excluded.gpu_max ELSE MAX(gpu_max, excluded.gpu_max) END,
			gpu_avg   = CASE WHEN gpu_n + excluded.gpu_n = 0 THEN NULL
			            ELSE (COALESCE(gpu_avg, 0) * gpu_n + COALESCE(excluded.gpu_avg, 0) * excluded.gpu_n) / (gpu_n + excluded.gpu_n) END,
			gpu_n     = gpu_n + excluded.gpu_n,
			heat_min  = MIN(heat_min, excluded.heat_min),
			heat_avg  = (heat_avg * n + excluded.heat_avg * excluded.n) / (n + excluded.n),
			cause     = CASE WHEN excluded.heat_max > heat_max THEN excluded.cause ELSE cause END,
			heat_max  = MAX(heat_max, excluded.heat_max),
			n         = n + excluded.n
	`, v.WorkerID, period, t, n, v.CPU, v.Load, v.Mem, int64(v.MemTotal), v.Swap, gpuN, gpu, v.Heat, v.HeatMax, v.Cause)
	return err
}

// recomputeDay rewrites workerID's daily roll-up of the day starting at dayT from its
// hourly roll-ups.
func recomputeDay(ctx context.Context, tx *sql.Tx, workerID string, dayT int64) error {
	hour := int64(store.VitalsHourly / time.Second)
	day := int64(store.VitalsDaily / time.Second)
	_, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO machine_vitals_rollup (`+rollupColumns+`)
		SELECT worker_id, ?1, ?4, SUM(n),
			MIN(cpu_min), SUM(cpu_avg * n) / SUM(n), MAX(cpu_max),
			MIN(load_min), SUM(load_avg * n) / SUM(n), MAX(load_max),
			MIN(mem_min), SUM(mem_avg * n) / SUM(n), MAX(mem_max), MAX(mem_total),
			MIN(swap_min), SUM(swap_avg * n) / SUM(n), MAX(swap_max),
			SUM(gpu_n), MIN(gpu_min), SUM(COALESCE(gpu_avg, 0) * gpu_n) / NULLIF(SUM(gpu_n), 0), MAX(gpu_max),
			MIN(heat_min), SUM(heat_avg * n) / SUM(n), MAX(heat_max),
			(SELECT c.cause FROM machine_vitals_rollup c
			 WHERE c.worker_id = ?3 AND c.period = ?2 AND c.t >= ?4 AND c.t < ?4 + ?1
			 ORDER BY c.heat_max DESC, c.t LIMIT 1)
		FROM machine_vitals_rollup
		WHERE worker_id = ?3 AND period = ?2 AND t >= ?4 AND t < ?4 + ?1
		GROUP BY worker_id
	`, day, hour, workerID, dayT)
	return err
}
