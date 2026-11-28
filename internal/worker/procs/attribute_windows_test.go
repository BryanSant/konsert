package procs

import (
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// A real process tree on Windows: cmd (the "build") starts a background ping (its
// daemon) and a foreground one. After cmd is killed the pings keep running with a dead
// parent PID (Windows doesn't reparent): they stay attributed by lineage.
func TestAttributeRealTree(t *testing.T) {
	build := exec.Command("cmd", "/c", "start /b ping -n 60 127.0.0.1 >nul & ping -n 60 127.0.0.1 >nul")
	if err := build.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(build.Process.Pid)).Run()
		_ = build.Wait()
	})
	s, a := NewSampler(), NewAttributor()
	roots := []Root{{Key: "task:build", PID: build.Process.Pid}}
	var u SessionUsage
	for range 50 {
		smp, err := s.Sample()
		if err != nil {
			t.Fatal(err)
		}
		u = byKey(a.Attribute(smp, roots))["task:build"]
		if u.Procs >= 3 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if u.Procs < 3 {
		t.Fatalf("while building: %+v", u)
	}
	pings := u.Procs - 1
	_ = build.Process.Kill()
	_ = build.Wait()
	smp, _ := s.Sample()
	u = byKey(a.Attribute(smp, nil))["task:build"]
	if u.Procs < 2 || u.Orphans != u.Procs || u.Procs > pings {
		t.Fatalf("after the build was killed: %+v (had %d children)", u, pings)
	}
}
