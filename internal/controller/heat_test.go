package controller

import (
	"math"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

func TestHeatLevelThresholds(t *testing.T) {
	cases := map[float64]string{0: HeatCool, 0.39: HeatCool, 0.4: HeatWarm, 0.75: HeatWarm, 0.751: HeatHot, 1: HeatHot}
	for score, want := range cases {
		if got := HeatLevel(score); got != want {
			t.Errorf("HeatLevel(%v) = %s, want %s", score, got, want)
		}
	}
}

func TestHeatComponentsAndCause(t *testing.T) {
	gpu := 90.0
	cases := []struct {
		name      string
		m         *protocol.WorkerMetrics
		cores     int
		active    int
		max       int
		wantCause string
		wantRaw   float64
	}{
		{"idle", &protocol.WorkerMetrics{CPUPercent: 5, MemUsedMB: 3000, MemTotalMB: 10000}, 8, 0, 4, HeatCauseCPU, 0.05},
		{"cpu", &protocol.WorkerMetrics{CPUPercent: 80, MemUsedMB: 3000, MemTotalMB: 10000}, 8, 1, 4, HeatCauseCPU, 0.8},
		{"load over cores", &protocol.WorkerMetrics{CPUPercent: 40, LoadAvg: 12, MemUsedMB: 3000, MemTotalMB: 10000}, 8, 0, 4, HeatCauseCPU, 1},
		{"memory", &protocol.WorkerMetrics{CPUPercent: 10, MemUsedMB: 9000, MemTotalMB: 10000}, 8, 0, 4, HeatCauseMem, (0.9 - 0.3) / 0.65},
		{"memory and swap", &protocol.WorkerMetrics{CPUPercent: 10, MemUsedMB: 8500, MemTotalMB: 10000, SwapUsedMB: 1000, SwapTotalMB: 2000}, 8, 0, 4, HeatCauseMem, 1},
		{"gpu", &protocol.WorkerMetrics{CPUPercent: 10, MemUsedMB: 3000, MemTotalMB: 10000, GPUPercent: &gpu}, 8, 0, 4, HeatCauseGPU, 0.9},
		{"full", &protocol.WorkerMetrics{CPUPercent: 10, MemUsedMB: 3000, MemTotalMB: 10000}, 8, 2, 2, HeatCauseSlots, 1},
		{"no metrics, scheduler default limit", nil, 0, 1, 0, HeatCauseSlots, 1},
	}
	for _, c := range cases {
		raw, cause := rawHeat(HeatComponents(c.m, c.cores, c.active, c.max))
		if cause != c.wantCause || math.Abs(raw-c.wantRaw) > 1e-9 {
			t.Errorf("%s: raw %v cause %s, want %v %s", c.name, raw, cause, c.wantRaw, c.wantCause)
		}
	}
}

func TestSmoothHeatEWMA(t *testing.T) {
	if got := SmoothHeat(0, false, 0.9, 0); got != 0.9 {
		t.Fatalf("first sample = %v, want it as is", got)
	}
	// One time constant moves ~63 % of the way.
	got := SmoothHeat(0, true, 1, HeatTau)
	if math.Abs(got-(1-math.Exp(-1))) > 1e-9 {
		t.Fatalf("after tau: %v", got)
	}
	// A 10 s spike barely registers; it takes ~a minute of sustained load to get hot.
	score := 0.1
	score = SmoothHeat(score, true, 1, 10*time.Second)
	if HeatLevel(score) != HeatCool {
		t.Fatalf("a 10 s spike made the machine %s (%v)", HeatLevel(score), score)
	}
	for i := 0; i < 5; i++ {
		score = SmoothHeat(score, true, 1, 10*time.Second)
	}
	if HeatLevel(score) != HeatHot {
		t.Fatalf("a minute at full load is %s (%v)", HeatLevel(score), score)
	}
	if got := SmoothHeat(0.5, true, 1, 0); got != 0.5 {
		t.Fatalf("zero dt changed the score to %v", got)
	}
}

func TestHubHeatFollowsHeartbeatsAndSchedulerFull(t *testing.T) {
	h := &Hub{}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := &protocol.WorkerMetrics{CPUPercent: 10, MemUsedMB: 3000, MemTotalMB: 10000}
	wh := h.updateHeat("w1", m, 8, 0, 2, t0)
	if wh.Level != HeatCool || wh.Full {
		t.Fatalf("idle machine: %+v", wh)
	}
	// The scheduler's limit is reached: shown hot at once, cause slots.
	wh = h.updateHeat("w1", m, 8, 2, 2, t0.Add(10*time.Second))
	if wh.Level != HeatHot || !wh.Full || wh.Cause != HeatCauseSlots {
		t.Fatalf("full machine: %+v", wh)
	}
	// CPU load ramps up the smoothed score over time.
	busy := &protocol.WorkerMetrics{CPUPercent: 95, MemUsedMB: 3000, MemTotalMB: 10000}
	h.updateHeat("w2", m, 8, 0, 4, t0)
	var last WorkerHeat
	for i := 1; i <= 3; i++ {
		last = h.updateHeat("w2", busy, 8, 0, 4, t0.Add(time.Duration(i)*10*time.Second))
	}
	if last.Cause != HeatCauseCPU || last.Score <= 0.4 || last.Raw != 0.95 {
		t.Fatalf("busy machine after 30 s: %+v", last)
	}
	if got := h.workerHeat("w2"); got == nil || got.Score != last.Score {
		t.Fatalf("workerHeat = %+v", got)
	}
	h.forgetHeat("w2")
	if h.workerHeat("w2") != nil {
		t.Fatal("forgotten heat still reported")
	}
}
