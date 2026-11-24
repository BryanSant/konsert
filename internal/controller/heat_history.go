package controller

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

// Vitals and heat history (KON-WY5F2). Every heartbeat's vitals and smoothed heat are
// folded into the machine's current 15-second bucket in memory; finished buckets are
// written to the raw tier once a minute (and at shutdown). Once an hour the store rolls
// complete hours up into hourly rows and complete days into daily rows and prunes each
// tier past its retention (12 h raw, 30 days hourly, 365 days daily). GET /api/fleet/heat
// reads the finest tier that covers its range: raw for 12h, hourly for 24h, 3d and 1w.

const (
	// vitalsFlushEvery is how often finished buckets are written.
	vitalsFlushEvery = time.Minute

	heatDefaultRange  = "12h"
	heatDefaultPoints = 48
	heatMinPoints     = 12
	heatMaxPoints     = 336
)

// VitalsRetentionText says how long machine history is kept, in plain words (shown with
// the heat strips).
const VitalsRetentionText = "Machine history keeps every sample for 12 hours, hourly summaries for 30 days and daily summaries for a year."

// heatRange is a GET /api/fleet/heat range and the tier it reads.
type heatRange struct {
	span   time.Duration
	hourly bool // read hourly roll-ups (points are whole hours); else the raw tier
}

// heatRanges are the ranges GET /api/fleet/heat accepts.
var heatRanges = map[string]heatRange{
	"12h": {span: 12 * time.Hour},
	"24h": {span: 24 * time.Hour, hourly: true},
	"3d":  {span: 3 * 24 * time.Hour, hourly: true},
	"1w":  {span: 7 * 24 * time.Hour, hourly: true},
}

// vitalsBucket accumulates one machine's heartbeats over the current VitalsBucket.
type vitalsBucket struct {
	start                      time.Time // bucket start, UTC
	n                          int
	cpu, load, mem, swap, heat float64 // sums
	memTotal                   uint64
	gpu                        float64 // sum over gpuN heartbeats
	gpuN                       int
	heatMax                    float64
	cause                      string // cause at heatMax
}

// add folds one heartbeat at now into the bucket. When now falls in a later bucket the
// accumulated one is returned as finished (n > 0) and a new one starts.
func (b vitalsBucket) add(now time.Time, m *protocol.WorkerMetrics, score float64, cause string) (cur, finished vitalsBucket) {
	start := now.UTC().Truncate(store.VitalsBucket)
	if b.n > 0 && !b.start.Equal(start) {
		finished, b = b, vitalsBucket{}
	}
	if b.n == 0 {
		b.start = start
	}
	if m != nil {
		b.cpu += m.CPUPercent
		b.load += m.LoadAvg
		b.mem += float64(m.MemUsedMB)
		b.swap += float64(m.SwapUsedMB)
		if m.MemTotalMB > 0 {
			b.memTotal = m.MemTotalMB
		}
		if m.GPUPercent != nil {
			b.gpu += *m.GPUPercent
			b.gpuN++
		}
	}
	b.heat += score
	b.n++
	if b.n == 1 || score > b.heatMax {
		b.heatMax, b.cause = score, cause
	}
	return b, finished
}

// sample is the bucket as a raw-tier row (means).
func (b vitalsBucket) sample(workerID string) store.VitalsSample {
	n := float64(b.n)
	s := store.VitalsSample{
		WorkerID: workerID, At: b.start, N: b.n,
		CPU: b.cpu / n, Load: b.load / n, Mem: b.mem / n, MemTotal: b.memTotal, Swap: b.swap / n,
		Heat: b.heat / n, HeatMax: b.heatMax, Cause: b.cause,
	}
	if b.gpuN > 0 {
		g := b.gpu / float64(b.gpuN)
		s.GPU, s.GPUN = &g, b.gpuN
	}
	return s
}

