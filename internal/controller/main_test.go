package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/secrets"
	"github.com/BryanSant/syre/internal/worker"
	"github.com/BryanSant/syre/internal/worker/drivers"
)

// newTestDaemon constructs a worker.Daemon with its own per-test state directory so
// tests (and -count=N runs) never share a spool, scrollback or link status file.
func newTestDaemon(t *testing.T, cfg config.WorkerConfig, reg *drivers.Registry, logger *slog.Logger) *worker.Daemon {
	t.Helper()
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	return worker.NewDaemon(cfg, reg, logger)
}

// fakeSSHSocketEnv makes the test binary stand in for `ssh host syred attach`: on the
// real machine sshd runs the hub key's forced command, which bridges the session's
// stdin/stdout to syred's socket; here the test binary does the bridging itself.
const fakeSSHSocketEnv = "SYRE_TEST_FAKE_SSH_SOCKET"

// TestMain points syred state (worker ID, machine ID, status outbox) at a
// throwaway directory so tests never write into the developer's ~/.syre.
func TestMain(m *testing.M) {
	if sock := os.Getenv(fakeSSHSocketEnv); sock != "" {
		if err := worker.Attach(context.Background(), sock, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "syre-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test state dir:", err)
		os.Exit(1)
	}
	os.Setenv("SYRE_STATE_DIR", dir)
	os.Setenv("SYRE_ATTACHMENTS_DIR", dir+"/attachments")
	// Daemons started by integration tests must not run the host's real agent CLIs or
	// import its wrapper scripts (e.g. ~/.local/bin/claude2): only mock/configured accounts.
	os.Setenv("SYRE_DISABLE_AGENT_DISCOVERY", "1")
	// In-process daemons the tests remove uninstall themselves for real: give them a
	// throwaway home (config dir, LaunchAgents, authorized_keys, ~/.claude) so nothing
	// of the developer's is touched.
	os.Setenv("HOME", dir+"/home")
	os.Setenv("USERPROFILE", dir+"/home")
	os.Unsetenv("XDG_CONFIG_HOME")
	// Servers built from the default config (relative "syre.db") would otherwise
	// create syre-secret.key in the package directory.
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	os.Setenv(secrets.KeyEnv, base64.StdEncoding.EncodeToString(key))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
