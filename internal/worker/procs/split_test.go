package procs

import (
	"testing"
	"time"
)

func TestSplitLoad(t *testing.T) {
	t0 := time.Unix(1000, 0)
	unreported := proc(900, 1, 0, 0)
	unreported.Partial = true
	smp := sample(t0,
		proc(1, 0, 1, 10),
		proc(50, 1, 2, 40), proc(51, 50, 3, 5), // syred and a git it runs
		proc(100, 1, 10, 300), proc(102, 100, 50, 200), // a session's agent and build
		proc(300, 1, 90, 1000), proc(301, 1, 5, 2000), proc(302, 1, 5, 100), // the user's own work
		unreported,
	)
	a := NewAttributor()
	usage := a.Attribute(smp, []Root{{Key: "task:t1", PID: 100}})
	sp := SplitLoad(smp, usage, 50, 2)
	if sp.AgentCPUPercent != 65 || sp.AgentRSS != 545<<20 {
		t.Fatalf("agent = %.0f%% %d MiB", sp.AgentCPUPercent, sp.AgentRSS>>20)
	}
	if sp.OtherCPUPercent != 101 || sp.OtherRSS != 3110<<20 || sp.Unreported != 1 {
		t.Fatalf("other = %.0f%% %d MiB, unreported %d", sp.OtherCPUPercent, sp.OtherRSS>>20, sp.Unreported)
	}
	// The hand-started build leads; the tie at 5% goes to the bigger process.
	if len(sp.TopOther) != 2 || sp.TopOther[0].PID != 300 || sp.TopOther[1].PID != 301 {
		t.Fatalf("top other = %+v", sp.TopOther)
	}
}
