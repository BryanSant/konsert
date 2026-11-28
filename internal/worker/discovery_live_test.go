package worker

import (
	"context"
	"os"
	"testing"
	"time"
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
