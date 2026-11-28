package capacity

import (
	"testing"
	"time"
)

func TestHeadroomSubtractsReservesAndExpectedGrowth(t *testing.T) {
	now := time.Now()
	h, ok := Compute(Inputs{
		TotalRAMMB: 32768, AvailRAMMB: 20000, SurvivalReserveMB: 2048, PolicyReserveMB: 4096,
		CPUs: 16, LoadAvg: 5.5, ReserveCPU: 2,
		Sessions: []Session{
			{Subject: "task:a", WorkKey: "p/t/r", ExpectedPeakMB: 6000, RSSMB: 2000}, // 4000 to come
			{Subject: "chat:b", RSSMB: 300},                                          // unknown: small class, 724 to come
			{Subject: "task:c", ExpectedPeakMB: 500, RSSMB: 900},                     // already past its peak: nothing
		},
		Now: now,
	})
	if !ok {
		t.Fatal("not computed")
	}
	if h.ExpectedGrowthMB != 4000+724 || h.RAMMB != 20000-2048-4096-4724 {
		t.Fatalf("ram: %+v", h)
	}
	if h.CPU == nil || *h.CPU != 8.5 || h.Weight != 1 || h.ShedCooldownUntil != nil {
		t.Fatalf("cpu/weight: %+v", h)
	}
	if !h.Fits(9132) || h.Fits(9133) || !h.Fits(0) {
		t.Fatalf("fits: %d", h.RAMMB)
	}
}

func TestHeadroomCPUOnWindowsAndUnknown(t *testing.T) {
	h, _ := Compute(Inputs{TotalRAMMB: 16384, AvailRAMMB: 8000, CPUs: 8, LoadAvg: 0, CPUPercent: 50})
	if h.CPU == nil || *h.CPU != 4 {
		t.Fatalf("windows cpu: %+v", h.CPU)
	}
	h, _ = Compute(Inputs{TotalRAMMB: 16384, AvailRAMMB: 8000, CPUs: 8, LoadAvg: -1})
	if h.CPU != nil {
		t.Fatalf("unknown load: %v", *h.CPU)
	}
	if _, ok := Compute(Inputs{TotalRAMMB: 16384, AvailRAMMB: -1}); ok {
		t.Fatal("unknown memory computed")
	}
}

func TestHeadroomShedCooldownDownWeights(t *testing.T) {
	now := time.Now()
	h, _ := Compute(Inputs{TotalRAMMB: 8192, AvailRAMMB: 4000, ShedCooldownUntil: now.Add(time.Minute), Now: now})
	if h.Weight != ShedWeight || h.ShedCooldownUntil == nil {
		t.Fatalf("cooling: %+v", h)
	}
	h, _ = Compute(Inputs{TotalRAMMB: 8192, AvailRAMMB: 4000, ShedCooldownUntil: now.Add(-time.Minute), Now: now})
	if h.Weight != 1 {
		t.Fatalf("cooldown over: %+v", h)
	}
}
