package worker

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/observe"
)

// TestLivePassiveScan prints what the passive scan finds on this machine
// (SYRE_LIVE_SCAN=1; a manual check on each OS).
func TestLivePassiveScan(t *testing.T) {
	if os.Getenv("SYRE_LIVE_SCAN") == "" {
		t.Skip("set SYRE_LIVE_SCAN=1")
	}
	var log emitLog
	disc := NewSessionDiscovery(t.TempDir(), nil, log.emit)
	disc.ScanPassiveProcesses(context.Background())
	time.Sleep(3 * time.Second)
	disc.ScanPassiveProcesses(context.Background())
	for _, p := range log.all() {
		t.Logf("%s agent=%s status=%s cwd=%q branch=%q", p.ExternalID, p.Agent, p.Status, p.Cwd, p.Branch)
	}
}

// TestLiveObservedCodex prints what the observed codex source reports on this machine
// for SYRE_LIVE_SECONDS (a manual check: start a codex session alongside).
func TestLiveObservedCodex(t *testing.T) {
	if os.Getenv("SYRE_LIVE_SCAN") == "" {
		t.Skip("set SYRE_LIVE_SCAN=1")
	}
	o := newObservedSessions(observedConfig{
		CodexDirs:   func() []observe.CodexDir { return codexHomes(nil) },
		Procs:       osProcTable(),
		OffsetsPath: filepath.Join(t.TempDir(), "offsets.json"),
	}, func(p protocol.ExternalSessionPayload) {
		t.Logf("%s %s agent=%s status=%s tool=%q cwd=%q summary=%q", time.Now().Format("15:04:05"), p.ExternalID, p.Agent, p.Status, p.Tool, p.Cwd, p.Summary)
	})
	secs, _ := strconv.Atoi(os.Getenv("SYRE_LIVE_SECONDS"))
	end := time.Now().Add(time.Duration(max(secs, 5)) * time.Second)
	for time.Now().Before(end) {
		o.Poll(context.Background())
		time.Sleep(2 * time.Second)
	}
}
