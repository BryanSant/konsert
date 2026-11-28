package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

// History retention (SYR-CSCWP; docs/design/retention.md). A pass walks the store's
// compaction steps (tasks, chats, fleet timeline, usage) in small batches, each one
// short transaction of at most retentionBatch rows, sleeping retentionPause between
// batches so other writers (the hub has a single SQLite connection) get in. Then it
// returns freed pages to the file system with incremental vacuum, again in small
// steps, and truncates the WAL. Every batch is atomic and the next pass starts from
// what the database holds, so a pass stopped anywhere (restart, shutdown) is safe.
//
// The hub runs a bounded pass a few minutes after start and then hourly; Settings →
// Storage → Compact now runs a full pass in the background.

const (
	retentionBatch = 500
	retentionPause = 20 * time.Millisecond
	// retentionTickBatches bounds a periodic pass (500k rows); Compact now is unbounded.
	retentionTickBatches = 1000
	// retentionVacuumPages is one incremental vacuum step (4 MB at 4 KiB pages);
	// retentionTickVacuumSteps bounds a periodic pass's vacuum.
	retentionVacuumPages     = 1024
	retentionTickVacuumSteps = 64
	retentionEvery           = time.Hour
	retentionStartDelay      = 3 * time.Minute
	// storageStatsTTL is how long a measured storage breakdown is reused.
	storageStatsTTL = 5 * time.Minute
)

// Retention settings keys (days).
const (
	settingRetentionDetail       = "retention_detail_days"
	settingRetentionFleet        = "retention_fleet_days"
	settingRetentionFleetSummary = "retention_fleet_summary_days"
	settingRetentionUsage        = "retention_usage_days"
	settingRetentionUsageDaily   = "retention_usage_daily_days"
	settingRetentionMetrics      = "retention_metrics_minute_days"
	settingRetentionMetricsHour  = "retention_metrics_hourly_days"
)

// AuditStorageCompact and AuditStorageRetention are the audit actions of Compact now and
// retention changes.
const (
	AuditStorageCompact   = "storage.compact"
	AuditStorageRetention = "storage.retention"
)

// RetentionDays is the retention policy in whole days (the settings and API form).
type RetentionDays struct {
	DetailDays       int `json:"detail_days"`
	FleetDays        int `json:"fleet_days"`
	FleetSummaryDays int `json:"fleet_summary_days"`
	UsageDays        int `json:"usage_days"`
	UsageDailyDays   int `json:"usage_daily_days"`
	// Per-session and agent/other load history: minute rows, then hourly rows.
	MetricsDays       int `json:"metrics_days"`
	MetricsHourlyDays int `json:"metrics_hourly_days"`
}

func daysOf(d time.Duration) int { return int(d / (24 * time.Hour)) }

func (r RetentionDays) policy() store.RetentionPolicy {
	d := func(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }
	return store.RetentionPolicy{
		Detail: d(r.DetailDays), Fleet: d(r.FleetDays), FleetSummary: d(r.FleetSummaryDays),
		UsageRaw: d(r.UsageDays), UsageDaily: d(r.UsageDailyDays),
		MetricsMinute: d(r.MetricsDays), MetricsHourly: d(r.MetricsHourlyDays),
	}
}

func defaultRetentionDays() RetentionDays {
	p := store.DefaultRetention
	return RetentionDays{
		DetailDays: daysOf(p.Detail), FleetDays: daysOf(p.Fleet), FleetSummaryDays: daysOf(p.FleetSummary),
		UsageDays: daysOf(p.UsageRaw), UsageDailyDays: daysOf(p.UsageDaily),
		MetricsDays: daysOf(p.MetricsMinute), MetricsHourlyDays: daysOf(p.MetricsHourly),
	}
}

// fields pairs each period with its settings key and bounds.
func (r *RetentionDays) fields() []struct {
	key   string
	v     *int
	bound store.RetentionBound
} {
	b := store.RetentionBounds
	return []struct {
		key   string
		v     *int
		bound store.RetentionBound
	}{
		{settingRetentionDetail, &r.DetailDays, b.Detail},
		{settingRetentionFleet, &r.FleetDays, b.Fleet},
		{settingRetentionFleetSummary, &r.FleetSummaryDays, b.FleetSummary},
		{settingRetentionUsage, &r.UsageDays, b.UsageRaw},
		{settingRetentionUsageDaily, &r.UsageDailyDays, b.UsageDaily},
		{settingRetentionMetrics, &r.MetricsDays, b.MetricsMinute},
		{settingRetentionMetricsHour, &r.MetricsHourlyDays, b.MetricsHourly},
	}
}

// validate checks every period is within its bounds and the summaries outlive the
// detail they summarise.
func (r RetentionDays) validate() error {
	for _, f := range r.fields() {
		if *f.v < f.bound.Min || *f.v > f.bound.Max {
			return fmt.Errorf("%s must be between %d and %d days", f.key, f.bound.Min, f.bound.Max)
		}
	}
	if r.FleetSummaryDays < r.FleetDays {
		return errors.New("the fleet timeline summary must be kept at least as long as the full timeline")
	}
	if r.UsageDailyDays < r.UsageDays {
		return errors.New("daily usage totals must be kept at least as long as per-call usage")
	}
	if r.MetricsHourlyDays < r.MetricsDays {
		return errors.New("hourly load history must be kept at least as long as the per-minute history")
	}
	return nil
}

