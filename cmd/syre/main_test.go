package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"testing"

	"github.com/BryanSant/syre/internal/secrets"
)

// TestMain keeps controllers started by these tests from writing syre-secret.key
// (next to a relative default database path) or syred state into the source tree
// or the developer's ~/.syre.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "syre-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test state dir:", err)
		os.Exit(1)
	}
	os.Setenv("SYRE_STATE_DIR", dir)
	os.Setenv("SYRE_ATTACHMENTS_DIR", dir+"/attachments")
	os.Setenv("SYRE_DISABLE_AGENT_DISCOVERY", "1")
	work := dir + "/work"
	if err := os.MkdirAll(work, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "create test work dir:", err)
		os.Exit(1)
	}
	os.Setenv("SYRE_ALLOWED_ROOTS", work)
	os.Setenv("SYRE_CHECKOUT_ROOTS", work)
	// Never read or write the developer's CLI credentials (credentials.yaml, local-hub.yaml).
	os.Setenv("SYRE_CONFIG_DIR", dir+"/config")
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	os.Setenv(secrets.KeyEnv, base64.StdEncoding.EncodeToString(key))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
