package worker

import (
	"runtime"
	"testing"
)

// The machine fingerprint must be available on every platform konsertd supports, or
// cloned state dirs go undetected.
func TestOSMachineHashAvailableAndStable(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		t.Skip("no machine fingerprint source on " + runtime.GOOS)
	}
	h := osMachineHash()
	if h == "" {
		t.Skipf("no machine fingerprint readable in this environment (%s container?)", runtime.GOOS)
	}
	if again := osMachineHash(); again != h || len(h) != 16 {
		t.Fatalf("osMachineHash = %q then %q", h, again)
	}
}
