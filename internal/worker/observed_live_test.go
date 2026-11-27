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