// retentionFromSettings reads the policy from settings (defaults for anything unset or
// out of bounds).
func retentionFromSettings(kv map[string]string) RetentionDays {
	r := defaultRetentionDays()
	for _, f := range r.fields() {
		if n, err := strconv.Atoi(kv[f.key]); err == nil && n >= f.bound.Min && n <= f.bound.Max {
			*f.v = n
		}
	}
	if r.FleetSummaryDays < r.FleetDays {
		r.FleetSummaryDays = r.FleetDays
	}
	if r.UsageDailyDays < r.UsageDays {
		r.UsageDailyDays = r.UsageDays
	}
	if r.MetricsHourlyDays < r.MetricsDays {
		r.MetricsHourlyDays = r.MetricsDays
	}
	return r
}

// RetentionText describes the policy in plain words, one line per kind of history.
func (r RetentionDays) RetentionText() []string {
	return []string{
		fmt.Sprintf("Running work and work finished in the last %d days keeps every detail.", r.DetailDays),
		fmt.Sprintf("After %d days, finished sessions keep their summary, outcome and transcript; tool calls, reasoning and other streamed events are dropped. Tasks keep their milestones, errors and your interventions.", r.DetailDays),
		fmt.Sprintf("The fleet timeline keeps everything for %d days, then only your actions and failures until %d days, then nothing.", r.FleetDays, r.FleetSummaryDays),
		fmt.Sprintf("Token usage is kept per call for %d days, then as daily totals for %d days.", r.UsageDays, r.UsageDailyDays),
		VitalsRetentionText,
		fmt.Sprintf("Load per session and agent vs other load are kept per minute for %d days, then hourly for %d days.", r.MetricsDays, r.MetricsHourlyDays),
		"The audit log keeps 90 days. Sessions marked Keep are never compacted.",
	}
}

// CompactionStep is one step's progress in a pass.
type CompactionStep struct {
	Step  string `json:"step"`
	Rows  int64  `json:"rows"`
	Items int    `json:"items"`
	Done  bool   `json:"done"`
}

// CompactionRun is the state of the current or last compaction pass.
type CompactionRun struct {
	Trigger    string           `json:"trigger"` // schedule | manual
	Running    bool             `json:"running"`
	Step       string           `json:"step,omitempty"` // current step, or "vacuum"
	Steps      []CompactionStep `json:"steps"`
	Batches    int              `json:"batches"`
	Rows       int64            `json:"rows"`
	FreedBytes int64            `json:"freed_bytes"`
	// LongestBatchMS is the longest single batch (one write transaction).
	LongestBatchMS float64    `json:"longest_batch_ms"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitzero"`
	DurationMS     float64    `json:"duration_ms"`
	// Complete: every step finished (a bounded periodic pass may stop early).
	Complete bool   `json:"complete"`
	Error    string `json:"error,omitempty"`
}

// retentionState is the hub's compaction state.
type retentionState struct {
	mu      sync.Mutex
	running bool
	run     *CompactionRun // current or last pass
	stats   *store.StorageStats
}

// retentionLoop runs a bounded pass shortly after start and then every retentionEvery.
func (h *Hub) retentionLoop(ctx context.Context) {
	if h.store == nil || h.RetentionEvery <= 0 {
		return
	}
	timer := time.NewTimer(h.RetentionStartDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := h.runCompaction(ctx, "schedule", false); err != nil && ctx.Err() == nil && !errors.Is(err, errCompactionRunning) {
			h.logger.Warn("history retention pass failed", "err", err)
		}
		timer.Reset(h.RetentionEvery)
	}
}

var errCompactionRunning = errors.New("a compaction pass is already running")

// StartCompaction starts a full pass in the background (Compact now). It returns the
// initial state, or errCompactionRunning.
func (h *Hub) StartCompaction() (CompactionRun, error) {
	h.retention.mu.Lock()
	if h.retention.running {
		run := *h.retention.run
		h.retention.mu.Unlock()
		return run, errCompactionRunning
	}
	h.retention.mu.Unlock()
	started := make(chan CompactionRun, 1)
	ok := h.goBackground(func(ctx context.Context) {
		_, err := h.runCompactionNotify(ctx, "manual", true, started)
		if err != nil && ctx.Err() == nil && !errors.Is(err, errCompactionRunning) {
			h.logger.Warn("compaction failed", "err", err)
		}
	})
	if !ok {
		return CompactionRun{}, errors.New("the hub is shutting down")
	}
	run, ok := <-started
	if !ok {
		return h.CompactionState(), errCompactionRunning
	}
	return run, nil
}

// CompactionState returns the current or last pass (zero when none ran).
func (h *Hub) CompactionState() CompactionRun {
	h.retention.mu.Lock()
	defer h.retention.mu.Unlock()
	if h.retention.run == nil {
		return CompactionRun{}
	}
	run := *h.retention.run
	run.Steps = append([]CompactionStep(nil), run.Steps...)
	return run
}

