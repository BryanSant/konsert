package procs

import (
	"os"
	"sort"
	"testing"
	"time"
)

// TestLiveOverhead measures what sampling the whole process table costs here, and logs
// the busiest processes. Opt-in: SYRE_PROCS_LIVE=1.
func TestLiveOverhead(t *testing.T) {
	if os.Getenv("SYRE_PROCS_LIVE") != "1" {
		t.Skip("set SYRE_PROCS_LIVE=1")
	}
	s := NewSampler()
	if _, err := s.Sample(); err != nil {
		t.Fatal(err)
	}
	const n = 20
	var smp Sample
	cpu0 := processCPU()
	var wall time.Duration
	for range n {
		time.Sleep(250 * time.Millisecond)
		var err error
		if smp, err = s.Sample(); err != nil {
			t.Fatal(err)
		}
		wall += smp.Cost
	}
	perSample := (processCPU() - cpu0) / n
	partial := 0
	for _, p := range smp.Procs {
		if p.Partial {
			partial++
		}
	}
	t.Logf("%d processes (%d partial); per sample: %s CPU, %s wall", len(smp.Procs), partial, perSample, wall/n)
	t.Logf("overhead: %.3f%% of one core at %s, %.3f%% at %s",
		100*perSample.Seconds()/BusyInterval.Seconds(), BusyInterval, 100*perSample.Seconds()/IdleInterval.Seconds(), IdleInterval)
	sort.Slice(smp.Procs, func(i, j int) bool { return smp.Procs[i].CPUPercent > smp.Procs[j].CPUPercent })
	for _, p := range smp.Procs[:min(5, len(smp.Procs))] {
		t.Logf("  %6d %-20s %6.1f%% %8.1f MiB", p.PID, p.Name, p.CPUPercent, float64(p.RSS)/(1<<20))
	}
	if perSample.Seconds()/BusyInterval.Seconds() > 0.01 {
		t.Fatalf("sampling costs %s per sample: over 1%% at %s", perSample, BusyInterval)
	}
}
