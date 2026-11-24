package sqlite

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/store"
)

func TestHeatSamplesRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "konsert.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, id := range []string{"w1", "w2"} {
		if err := st.UpsertWorker(ctx, store.Worker{ID: id, Name: id, HostGroup: "g", OS: "linux", Arch: "amd64", Status: "online", LastSeenAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	put := func(s store.HeatSample) {
		t.Helper()
		if err := st.UpsertHeatSample(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	put(store.HeatSample{WorkerID: "w1", BucketStart: t0, ScoreMax: 0.6, ScoreAvg: 0.4, Samples: 3, Cause: "cpu"})
	// The same minute again (a partial minute flushed before a restart): merged.
	put(store.HeatSample{WorkerID: "w1", BucketStart: t0.Add(20 * time.Second), ScoreMax: 0.9, ScoreAvg: 0.8, Samples: 1, Cause: "mem"})
	put(store.HeatSample{WorkerID: "w1", BucketStart: t0.Add(time.Minute), ScoreMax: 0.2, ScoreAvg: 0.1, Samples: 6, Cause: "slots"})
	put(store.HeatSample{WorkerID: "w2", BucketStart: t0.Add(time.Minute), ScoreMax: 0.3, ScoreAvg: 0.3, Samples: 6, Cause: "gpu"})
	put(store.HeatSample{WorkerID: "w2", BucketStart: t0.Add(-9 * 24 * time.Hour), ScoreMax: 1, ScoreAvg: 1, Samples: 6, Cause: "cpu"})

	// A sample for an unknown worker is refused (foreign key).
	if err := st.UpsertHeatSample(ctx, store.HeatSample{WorkerID: "ghost", BucketStart: t0, Samples: 1}); err == nil {
		t.Fatal("sample for an unknown worker stored")
	}

	got, err := st.ListHeatSamples(ctx, t0, t0.Add(time.Hour))
	if err != nil || len(got) != 3 {
		t.Fatalf("list: %+v %v", got, err)
	}
	m := got[0]
	if m.WorkerID != "w1" || !m.BucketStart.Equal(t0) || m.ScoreMax != 0.9 || m.Cause != "mem" || m.Samples != 4 || math.Abs(m.ScoreAvg-0.5) > 1e-9 {
		t.Fatalf("merged minute = %+v", m)
	}
	if got[1].WorkerID != "w1" || got[2].WorkerID != "w2" || got[2].Cause != "gpu" || !got[2].BucketStart.Equal(t0.Add(time.Minute)) {
		t.Fatalf("order/fields: %+v", got)
	}

	n, err := st.PruneHeatSamples(ctx, t0.Add(-8*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}

	if err := st.DeleteWorker(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ListHeatSamples(ctx, time.Time{}, t0.Add(time.Hour))
	if len(got) != 1 || got[0].WorkerID != "w2" {
		t.Fatalf("after deleting w1: %+v", got)
	}
}
