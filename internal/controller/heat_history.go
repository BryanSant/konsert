package controller

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/BryanSant/konsert/internal/store"
)

// Heat history: each worker's smoothed heat is downsampled to one bucket per minute (max,
// mean, cause at the max), accumulated in memory and written to SQLite when the minute is
// over, kept HeatRetention. GET /api/fleet/heat aggregates those minutes into a fixed
// number of points for the fleet heat strips.

const (
	// HeatRetention is how long minute buckets are kept (the longest range plus a day).
	HeatRetention = 8 * 24 * time.Hour
	// heatPruneEvery is how often old buckets are pruned (on the next flush).
	heatPruneEvery = time.Hour

	heatDefaultRange  = "12h"
	heatDefaultPoints = 48
	heatMinPoints     = 12
	heatMaxPoints     = 336
)

// heatRanges are the ranges GET /api/fleet/heat accepts.
var heatRanges = map[string]time.Duration{
	"12h": 12 * time.Hour,
	"24h": 24 * time.Hour,
	"3d":  3 * 24 * time.Hour,
	"1w":  7 * 24 * time.Hour,
}

// heatMinute accumulates one worker's heat over the current minute.
type heatMinute struct {
	start time.Time // start of the minute, UTC
	max   float64
	sum   float64
	n     int
	cause string // cause at max
}

// add folds a smoothed score at now into the minute. When now falls in a different minute
// the accumulated one is returned as finished (n > 0) and a new one starts.
func (m heatMinute) add(now time.Time, score float64, cause string) (cur, finished heatMinute) {
	start := now.UTC().Truncate(time.Minute)
	if m.n > 0 && !m.start.Equal(start) {
		finished, m = m, heatMinute{}
	}
	if m.n == 0 {
		m.start = start
	}
	m.sum += score
	m.n++
	if m.n == 1 || score > m.max {
		m.max, m.cause = score, cause
	}
	return m, finished
}

func (m heatMinute) sample(workerID string) store.HeatSample {
	return store.HeatSample{
		WorkerID: workerID, BucketStart: m.start, ScoreMax: m.max,
		ScoreAvg: m.sum / float64(m.n), Samples: m.n, Cause: m.cause,
	}
}

// persistHeatMinute writes a finished minute in the background (off the heartbeat path)
// and prunes buckets past HeatRetention at most once per heatPruneEvery.
func (h *Hub) persistHeatMinute(workerID string, m heatMinute, now time.Time) {
	if h.store == nil {
		return
	}
	h.heat.mu.Lock()
	prune := now.Sub(h.heat.lastPrune) >= heatPruneEvery
	if prune {
		h.heat.lastPrune = now
	}
	h.heat.mu.Unlock()
	s := m.sample(workerID)
	h.goBackground(func(ctx context.Context) {
		// Not bound to the hub lifetime: Shutdown waits for this write to finish.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		// Fails only for a worker removed meanwhile (foreign key): nothing to keep.
		if err := h.store.UpsertHeatSample(ctx, s); err != nil {
			h.logger.Debug("heat sample not stored", "worker_id", workerID, "err", err)
		}
		if prune {
			if _, err := h.store.PruneHeatSamples(ctx, now.Add(-HeatRetention)); err != nil {
				h.logger.Warn("prune heat samples", "err", err)
			}
		}
	})
}

// flushHeat writes every worker's in-progress minute (at shutdown, so a restart keeps it;
// the restarted hub's partial minute merges into the same row) and resets them.
func (h *Hub) flushHeat(ctx context.Context) {
	if h.store == nil {
		return
	}
	h.heat.mu.Lock()
	var pending []store.HeatSample
	for id, e := range h.heat.workers {
		if e.minute.n > 0 {
			pending = append(pending, e.minute.sample(id))
			e.minute = heatMinute{}
			h.heat.workers[id] = e
		}
	}
	h.heat.mu.Unlock()
	for _, s := range pending {
		if err := h.store.UpsertHeatSample(ctx, s); err != nil {
			h.logger.Debug("heat sample not stored", "worker_id", s.WorkerID, "err", err)
		}
	}
}

// liveHeatMinutes returns the in-progress minute of every worker that has one.
func (h *Hub) liveHeatMinutes() []store.HeatSample {
	h.heat.mu.Lock()
	defer h.heat.mu.Unlock()
	var out []store.HeatSample
	for id, e := range h.heat.workers {
		if e.minute.n > 0 {
			out = append(out, e.minute.sample(id))
		}
	}
	return out
}

// HeatPoint is one aggregated point of a heat strip: the mean of the minute means, the max
// of the minute maxes and the cause at that max.
type HeatPoint struct {
	Score float64 `json:"score"`
	Max   float64 `json:"max"`
	Cause string  `json:"cause"`
}

// WorkerHeatHistory is one worker's heat strip; a nil point has no data (offline, or
// before history began).
type WorkerHeatHistory struct {
	WorkerID string       `json:"worker_id"`
	Samples  []*HeatPoint `json:"samples"`
}

