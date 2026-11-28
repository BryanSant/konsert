//go:build linux || darwin

package procs

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A real process tree: a "build" (sh) whose daemon (a backgrounded sleep in its own
// session) outlives it. Both are attributed to the session while the build runs, and
// the daemon stays attributed after the build exits and it is reparented.
func TestAttributeRealTree(t *testing.T) {
	pidFile := t.TempDir() + "/daemon.pid"
	build := exec.Command("sh", "-c", "sleep 60 & echo $! > "+pidFile+"; exec sleep 60")
	if err := build.Start(); err != nil {
		t.Fatal(err)
	}
	var daemon int
	for range 100 {
		if raw, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(raw)) != "" {
			daemon, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if daemon == 0 {
		t.Fatal("no daemon pid")
	}
	t.Cleanup(func() { _ = syscall.Kill(daemon, syscall.SIGKILL); _ = build.Process.Kill(); _ = build.Wait() })

	s, a := NewSampler(), NewAttributor()
	roots := []Root{{Key: "task:build", PID: build.Process.Pid}}
	smp, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	u := byKey(a.Attribute(smp, roots))["task:build"]
	if u.Procs != 2 || u.RSS == 0 || !containsPID(u.PIDs, daemon) {
		t.Fatalf("while building: %+v (daemon %d)", u, daemon)
	}

	// The build exits; its daemon is reparented and keeps running.
	_ = build.Process.Kill()
	_ = build.Wait()
	smp, _ = s.Sample()
	u = byKey(a.Attribute(smp, nil))["task:build"]
	if u.Procs != 1 || u.Orphans != 1 || u.PIDs[0] != daemon {
		t.Fatalf("after the build exited: %+v", u)
	}
}

func containsPID(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}
