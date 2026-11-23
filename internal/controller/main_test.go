package controller

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"testing"

	"github.com/BryanSant/konsert/internal/secrets"
)

// TestMain points konsertd state (worker ID, machine ID, status outbox) at a
// throwaway directory so tests never write into the developer's ~/.konsert.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "konsert-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test state dir:", err)
		os.Exit(1)
	}
	os.Setenv("KONSERT_STATE_DIR", dir)
	// Servers built from the default config (relative "konsert.db") would otherwise
	// create konsert-secret.key in the package directory.
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	os.Setenv(secrets.KeyEnv, base64.StdEncoding.EncodeToString(key))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
