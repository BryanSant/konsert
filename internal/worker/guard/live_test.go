package guard

import (
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// TestLiveShed is the anti-lockup check on a real machine (opt-in: it eats the host's
// memory). It runs a memory hog under the native guard in the host's auto mode and
// expects the guard to shed it while this process stays responsive.
//
//	SYRE_GUARD_LIVE=1 go test -run TestLiveShed -v -timeout 10m ./internal/worker/guard
func TestLiveShed(t *testing.T) {
	if os.Getenv("SYRE_GUARD_HOG") == "1" {
		hog()
		return
	}
	if os.Getenv("SYRE_GUARD_LIVE") != "1" {
		t.Skip("set SYRE_GUARD_LIVE=1 to run the live anti-lockup check")
	}
	total := hostTotalRAM()
	virt := os.Getenv("SYRE_GUARD_VIRT")
	if virt == "" {
		virt = "bare_metal"
	}
	g := New(Config{
		Baseline: NewBaseline(total, DetectDesktop(), virt),
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	st := g.Status()
	t.Logf("mode %s via %s, reserve %d MiB of %d MiB", st.Mode, st.Mechanism, st.Reserve>>20, st.TotalRAM>>20)
	for _, n := range st.ProtectNotes {
		t.Log(n)
	}

	exe, _ := os.Executable()
	l := g.Prepare(LaunchSpec{Binary: exe, Args: []string{"-test.run", "^TestLiveShed$"}, Owner: "hog", Kind: "task", Priority: PriorityTicket, Headless: true})
	cmd := exec.Command(l.Binary, l.Args...)
	cmd.Env = append(os.Environ(), "SYRE_GUARD_HOG=1")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	setGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := l.Attach(cmd.Process.Pid)
	start := time.Now()
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	// Responsiveness probe: how late a 100 ms ticker fires while the hog runs.
	var worst time.Duration
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	last := time.Now()
	deadline := time.After(5 * time.Minute)
loop:
	for {
		select {
		case <-exited:
			break loop
		case now := <-tick.C:
			worst = max(worst, now.Sub(last)-100*time.Millisecond)
			last = now
		case <-deadline:
			_ = cmd.Process.Kill()
			t.Fatal("hog not shed within 5 minutes")
		}
	}
	u := s.Release(cmd.ProcessState)
	if u.Shed == nil {
		t.Fatalf("hog exited without a shed (state %v)", cmd.ProcessState)
	}
	t.Logf("shed after %s: reason %s, rss %d MiB, host avail %d MiB, peak %d MiB, worst ticker lag %s",
		time.Since(start).Round(time.Millisecond), u.Shed.Reason, u.Shed.RSS>>20, u.Shed.HostAvail>>20, u.PeakRSS>>20, worst.Round(time.Millisecond))
	if worst > 5*time.Second {
		t.Errorf("host unresponsive: a 100ms ticker was %s late", worst)
	}
}

// hog allocates and touches 128 MiB every 100 ms until it is killed.
func hog() {
	var keep [][]byte
	page := os.Getpagesize()
	for {
		b := make([]byte, 128<<20)
		for i := 0; i < len(b); i += page {
			b[i] = 1
		}
		keep = append(keep, b)
		runtime.KeepAlive(keep)
		time.Sleep(100 * time.Millisecond)
	}
}
