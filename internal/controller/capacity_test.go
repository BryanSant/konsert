package controller

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/capacity"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestWorkerHeadroomFromMetricsPolicyAndProfiles(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	upsertBox(t, cs.st, "w1", "box")
	w, _ := cs.st.GetWorker(ctx, "w1")
	facts, _ := json.Marshal(protocol.MachineFacts{RAMTotalMB: 16 * 1024, CPUCores: 8, SurvivalReserveMB: 800})
	pol, _ := json.Marshal(protocol.MachinePolicy{ReserveRAMMB: 1000, ReserveCPU: 2})
	w.FactsJSON, w.PolicyJSON = string(facts), string(pol)
	if err := cs.st.UpsertWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := cs.st.SetWorkerPolicy(ctx, "w1", string(pol)); err != nil {
		t.Fatal(err)
	}
	w, _ = cs.st.GetWorker(ctx, "w1")
	if err := cs.st.CreateTask(ctx, store.Task{ID: "t-run", Agent: "mock", Workdir: "/src/demo", WorkerID: "w1", Status: protocol.TaskStatusRunning}); err != nil {
		t.Fatal(err)
	}
	task, _ := cs.st.GetTask(ctx, "t-run")
	key, class := cs.h.workKeyFor(ctx, store.Attempt{}, task), cs.h.machineClassFor(ctx, "w1")
	// This work peaked at 5000 MB here before.
	if err := cs.st.UpsertWorkProfile(ctx, store.WorkProfile{WorkKey: key, MachineClass: class, Runs: 2, PeakRSSMaxMB: 5000}); err != nil {
		t.Fatal(err)
	}
	cs.h.mu.Lock()
	cs.h.workerMetrics["w1"] = &protocol.WorkerMetrics{MemTotalMB: 16384, MemUsedMB: 6384, LoadAvg: 3, Load: &protocol.MachineLoad{
		Sessions: []protocol.SessionLoad{
			{TaskID: "t-run", RSSMB: 1000},     // 4000 MB still to come
			{ChatID: "c1", RSSMB: 200},         // no profile: small class, 824 to come
			{ExternalID: "obs-1", RSSMB: 3000}, // observed: already in the used figure
		}}}
	cs.h.mu.Unlock()

	hr := cs.h.workerHeadroom(ctx, w)
	if hr == nil {
		t.Fatal("no headroom")
	}
	want := 10000 - 800 - 1000 - (4000 + 824)
	if hr.RAMMB != want || hr.ExpectedGrowthMB != 4824 || len(hr.Sessions) != 2 || hr.Sessions[0].WorkKey != key ||
		hr.Sessions[0].ExpectedPeakMB != 5000 || hr.CPU == nil || *hr.CPU != 3 || hr.Weight != 1 {
		t.Fatalf("headroom: %+v (want ram %d)", hr, want)
	}

	// A memory-pressure shed down-weights the machine while it cools down.
	cs.h.shedCooldown.start("w1", time.Now())
	if hr = cs.h.workerHeadroom(ctx, w); hr.Weight != capacity.ShedWeight || hr.ShedCooldownUntil == nil {
		t.Fatalf("after a shed: %+v", hr)
	}

	// No heartbeat: no headroom.
	cs.h.mu.Lock()
	delete(cs.h.workerMetrics, "w1")
	cs.h.mu.Unlock()
	if hr = cs.h.workerHeadroom(ctx, w); hr != nil {
		t.Fatalf("without metrics: %+v", hr)
	}
}
