package sqlite

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/store"
)

func openVitalsStore(t *testing.T, workers ...string) (*SQLiteStore, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "konsert.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, id := range workers {
		if err := st.UpsertWorker(ctx, store.Worker{ID: id, Name: id, HostGroup: "g", OS: "linux", Arch: "amd64", Status: "online", LastSeenAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	return st, path
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func gpuPtr(v float64) *float64 { return &v }

func TestUpsertVitalsMerges(t *testing.T) {
	ctx := context.Background()
	st, _ := openVitalsStore(t, "w1")
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	put := func(v ...store.VitalsSample) {
		t.Helper()
		if err := st.UpsertVitals(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	put(store.VitalsSample{WorkerID: "w1", At: t0, N: 1, CPU: 10, Load: 1, Mem: 1000, MemTotal: 8000, Swap: 0, Heat: 0.2, HeatMax: 0.2, Cause: "cpu"})
	// Same 15 s bucket (a partial bucket flushed before a restart, then the rest).
	put(store.VitalsSample{WorkerID: "w1", At: t0.Add(10 * time.Second), N: 3, CPU: 50, Load: 3, Mem: 2000, MemTotal: 8000, Swap: 100,
		GPU: gpuPtr(80), GPUN: 3, Heat: 0.6, HeatMax: 0.7, Cause: "gpu"})
	// A sample for a machine that does not exist is skipped, not an error.
	put(store.VitalsSample{WorkerID: "ghost", At: t0, N: 1})
	got, err := st.ListVitals(ctx, t0, t0.Add(time.Minute))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d rows, err %v", len(got), err)
	}
	v := got[0]
	if v.N != 4 || !near(v.CPU, 40) || !near(v.Load, 2.5) || !near(v.Mem, 1750) || !near(v.Swap, 75) || !near(v.Heat, 0.5) ||
		v.HeatMax != 0.7 || v.Cause != "gpu" || v.GPU == nil || *v.GPU != 80 || v.GPUN != 3 || !v.At.Equal(t0) {
		t.Fatalf("merged %+v", v)
	}
}

// fillHour writes one sample every 15 s for the hour starting at h, with the given CPU
// and heat (the last bucket of the hour gets the heat peak and cause).
func fillHour(t *testing.T, st *SQLiteStore, worker string, h time.Time, cpu, heat float64, cause string) {
	t.Helper()
	var batch []store.VitalsSample
	for i := range 240 {
		v := store.VitalsSample{WorkerID: worker, At: h.Add(time.Duration(i) * store.VitalsBucket), N: 1,
			CPU: cpu, Load: cpu / 25, Mem: 1000, MemTotal: 4000, Heat: heat, HeatMax: heat, Cause: "cpu"}
		if i == 239 {
			v.HeatMax, v.Cause = heat+0.1, cause
		}
		batch = append(batch, v)
	}
	if err := st.UpsertVitals(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, st *SQLiteStore, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRollupVitalsHourlyDailyAndIdempotent(t *testing.T) {
	ctx := context.Background()
	st, path := openVitalsStore(t, "w1")
	day0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	// 22:00 and 23:00 on day 0, 00:00 on day 1.
	fillHour(t, st, "w1", day0.Add(22*time.Hour), 20, 0.2, "mem")
	fillHour(t, st, "w1", day0.Add(23*time.Hour), 60, 0.6, "slots")
	fillHour(t, st, "w1", day0.Add(24*time.Hour), 40, 0.4, "gpu")
	// A partial current hour (01:00-01:10 on day 1) is not rolled up.
	if err := st.UpsertVitals(ctx, []store.VitalsSample{{WorkerID: "w1", At: day0.Add(25 * time.Hour), N: 1, CPU: 99, Heat: 0.9, HeatMax: 0.9, Cause: "cpu"}}); err != nil {
		t.Fatal(err)
	}
	now := day0.Add(25*time.Hour + 10*time.Minute)
	res, err := st.RollupVitals(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Hourly != 3 || res.Daily != 1 {
		t.Fatalf("result %+v, want 3 hourly and 1 daily", res)
	}
	hourly, _ := st.ListVitalsRollups(ctx, store.VitalsHourly, day0, now)
	if len(hourly) != 3 {
		t.Fatalf("hourly rows %d", len(hourly))
	}
	h := hourly[1] // 23:00
	if h.N != 240 || !near(h.CPU.Avg, 60) || h.CPU.Min != 60 || h.CPU.Max != 60 || h.Heat.Max != 0.7 || h.Cause != "slots" ||
		h.MemTotal != 4000 || h.GPU != nil || !h.At.Equal(day0.Add(23*time.Hour)) {
		t.Fatalf("hourly 23:00 %+v", h)
	}
	daily, _ := st.ListVitalsRollups(ctx, store.VitalsDaily, day0, now)
	if len(daily) != 1 {
		t.Fatalf("daily rows %d", len(daily))
	}
	d := daily[0] // day 0 = 22:00 and 23:00 only (day 1 is not over)
	if d.N != 480 || !near(d.CPU.Avg, 40) || d.CPU.Min != 20 || d.CPU.Max != 60 || d.Heat.Max != 0.7 || d.Cause != "slots" || !d.At.Equal(day0) {
		t.Fatalf("daily %+v", d)
	}

	// Rolling up again, or after a "restart" (reopen), changes nothing.
	snapshot := func() []store.VitalsRollup {
		a, _ := st.ListVitalsRollups(ctx, store.VitalsHourly, time.Time{}, now.Add(time.Hour))
		b, _ := st.ListVitalsRollups(ctx, store.VitalsDaily, time.Time{}, now.Add(time.Hour))
		return append(a, b...)
	}
	before := snapshot()
	if _, err := st.RollupVitals(ctx, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	st = st2
	if _, err := st.RollupVitals(ctx, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	after := snapshot()
	if len(after) != len(before) {
		t.Fatalf("rows %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].N != after[i].N || !near(before[i].CPU.Avg, after[i].CPU.Avg) || before[i].Heat.Max != after[i].Heat.Max {
			t.Fatalf("row %d changed: %+v -> %+v", i, before[i], after[i])
		}
	}
}

func TestRollupVitalsAfterGapAndRetention(t *testing.T) {
	ctx := context.Background()
	st, _ := openVitalsStore(t, "w1", "w2")
	day0 := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	fillHour(t, st, "w1", day0.Add(9*time.Hour), 30, 0.3, "cpu")
	fillHour(t, st, "w2", day0.Add(9*time.Hour), 70, 0.8, "mem")
	// The hub was down for 3 days: the first roll-up after the gap still rolls those raw
	// rows up before pruning them.
	now := day0.Add(3*24*time.Hour + 5*time.Hour)
	res, err := st.RollupVitals(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Hourly != 2 || res.Daily != 2 || res.Pruned != 480 {
		t.Fatalf("result %+v", res)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM machine_vitals`); n != 0 {
		t.Fatalf("%d raw rows left past 12 h", n)
	}
	daily, _ := st.ListVitalsRollups(ctx, store.VitalsDaily, day0, now)
	if len(daily) != 2 || daily[0].WorkerID != "w1" || !near(daily[0].CPU.Avg, 30) || daily[1].Cause != "mem" {
		t.Fatalf("daily %+v", daily)
	}
	// 31 days on, the hourly rows are gone; 366 days on, the daily ones too.
	if _, err := st.RollupVitals(ctx, day0.Add(31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM machine_vitals_rollup WHERE period = 3600`); n != 0 {
		t.Fatalf("%d hourly rows past 30 days", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM machine_vitals_rollup WHERE period = 86400`); n != 2 {
		t.Fatalf("%d daily rows, want 2", n)
	}
	if _, err := st.RollupVitals(ctx, day0.Add(367*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM machine_vitals_rollup`); n != 0 {
		t.Fatalf("%d roll-up rows past a year", n)
	}
}

// TestRollupVitalsBoundedRows simulates two days of one heartbeat every 10 s with an
// hourly roll-up, and checks the raw tier stays within 13 hours of buckets.
func TestRollupVitalsBoundedRows(t *testing.T) {
	ctx := context.Background()
	st, _ := openVitalsStore(t, "w1")
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for h := range 48 {
		hourStart := start.Add(time.Duration(h) * time.Hour)
		var batch []store.VitalsSample
		for s := 0; s < 3600; s += 10 {
			batch = append(batch, store.VitalsSample{WorkerID: "w1", At: hourStart.Add(time.Duration(s) * time.Second), N: 1, CPU: float64(h), Heat: 0.1, HeatMax: 0.1})
		}
		if err := st.UpsertVitals(ctx, batch); err != nil {
			t.Fatal(err)
		}
		if _, err := st.RollupVitals(ctx, hourStart.Add(time.Hour+time.Minute)); err != nil {
			t.Fatal(err)
		}
		raw := countRows(t, st, `SELECT COUNT(*) FROM machine_vitals`)
		if raw > 13*240 {
			t.Fatalf("hour %d: %d raw rows", h, raw)
		}
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM machine_vitals_rollup WHERE period = 3600`); n != 48 {
		t.Fatalf("%d hourly rows, want 48", n)
	}
	// Two 10 s heartbeats share some 15 s buckets: every sample is counted once.
	if n := countRows(t, st, `SELECT SUM(n) FROM machine_vitals_rollup WHERE period = 3600`); n != 48*360 {
		t.Fatalf("%d samples rolled up, want %d", n, 48*360)
	}
}

func TestDeleteWorkerDropsVitals(t *testing.T) {
	ctx := context.Background()
	st, _ := openVitalsStore(t, "w1")
	h := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fillHour(t, st, "w1", h, 10, 0.1, "cpu")
	if _, err := st.RollupVitals(ctx, h.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteWorker(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, `SELECT (SELECT COUNT(*) FROM machine_vitals) + (SELECT COUNT(*) FROM machine_vitals_rollup)`); n != 0 {
		t.Fatalf("%d vitals rows left", n)
	}
}