// HeatHistory is the GET /api/fleet/heat response.
type HeatHistory struct {
	Range       string              `json:"range"`
	Start       time.Time           `json:"start"`
	End         time.Time           `json:"end"`
	StepSeconds int64               `json:"step_seconds"`
	Workers     []WorkerHeatHistory `json:"workers"`
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// bucketHeat aggregates one worker's minute samples into points buckets of step starting
// at start (bucket i covers [start+i*step, start+(i+1)*step)), oldest first. Samples
// outside the window are ignored; empty buckets are nil.
func bucketHeat(samples []store.HeatSample, start time.Time, step time.Duration, points int) []*HeatPoint {
	type acc struct {
		sum, max float64
		n        int
		cause    string
	}
	accs := make([]acc, points)
	for _, s := range samples {
		d := s.BucketStart.Sub(start)
		if d < 0 || step <= 0 {
			continue
		}
		i := int(d / step)
		if i >= points {
			continue
		}
		a := &accs[i]
		if a.n == 0 || s.ScoreMax > a.max {
			a.max, a.cause = s.ScoreMax, s.Cause
		}
		a.sum += s.ScoreAvg
		a.n++
	}
	out := make([]*HeatPoint, points)
	for i, a := range accs {
		if a.n > 0 {
			out[i] = &HeatPoint{Score: round3(a.sum / float64(a.n)), Max: round3(a.max), Cause: a.cause}
		}
	}
	return out
}

// heatWindow returns the step and the [start, end) window for a range split into points:
// the step is whole seconds (rounded up), end is the end of the step-aligned bucket that
// holds the current minute (so the in-progress minute is in the last point and points
// stay put between polls), and start = end - points*step.
func heatWindow(rng time.Duration, points int, now time.Time) (step time.Duration, start, end time.Time) {
	stepSec := int64(math.Ceil(rng.Seconds() / float64(points)))
	if stepSec < 1 {
		stepSec = 1
	}
	// The current minute must fall before end.
	cur := now.UTC().Truncate(time.Minute).Unix() + 60
	endSec := (cur + stepSec - 1) / stepSec * stepSec
	step = time.Duration(stepSec) * time.Second
	end = time.Unix(endSec, 0).UTC()
	start = end.Add(-time.Duration(points) * step)
	return step, start, end
}

// mergeHeatSample folds b into a (same worker and minute), as UpsertHeatSample does.
func mergeHeatSample(a, b store.HeatSample) store.HeatSample {
	na, nb := max(a.Samples, 1), max(b.Samples, 1)
	a.ScoreAvg = (a.ScoreAvg*float64(na) + b.ScoreAvg*float64(nb)) / float64(na+nb)
	if b.ScoreMax > a.ScoreMax {
		a.ScoreMax, a.Cause = b.ScoreMax, b.Cause
	}
	a.Samples = na + nb
	return a
}

// HeatHistory returns every known worker's heat strip over rangeName (a heatRanges key) in
// points buckets ending now, including each worker's in-progress minute.
func (h *Hub) HeatHistory(ctx context.Context, rangeName string, points int, now time.Time) (HeatHistory, error) {
	rng, ok := heatRanges[rangeName]
	if !ok {
		return HeatHistory{}, fmt.Errorf("unknown range %q", rangeName)
	}
	step, start, end := heatWindow(rng, points, now)
	workers, err := h.store.ListWorkers(ctx)
	if err != nil {
		return HeatHistory{}, err
	}
	stored, err := h.store.ListHeatSamples(ctx, start, end)
	if err != nil {
		return HeatHistory{}, err
	}
	type key struct {
		worker string
		minute int64
	}
	byWorker := map[string][]store.HeatSample{}
	index := map[key]int{}
	for _, s := range stored {
		index[key{s.WorkerID, s.BucketStart.Unix()}] = len(byWorker[s.WorkerID])
		byWorker[s.WorkerID] = append(byWorker[s.WorkerID], s)
	}
	for _, s := range h.liveHeatMinutes() {
		if s.BucketStart.Before(start) || !s.BucketStart.Before(end) {
			continue
		}
		if i, ok := index[key{s.WorkerID, s.BucketStart.Unix()}]; ok {
			byWorker[s.WorkerID][i] = mergeHeatSample(byWorker[s.WorkerID][i], s)
		} else {
			byWorker[s.WorkerID] = append(byWorker[s.WorkerID], s)
		}
	}
	res := HeatHistory{
		Range: rangeName, Start: start, End: end, StepSeconds: int64(step / time.Second),
		Workers: make([]WorkerHeatHistory, 0, len(workers)),
	}
	for _, w := range workers {
		res.Workers = append(res.Workers, WorkerHeatHistory{
			WorkerID: w.ID, Samples: bucketHeat(byWorker[w.ID], start, step, points),
		})
	}
	return res, nil
}

// handleFleetHeat serves GET /api/fleet/heat?range=12h|24h|3d|1w&points=N.
func (s *Server) handleFleetHeat(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rangeName := q.Get("range")
	if rangeName == "" {
		rangeName = heatDefaultRange
	}
	if _, ok := heatRanges[rangeName]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "range must be 12h, 24h, 3d or 1w"})
		return
	}
	points := heatDefaultPoints
	if v := q.Get("points"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "points must be an integer"})
			return
		}
		points = min(max(n, heatMinPoints), heatMaxPoints)
	}
	res, err := s.hub.HeatHistory(r.Context(), rangeName, points, time.Now())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
