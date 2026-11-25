package worker

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/chat"
)

// TestMain points syred state (worker ID, machine ID, outbound spool) at a
// throwaway directory so tests never write into the developer's ~/.syre, and
// stubs machine-fact probes.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "syre-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test state dir:", err)
		os.Exit(1)
	}
	os.Setenv("SYRE_STATE_DIR", dir)
	os.Setenv("SYRE_ATTACHMENTS_DIR", dir+"/attachments")
	// Daemons under test report stub facts instead of probing the host.
	collectFacts = func(context.Context) protocol.MachineFacts {
		return protocol.MachineFacts{OSName: runtime.GOOS, CPUCores: runtime.NumCPU(), CollectedAt: time.Now().UTC()}
	}
	discoverCheckouts = func(context.Context, []string, int) []protocol.ReportedCheckout { return nil }
	// No ioreg / nvidia-smi / lsof / ss from daemons under test.
	readGPU = func(context.Context) *float64 { return nil }
	collectPorts = func(context.Context) []protocol.ListeningPort {
		return []protocol.ListeningPort{{Port: 5173, Process: "vite"}}
	}
	// Agent accounts: never discover the developer's real CLIs or wrapper scripts, and
	// never run their status commands.
	newAccountRegistry = func(o chat.AccountOptions) *chat.AccountRegistry {
		o.LookPath = func(string) (string, bool) { return "", false }
		o.DisableWrappers = true
		return chat.NewAccountRegistry(o)
	}
	chatStatusChecker = func(_ context.Context, a chat.Account) protocol.AgentAccount {
		return protocol.AgentAccount{ID: a.ID, Agent: a.Agent, Auth: protocol.AgentAuthStatus{State: protocol.AuthUnknown}}
	}
	// A removed test daemon records the removal instead of uninstalling anything.
	defaultSelfRemove = func(*Daemon) func(context.Context, string) (bool, error) {
		return func(context.Context, string) (bool, error) { return false, nil }
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
