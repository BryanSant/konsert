//go:build unix

package worker

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// shBackend runs script under /bin/sh as the "helper" so the process handling is tested
// without caffeinate or systemd-inhibit.
func shBackend(script string) *commandWakeBackend {
	return &commandWakeBackend{
		method:       "sh",
		argv:         func(string) []string { return []string{"sh", "-c", script} },
		startupProbe: 100 * time.Millisecond,
		stopTimeout:  2 * time.Second,
	}
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func TestCommandWakeBackendReleaseKillsGroup(t *testing.T) {
	// The helper has a child, as systemd-inhibit does: both must go.
	b := shBackend("sleep 1000 & wait")
	h, err := b.Acquire("test")
	if err != nil {
		t.Fatal(err)
	}
	ph := h.(*procWakeHolder)
	pgid := ph.cmd.Process.Pid
	if !groupAlive(pgid) {
		t.Fatal("helper group not running after Acquire")
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	default:
		t.Fatal("Release returned before the helper was reaped")
	}
	waitUntil(t, 2*time.Second, "helper group to exit", func() bool { return !groupAlive(pgid) })
	if err := h.Release(); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestCommandWakeBackendSIGKILLFallback(t *testing.T) {
	b := shBackend(`trap "" TERM; while :; do sleep 0.05; done`)
	b.stopTimeout = 200 * time.Millisecond
	h, err := b.Acquire("test")
	if err != nil {
		t.Fatal(err)
	}
	pgid := h.(*procWakeHolder).cmd.Process.Pid
	start := time.Now()
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("helper ignoring SIGTERM exited too early; trap not effective?")
	}
	waitUntil(t, 2*time.Second, "helper group to exit", func() bool { return !groupAlive(pgid) })
}

func TestCommandWakeBackendMissingTool(t *testing.T) {
	b := &commandWakeBackend{
		method:   "systemd-inhibit",
		argv:     func(string) []string { return []string{"systemd-inhibit", "sleep", "infinity"} },
		lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
	}
	_, err := b.Acquire("x")
	if err == nil || !strings.Contains(err.Error(), "systemd-inhibit not found on PATH") {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestCommandWakeBackendImmediateExit(t *testing.T) {
	// e.g. systemd-inhibit in a container: "Failed to inhibit: ... bus".
	b := shBackend("echo 'Failed to inhibit: no bus' >&2; exit 1")
	_, err := b.Acquire("x")
	if err == nil || !strings.Contains(err.Error(), "Failed to inhibit: no bus") {
		t.Fatalf("err = %v, want helper stderr", err)
	}
}

func TestCommandWakeBackendLateExitReportedByManager(t *testing.T) {
	m := NewWakeLockManager(shBackend("sleep 0.3; echo gone >&2; exit 2"), nil, nil)
	if st := m.Set(true, ""); !st.Active {
		t.Fatalf("state = %+v", st)
	}
	waitUntil(t, 3*time.Second, "helper exit to be reported", func() bool { return !m.State().Active })
	if st := m.State(); !strings.Contains(st.Error, "gone") {
		t.Fatalf("state = %+v, want helper stderr in error", st)
	}
}

func TestCommandWakeBackendReceivesReason(t *testing.T) {
	var got string
	b := &commandWakeBackend{
		method: "sh",
		argv: func(reason string) []string {
			got = reason
			return []string{"sh", "-c", "sleep 1000"}
		},
		startupProbe: 50 * time.Millisecond,
	}
	m := NewWakeLockManager(b, nil, nil)
	m.Set(true, "nightly build")
	defer m.Shutdown()
	if got != "nightly build" {
		t.Fatalf("reason = %q", got)
	}
}
