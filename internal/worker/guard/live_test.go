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
//	SYRE_GUARD_LIVE=1 SYRE_GUARD_LIVE_HOG=commit go test -run TestLiveShed -v ./internal/worker/guard  (Windows)
func TestLiveShed(t *testing.T) {
	switch os.Getenv("SYRE_GUARD_HOG") {
	case "1":
		hog()
		return
	case "commit":
		commitHog()
		return
	}
	if os.Getenv("SYRE_GUARD_LIVE") != "1" {
		t.Skip("set SYRE_GUARD_LIVE=1 to run the live anti-lockup check")
	}
	g := liveGuard(t)
	st := g.Status()
	t.Logf("mode %s via %s, reserve %d MiB of %d MiB", st.Mode, st.Mechanism, st.Reserve>>20, st.TotalRAM>>20)
	for _, n := range st.ProtectNotes {
		t.Log(n)
	}

	exe, _ := os.Executable()
	l := g.Prepare(LaunchSpec{Binary: exe, Args: []string{"-test.run", "^TestLiveShed$"}, Owner: "hog", Kind: "task", Priority: PriorityTicket, Headless: true})
	cmd := exec.Command(l.Binary, l.Args...)
	// SYRE_GUARD_LIVE_HOG=commit runs a hog that commits memory without touching it
	// (Windows: it must be shed before the commit limit, with RAM still free).
	kind := os.Getenv("SYRE_GUARD_LIVE_HOG")
	if kind == "" {
		kind = "1"
	}
	cmd.Env = append(os.Environ(), "SYRE_GUARD_HOG="+kind)
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

// liveGuard is the native guard for a live test: the host's auto mode, unless
// SYRE_GUARD_MODE names one (adaptive_strict on a VM exercises the slice's MemoryHigh
// and the pool-stall trip). SYRE_GUARD_VIRT overrides the virtualization class.
func liveGuard(t *testing.T) *Guard {
	t.Helper()
	virt := os.Getenv("SYRE_GUARD_VIRT")
	if virt == "" {
		virt = "bare_metal"
	}
	mode, err := ParseMode(os.Getenv("SYRE_GUARD_MODE"))
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{
		Baseline: NewBaseline(hostTotalRAM(), DetectDesktop(), virt),
		Mode:     mode,
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
}

// liveSession starts this test binary as a managed session running hog kind
// (SYRE_GUARD_HOG) under g.
func liveSession(t *testing.T, g *Guard, test, kind, owner string, prio Priority) (*exec.Cmd, *Session) {
	t.Helper()
	exe, _ := os.Executable()
	l := g.Prepare(LaunchSpec{Binary: exe, Args: []string{"-test.run", "^" + test + "$"}, Owner: owner, Kind: "task", Priority: prio, Headless: true})
	cmd := exec.Command(l.Binary, l.Args...)
	cmd.Env = append(os.Environ(), "SYRE_GUARD_HOG="+kind)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	setGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, l.Attach(cmd.Process.Pid)
}

// TestLiveVictimChoice runs a growing hog (an older ticket run), a small ticket run
// started after it, and a small interactive session, and expects the guard to shed the
// hog: the largest session within the lowest priority, not the newest.
//
//	SYRE_GUARD_LIVE=1 go test -run TestLiveVictimChoice -v ./internal/worker/guard
func TestLiveVictimChoice(t *testing.T) {
	switch os.Getenv("SYRE_GUARD_HOG") {
	case "1":
		hog()
		return
	case "small":
		smallHolder()
		return
	}
	if os.Getenv("SYRE_GUARD_LIVE") != "1" {
		t.Skip("set SYRE_GUARD_LIVE=1 to run the live victim-choice check")
	}
	g := liveGuard(t)
	st := g.Status()
	t.Logf("mode %s via %s, reserve %d MiB of %d MiB", st.Mode, st.Mechanism, st.Reserve>>20, st.TotalRAM>>20)
	hogCmd, hogS := liveSession(t, g, "TestLiveVictimChoice", "1", "hog", PriorityTicket)
	time.Sleep(500 * time.Millisecond)
	smallCmd, smallS := liveSession(t, g, "TestLiveVictimChoice", "small", "small-ticket", PriorityTicket)
	chatCmd, chatS := liveSession(t, g, "TestLiveVictimChoice", "small", "small-chat", PriorityInteractive)
	defer func() {
		for _, c := range []*exec.Cmd{smallCmd, chatCmd} {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
		smallS.Release(smallCmd.ProcessState)
		chatS.Release(chatCmd.ProcessState)
	}()
	done := make(chan struct{})
	go func() { _ = hogCmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Minute):
		_ = hogCmd.Process.Kill()
		t.Fatal("hog not shed within 5 minutes")
	}
	u := hogS.Release(hogCmd.ProcessState)
	if u.Shed == nil {
		t.Fatalf("hog exited without a shed (%v)", hogCmd.ProcessState)
	}
	if smallS.Shed() != nil || chatS.Shed() != nil {
		t.Fatalf("a small session was shed too: ticket %v chat %v", smallS.Shed(), chatS.Shed())
	}
	for name, c := range map[string]*exec.Cmd{"small ticket": smallCmd, "chat": chatCmd} {
		if c.ProcessState != nil {
			t.Fatalf("%s died: %v", name, c.ProcessState)
		}
	}
	t.Logf("hog shed (%s, %d MiB, bystander %v, %d sessions); the newer small ticket run and the chat survived",
		u.Shed.Reason, u.Shed.RSS>>20, u.Shed.Bystander, u.Shed.ActiveSessions)
}

// smallHolder touches 200 MiB and waits to be killed.
func smallHolder() {
	b := make([]byte, 200<<20)
	for i := range b {
		b[i] = byte(i)
	}
	for {
		time.Sleep(time.Second)
		runtime.KeepAlive(b)
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

// commitHog commits 512 MiB every 100 ms and never touches it: on Windows the Go
// runtime commits what it allocates, so the commit charge climbs while RAM stays free.
func commitHog() {
	var keep [][]byte
	for {
		keep = append(keep, make([]byte, 512<<20))
		runtime.KeepAlive(keep)
		time.Sleep(100 * time.Millisecond)
	}
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
