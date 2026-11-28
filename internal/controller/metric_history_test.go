package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestMetricHistoryFromHeartbeats(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	fw := cs.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	createQueuedTask(t, cs.st, "t-load", WorkerSelector{}, false)

	beat := func(cpu float64, rss uint64) {
		fw.send(protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{At: time.Now().UTC(), Metrics: &protocol.WorkerMetrics{
			MemTotalMB: 16000, MemUsedMB: 8000,
			Load: &protocol.MachineLoad{At: time.Now().UTC(),
				Sessions:        []protocol.SessionLoad{{TaskID: "t-load", CPUPercent: cpu, RSSMB: rss, PeakRSSMB: rss, Procs: 4}, {ExternalID: "s1", CPUPercent: 1, RSSMB: 300, Procs: 2}},
				AgentCPUPercent: cpu + 2, AgentRSSMB: rss + 340, OtherCPUPercent: 90, OtherRSSMB: 5000},
		}})
	}
	beat(100, 1000)
	beat(300, 3000)
	waitFor(t, 3*time.Second, "load folded", func() bool {
		cs.h.metrics.mu.Lock()
		defer cs.h.metrics.mu.Unlock()
		b := cs.h.metrics.open["task:t-load"]
		return b != nil && b.n == 2
	})
	// What Shutdown does: open minutes are written too.
	cs.h.flushMetrics(ctx, true)

	rows, err := cs.st.ListMetricSamples(ctx, store.MetricFilter{WorkerID: "w1", Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.MetricSample{}
	for _, r := range rows {
		got[r.Subject] = r
	}
	if m := got["task:t-load"]; m.N != 2 || m.CPUAvg != 200 || m.CPUMax != 300 || m.RSSMax != 3000 || m.ProcsMax != 4 {
		t.Fatalf("task row: %+v", m)
	}
	if _, ok := got["observed:w1/s1"]; !ok || got["other:w1"].CPUAvg != 90 || got["agent:w1"].RSSMax != 3340 {
		t.Fatalf("rows: %+v", got)
	}
	if task, _ := cs.st.GetTask(ctx, "t-load"); task.PeakRSSMB != 3000 || task.PeakCPUPct != 300 {
		t.Fatalf("task peaks: %d MB, %.0f%%", task.PeakRSSMB, task.PeakCPUPct)
	}

	code, raw := cs.api(t, http.MethodGet, "/api/metrics?subject=task:t-load&range=1h", nil)
	var res struct {
		Series []MetricSeries `json:"series"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &res) != nil || len(res.Series) != 1 || res.Series[0].Period != store.MetricMinute ||
		len(res.Series[0].Points) != 1 {
		t.Fatalf("GET /api/metrics: %d %s", code, raw)
	}
	if code, _ := cs.api(t, http.MethodGet, "/api/metrics?range=1h", nil); code != http.StatusBadRequest {
		t.Fatalf("no subject: %d", code)
	}
	// fleet_status: a machine's agent and other history, and its current load.
	v, err := cs.svc.FleetStatusView(ctx, FleetStatusInput{Detail: "history", Machine: "box", Range: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(v); !strings.Contains(string(b), `"agent:w1"`) || !strings.Contains(string(b), `"other:w1"`) {
		t.Fatalf("history view: %s", b)
	}
	v, _ = cs.svc.FleetStatusView(ctx, FleetStatusInput{Machine: "box"})
	if b, _ := json.Marshal(v); !strings.Contains(string(b), `"other_cpu_percent":90`) {
		t.Fatalf("machines view: %s", b)
	}
}

func TestRetentionPutKeepsUnsentMetricsPeriods(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	if err := cs.st.SetSettings(ctx, map[string]string{settingRetentionMetrics: "5", settingRetentionMetricsHour: "60"}); err != nil {
		t.Fatal(err)
	}
	// An older PWA sends only the periods it knows.
	code, raw := cs.api(t, http.MethodPut, "/api/storage/retention", map[string]any{
		"detail_days": 30, "fleet_days": 90, "fleet_summary_days": 365, "usage_days": 30, "usage_daily_days": 365,
	})
	if code != http.StatusOK {
		t.Fatalf("put: %d %s", code, raw)
	}
	kv, _ := cs.st.GetAllSettings(ctx)
	if r := retentionFromSettings(kv); r.MetricsDays != 5 || r.MetricsHourlyDays != 60 {
		t.Fatalf("metrics periods = %d/%d", r.MetricsDays, r.MetricsHourlyDays)
	}
	if code, _ := cs.api(t, http.MethodPut, "/api/storage/retention", map[string]any{"metrics_hourly_days": 3}); code != http.StatusBadRequest {
		t.Fatalf("hourly history under a week accepted: %d", code)
	}
}

// Heartbeats tell browsers which chats are still open on the machine, so an idle open
// chat stays listed as a live session.
func TestHeartbeatReportsOpenChats(t *testing.T) {
	th := newTestHub(t)
	fw := th.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	fw.send(protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{At: time.Now().UTC(), ActiveChatIDs: []string{"c-open"},
		Metrics: &protocol.WorkerMetrics{MemTotalMB: 1}})
	waitFor(t, 3*time.Second, "open chats stored", func() bool {
		m := th.h.WorkerMetrics("w1")
		return m != nil && len(m.OpenChats) == 1 && m.OpenChats[0] == "c-open"
	})
}
