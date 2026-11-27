//go:build windows

package guard

import (
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestJobFlagsNeverLimitMemory: commit limits crash JVM/.NET/V8 with RAM free.
func TestJobFlagsNeverLimitMemory(t *testing.T) {
	for _, bg := range []bool{false, true} {
		f := jobLimitFlags(bg)
		if f&forbiddenMemoryLimits != 0 {
			t.Fatalf("job flags %#x include a memory limit", f)
		}
		if f&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
			t.Fatalf("job flags %#x lack KILL_ON_JOB_CLOSE", f)
		}
	}
}

// TestJobKillsTree: a session's Job Object takes the whole tree down, grandchildren too.
func TestJobKillsTree(t *testing.T) {
	// The first ping delays the spawns past the Job assignment below.
	cmd := exec.Command("cmd", "/c", "ping -n 2 127.0.0.1 >nul & start /b ping -n 60 127.0.0.1 >nul & ping -n 60 127.0.0.1 >nul")
	if err := cmd.Start(); err != nil {
		t.Skip("cmd unavailable:", err)
	}
	g := newGuard(nativePlatform(), Config{Mode: ModePassthrough})
	s := g.Prepare(LaunchSpec{Binary: "cmd", Kind: "test"}).Attach(cmd.Process.Pid)
	if s.Job() == 0 {
		t.Fatal("no job object")
	}
	time.Sleep(2500 * time.Millisecond)
	if n := len(jobPIDs(s.Job())); n < 2 {
		t.Fatalf("job holds %d processes, want the tree", n)
	}
	if !s.Kill() {
		t.Fatal("kill failed")
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("root did not exit after job kill")
	}
	if n := len(jobPIDs(s.Job())); n != 0 {
		t.Fatalf("%d processes survived the job kill", n)
	}
	s.Release(cmd.ProcessState)
}
