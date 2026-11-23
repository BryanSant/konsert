package controller

import (
	"math"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// Machine heat (KON-RJ8P7): one smoothed 0..1 load score per machine and its dominant
// cause, from the heartbeat metrics konsertd already sends. The raw score is the maximum
// of its components (each 0..1):
//
//   - cpu:   CPU% / 100, or load average / cores when that is higher (run-queue demand)
//   - mem:   memory pressure: used/total mapped from 30 % (0) to 95 % (1), raised by swap
//     in use while memory is above 80 %
//   - gpu:   GPU utilization / 100, where the platform reports it
//   - slots: active tasks / max_concurrent; 1 exactly when the scheduler considers the
//     machine full (it places nothing more there)
//
// The raw score is smoothed with an exponentially weighted moving average (time constant
// HeatTau, ~30 s) so the colour drifts rather than flickers; a full machine is never shown
// cooler than hot. Levels: cool < 0.4, warm 0.4–0.75, hot > 0.75.

// Heat levels and thresholds.
const (
	HeatCool = "cool"
	HeatWarm = "warm"
	HeatHot  = "hot"

	HeatWarmAt = 0.4
	HeatHotAt  = 0.75
)

// Heat causes.
const (
	HeatCauseCPU   = "cpu"
	HeatCauseMem   = "mem"
	HeatCauseGPU   = "gpu"
	HeatCauseSlots = "slots"
)

// HeatTau is the EWMA time constant.
const HeatTau = 30 * time.Second

// WorkerHeat is a machine's heat as shown in the fleet views.
type WorkerHeat struct {
	// Score is the smoothed 0..1 score; Raw the latest unsmoothed one.
	Score float64 `json:"score"`
	Raw   float64 `json:"raw"`
	Level string  `json:"level"` // cool | warm | hot
	// Cause is the component that dominates the raw score: cpu, mem, gpu or slots.
	Cause string `json:"cause"`
	// Components are the latest 0..1 component values (absent when unknown).
	Components map[string]float64 `json:"components,omitempty"`
	// Full is set when the scheduler considers the machine full (active >= max_concurrent).
	Full      bool      `json:"full,omitzero"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HeatLevel maps a score to cool / warm / hot.
func HeatLevel(score float64) string {
	switch {
	case score > HeatHotAt:
		return HeatHot
	case score >= HeatWarmAt:
		return HeatWarm
	default:
		return HeatCool
	}
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// HeatComponents computes the 0..1 components of one sample. cores is the machine's CPU
// count (0 = unknown); maxConcurrent the scheduler's task limit (<= 0 means 1, as in the
// scheduler).
func HeatComponents(m *protocol.WorkerMetrics, cores, active, maxConcurrent int) map[string]float64 {
	c := map[string]float64{}
	if m != nil {
		cpu := m.CPUPercent / 100
		if cores > 0 && m.LoadAvg > 0 {
			cpu = math.Max(cpu, m.LoadAvg/float64(cores))
		}
		c[HeatCauseCPU] = clamp01(cpu)
		if m.MemTotalMB > 0 {
			used := float64(m.MemUsedMB) / float64(m.MemTotalMB)
			mem := (used - 0.30) / 0.65
			if m.SwapTotalMB > 0 && m.SwapUsedMB > 0 && used > 0.8 {
				// Swapping under pressure: push toward hot by the share of swap in use.
				mem += float64(m.SwapUsedMB) / float64(m.SwapTotalMB) * 0.5
			}
			c[HeatCauseMem] = clamp01(mem)
		}
		if m.GPUPercent != nil {
			c[HeatCauseGPU] = clamp01(*m.GPUPercent / 100)
		}
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	c[HeatCauseSlots] = clamp01(float64(active) / float64(maxConcurrent))
	return c
}

// rawHeat is the maximum component and its name (ties: cpu, mem, gpu, slots).
func rawHeat(c map[string]float64) (float64, string) {
	best, cause := -1.0, HeatCauseCPU
	for _, k := range []string{HeatCauseCPU, HeatCauseMem, HeatCauseGPU, HeatCauseSlots} {
		if v, ok := c[k]; ok && v > best {
			best, cause = v, k
		}
	}
	if best < 0 {
		best = 0
	}
	return best, cause
}

// SmoothHeat folds a raw sample into the previous smoothed score: an EWMA over dt with
// time constant HeatTau (the first sample is taken as is).
func SmoothHeat(prev float64, hasPrev bool, raw float64, dt time.Duration) float64 {
	if !hasPrev || dt <= 0 {
		if hasPrev {
			return prev
		}
		return raw
	}
	alpha := 1 - math.Exp(-dt.Seconds()/HeatTau.Seconds())
	return prev + alpha*(raw-prev)
}

type heatEntry struct {
	heat WorkerHeat
	at   time.Time
}

type heatState struct {
	mu      sync.Mutex
	workers map[string]heatEntry
}

// updateHeat folds a heartbeat into a worker's heat and returns it.
func (h *Hub) updateHeat(workerID string, m *protocol.WorkerMetrics, cores, active, maxConcurrent int, now time.Time) WorkerHeat {
	comps := HeatComponents(m, cores, active, maxConcurrent)
	raw, cause := rawHeat(comps)
	full := comps[HeatCauseSlots] >= 1
	h.heat.mu.Lock()
	defer h.heat.mu.Unlock()
	if h.heat.workers == nil {
		h.heat.workers = map[string]heatEntry{}
	}
	prev, had := h.heat.workers[workerID]
	score := SmoothHeat(prev.heat.Score, had, raw, now.Sub(prev.at))
	if full && score <= HeatHotAt {
		// The scheduler places nothing more here: never show a full machine as cool.
		score = math.Max(score, HeatHotAt+0.01)
	}
	score = math.Round(clamp01(score)*1000) / 1000
	wh := WorkerHeat{
		Score: score, Raw: math.Round(raw*1000) / 1000, Level: HeatLevel(score), Cause: cause,
		Components: comps, Full: full, UpdatedAt: now.UTC(),
	}
	h.heat.workers[workerID] = heatEntry{heat: wh, at: now}
	return wh
}

// workerHeat returns a worker's latest heat (nil before its first heartbeat).
func (h *Hub) workerHeat(workerID string) *WorkerHeat {
	h.heat.mu.Lock()
	defer h.heat.mu.Unlock()
	e, ok := h.heat.workers[workerID]
	if !ok {
		return nil
	}
	wh := e.heat
	return &wh
}

// forgetHeat drops a worker's heat (disconnected or removed).
func (h *Hub) forgetHeat(workerID string) {
	h.heat.mu.Lock()
	delete(h.heat.workers, workerID)
	h.heat.mu.Unlock()
}
