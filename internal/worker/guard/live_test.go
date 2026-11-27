package guard

import (
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
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

// TestLiveOOMLabel lets the kernel's OOM killer take a hog (the guard in passthrough, so
// it doesn't shed) and expects the session to be reported OOM-killed. Run it in a
// memory-limited container so the host is not at risk.
//
//	SYRE_GUARD_LIVE=1 go test -run TestLiveOOMLabel -v ./internal/worker/guard
func TestLiveOOMLabel(t *testing.T) {
	if os.Getenv("SYRE_GUARD_HOG") == "1" {
		hog()
		return
	}
	if os.Getenv("SYRE_GUARD_LIVE") != "1" || runtime.GOOS != "linux" {
		t.Skip("set SYRE_GUARD_LIVE=1 on Linux (in a memory-limited container) to run the live OOM label check")
	}
	g := New(Config{Baseline: NewBaseline(hostTotalRAM(), false, "container"), Mode: ModePassthrough})
	exe, _ := os.Executable()
	l := g.Prepare(LaunchSpec{Binary: exe, Args: []string{"-test.run", "^TestLiveOOMLabel$"}, Owner: "hog", Kind: "task", Priority: PriorityTicket, Headless: true})
	cmd := exec.Command(l.Binary, l.Args...)
	cmd.Env = append(os.Environ(), "SYRE_GUARD_HOG=1")
	setGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := l.Attach(cmd.Process.Pid)
	_ = cmd.Wait()
	u := s.Release(cmd.ProcessState)
	if !u.OOMKilled || !g.OwnerOOMKilled("hog") {
		t.Fatalf("hog ended (%v) without an OOM label: %+v", cmd.ProcessState, u)
	}
	t.Logf("hog OOM-killed and labeled (%v)", cmd.ProcessState)
}

// TestLiveHighCommit runs a job that commits far more memory than it touches (a JVM
// with a large -Xms, Node with big untouched ArrayBuffers) under the native guard and
// expects it to finish untouched: the guard must judge physical memory, not commit.
//
//	SYRE_GUARD_LIVE=1 SYRE_GUARD_CMD='sh job.sh' go test -run TestLiveHighCommit -v ./internal/worker/guard
func TestLiveHighCommit(t *testing.T) {
	if os.Getenv("SYRE_GUARD_LIVE") != "1" || os.Getenv("SYRE_GUARD_CMD") == "" {
		t.Skip("set SYRE_GUARD_LIVE=1 and SYRE_GUARD_CMD to run the live high-commit check")
	}
	argv := strings.Fields(os.Getenv("SYRE_GUARD_CMD"))
	virt := os.Getenv("SYRE_GUARD_VIRT")
	if virt == "" {
		virt = "bare_metal"
	}
	g := New(Config{
		Baseline: NewBaseline(hostTotalRAM(), DetectDesktop(), virt),
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	st := g.Status()
	t.Logf("mode %s via %s, reserve %d MiB of %d MiB", st.Mode, st.Mechanism, st.Reserve>>20, st.TotalRAM>>20)

	bin, err := exec.LookPath(argv[0])
	if err != nil {
		t.Fatal(err)
	}
	l := g.Prepare(LaunchSpec{Binary: bin, Args: argv[1:], Owner: "high-commit", Kind: "task", Priority: PriorityTicket, Headless: true})
	cmd := exec.Command(l.Binary, l.Args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	setGroup(cmd)
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := l.Attach(cmd.Process.Pid)
	werr := cmd.Wait()
	u := s.Release(cmd.ProcessState)
	if u.Shed != nil {
		t.Fatalf("high-commit job shed: %+v", *u.Shed)
	}
	if werr != nil {
		t.Fatalf("high-commit job failed: %v", werr)
	}
	t.Logf("finished in %s untouched, peak %d MiB", time.Since(start).Round(time.Millisecond), u.PeakRSS>>20)
}

// hog fills 128 MiB with incompressible data every 100 ms until it is killed. (Touching
// one byte per page is not enough: macOS compresses near-zero pages to almost nothing.)
func hog() {
	var keep [][]uint64
	x := uint64(time.Now().UnixNano()) | 1
	for {
		b := make([]uint64, (128<<20)/8)
		for i := range b {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			b[i] = x
		}
		keep = append(keep, b)
		runtime.KeepAlive(keep)
		time.Sleep(100 * time.Millisecond)
	}
}
