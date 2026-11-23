package controller

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

func TestBucketHeat(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	minute := func(m int, avg, mx float64, cause string) store.HeatSample {
		return store.HeatSample{WorkerID: "w", BucketStart: start.Add(time.Duration(m) * time.Minute), ScoreAvg: avg, ScoreMax: mx, Cause: cause, Samples: 6}
	}
	p := func(score, mx float64, cause string) *HeatPoint {
		return &HeatPoint{Score: score, Max: mx, Cause: cause}
	}
	cases := []struct {
		name    string
		samples []store.HeatSample
		step    time.Duration
		points  int
		want    []*HeatPoint
	}{
		{"no data", nil, 15 * time.Minute, 4, []*HeatPoint{nil, nil, nil, nil}},
		{"one minute", []store.HeatSample{minute(16, 0.2, 0.3, "cpu")}, 15 * time.Minute, 4,
			[]*HeatPoint{nil, p(0.2, 0.3, "cpu"), nil, nil}},
		{"mean of means, max of maxes, cause at max", []store.HeatSample{
			minute(0, 0.1, 0.2, "cpu"), minute(1, 0.4, 0.9, "mem"), minute(14, 0.25, 0.5, "slots"),
		}, 15 * time.Minute, 2, []*HeatPoint{p(0.25, 0.9, "mem"), nil}},
		{"bucket edges", []store.HeatSample{minute(14, 0.1, 0.1, "cpu"), minute(15, 0.2, 0.2, "gpu"), minute(59, 0.3, 0.3, "mem")}, 15 * time.Minute, 4,
			[]*HeatPoint{p(0.1, 0.1, "cpu"), p(0.2, 0.2, "gpu"), nil, p(0.3, 0.3, "mem")}},
		{"outside the window is ignored", []store.HeatSample{minute(-1, 0.9, 0.9, "cpu"), minute(60, 0.9, 0.9, "cpu")}, 15 * time.Minute, 4,
			[]*HeatPoint{nil, nil, nil, nil}},
		{"rounded to 3 decimals", []store.HeatSample{minute(0, 0.1, 0.12345, "cpu"), minute(1, 0.2, 0.1, "cpu"), minute(2, 0.2, 0.1, "cpu")}, time.Hour, 1,
			[]*HeatPoint{p(0.167, 0.123, "cpu")}},
		{"tie keeps the first max", []store.HeatSample{minute(0, 0.5, 0.5, "cpu"), minute(1, 0.5, 0.5, "mem")}, time.Hour, 1,
			[]*HeatPoint{p(0.5, 0.5, "cpu")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := bucketHeat(c.samples, start, c.step, c.points)
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
		wantStep  time.Duration
		wantStart time.Time
		wantEnd   time.Time
	}{
		// 15-minute steps: the current minute (10:07) is in the bucket ending 10:15.
		{12 * time.Hour, 48, 15 * time.Minute, time.Date(2025, 12, 31, 22, 15, 0, 0, time.UTC), time.Date(2026, 1, 1, 10, 15, 0, 0, time.UTC)},
		{7 * 24 * time.Hour, 336, 30 * time.Minute, time.Date(2025, 12, 25, 10, 30, 0, 0, time.UTC), time.Date(2026, 1, 1, 10, 30, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		step, start, end := heatWindow(c.rng, c.points, now)
		if step != c.wantStep || !end.Equal(c.wantEnd) || !start.Equal(c.wantStart) {
			t.Errorf("heatWindow(%v, %d) = %v %v %v, want %v %v %v", c.rng, c.points, step, start, end, c.wantStep, c.wantStart, c.wantEnd)
		}
	}
	// A range that does not divide evenly: the step rounds up to whole seconds, the window
	// is step-aligned and holds the current minute.
	for _, points := range []int{13, 47, 100, 336} {
		step, start, end := heatWindow(12*time.Hour, points, now)
		if step%time.Second != 0 || time.Duration(points)*step < 12*time.Hour || end.Unix()%int64(step/time.Second) != 0 {
			t.Errorf("points %d: step %v start %v end %v", points, step, start, end)
		}
		if cur := now.Truncate(time.Minute); cur.Before(start) || !cur.Before(end) {
			t.Errorf("points %d: current minute %v outside [%v, %v)", points, cur, start, end)
		}
	}
}

func TestHeatMinuteAccumulates(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 10, 0, 5, 0, time.UTC)
	var m, done heatMinute
	m, done = m.add(t0, 0.2, "cpu")
	m, _ = m.add(t0.Add(20*time.Second), 0.8, "mem")
	m, _ = m.add(t0.Add(40*time.Second), 0.5, "cpu")
	if done.n != 0 || m.n != 3 || m.max != 0.8 || m.cause != "mem" || !m.start.Equal(t0.Truncate(time.Minute)) {
		t.Fatalf("minute = %+v", m)
	}
	m, done = m.add(t0.Add(60*time.Second), 0.1, "slots")
	s := done.sample("w")
	if done.n != 3 || s.ScoreAvg != 0.5 || s.ScoreMax != 0.8 || s.Cause != "mem" || s.Samples != 3 {
		t.Fatalf("finished = %+v", s)
	}
	if m.n != 1 || m.max != 0.1 || m.cause != "slots" {
		t.Fatalf("next minute = %+v", m)
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
	past := now.Add(-3 * time.Hour).Truncate(15 * time.Minute) // two minutes in one 15-minute point
	for i, s := range []store.HeatSample{
		{WorkerID: "w1", BucketStart: past, ScoreMax: 0.62, ScoreAvg: 0.41, Samples: 6, Cause: "cpu"},
		{WorkerID: "w1", BucketStart: past.Add(time.Minute), ScoreMax: 0.3, ScoreAvg: 0.2, Samples: 6, Cause: "mem"},
		{WorkerID: "w1", BucketStart: now.Add(-13 * time.Hour), ScoreMax: 1, ScoreAvg: 1, Samples: 6, Cause: "cpu"}, // out of range
	} {
		if err := st.UpsertHeatSample(ctx, s); err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
	}
	// A live heartbeat: its in-progress minute is in the last point.
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
	if code != http.StatusOK || res.Range != "12h" || res.StepSeconds != 900 || len(res.Workers) != 2 {
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

	// Raw wire shape: nulls for empty points, fields as documented.
	resp := doReq(t, http.MethodGet, ts.URL+"/api/fleet/heat?range=1w&points=336", nil, nil)
	var raw map[string]any
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["range"] != "1w" || raw["step_seconds"] != float64(1800) {
		t.Fatalf("1w: %v %v", raw["range"], raw["step_seconds"])
	}
	for _, k := range []string{"start", "end", "workers"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("missing %q in %s", k, body)
		}
	}
	ws := raw["workers"].([]any)
	if samples := ws[1].(map[string]any)["samples"].([]any); len(samples) != 336 || samples[0] != nil {
		t.Fatalf("w2 samples on the wire: %v", samples[:2])
	}

	for _, c := range []struct {
		q      string
		code   int
		points int
	}{
		{"?range=24h", 200, 48},
		{"?range=3d&points=5", 200, 12},
		{"?range=1w&points=5000", 200, 336},
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

func TestFleetHeatRequiresAuth(t *testing.T) {
	_, ts := newSecurityTestServer(t, func(c *config.ControllerConfig) {
		c.NoAuth = false
		c.AdminToken = "admin"
	})
	resp := doReq(t, http.MethodGet, ts.URL+"/api/fleet/heat", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
}

func TestHeatMinutePersistedAndFlushed(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	if err := th.st.UpsertWorker(ctx, store.Worker{ID: "w1", Name: "w1", HostGroup: "g", OS: "linux", Arch: "amd64", Status: "online", LastSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	m := &protocol.WorkerMetrics{CPUPercent: 40, MemUsedMB: 1000, MemTotalMB: 10000}
	th.h.updateHeat("w1", m, 8, 0, 4, t0)
	th.h.updateHeat("w1", m, 8, 0, 4, t0.Add(30*time.Second))
	// The next minute's first heartbeat writes the finished one.
	th.h.updateHeat("w1", m, 8, 0, 4, t0.Add(65*time.Second))
	waitFor(t, 2*time.Second, "finished minute stored", func() bool {
		got, _ := th.st.ListHeatSamples(ctx, t0, t0.Add(time.Hour))
		return len(got) == 1 && got[0].Samples == 2 && got[0].ScoreMax == 0.4 && got[0].Cause == HeatCauseCPU
	})
	// Shutdown flushes the in-progress minute.
	th.h.Close()
	got, err := th.st.ListHeatSamples(ctx, t0, t0.Add(time.Hour))
	if err != nil || len(got) != 2 || !got[1].BucketStart.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after shutdown: %+v %v", got, err)
	}
}
