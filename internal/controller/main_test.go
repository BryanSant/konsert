package controller

import (
	"fmt"
	"os"
	"testing"
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
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
