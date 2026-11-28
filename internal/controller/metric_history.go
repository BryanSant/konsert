package controller

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Metric history (SYR-PEEBE). Every heartbeat's load (WorkerMetrics.Load: per-session
// processes, agent vs other load) is folded into one-minute buckets per subject in
// memory. Finished minutes are written each vitalsFlushEvery with the vitals (and the
// open ones at shutdown, merging into the same rows after a restart); tasks' peaks are
// raised with them. Once an hour complete hours roll up into hourly rows and both tiers
// are pruned by the retention settings (Settings → Storage).

// Metric subjects.
func taskSubject(id string) string        { return "task:" + id }
func chatSubject(id string) string        { return "chat:" + id }
func observedSubject(w, id string) string { return "observed:" + w + "/" + id }
func agentSubject(w string) string        { return "agent:" + w }
func otherSubject(w string) string        { return "other:" + w }

type metricBucket struct {
	worker         string
	start          time.Time
	n              int
	cpuSum, rssSum float64
	cpuMax, rssMax float64
	procsMax       int
}

func (b *metricBucket) sample(subject string) store.MetricSample {
	n := float64(b.n)
	return store.MetricSample{Subject: subject, WorkerID: b.worker, Period: store.MetricMinute, At: b.start, N: b.n,
		CPUAvg: b.cpuSum / n, CPUMax: b.cpuMax, RSSAvg: b.rssSum / n, RSSMax: b.rssMax, ProcsMax: b.procsMax}
}

type metricHistory struct {
	mu      sync.Mutex
	open    map[string]*metricBucket // by subject: the current minute
	pending []store.MetricSample     // finished minutes not yet written
}

// foldLoad adds one heartbeat's load at now.
func (h *Hub) foldLoad(workerID string, l *protocol.MachineLoad, now time.Time) {
	if l == nil {
		return
	}
	add := func(subject string, cpu float64, rssMB uint64, procs int) {
		h.metrics.fold(workerID, subject, now, cpu, float64(rssMB), procs)
	}
	for _, s := range l.Sessions {
		switch {
		case s.TaskID != "":
			add(taskSubject(s.TaskID), s.CPUPercent, s.RSSMB, s.Procs)
		case s.ChatID != "":
			add(chatSubject(s.ChatID), s.CPUPercent, s.RSSMB, s.Procs)
		case s.ExternalID != "":
			add(observedSubject(workerID, s.ExternalID), s.CPUPercent, s.RSSMB, s.Procs)
		}
	}
	add(agentSubject(workerID), l.AgentCPUPercent, l.AgentRSSMB, 0)
	add(otherSubject(workerID), l.OtherCPUPercent, l.OtherRSSMB, 0)
}

func (m *metricHistory) fold(worker, subject string, now time.Time, cpu, rss float64, procs int) {
	start := now.UTC().Truncate(time.Minute)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.open == nil {
		m.open = map[string]*metricBucket{}
	}
	b := m.open[subject]
	if b != nil && !b.start.Equal(start) {
		m.pending = append(m.pending, b.sample(subject))
		b = nil
	}
	if b == nil {
		b = &metricBucket{worker: worker, start: start}
		m.open[subject] = b
	}
	b.n++
	b.cpuSum += cpu
	b.rssSum += rss
	b.cpuMax = max(b.cpuMax, cpu)
	b.rssMax = max(b.rssMax, rss)
	b.procsMax = max(b.procsMax, procs)
}

// take returns the finished minutes (and with current every open one), and forgets
// open buckets older than now's minute (subjects that stopped reporting).
func (m *metricHistory) take(now time.Time, current bool) []store.MetricSample {
	cur := now.UTC().Truncate(time.Minute)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.pending
	m.pending = nil
	for subject, b := range m.open {
		if current || b.start.Before(cur) {
			out = append(out, b.sample(subject))
			delete(m.open, subject)
		}
	}
	return out
}