// vitalsLoop writes finished buckets every vitalsFlushEvery and rolls up once an hour
// (and once at start, catching up after a restart or a long gap) until the hub stops.
func (h *Hub) vitalsLoop(ctx context.Context) {
	if h.store == nil {
		return
	}
	h.rollupVitals(ctx, time.Now())
	t := time.NewTicker(vitalsFlushEvery)
	defer t.Stop()
	lastHour := time.Now().Truncate(time.Hour)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			h.flushVitals(ctx, false)
			if hr := now.Truncate(time.Hour); hr.After(lastHour) {
				lastHour = hr
				h.rollupVitals(ctx, now)
			}
		}
	}
}

// rollupVitals runs the store's roll-up and retention.
func (h *Hub) rollupVitals(ctx context.Context, now time.Time) {
	res, err := h.store.RollupVitals(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			h.logger.Warn("machine history roll-up failed", "err", err)
		}
		return
	}
	h.logger.Debug("machine history rolled up", "hourly", res.Hourly, "daily", res.Daily, "pruned", res.Pruned)
}

// flushVitals writes the finished buckets, and with current also every machine's
// in-progress bucket (at shutdown, so a restart keeps it: the restarted hub's partial
// bucket merges into the same row).
func (h *Hub) flushVitals(ctx context.Context, current bool) {
	if h.store == nil {
		return
	}
	h.heat.mu.Lock()
	pending := h.heat.pending
	h.heat.pending = nil
	if current {
		for id, e := range h.heat.workers {
			if e.bucket.n > 0 {
				pending = append(pending, e.bucket.sample(id))
				e.bucket = vitalsBucket{}
				h.heat.workers[id] = e
			}
		}
	}
	h.heat.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	// Not bound to the hub lifetime: Shutdown flushes after canceling it.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := h.store.UpsertVitals(wctx, pending); err != nil {
		h.logger.Warn("machine history not stored", "samples", len(pending), "err", err)
	}
}

// liveVitals returns the buckets not yet written: finished ones waiting for the next
// flush and every machine's in-progress bucket.
func (h *Hub) liveVitals() []store.VitalsSample {
	h.heat.mu.Lock()
	defer h.heat.mu.Unlock()
	out := append([]store.VitalsSample(nil), h.heat.pending...)
	for id, e := range h.heat.workers {
		if e.bucket.n > 0 {
			out = append(out, e.bucket.sample(id))
		}
	}
	return out
}

