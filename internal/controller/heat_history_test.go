package controller

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestBucketHeat(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cell := func(m int, avg, mx float64, cause string) heatCell {
		return heatCell{at: start.Add(time.Duration(m) * time.Minute), n: 4, sum: avg * 4, max: mx, cause: cause}
	}
	p := func(score, mx float64, cause string) *HeatPoint {
		return &HeatPoint{Score: score, Max: mx, Cause: cause}
	}
	cases := []struct {
		name   string
		cells  []heatCell
		step   time.Duration
		points int
		want   []*HeatPoint
	}{
		{"no data", nil, 15 * time.Minute, 4, []*HeatPoint{nil, nil, nil, nil}},
		{"one cell", []heatCell{cell(16, 0.2, 0.3, "cpu")}, 15 * time.Minute, 4,
			[]*HeatPoint{nil, p(0.2, 0.3, "cpu"), nil, nil}},
		{"weighted mean, max of maxes, cause at max", []heatCell{
			cell(0, 0.1, 0.2, "cpu"), cell(1, 0.4, 0.9, "mem"), {at: start.Add(14 * time.Minute), n: 8, sum: 8 * 0.25, max: 0.5, cause: "slots"},
		}, 15 * time.Minute, 2, []*HeatPoint{p(0.25, 0.9, "mem"), nil}},
		{"bucket edges", []heatCell{cell(14, 0.1, 0.1, "cpu"), cell(15, 0.2, 0.2, "gpu"), cell(59, 0.3, 0.3, "mem")}, 15 * time.Minute, 4,
			[]*HeatPoint{p(0.1, 0.1, "cpu"), p(0.2, 0.2, "gpu"), nil, p(0.3, 0.3, "mem")}},
		{"outside the window is ignored", []heatCell{cell(-1, 0.9, 0.9, "cpu"), cell(60, 0.9, 0.9, "cpu")}, 15 * time.Minute, 4,
			[]*HeatPoint{nil, nil, nil, nil}},
		{"rounded to 3 decimals", []heatCell{cell(0, 0.1, 0.12345, "cpu"), cell(1, 0.2, 0.1, "cpu"), cell(2, 0.2, 0.1, "cpu")}, time.Hour, 1,
			[]*HeatPoint{p(0.167, 0.123, "cpu")}},
		{"tie keeps the first max", []heatCell{cell(0, 0.5, 0.5, "cpu"), cell(1, 0.5, 0.5, "mem")}, time.Hour, 1,
			[]*HeatPoint{p(0.5, 0.5, "cpu")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := bucketHeat(c.cells, start, c.step, c.points)
			if len(got) != len(c.want) {
				t.Fatalf("len %d, want %d", len(got), len(c.want))
			}
			for i := range got {
				if (got[i] == nil) != (c.want[i] == nil) || (got[i] != nil && *got[i] != *c.want[i]) {
					t.Fatalf("point %d = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestHeatWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 10, 7, 30, 0, time.UTC)
	cases := []struct {
		rng       time.Duration
		points    int
		align     time.Duration
		wantStep  time.Duration
		wantN     int
		wantStart time.Time
		wantEnd   time.Time
	}{
		// Raw tier, 15-minute steps: the current minute (10:07) is in the bucket ending 10:15.
		{12 * time.Hour, 48, store.VitalsBucket, 15 * time.Minute, 48, time.Date(2025, 12, 31, 22, 15, 0, 0, time.UTC), time.Date(2026, 1, 1, 10, 15, 0, 0, time.UTC)},
		// Hourly tier: 48 points over 24 h would be 30-minute steps, finer than the tier,
		// so it becomes 24 one-hour points ending after the current hour.
		{24 * time.Hour, 48, time.Hour, time.Hour, 24, time.Date(2025, 12, 31, 11, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)},
		{3 * 24 * time.Hour, 48, time.Hour, 2 * time.Hour, 36, time.Date(2025, 12, 29, 12, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
		{7 * 24 * time.Hour, 336, time.Hour, time.Hour, 168, time.Date(2025, 12, 25, 11, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		step, n, start, end := heatWindow(c.rng, c.points, c.align, now)
		if step != c.wantStep || n != c.wantN || !end.Equal(c.wantEnd) || !start.Equal(c.wantStart) {
			t.Errorf("heatWindow(%v, %d, %v) = %v %d %v %v, want %v %d %v %v", c.rng, c.points, c.align, step, n, start, end, c.wantStep, c.wantN, c.wantStart, c.wantEnd)
		}
	}
	// A range that does not divide evenly: the step rounds up to the alignment, the
	// window is step-aligned, covers the range and holds the current period.
	for _, align := range []time.Duration{store.VitalsBucket, time.Hour} {
		for _, points := range []int{13, 47, 100, 336} {
			step, n, start, end := heatWindow(12*time.Hour, points, align, now)
			if step%align != 0 || n > points || time.Duration(n)*step < 12*time.Hour || end.Unix()%int64(step/time.Second) != 0 {
				t.Errorf("points %d align %v: step %v n %d start %v end %v", points, align, step, n, start, end)
			}
			if cur := now.Truncate(align); cur.Before(start) || !cur.Before(end) {
				t.Errorf("points %d align %v: current period %v outside [%v, %v)", points, align, cur, start, end)
			}
		}
	}
}

func TestVitalsBucketAccumulates(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 10, 0, 1, 0, time.UTC)
	gpu := 60.0
	m1 := &protocol.WorkerMetrics{CPUPercent: 20, LoadAvg: 1, MemUsedMB: 1000, MemTotalMB: 8000, SwapUsedMB: 10, GPUPercent: &gpu}
	m2 := &protocol.WorkerMetrics{CPUPercent: 40, LoadAvg: 3, MemUsedMB: 3000, MemTotalMB: 8000, SwapUsedMB: 30}
	var b, done vitalsBucket
	b, done = b.add(t0, m1, 0.2, "cpu")
	b, _ = b.add(t0.Add(10*time.Second), m2, 0.8, "mem")
	if done.n != 0 || b.n != 2 || b.heatMax != 0.8 || b.cause != "mem" || !b.start.Equal(t0.Truncate(store.VitalsBucket)) {
		t.Fatalf("bucket = %+v", b)
	}
	b, done = b.add(t0.Add(15*time.Second), m2, 0.1, "slots")
	s := done.sample("w")
	if done.n != 2 || s.CPU != 30 || s.Load != 2 || s.Mem != 2000 || s.Swap != 20 || s.MemTotal != 8000 ||
		s.GPU == nil || *s.GPU != 60 || s.GPUN != 1 || s.Heat != 0.5 || s.HeatMax != 0.8 || s.Cause != "mem" || s.N != 2 {
		t.Fatalf("finished = %+v", s)
	}
	if b.n != 1 || b.heatMax != 0.1 || b.cause != "slots" {
		t.Fatalf("next bucket = %+v", b)
	}
}

func TestFleetHeatEndpoint(t *testing.T) {
	srv, ts := newSecurityTestServer(t, nil)
	ctx := context.Background()
	st := srv.store
	for _, id := range []string{"w1", "w2"} {
		if err := st.UpsertWorker(ctx, store.Worker{ID: id, Name: id, HostGroup: "g", OS: "linux", Arch: "amd64", Status: "online", LastSeenAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	past := now.Add(-3 * time.Hour).Truncate(15 * time.Minute) // two buckets in one 15-minute point
	if err := st.UpsertVitals(ctx, []store.VitalsSample{
		{WorkerID: "w1", At: past, N: 1, Heat: 0.41, HeatMax: 0.62, Cause: "cpu"},
		{WorkerID: "w1", At: past.Add(time.Minute), N: 1, Heat: 0.2, HeatMax: 0.3, Cause: "mem"},
		{WorkerID: "w1", At: now.Add(-13 * time.Hour), N: 1, Heat: 1, HeatMax: 1, Cause: "cpu"}, // out of the 12h range
	}); err != nil {
		t.Fatal(err)
	}
	// A live heartbeat: its in-progress bucket is in the last point.
	srv.Hub().updateHeat("w1", &protocol.WorkerMetrics{CPUPercent: 50, MemUsedMB: 1000, MemTotalMB: 10000}, 8, 0, 4, now)

	get := func(query string) (int, HeatHistory) {
		t.Helper()
		resp := doReq(t, http.MethodGet, ts.URL+"/api/fleet/heat"+query, nil, nil)
		body, _ := io.ReadAll(resp.Body)
		var res HeatHistory
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(body, &res); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
		}
		return resp.StatusCode, res
	}

	code, res := get("")
	if code != http.StatusOK || res.Range != "12h" || res.StepSeconds != 900 || res.Tier != "raw" || res.Retention == "" || len(res.Workers) != 2 {
		t.Fatalf("default: %d %+v", code, res)
	}
	if got := res.End.Sub(res.Start); got != 12*time.Hour {
		t.Fatalf("window %v", got)
	}
	byID := map[string][]*HeatPoint{}
	for _, w := range res.Workers {
		if len(w.Samples) != 48 {
			t.Fatalf("%s has %d samples", w.WorkerID, len(w.Samples))
		}
		byID[w.WorkerID] = w.Samples
	}
	for i, p := range byID["w2"] {
		if p != nil {
			t.Fatalf("w2 point %d = %+v, want null", i, p)
		}
	}
	w1 := byID["w1"]
	last := w1[47]
	if last == nil {
		last = w1[46] // the clock crossed a step edge between the heartbeat and the request
	}
	if last == nil || last.Cause != HeatCauseCPU || last.Max != 0.5 {
		t.Fatalf("live point = %+v", last)
	}
	idx := int(past.Sub(res.Start) / (900 * time.Second))
	if got, want := w1[idx], (HeatPoint{Score: 0.305, Max: 0.62, Cause: "cpu"}); got == nil || *got != want {
		t.Fatalf("point %d = %+v, want %+v", idx, got, want)
	}
	nonNull := 0
	for _, p := range w1 {
		if p != nil {
			nonNull++
		}
	}
	if nonNull != 2 {
		t.Fatalf("w1 has %d non-null points, want 2", nonNull)
	}

	// Raw wire shape: nulls for empty points, fields as documented. 1w reads hourly
	// roll-ups: one point per hour at most.
	resp := doReq(t, http.MethodGet, ts.URL+"/api/fleet/heat?range=1w&points=336", nil, nil)
	var raw map[string]any
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["range"] != "1w" || raw["step_seconds"] != float64(3600) || raw["tier"] != "hourly" {
		t.Fatalf("1w: %v %v %v", raw["range"], raw["step_seconds"], raw["tier"])
	}
	for _, k := range []string{"start", "end", "workers", "retention"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("missing %q in %s", k, body)
		}
	}
	ws := raw["workers"].([]any)
	if samples := ws[1].(map[string]any)["samples"].([]any); len(samples) != 168 || samples[0] != nil {
		t.Fatalf("w2 samples on the wire: %d", len(samples))
	}

	for _, c := range []struct {
		q      string
		code   int
		points int
	}{
		{"?range=24h", 200, 24},
		{"?range=3d&points=5", 200, 12},
		{"?range=1w&points=5000", 200, 168},
		{"?range=12h&points=5000", 200, 320}, // 135 s steps (a multiple of 15 s)
		{"?range=2h", 400, 0},
		{"?points=abc", 400, 0},
	} {
		code, res := get(c.q)
		if code != c.code {
			t.Fatalf("%s: status %d, want %d", c.q, code, c.code)
		}
		if code == 200 && len(res.Workers[0].Samples) != c.points {
			t.Fatalf("%s: %d points, want %d", c.q, len(res.Workers[0].Samples), c.points)
		}
	}
}

// TestHeatHistoryHourlyTier: 24h reads hourly roll-ups for hours that left the raw tier
// and sums raw samples for recent hours (including ones not rolled up yet).
func TestHeatHistoryHourlyTier(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	if err := th.st.UpsertWorker(ctx, store.Worker{ID: "w1", Name: "w1", HostGroup: "g", OS: "linux", Arch: "amd64", Status: "online", LastSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC)
	old := now.Truncate(time.Hour).Add(-20 * time.Hour) // 16:00 the day before
	// Twenty hours ago: written raw, rolled up, then pruned from the raw tier.
	var batch []store.VitalsSample
	for i := range 240 {
		batch = append(batch, store.VitalsSample{WorkerID: "w1", At: old.Add(time.Duration(i) * store.VitalsBucket), N: 1, Heat: 0.3, HeatMax: 0.3, Cause: "cpu"})
	}
	batch[100].HeatMax, batch[100].Cause = 0.9, "gpu"
	// Two hours ago: still raw, not rolled up yet.
	recent := now.Truncate(time.Hour).Add(-2 * time.Hour)
	batch = append(batch, store.VitalsSample{WorkerID: "w1", At: recent, N: 2, Heat: 0.5, HeatMax: 0.6, Cause: "mem"})
	if err := th.st.UpsertVitals(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := th.st.RollupVitals(ctx, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := th.st.RollupVitals(ctx, now); err != nil { // prunes the 16:00 raw rows
		t.Fatal(err)
	}
	if raw, _ := th.st.ListVitals(ctx, old, old.Add(time.Hour)); len(raw) != 0 {
		t.Fatalf("raw rows for 16:00 still there: %d", len(raw))
	}
	res, err := th.h.HeatHistory(ctx, "24h", 48, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tier != "hourly" || res.StepSeconds != 3600 || len(res.Workers) != 1 || len(res.Workers[0].Samples) != 24 {
		t.Fatalf("history %+v", res)
	}
	s := res.Workers[0].Samples
	at := func(t0 time.Time) *HeatPoint { return s[int(t0.Sub(res.Start)/time.Hour)] }
	if p := at(old); p == nil || p.Score != 0.3 || p.Max != 0.9 || p.Cause != "gpu" {
		t.Fatalf("16:00 point %+v", p)
	}
	if p := at(recent); p == nil || p.Score != 0.5 || p.Max != 0.6 || p.Cause != "mem" {
		t.Fatalf("recent point %+v", p)
	}
}

func TestFleetHeatRequiresAuth(t *testing.T) {
	_, ts := newSecurityTestServer(t, func(c *config.ControllerConfig) {
		c.NoAuth = false
		c.RecoveryKey = "admin"
	})
	resp := doReq(t, http.MethodGet, ts.URL+"/api/fleet/heat", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
}

func TestVitalsBucketFlushedAndKeptAcrossShutdown(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	if err := th.st.UpsertWorker(ctx, store.Worker{ID: "w1", Name: "w1", HostGroup: "g", OS: "linux", Arch: "amd64", Status: "online", LastSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	m := &protocol.WorkerMetrics{CPUPercent: 40, MemUsedMB: 1000, MemTotalMB: 10000}
	th.h.updateHeat("w1", m, 8, 0, 4, t0)
	th.h.updateHeat("w1", m, 8, 0, 4, t0.Add(10*time.Second))
	// The next bucket's first heartbeat finishes the first; a flush writes it.
	th.h.updateHeat("w1", m, 8, 0, 4, t0.Add(20*time.Second))
	th.h.flushVitals(ctx, false)
	got, _ := th.st.ListVitals(ctx, t0, t0.Add(time.Hour))
	if len(got) != 1 || got[0].N != 2 || got[0].HeatMax != 0.4 || got[0].CPU != 40 || got[0].Cause != HeatCauseCPU {
		t.Fatalf("after flush: %+v", got)
	}
	// Shutdown flushes the in-progress bucket.
	th.h.Close()
	got, err := th.st.ListVitals(ctx, t0, t0.Add(time.Hour))
	if err != nil || len(got) != 2 || !got[1].At.Equal(t0.Add(15*time.Second)) || got[1].N != 1 {
		t.Fatalf("after shutdown: %+v %v", got, err)
	}
}
