package drivers

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points the agent state the drivers read by default (Copilot's account cache) at
// an empty throwaway directory, so tests never read the developer's real files.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "konsert-drivers-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test dir:", err)
		os.Exit(1)
	}
	os.Setenv("COPILOT_CACHE_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
