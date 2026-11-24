package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"testing"

	"github.com/BryanSant/konsert/internal/secrets"
	"github.com/BryanSant/konsert/internal/worker"
)

// fakeSSHSocketEnv makes the test binary stand in for `ssh host konsertd attach`: on the
// real machine sshd runs the hub key's forced command, which bridges the session's
// stdin/stdout to konsertd's socket; here the test binary does the bridging itself.
const fakeSSHSocketEnv = "KONSERT_TEST_FAKE_SSH_SOCKET"

// TestMain points konsertd state (worker ID, machine ID, status outbox) at a
// throwaway directory so tests never write into the developer's ~/.konsert.
func TestMain(m *testing.M) {
	if sock := os.Getenv(fakeSSHSocketEnv); sock != "" {
		if err := worker.Attach(context.Background(), sock, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "konsert-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test state dir:", err)
		os.Exit(1)
	}
	os.Setenv("KONSERT_STATE_DIR", dir)
	os.Setenv("KONSERT_ATTACHMENTS_DIR", dir+"/attachments")
	// Daemons started by integration tests must not run the host's real agent CLIs or
	// import its wrapper scripts (e.g. ~/.local/bin/claude2): only mock/configured accounts.
	os.Setenv("KONSERT_DISABLE_AGENT_DISCOVERY", "1")
	// In-process daemons the tests remove uninstall themselves for real: give them a
	// throwaway home (config dir, LaunchAgents, authorized_keys, ~/.claude) so nothing
	// of the developer's is touched.
	os.Setenv("HOME", dir+"/home")
	os.Setenv("USERPROFILE", dir+"/home")
	os.Unsetenv("XDG_CONFIG_HOME")
	// Servers built from the default config (relative "konsert.db") would otherwise
	// create konsert-secret.key in the package directory.
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	os.Setenv(secrets.KeyEnv, base64.StdEncoding.EncodeToString(key))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
