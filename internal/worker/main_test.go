package worker

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// TestMain points konsertd state (worker ID, machine ID, status outbox) at a
// throwaway directory so tests never write into the developer's ~/.konsert, and
// stubs machine-fact probes.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "konsert-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test state dir:", err)
		os.Exit(1)
	}
	os.Setenv("KONSERT_STATE_DIR", dir)
	// Daemons under test report stub facts instead of probing the host.
	collectFacts = func(context.Context) protocol.MachineFacts {
		return protocol.MachineFacts{OSName: runtime.GOOS, CPUCores: runtime.NumCPU(), CollectedAt: time.Now().UTC()}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