// HeatPoint is one aggregated point of a heat strip: the sample-weighted mean heat, the
// highest heat and the cause at that maximum.
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
	Range       string    `json:"range"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	StepSeconds int64     `json:"step_seconds"`
	// Tier is the history tier the points come from: raw or hourly.
	Tier      string              `json:"tier"`
	Retention string              `json:"retention"`
	Workers   []WorkerHeatHistory `json:"workers"`
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// heatCell is one stored (or live) bucket's heat: n samples summing to sum, peak max.
type heatCell struct {
	at    time.Time
	n     int
	sum   float64
	max   float64
	cause string
}

func cellOf(s store.VitalsSample) heatCell {
	n := max(s.N, 1)
	return heatCell{at: s.At, n: n, sum: s.Heat * float64(n), max: s.HeatMax, cause: s.Cause}
}

func (a *heatCell) merge(b heatCell) {
	if a.n == 0 || b.max > a.max {
		a.max, a.cause = b.max, b.cause
	}
	a.sum += b.sum
	a.n += b.n
}

// bucketHeat aggregates one worker's cells into points buckets of step starting at start
// (bucket i covers [start+i*step, start+(i+1)*step)), oldest first. Cells outside the
// window are ignored; empty buckets are nil.
func bucketHeat(cells []heatCell, start time.Time, step time.Duration, points int) []*HeatPoint {
	accs := make([]heatCell, points)
	for _, c := range cells {
		d := c.at.Sub(start)
		if d < 0 || step <= 0 {
			continue
		}
		i := int(d / step)
		if i >= points {
			continue
		}
		accs[i].merge(c)
	}
	out := make([]*HeatPoint, points)
	for i, a := range accs {
		if a.n > 0 {
			out[i] = &HeatPoint{Score: round3(a.sum / float64(a.n)), Max: round3(a.max), Cause: a.cause}
		}
	}
	return out
}

// heatWindow returns the step, the point count and the [start, end) window for a range
// split into at most points: the step is whole seconds rounded up to a multiple of align
// (15 s for the raw tier, an hour for hourly roll-ups; points shrink to fit), end is
// the end of the step-aligned bucket holding the current align period (so it is in the
// last point and points stay put between polls), and start = end - points*step.
func heatWindow(span time.Duration, points int, align time.Duration, now time.Time) (step time.Duration, n int, start, end time.Time) {
	alignSec := int64(align / time.Second)
	stepSec := int64(math.Ceil(span.Seconds() / float64(points)))
	stepSec = max((stepSec+alignSec-1)/alignSec*alignSec, alignSec)
	n = int((int64(span/time.Second) + stepSec - 1) / stepSec)
	cur := now.UTC().Unix()/alignSec*alignSec + alignSec
	endSec := (cur + stepSec - 1) / stepSec * stepSec
	step = time.Duration(stepSec) * time.Second
	end = time.Unix(endSec, 0).UTC()
	start = end.Add(-time.Duration(n) * step)
	return step, n, start, end
}

// HeatHistory returns every known worker's heat strip over rangeName (a heatRanges key) in
// at most points buckets ending now, including buckets not yet written.
func (h *Hub) HeatHistory(ctx context.Context, rangeName string, points int, now time.Time) (HeatHistory, error) {
	rng, ok := heatRanges[rangeName]
	if !ok {
		return HeatHistory{}, fmt.Errorf("unknown range %q", rangeName)
	}
	align, tier := store.VitalsBucket, "raw"
	if rng.hourly {
		align, tier = time.Hour, "hourly"
	}
	step, n, start, end := heatWindow(rng.span, points, align, now)
	workers, err := h.store.ListWorkers(ctx)
	if err != nil {
		return HeatHistory{}, err
	}
	// The raw tier covers the last 12-13 hours; older hours come from roll-ups.
	rawSince := start
	if rng.hourly {
		if since := now.UTC().Truncate(time.Hour).Add(-store.VitalsRawKeep - time.Hour); since.After(start) {
			rawSince = since
		}
	}
	raw, err := h.store.ListVitals(ctx, rawSince, end)
	if err != nil {
		return HeatHistory{}, err
	}
	for _, s := range h.liveVitals() {
		if !s.At.Before(rawSince) && s.At.Before(end) {
			raw = append(raw, s)
		}
	}
	cells := map[string][]heatCell{}
	if !rng.hourly {
		for _, s := range raw {
			cells[s.WorkerID] = append(cells[s.WorkerID], cellOf(s))
		}
	} else {
		// Hours with raw samples are summed from them (that includes the hours not yet
		// rolled up); the rest come from the hourly roll-ups.
		type key struct {
			worker string
			hour   int64
		}
		hours := map[key]*heatCell{}
		for _, s := range raw {
			k := key{s.WorkerID, s.At.Truncate(time.Hour).Unix()}
			c := hours[k]
			if c == nil {
				c = &heatCell{at: s.At.Truncate(time.Hour)}
				hours[k] = c
			}
			c.merge(cellOf(s))
		}
		rolled, err := h.store.ListVitalsRollups(ctx, store.VitalsHourly, start, end)
		if err != nil {
			return HeatHistory{}, err
		}
		for _, r := range rolled {
			if _, ok := hours[key{r.WorkerID, r.At.Unix()}]; ok {
				continue
			}
			n := max(r.N, 1)
			cells[r.WorkerID] = append(cells[r.WorkerID], heatCell{at: r.At, n: n, sum: r.Heat.Avg * float64(n), max: r.Heat.Max, cause: r.Cause})
		}
		for k, c := range hours {
			cells[k.worker] = append(cells[k.worker], *c)
		}
	}
	res := HeatHistory{
		Range: rangeName, Start: start, End: end, StepSeconds: int64(step / time.Second),
		Tier: tier, Retention: VitalsRetentionText,
		Workers: make([]WorkerHeatHistory, 0, len(workers)),
	}
	for _, w := range workers {
		res.Workers = append(res.Workers, WorkerHeatHistory{
			WorkerID: w.ID, Samples: bucketHeat(cells[w.ID], start, step, n),
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
