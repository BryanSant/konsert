package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestMetricSamplesSurviveRestartRollupAndPrune(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorker(ctx, store.Worker{ID: "w1", Name: "box", Status: "online", LastSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, store.Task{ID: "t1", Agent: "mock", Status: protocol.TaskStatusRunning}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 27, 12, 30, 0, 0, time.UTC)
	min := func(at time.Time, cpu, rss float64) store.MetricSample {
		return store.MetricSample{Subject: "task:t1", WorkerID: "w1", Period: store.MetricMinute, At: at, N: 6,
			CPUAvg: cpu, CPUMax: cpu * 2, RSSAvg: rss, RSSMax: rss + 100, ProcsMax: 3}
	}
	var rows []store.MetricSample
	for m := range 60 { // 11:00-11:59, a complete hour
		rows = append(rows, min(now.Add(-90*time.Minute+time.Duration(m)*time.Minute), 10, 1000))
	}
	rows = append(rows, min(now.Add(-10*time.Minute), 50, 2000))
	rows = append(rows, store.MetricSample{Subject: "task:t1", WorkerID: "gone", Period: 60, At: now, N: 1}) // unknown machine: dropped
	if err := st.UpsertMetricSamples(ctx, rows); err != nil {
		t.Fatal(err)
	}
	// The same minute again (a hub that restarted mid-minute): merged, not replaced.
	if err := st.UpsertMetricSamples(ctx, []store.MetricSample{min(now.Add(-10*time.Minute), 20, 500)}); err != nil {
		t.Fatal(err)
	}
	if err := st.RaiseTaskPeaks(ctx, "t1", 2100, 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RaiseTaskPeaks(ctx, "t1", 900, 40); err != nil { // never lowered
		t.Fatal(err)
	}
	st.Close()

	st, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.ListMetricSamples(ctx, store.MetricFilter{Subjects: []string{"task:t1"}, Since: now.Add(-15 * time.Minute)})
	if err != nil || len(got) != 1 {
		t.Fatalf("after reopen: %+v %v", got, err)
	}
	if m := got[0]; m.N != 12 || m.CPUAvg != 35 || m.CPUMax != 100 || m.RSSAvg != 1250 || m.RSSMax != 2100 {
		t.Fatalf("merged minute: %+v", m)
	}
	if task, _ := st.GetTask(ctx, "t1"); task.PeakRSSMB != 2100 || task.PeakCPUPct != 100 {
		t.Fatalf("task peaks: %d MB %.0f%%", task.PeakRSSMB, task.PeakCPUPct)
	}

	res, err := st.RollupMetrics(ctx, now, 2*24*time.Hour, 30*24*time.Hour)
	if err != nil || res.Hourly != 1 {
		t.Fatalf("rollup: %+v %v", res, err)
	}
	hours, _ := st.ListMetricSamples(ctx, store.MetricFilter{WorkerID: "w1", Period: store.MetricHour})
	if len(hours) != 1 || !hours[0].At.Equal(now.Add(-90*time.Minute)) || hours[0].N != 360 || hours[0].CPUAvg != 10 || hours[0].RSSMax != 1100 {
		t.Fatalf("hourly: %+v", hours)
	}
	// Idempotent.
	if _, err := st.RollupMetrics(ctx, now, 2*24*time.Hour, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if hours, _ = st.ListMetricSamples(ctx, store.MetricFilter{Period: store.MetricHour}); len(hours) != 1 || hours[0].N != 360 {
		t.Fatalf("rolled up twice: %+v", hours)
	}

	// Three days later minutes are past a 2-day retention and pruned; the hour stays
	// (30 days). A day after a 1-day hourly retention, it goes too.
	later := now.Add(3 * 24 * time.Hour)
	res, err = st.RollupMetrics(ctx, later, 2*24*time.Hour, 30*24*time.Hour)
	if err != nil || res.Pruned != 61 {
		t.Fatalf("prune minutes: %+v %v", res, err)
	}
	if left, _ := st.ListMetricSamples(ctx, store.MetricFilter{}); len(left) != 0 {
		t.Fatalf("minutes left: %d", len(left))
	}
	if hours, _ = st.ListMetricSamples(ctx, store.MetricFilter{Period: store.MetricHour}); len(hours) != 1 || hours[0].N != 360 {
		t.Fatalf("an hour recomputed from pruned minutes: %+v", hours)
	}
	if res, _ = st.RollupMetrics(ctx, later, 2*24*time.Hour, 24*time.Hour); res.Pruned != 1 {
		t.Fatalf("prune hours: %+v", res)
	}
}
