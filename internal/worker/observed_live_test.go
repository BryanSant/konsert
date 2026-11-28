package worker

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/chat"
	"github.com/BryanSant/syre/internal/worker/observe"
	"github.com/BryanSant/syre/internal/worker/procs"
)

// TestLiveObservedClaude watches this machine's real claude accounts for
// SYRE_OBSERVE_LIVE_S seconds (default 20) and logs every payload a daemon would send.
// Opt-in: SYRE_OBSERVE_LIVE=1. Start or use claude sessions by hand meanwhile.
func TestLiveObservedClaude(t *testing.T) {
	if os.Getenv("SYRE_OBSERVE_LIVE") != "1" {
		t.Skip("set SYRE_OBSERVE_LIVE=1")
	}
	secs := 20
	if v := os.Getenv("SYRE_OBSERVE_LIVE_S"); v != "" {
		if _, err := fmt.Sscan(v, &secs); err != nil {
			t.Fatal(err)
		}
	}
	accounts := chat.NewAccountRegistry(chat.AccountOptions{})
	dirs := claudeConfigDirs(accounts)
	t.Logf("claude dirs: %+v", dirs)
	self := os.Getpid()
	o := newObservedSessions(observedConfig{
		ClaudeDirs:  func() []observe.ClaudeDir { return dirs },
		Procs:       osProcTable(),
		IsManaged:   func(pid int) bool { return pid == self },
		OffsetsPath: "",
	}, func(p protocol.ExternalSessionPayload) {
		t.Logf("%s %-8s %-9s account=%s tool=%q waiting=%q model=%s cwd=%s files=%v summary=%q",
			time.Now().Format("15:04:05"), p.ExternalID[:8], p.Status, p.Account, p.Tool, p.Waiting, p.Model, p.Cwd, p.ChangedFiles, p.Summary)
	})
	ctx := context.Background()
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	var first, worst, total time.Duration
	var cpu0 = processCPU()
	n := 0
	for time.Now().Before(deadline) {
		start := time.Now()
		o.Poll(ctx)
		d := time.Since(start)
		// The first polls hold new sessions back (grace), then read them whole: the
		// steady state starts after that.
		if n < 3 {
			first += d
			cpu0 = processCPU()
		} else {
			worst, total = max(worst, d), total+d
		}
		n++
		time.Sleep(2 * time.Second)
	}
	if n > 3 {
		cpu := processCPU() - cpu0
		wall := time.Duration(n-3) * 2 * time.Second
		t.Logf("first 3 polls %s; then %d polls: mean %s, slowest %s, CPU %s over %s (%.2f%%)",
			first, n-3, total/time.Duration(n-3), worst, cpu, wall, 100*cpu.Seconds()/wall.Seconds())
	}
}

// TestLiveAttribution attributes this machine's processes to its observed claude
// sessions for SYRE_OBSERVE_LIVE_S seconds and logs each session's tree. Opt-in:
// SYRE_OBSERVE_LIVE=1. Run a build from a claude session meanwhile.
func TestLiveAttribution(t *testing.T) {
	if os.Getenv("SYRE_OBSERVE_LIVE") != "1" {
		t.Skip("set SYRE_OBSERVE_LIVE=1")
	}
	secs := 20
	if v := os.Getenv("SYRE_OBSERVE_LIVE_S"); v != "" {
		_, _ = fmt.Sscan(v, &secs)
	}
	dirs := claudeConfigDirs(chat.NewAccountRegistry(chat.AccountOptions{}))
	o := newObservedSessions(observedConfig{ClaudeDirs: func() []observe.ClaudeDir { return dirs }, Procs: osProcTable(), Grace: -1},
		func(protocol.ExternalSessionPayload) {})
	s, a := procs.NewSampler(), procs.NewAttributor()
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for time.Now().Before(deadline) {
		o.Poll(context.Background())
		smp, err := s.Sample()
		if err != nil {
			t.Fatal(err)
		}
		var roots []procs.Root
		for id, pid := range o.pids() {
			roots = append(roots, procs.Root{Key: "observed:" + id, PID: pid})
		}
		names := map[int]string{}
		for _, p := range smp.Procs {
			names[p.PID] = p.Name
		}
		for _, u := range a.Attribute(smp, roots) {
			var top []string
			for _, pid := range u.PIDs {
				top = append(top, names[pid])
			}
			if len(top) > 8 {
				top = append(top[:8], "…")
			}
			t.Logf("%s %-46s cpu %6.1f%% rss %5d MiB peak %5d procs %3d orphans %d %v",
				time.Now().Format("15:04:05"), u.Key, u.CPUPercent, u.RSS>>20, u.PeakRSS>>20, u.Procs, u.Orphans, top)
		}
		time.Sleep(procs.BusyInterval)
	}
}

// TestLiveLoadSplit logs this machine's agent/other split, the top other processes and
// the user idle time for SYRE_OBSERVE_LIVE_S seconds. Opt-in: SYRE_OBSERVE_LIVE=1.
func TestLiveLoadSplit(t *testing.T) {
	if os.Getenv("SYRE_OBSERVE_LIVE") != "1" {
		t.Skip("set SYRE_OBSERVE_LIVE=1")
	}
	secs := 15
	if v := os.Getenv("SYRE_OBSERVE_LIVE_S"); v != "" {
		_, _ = fmt.Sscan(v, &secs)
	}
	prev := readUserIdle
	readUserIdle = platformUserIdle // TestMain stubs it for daemons under test
	t.Cleanup(func() { readUserIdle = prev })
	d := &Daemon{}
	d.sampler, d.attrib = procs.NewSampler(), procs.NewAttributor()
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for time.Now().Before(deadline) {
		smp, err := d.sampler.Sample()
		if err != nil {
			t.Fatal(err)
		}
		d.attribute(smp)
		l := d.load.Load()
		idle := "none"
		if l.UserIdleS != nil {
			idle = fmt.Sprintf("%ds (%s)", *l.UserIdleS, l.UserIdleSource)
		}
		var top []string
		for _, p := range l.TopOther {
			top = append(top, fmt.Sprintf("%s:%.0f%%/%dMiB", p.Name, p.CPUPercent, p.RSSMB))
		}
		t.Logf("%s agent %.1f%% %d MiB | other %.1f%% %d MiB | unreported %d | idle %s | top %v",
			time.Now().Format("15:04:05"), l.AgentCPUPercent, l.AgentRSSMB, l.OtherCPUPercent, l.OtherRSSMB, l.Unreported, idle, top)
		time.Sleep(3 * time.Second)
	}
}
