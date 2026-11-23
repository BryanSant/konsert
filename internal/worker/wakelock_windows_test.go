//go:build windows

package worker

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

func TestExecStateBackendUsesOneThread(t *testing.T) {
	var mu sync.Mutex
	var calls []uint32
	b := &execStateWakeBackend{setState: func(flags uint32) (uint32, error) {
		mu.Lock()
		calls = append(calls, flags)
		mu.Unlock()
		return esContinuous, nil
	}}
	h, err := b.Acquire("x")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	_ = h.Release()
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0] != esContinuous|esSystemRequired || calls[1] != esContinuous {
		t.Fatalf("calls = %#x", calls)
	}
}

func TestExecStateBackendFailure(t *testing.T) {
	b := &execStateWakeBackend{setState: func(uint32) (uint32, error) { return 0, errors.New("denied") }}
	m := NewWakeLockManager(b, nil, nil)
	if st := m.Set(true, ""); st.Active || !strings.Contains(st.Error, "denied") {
		t.Fatalf("state = %+v", st)
	}
}

// TestRealExecutionState sets the real execution state (KONSERT_TEST_REAL_WAKELOCK=1) and,
// when powercfg can read requests (needs an elevated shell), checks it is listed.
func TestRealExecutionState(t *testing.T) {
	if os.Getenv("KONSERT_TEST_REAL_WAKELOCK") != "1" {
		t.Skip("set KONSERT_TEST_REAL_WAKELOCK=1 to set a real execution state")
	}
	exe, _ := os.Executable()
	exeName := strings.ToLower(exe[strings.LastIndexAny(exe, `\/`)+1:])
	requests := func() (string, bool) {
		out, err := exec.Command("powercfg", "/requests").CombinedOutput()
		return strings.ToLower(string(out)), err == nil
	}
	m := NewWakeLockManager(nil, nil, nil)
	st := m.Set(true, "konsert real wakelock test")
	if !st.Active || st.Method != "SetThreadExecutionState" {
		t.Fatalf("enable: %+v", st)
	}
	if out, ok := requests(); ok {
		t.Logf("powercfg /requests while held:\n%s", out)
		if !strings.Contains(out, exeName) {
			t.Errorf("powercfg /requests does not list %s", exeName)
		}
	} else {
		t.Logf("powercfg /requests unavailable (not elevated?): %s", out)
	}
	m.Shutdown()
	if out, ok := requests(); ok && strings.Contains(out, exeName) {
		t.Errorf("powercfg /requests still lists %s after release:\n%s", exeName, out)
	}
}
