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

func TestWorkProfileUpdatesAfterRuns(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	upsertBox(t, cs.st, "w1", "box")
	w, _ := cs.st.GetWorker(ctx, "w1")
	facts, _ := json.Marshal(protocol.MachineFacts{RAMTotalMB: 31 * 1024})
	w.FactsJSON, w.Isolation = string(facts), "host"
	if err := cs.st.UpsertWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := cs.st.SetWorkerPolicy(ctx, "w1", ""); err != nil { // the row exists
		t.Fatal(err)
	}
	if got := cs.h.machineClassFor(ctx, "w1"); got != "linux/amd64/32g" {
		t.Fatalf("class = %q", got)
	}

	run := func(taskID, status, category string, cpu, rss float64) {
		t.Helper()
		if err := cs.st.CreateTask(ctx, store.Task{ID: taskID, Agent: "mock", Workdir: "/src/demo", WorkerID: "w1",
			Status: protocol.TaskStatusCompleted}); err != nil {
			t.Fatal(err)
		}
		end := time.Now().UTC().Truncate(time.Minute)
		start := end.Add(-3 * time.Minute)
		var rows []store.MetricSample
		for m := range 3 {
			rows = append(rows, store.MetricSample{Subject: taskSubject(taskID), WorkerID: "w1", Period: store.MetricMinute,
				At: start.Add(time.Duration(m) * time.Minute), N: 6, CPUAvg: cpu, CPUMax: cpu, RSSAvg: rss, RSSMax: rss + float64(m)*100, ProcsMax: 3})
		}
		if err := cs.st.UpsertMetricSamples(ctx, rows); err != nil {
			t.Fatal(err)
		}
		dur := int64(180)
		if _, err := cs.st.UpsertAttempt(ctx, store.Attempt{TaskID: taskID, Attempt: 1, WorkerID: "w1", Status: status,
			FailureCategory: category, StartedAt: &start, EndedAt: &end, DurationS: &dur}); err != nil {
			t.Fatal(err)
		}
		cs.h.attemptEnded(ctx, "", taskID, 1)
	}

	run("t-p1", store.AttemptCompleted, "", 200, 1000) // 2 cores for 3 minutes: 360 CPU-s
	_, list, err := cs.svc.WorkProfiles(ctx, "", "t-p1")
	if err != nil || len(list) != 1 {
		t.Fatalf("after one run: %+v %v", list, err)
	}
	p := list[0]
	if p.WorkKey != "p-00000/adhoc/demo" || p.MachineClass != "linux/amd64/32g" || p.Runs != 1 || p.SuccessRate != 1 ||
		p.PeakRSSMaxMB != 1200 || p.CPUSecondsAvg != 360 || p.DurationP50S != 180 {
		t.Fatalf("profile after one run: %+v", p)
	}

	run("t-p2", store.AttemptFailed, protocol.ErrKindOOM, 100, 8000)
	_, list, _ = cs.svc.WorkProfiles(ctx, p.WorkKey, "")
	if p = list[0]; p.Runs != 2 || p.SuccessRate != 0.5 || p.PeakRSSMaxMB != 8200 || len(p.CommonFailures) != 1 || p.CommonFailures[0].Category != "oom" {
		t.Fatalf("profile after a failed run: %+v", p)
	}
	rows, _ := cs.st.ListAttempts(ctx, "", "t-p2")
	if a := rows[0]; a.WorkKey != p.WorkKey || a.MachineClass != p.MachineClass || a.PeakRSSMB != 8200 || a.CPUSeconds != 180 {
		t.Fatalf("attempt stamp: %+v", a)
	}

	// Agents read it with inspect kind=profile, by task or by work key; REST too.
	res, err := cs.svc.Inspect(ctx, InspectRequest{Kind: protocol.InspectKindProfile, Target: p.WorkKey})
	if err != nil || !strings.Contains(res.Text, `"success_rate":0.5`) {
		t.Fatalf("inspect: %+v %v", res, err)
	}
	if res, _ = cs.svc.Inspect(ctx, InspectRequest{Kind: protocol.InspectKindProfile, Target: "nobody/adhoc/none"}); !strings.Contains(res.Text, "unknown") {
		t.Fatalf("unknown key: %q", res.Text)
	}
	if code, raw := cs.api(t, http.MethodGet, "/api/work-profiles?target=t-p1", nil); code != http.StatusOK || !strings.Contains(string(raw), `"runs":2`) {
		t.Fatalf("REST: %d %s", code, raw)
	}
}
