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
	"github.com/BryanSant/syre/internal/worker/observe"
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
	// Tests that fake HOME must not find the developer's XDG directories instead.
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		os.Unsetenv(k)
	}
	os.Setenv("SYRE_STATE_DIR", dir)
	os.Setenv("SYRE_ATTACHMENTS_DIR", dir+"/attachments")
	// Checkout scans from a daemon with no config stay inside this directory.
	// Allowed roots stay unset: empty means unrestricted, and resolveWorkdirRoot
	// refuses the ~/src default while tests are running.
	work := dir + "/work"
	if err := os.MkdirAll(work, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "create test work dir:", err)
		os.Exit(1)
	}
	os.Setenv("SYRE_CHECKOUT_ROOTS", work)
	// Daemons under test report stub facts instead of probing the host.
	collectFacts = func(context.Context) protocol.MachineFacts {
		return protocol.MachineFacts{OSName: runtime.GOOS, CPUCores: runtime.NumCPU(), CollectedAt: time.Now().UTC()}
	}
	discoverCheckouts = func(context.Context, []string, int) []protocol.ReportedCheckout { return nil }
	// No ioreg / nvidia-smi / lsof / ss from daemons under test.
	readGPU = func(context.Context) *float64 { return nil }
	readUserIdle = func(context.Context) (time.Duration, string, bool) { return 0, "", false }
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
	// Observed sessions: never read the developer's claude transcripts.
	observedClaudeDirs = func(*chat.AccountRegistry) []observe.ClaudeDir { return nil }
	observedCodexDirs = func(*chat.AccountRegistry) []observe.CodexDir { return nil }
	observedGrokDirs = func(*chat.AccountRegistry) []observe.GrokDir { return nil }
	observedCopilotDirs = func(*chat.AccountRegistry) []observe.CopilotDir { return nil }
	// A removed test daemon records the removal instead of uninstalling anything.
	defaultSelfRemove = func(*Daemon) func(context.Context, string) (bool, error) {
		return func(context.Context, string) (bool, error) { return false, nil }
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
