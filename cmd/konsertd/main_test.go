package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"testing"

	"github.com/BryanSant/konsert/internal/secrets"
)

// TestMain keeps controllers started by these tests from writing konsert-secret.key
// (next to a relative default database path) or konsertd state into the source tree
// or the developer's ~/.konsert.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "konsert-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test state dir:", err)
		os.Exit(1)
	}
	os.Setenv("KONSERT_STATE_DIR", dir)
	os.Setenv("KONSERT_DISABLE_AGENT_DISCOVERY", "1")
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	os.Setenv(secrets.KeyEnv, base64.StdEncoding.EncodeToString(key))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
