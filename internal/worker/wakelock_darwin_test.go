//go:build darwin

package worker

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRealCaffeinateAssertion takes a real caffeinate wake lock and checks that macOS
// reports the idle-sleep assertion while held and drops it after release and shutdown.
// It changes host power state, so it only runs with SYRE_TEST_REAL_WAKELOCK=1.
func TestRealCaffeinateAssertion(t *testing.T) {
	if os.Getenv("SYRE_TEST_REAL_WAKELOCK") != "1" {
		t.Skip("set SYRE_TEST_REAL_WAKELOCK=1 to take a real caffeinate assertion")
	}
	assertionFor := func(pid int) bool {
		out, err := exec.Command("pmset", "-g", "assertions").Output()
		if err != nil {
			t.Fatalf("pmset: %v", err)
		}
		needle := "pid " + strconv.Itoa(pid) + "(caffeinate)"
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, needle) && strings.Contains(line, "PreventUserIdleSystemSleep") {
				return true
			}
		}
		return false
	}

	m := NewWakeLockManager(nil, nil, nil)
	for _, release := range []func(){func() { m.Set(false, "") }, m.Shutdown} {
		st := m.Set(true, "syre real wakelock test")
		if !st.Active || st.Method != "caffeinate" {
			t.Fatalf("enable: %+v", st)
		}
		m.mu.Lock()
		pid := m.holder.(*procWakeHolder).cmd.Process.Pid
		m.mu.Unlock()
		waitUntil(t, 5*time.Second, "caffeinate assertion to appear", func() bool { return assertionFor(pid) })
		t.Logf("caffeinate pid %d holds PreventUserIdleSystemSleep", pid)

		release()
		waitUntil(t, 5*time.Second, "caffeinate assertion to disappear", func() bool { return !assertionFor(pid) })
		if err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run(); err == nil {
			t.Fatalf("caffeinate pid %d still running after release", pid)
		}
	}
}