// flushMetrics writes finished minutes (all open ones too with current) and raises
// the peaks of the tasks among them.
func (h *Hub) flushMetrics(ctx context.Context, current bool) {
	if h.store == nil {
		return
	}
	rows := h.metrics.take(time.Now(), current)
	if len(rows) == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := h.store.UpsertMetricSamples(wctx, rows); err != nil {
		h.logger.Warn("metric history not stored", "samples", len(rows), "err", err)
		return
	}
	for _, r := range rows {
		if id, ok := strings.CutPrefix(r.Subject, "task:"); ok {
			if err := h.store.RaiseTaskPeaks(wctx, id, uint64(r.RSSMax), r.CPUMax); err != nil {
				h.logger.Debug("task peaks not raised", "task_id", id, "err", err)
			}
		}
	}
}

// rollupMetrics rolls complete hours up and prunes by the retention settings.
func (h *Hub) rollupMetrics(ctx context.Context, now time.Time) {
	kv, _ := h.store.GetAllSettings(ctx)
	p := retentionFromSettings(kv).policy()
	res, err := h.store.RollupMetrics(ctx, now, p.MetricsMinute, p.MetricsHourly)
	if err != nil {
		if ctx.Err() == nil {
			h.logger.Warn("metric history roll-up failed", "err", err)
		}
		return
	}
	h.logger.Debug("metric history rolled up", "hourly", res.Hourly, "pruned", res.Pruned)
}

// MetricSeries is one subject's history.
type MetricSeries struct {
	Subject string               `json:"subject"`
	Period  int                  `json:"period"`
	Points  []store.MetricSample `json:"points"`
}

// metricRanges are the ranges GET /api/metrics accepts (the fleet timeline's).
var metricRanges = map[string]time.Duration{
	"1h": time.Hour, "12h": 12 * time.Hour, "24h": 24 * time.Hour, "3d": 72 * time.Hour, "1w": 7 * 24 * time.Hour,
}

// MetricHistory reads the history of subjects (or every subject of workerID) over the
// span ending now: per-minute rows when they cover it, else hourly ones.
func (h *Hub) MetricHistory(ctx context.Context, subjects []string, workerID string, span time.Duration) ([]MetricSeries, error) {
	kv, _ := h.store.GetAllSettings(ctx)
	p := retentionFromSettings(kv).policy()
	period := store.MetricMinute
	if span > p.MetricsMinute || span > 24*time.Hour {
		period = store.MetricHour
	}
	now := time.Now()
	rows, err := h.store.ListMetricSamples(ctx, store.MetricFilter{Subjects: subjects, WorkerID: workerID, Period: period, Since: now.Add(-span)})
	if err != nil {
		return nil, err
	}
	bySubject := map[string]*MetricSeries{}
	var out []MetricSeries
	var order []string
	for _, r := range rows {
		s := bySubject[r.Subject]
		if s == nil {
			s = &MetricSeries{Subject: r.Subject, Period: period}
			bySubject[r.Subject] = s
			order = append(order, r.Subject)
		}
		s.Points = append(s.Points, r)
	}
	for _, sub := range order {
		out = append(out, *bySubject[sub])
	}
	return out, nil
}

// handleMetrics serves GET /api/metrics?subject=task:<id>[,…]|&worker=<id>&range=12h.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var subjects []string
	for sub := range strings.SplitSeq(q.Get("subject"), ",") {
		if sub = strings.TrimSpace(sub); sub != "" {
			subjects = append(subjects, sub)
		}
	}
	worker := strings.TrimSpace(q.Get("worker"))
	if len(subjects) == 0 && worker == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "subject or worker required"})
		return
	}
	rng := q.Get("range")
	if rng == "" {
		rng = "12h"
	}
	span, ok := metricRanges[rng]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "range must be 1h, 12h, 24h, 3d or 1w"})
		return
	}
	series, err := s.hub.MetricHistory(r.Context(), subjects, worker, span)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"range": rng, "series": series})
}