func (h *Hub) runCompaction(ctx context.Context, trigger string, full bool) (CompactionRun, error) {
	return h.runCompactionNotify(ctx, trigger, full, nil)
}

// runCompactionNotify runs one pass (bounded unless full). started, when set, receives
// the initial state once the pass owns the single-flight slot (closed if it does not).
func (h *Hub) runCompactionNotify(ctx context.Context, trigger string, full bool, started chan<- CompactionRun) (CompactionRun, error) {
	h.retention.mu.Lock()
	if h.retention.running {
		h.retention.mu.Unlock()
		if started != nil {
			close(started)
		}
		return CompactionRun{}, errCompactionRunning
	}
	begin := time.Now()
	run := &CompactionRun{Trigger: trigger, Running: true, StartedAt: begin.UTC()}
	h.retention.running, h.retention.run = true, run
	if started != nil {
		started <- *run
	}
	h.retention.mu.Unlock()

	err := h.compactionPass(ctx, run, full)

	h.retention.mu.Lock()
	fin := time.Now()
	run.Running, run.Step = false, ""
	run.FinishedAt = new(fin.UTC())
	run.DurationMS = float64(fin.Sub(begin).Microseconds()) / 1000
	if err != nil {
		run.Error = err.Error()
	}
	h.retention.running = false
	h.retention.stats = nil // re-measure on next view
	out := *run
	h.retention.mu.Unlock()
	h.logger.Info("history retention pass", "trigger", trigger, "rows", out.Rows, "batches", out.Batches,
		"freed_bytes", out.FreedBytes, "longest_batch_ms", out.LongestBatchMS, "duration_ms", out.DurationMS, "complete", out.Complete)
	return out, err
}

// compactionPass does the work of one pass, updating run under the retention lock.
func (h *Hub) compactionPass(ctx context.Context, run *CompactionRun, full bool) error {
	kv, err := h.store.GetAllSettings(ctx)
	if err != nil {
		return err
	}
	policy := retentionFromSettings(kv).policy()
	h.prunePlacementLog(ctx) // fixed window, apart from the configurable steps below
	batch := h.RetentionBatch
	if batch <= 0 {
		batch = retentionBatch
	}
	update := func(f func()) {
		h.retention.mu.Lock()
		f()
		h.retention.mu.Unlock()
	}
	budget := retentionTickBatches
	complete := true
	for _, step := range store.CompactSteps {
		update(func() {
			run.Step = step
			run.Steps = append(run.Steps, CompactionStep{Step: step})
		})
		for {
			if !full && budget <= 0 {
				complete = false
				break
			}
			budget--
			t0 := time.Now()
			res, err := h.store.CompactHistory(ctx, step, h.retentionNow(), policy, batch)
			took := time.Since(t0)
			if err != nil {
				return fmt.Errorf("%s: %w", step, err)
			}
			update(func() {
				s := &run.Steps[len(run.Steps)-1]
				s.Rows += res.Rows
				s.Items += res.Items
				s.Done = res.Done
				run.Rows += res.Rows
				run.Batches++
				run.LongestBatchMS = max(run.LongestBatchMS, float64(took.Microseconds())/1000)
			})
			if res.Done {
				break
			}
			if err := h.retentionYield(ctx); err != nil {
				return err
			}
		}
	}
	update(func() { run.Step = "vacuum" })
	for i := 0; full || i < retentionTickVacuumSteps; i++ {
		t0 := time.Now()
		freed, left, err := h.store.IncrementalVacuum(ctx, retentionVacuumPages)
		took := time.Since(t0)
		if err != nil {
			return fmt.Errorf("vacuum: %w", err)
		}
		update(func() {
			run.FreedBytes += freed
			run.LongestBatchMS = max(run.LongestBatchMS, float64(took.Microseconds())/1000)
		})
		if left == 0 || freed == 0 {
			break
		}
		if err := h.retentionYield(ctx); err != nil {
			return err
		}
	}
	if err := h.store.CheckpointWAL(ctx); err != nil {
		h.logger.Debug("wal checkpoint after compaction", "err", err)
	}
	update(func() { run.Complete = complete })
	return nil
}

// retentionYield pauses between batches so other writers get the connection.
func (h *Hub) retentionYield(ctx context.Context) error {
	if h.RetentionPause <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(h.RetentionPause):
		return nil
	}
}

// StorageStats returns the (cached) storage breakdown.
func (h *Hub) StorageStats(ctx context.Context, fresh bool) (store.StorageStats, error) {
	h.retention.mu.Lock()
	cached := h.retention.stats
	h.retention.mu.Unlock()
	if !fresh && cached != nil && time.Since(cached.MeasuredAt) < storageStatsTTL {
		return *cached, nil
	}
	st, err := h.store.StorageStats(ctx)
	if err != nil {
		return st, err
	}
	h.retention.mu.Lock()
	h.retention.stats = &st
	h.retention.mu.Unlock()
	return st, nil
}
