//go:build linux

package worker

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRealSystemdInhibit takes a real systemd-inhibit lock (SYRE_TEST_REAL_WAKELOCK=1).
// Where logind is unreachable (containers) it checks that the failure is reported cleanly.
func TestRealSystemdInhibit(t *testing.T) {
	if os.Getenv("SYRE_TEST_REAL_WAKELOCK") != "1" {
		t.Skip("set SYRE_TEST_REAL_WAKELOCK=1 to take a real systemd-inhibit lock")
	}
	listed := func() bool {
		out, _ := exec.Command("systemd-inhibit", "--list", "--no-pager").CombinedOutput()
		return strings.Contains(string(out), "syre real wakelock test")
	}
	m := NewWakeLockManager(nil, nil, nil)
	st := m.Set(true, "syre real wakelock test")
	t.Logf("enable state: %+v", st)
	if !st.Active {
		if st.Error == "" {
			t.Fatal("inactive without an error")
		}
		t.Skipf("systemd-inhibit unavailable here (reported cleanly): %s", st.Error)
	}
	waitUntil(t, 5*time.Second, "inhibitor to be listed", listed)
	m.Shutdown()
	waitUntil(t, 5*time.Second, "inhibitor to be dropped", func() bool { return !listed() })
}
