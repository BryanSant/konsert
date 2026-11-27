//go:build windows

package guard

import (
	"os"
	"os/exec"
	"runtime"
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

// TestPeakIsResidentNotCommitted: a process that commits 2 GiB but touches 128 MiB must
// report a peak near 128 MiB, not its commit (what PeakJobMemoryUsed would say).
func TestPeakIsResidentNotCommitted(t *testing.T) {
	if os.Getenv("SYRE_GUARD_ALLOC_CHILD") == "1" {
		if _, err := windows.VirtualAlloc(0, 2<<30, windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_READWRITE); err != nil {
			os.Exit(3)
		}
		touched := make([]byte, 128<<20)
		for i := range touched {
			touched[i] = byte(i)
		}
		time.Sleep(time.Second)
		runtime.KeepAlive(touched)
		os.Exit(0)
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run", "^TestPeakIsResidentNotCommitted$")
	cmd.Env = append(os.Environ(), "SYRE_GUARD_ALLOC_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Passthrough: no watchdog samples, so the root's own peak (read after exit) counts.
	g := newGuard(nativePlatform(), Config{Mode: ModePassthrough})
	s := g.Prepare(LaunchSpec{Binary: exe, Kind: "test"}).Attach(cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child: %v", err)
	}
	u := s.Release(cmd.ProcessState)
	if u.PeakRSS < 100<<20 || u.PeakRSS > 1<<30 {
		t.Fatalf("peak %d MiB, want about 128 MiB (resident), not the 2 GiB commit", u.PeakRSS>>20)
	}
}
