package guard

import (
	"sync"
	"testing"
	"time"
)

type fakePlat struct {
	mu       sync.Mutex
	sig      Signal
	frozen   []int
	killed   []int
	bg       map[int]bool
	order    []string
	wrapped  bool
	rssBy    map[int]uint64 // per-pid RSS; 42 MiB when unset
	ooms     uint64
	commitBy map[int]uint64
}

func (f *fakePlat) mechanism(Mode) string                   { return "fake" }
func (f *fakePlat) configure(Mode, Baseline, throttleState) {}
func (f *fakePlat) wrap(l *Launch, m Mode) {
	if m != ModePassthrough {
		f.wrapped = true
	}
}
func (f *fakePlat) attach(*Session, Mode) {}
func (f *fakePlat) release(*Session)      {}
func (f *fakePlat) rss(s *Session) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rssBy[s.pid]; ok {
		return r
	}
	return 42 * mib
}
func (f *fakePlat) peak(*Session) uint64             { return 0 }
func (f *fakePlat) members(s *Session) []int         { return []int{s.pid} }
func (f *fakePlat) throttle(*Session, throttleState) {}
func (f *fakePlat) selfProtect() []string            { return nil }
func (f *fakePlat) commit(s *Session) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commitBy[s.pid]
}
func (f *fakePlat) oomKills() (uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ooms, true
}
func (f *fakePlat) sample() Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sig
}
func (f *fakePlat) set(sig Signal) {
	f.mu.Lock()
	f.sig = sig
	f.mu.Unlock()
}
func (f *fakePlat) freeze(s *Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frozen = append(f.frozen, s.pid)
	f.order = append(f.order, "freeze")
	return nil
}
func (f *fakePlat) kill(s *Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, s.pid)
	f.order = append(f.order, "kill")
	return nil
}
func (f *fakePlat) background(s *Session, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bg == nil {
		f.bg = map[int]bool{}
	}
	f.bg[s.pid] = on
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }
func testGuard(mode Mode) (*Guard, *fakePlat, *fakeClock) {
	p := &fakePlat{}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newGuard(p, Config{Baseline: NewBaseline(8<<30, true, "bare_metal"), Mode: mode})
	g.now = clk.now
	g.running = true // tests drive tick() by hand; keep the loop from starting
	return g, p, clk
}

func attach(g *Guard, clk *fakeClock, pid int, prio Priority, owner string) *Session {
	clk.add(time.Second)
	return g.Prepare(LaunchSpec{Binary: "x", Owner: owner, Kind: "task", Priority: prio}).Attach(pid)
}

func TestSurvivalReserve(t *testing.T) {
	cases := []struct {
		total   uint64
		desktop bool
		want    uint64
	}{
		{1 << 30, false, 96 * mib},      // 1 GiB VM: the floor
		{8 << 30, true, (8 << 30) / 20}, // 8 GiB laptop: ~410 MiB
		{32 << 30, true, 1536 * mib},    // 32 GiB workstation: desktop cap
		{32 << 30, false, 768 * mib},    // 32 GiB headless server: headless cap
		{64 << 30, true, 1536 * mib},    // never more than 1.5 GiB
		{128 * mib, false, 64 * mib},    // tiny: never the whole machine
	}
	for _, c := range cases {
		if got := ComputeSurvivalReserve(c.total, c.desktop); got != c.want {
			t.Errorf("reserve(%d MiB, desktop=%v) = %d MiB, want %d MiB", c.total/mib, c.desktop, got/mib, c.want/mib)
		}
	}
}

func TestDefaultModeAndPrecedence(t *testing.T) {
	for virt, want := range map[string]Mode{
		"container": ModeAdaptive, "mac_container": ModeAdaptive,
		"vm": ModeAdaptive, "wsl": ModeAdaptive, "unknown": ModeAdaptive,
		"bare_metal": ModeAdaptiveStrict,
	} {
		if got := DefaultMode(virt); got != want {
			t.Errorf("DefaultMode(%s) = %s, want %s", virt, got, want)
		}
	}
	g, _, _ := testGuard(ModeAuto)
	if st := g.Status(); st.Mode != ModeAdaptiveStrict || st.ModeSource != "auto" {
		t.Fatalf("auto on bare metal = %s/%s", st.Mode, st.ModeSource)
	}
	g.configMode = ModeAdaptive
	if st := g.Status(); st.Mode != ModeAdaptive || st.ModeSource != "config" {
		t.Fatalf("config = %s/%s", st.Mode, st.ModeSource)
	}
	g.SetHubMode(ModePassthrough)
	if st := g.Status(); st.Mode != ModePassthrough || st.ModeSource != "hub" || st.BaseMode != ModeAdaptive {
		t.Fatalf("hub = %+v", st)
	}
	g.SetHubMode("")
	if g.Mode() != ModeAdaptive {
		t.Fatalf("cleared hub mode = %s", g.Mode())
	}
}

func TestParseModeAndStrictness(t *testing.T) {
	if _, err := ParseMode("hermetic"); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if m, _ := ParseMode(" Adaptive_Strict "); m != ModeAdaptiveStrict {
		t.Fatalf("parse = %s", m)
	}
	if !(Strictness(ModePassthrough) < Strictness(ModeAdaptive) && Strictness(ModeAdaptive) < Strictness(ModeAdaptiveStrict)) {
		t.Fatal("strictness order")
	}
}

func TestShedDebounceAndLIFO(t *testing.T) {
	g, p, clk := testGuard(ModeAuto)
	reserve := g.baseline.Reserve
	old := attach(g, clk, 100, PriorityTicket, "t-old")
	newer := attach(g, clk, 200, PriorityTicket, "t-new")
	user := attach(g, clk, 300, PriorityInteractive, "chat-user")

	var got []Evidence
	defer OnShed(func(ev Evidence) { got = append(got, ev) })()

	p.set(Signal{Avail: reserve - 1, AvailOK: true, PoolStall: -1})
	g.tick() // first low sample: debounce starts
	clk.add(250 * time.Millisecond)
	g.tick()
	if len(p.killed) != 0 {
		t.Fatalf("shed before the 500ms debounce: %v", p.killed)
	}
	clk.add(300 * time.Millisecond)
	g.tick()
	if len(p.killed) != 1 || p.killed[0] != 200 {
		t.Fatalf("killed = %v, want the newest ticket session (200)", p.killed)
	}
	if p.order[0] != "freeze" || p.order[1] != "kill" {
		t.Fatalf("order = %v, want freeze before kill", p.order)
	}
	if len(got) != 1 || got[0].Owner != "t-new" || got[0].Reason != ReasonBelowReserve || got[0].RSS != 42*mib || got[0].ActiveSessions != 3 {
		t.Fatalf("evidence = %+v", got)
	}
	if newer.Shed() == nil || old.Shed() != nil || user.Shed() != nil {
		t.Fatal("Shed() markers wrong")
	}

	// Settle: no second shed straight away even though pressure persists.
	clk.add(time.Second)
	g.tick()
	if len(p.killed) != 1 {
		t.Fatalf("shed during settle: %v", p.killed)
	}
	clk.add(2 * time.Second)
	g.tick()
	clk.add(600 * time.Millisecond)
	g.tick()
	if len(p.killed) != 2 || p.killed[1] != 100 {
		t.Fatalf("second victim = %v, want 100 (interactive is spared while others remain)", p.killed)
	}
	// Only the interactive session remains: it goes last.
	clk.add(3 * time.Second)
	g.tick()
	clk.add(600 * time.Millisecond)
	g.tick()
	if len(p.killed) != 3 || p.killed[2] != 300 {
		t.Fatalf("third victim = %v, want the interactive session", p.killed)
	}
}

// TestShedTakesTheHogAndMarksBystanders: within a priority the session using the most
// memory goes first, and a victim holding little of the used memory is a bystander.
func TestShedTakesTheHogAndMarksBystanders(t *testing.T) {
	g, p, clk := testGuard(ModeAuto) // 8 GiB host
	reserve := g.baseline.Reserve
	p.rssBy = map[int]uint64{100: 6 << 30, 200: 300 * mib, 300: 1 << 30}
	attach(g, clk, 100, PriorityTicket, "hog")
	attach(g, clk, 200, PriorityTicket, "small")
	attach(g, clk, 300, PriorityInteractive, "chat-user")

	var got []Evidence
	defer OnShed(func(ev Evidence) { got = append(got, ev) })()
	p.set(Signal{Avail: reserve - 1, AvailOK: true})
	g.tick()
	clk.add(600 * time.Millisecond)
	g.tick()
	if len(p.killed) != 1 || p.killed[0] != 100 {
		t.Fatalf("killed = %v, want the 6 GiB ticket run (100), not the newer small one", p.killed)
	}
	if got[0].Bystander || got[0].HostUsed != g.baseline.TotalRAM-(reserve-1) {
		t.Fatalf("hog evidence = %+v, want a culprit with host used memory", got[0])
	}

	// Pressure persists (from outside Syre): the small run goes next, as a bystander.
	clk.add(3 * time.Second)
	g.tick()
	clk.add(600 * time.Millisecond)
	g.tick()
	if len(p.killed) != 2 || p.killed[1] != 200 {
		t.Fatalf("killed = %v, want the small run next", p.killed)
	}
	if !got[1].Bystander {
		t.Fatalf("300 MiB of ~7.6 GiB used must be a bystander: %+v", got[1])
	}
}

func TestRecoveryResetsDebounce(t *testing.T) {
	g, p, clk := testGuard(ModeAuto)
	attach(g, clk, 1, PriorityTask, "a")
	reserve := g.baseline.Reserve
	p.set(Signal{Avail: reserve - 1, AvailOK: true})
	g.tick()
	clk.add(400 * time.Millisecond)
	p.set(Signal{Avail: reserve + 1, AvailOK: true})
	g.tick()
	clk.add(400 * time.Millisecond)
	p.set(Signal{Avail: reserve - 1, AvailOK: true})
	g.tick()
	if len(p.killed) != 0 {
		t.Fatal("a brief dip that recovered still shed")
	}
}

func TestPoolStallAndCritical(t *testing.T) {
	g, p, clk := testGuard(ModeAuto)
	attach(g, clk, 7, PriorityTask, "stuck")
	// Host memory looks fine (MemoryHigh keeps it above the reserve) but the worker
	// pool is stalled in reclaim.
	p.set(Signal{Avail: 4 << 30, AvailOK: true, PoolStall: 35})
	g.tick()
	clk.add(4 * time.Second)
	g.tick()
	if len(p.killed) != 0 {
		t.Fatal("stall shed before 5s")
	}
	clk.add(1500 * time.Millisecond)
	g.tick()
	if len(p.killed) != 1 {
		t.Fatal("a 5s pool stall did not shed")
	}

	g2, p2, clk2 := testGuard(ModeAuto)
	attach(g2, clk2, 8, PriorityTask, "x")
	p2.set(Signal{Level: LevelCritical})
	g2.tick()
	if len(p2.killed) != 1 {
		t.Fatal("critical pressure did not shed at once")
	}
}

func TestBackgroundOnWarnAndThrottle(t *testing.T) {
	g, p, clk := testGuard(ModeAuto)
	attach(g, clk, 9, PriorityTask, "x")
	p.set(Signal{Level: LevelWarn, Avail: 4 << 30, AvailOK: true})
	g.tick()
	if !p.bg[9] {
		t.Fatal("warn did not background the session")
	}
	p.set(Signal{Level: LevelNormal, Avail: 4 << 30, AvailOK: true})
	g.tick()
	if p.bg[9] {
		t.Fatal("normal did not restore the session")
	}
	g.SetThrottle(true, 0)
	g.tick()
	if !p.bg[9] {
		t.Fatal("throttle did not background the session")
	}
}

func TestPassthroughNeitherWrapsNorWatches(t *testing.T) {
	g, p, _ := testGuard(ModePassthrough)
	g.running = false
	s := g.Prepare(LaunchSpec{Binary: "x"}).Attach(5)
	if p.wrapped {
		t.Fatal("passthrough wrapped the launch")
	}
	time.Sleep(20 * time.Millisecond)
	g.mu.Lock()
	running := g.running
	g.mu.Unlock()
	if running {
		t.Fatal("watchdog runs in passthrough")
	}
	if u := s.Release(nil); u.Shed != nil {
		t.Fatal("unexpected shed")
	}
	if g.Status().Sessions != 0 {
		t.Fatal("session not released")
	}
}

func TestWatchdogStopsWhenIdle(t *testing.T) {
	p := &fakePlat{}
	g := newGuard(p, Config{Baseline: NewBaseline(8<<30, true, "bare_metal")})
	s := g.Prepare(LaunchSpec{Binary: "x"}).Attach(11)
	time.Sleep(30 * time.Millisecond)
	g.mu.Lock()
	running := g.running
	g.mu.Unlock()
	if !running {
		t.Fatal("watchdog not running with a session")
	}
	s.Release(nil)
	g.kick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		running = g.running
		g.mu.Unlock()
		if !running {
			return
		}
		g.kick()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("watchdog kept polling with no sessions")
}

func TestSwapGrowthSheds(t *testing.T) {
	g, p, clk := testGuard(ModeAuto) // 8 GiB: budget 2 GiB
	attach(g, clk, 21, PriorityTask, "pager")
	avail := uint64(4 << 30) // "available" looks fine while the host pages out
	p.set(Signal{Avail: avail, AvailOK: true, Level: LevelWarn, SwapUsed: 3 << 30, SwapOK: true})
	g.tick() // baseline: swap that predates the work does not count
	clk.add(time.Second)
	p.set(Signal{Avail: avail, AvailOK: true, Level: LevelWarn, SwapUsed: 4 << 30, SwapOK: true})
	g.tick()
	if len(p.killed) != 0 {
		t.Fatal("shed within the swap budget")
	}
	p.set(Signal{Avail: avail, AvailOK: true, Level: LevelWarn, SwapUsed: 5<<30 + 1, SwapOK: true})
	g.tick()
	clk.add(600 * time.Millisecond)
	g.tick()
	if len(p.killed) != 1 {
		t.Fatal("swap growth past the budget did not shed")
	}
}

func TestSwapGrowthNearReserveSheds(t *testing.T) {
	g, p, clk := testGuard(ModeAuto)
	attach(g, clk, 21, PriorityTask, "pager")
	near := g.baseline.Reserve + g.baseline.Reserve/2 // above the reserve, within 2×
	p.set(Signal{Avail: near, AvailOK: true, Level: LevelNormal, SwapUsed: 1 << 30, SwapOK: true})
	g.tick()
	p.set(Signal{Avail: near, AvailOK: true, Level: LevelNormal, SwapUsed: 4 << 30, SwapOK: true})
	g.tick()
	clk.add(600 * time.Millisecond)
	g.tick()
	if len(p.killed) != 1 {
		t.Fatal("swap growth with memory near the reserve did not shed")
	}
}

// A person's own apps grow swap while pressure is normal and memory healthy: no shed.
func TestSwapGrowthAloneDoesNotShed(t *testing.T) {
	g, p, clk := testGuard(ModeAuto)
	attach(g, clk, 21, PriorityTask, "bystander")
	avail := uint64(4 << 30)
	for swap := uint64(1 << 30); swap <= 9<<30; swap += 1 << 30 {
		p.set(Signal{Avail: avail, AvailOK: true, Level: LevelNormal, SwapUsed: swap, SwapOK: true})
		g.tick()
		clk.add(time.Second)
	}
	if len(p.killed) != 0 {
		t.Fatal("swap growth alone shed a session")
	}
}

// Swap added before a calm spell is not blamed on work once pressure returns.
func TestSwapRebasesAfterCalm(t *testing.T) {
	g, p, clk := testGuard(ModeAuto)
	attach(g, clk, 21, PriorityTask, "later")
	avail := uint64(4 << 30)
	p.set(Signal{Avail: avail, AvailOK: true, Level: LevelNormal, SwapUsed: 1 << 30, SwapOK: true})
	g.tick()
	p.set(Signal{Avail: avail, AvailOK: true, Level: LevelNormal, SwapUsed: 6 << 30, SwapOK: true})
	g.tick()
	clk.add(swapRebase)
	g.tick() // calm long enough: the baseline moves to 6 GiB
	p.set(Signal{Avail: avail, AvailOK: true, Level: LevelWarn, SwapUsed: 7 << 30, SwapOK: true})
	g.tick()
	clk.add(600 * time.Millisecond)
	g.tick()
	if len(p.killed) != 0 {
		t.Fatal("shed for swap added before the calm")
	}
	p.set(Signal{Avail: avail, AvailOK: true, Level: LevelWarn, SwapUsed: 8<<30 + 1, SwapOK: true})
	g.tick()
	clk.add(600 * time.Millisecond)
	g.tick()
	if len(p.killed) != 1 {
		t.Fatal("swap growth past the rebased budget under WARN did not shed")
	}
}

// Under a hard limit (a container) there is no debounce: below the reserve sheds at
// once, and so does a decline that would get there before the next poll.
func TestHardLimitShedsWithoutDebounce(t *testing.T) {
	g, p, clk := testGuard(ModeAuto)
	attach(g, clk, 31, PriorityTicket, "hog")
	r := g.baseline.Reserve
	p.set(Signal{Avail: r - 1, AvailOK: true, AvailHard: true})
	g.tick()
	if len(p.killed) != 1 {
		t.Fatal("below the reserve under a hard limit did not shed at once")
	}

	g, p, clk = testGuard(ModeAuto)
	attach(g, clk, 32, PriorityTicket, "fast")
	p.set(Signal{Avail: r + 2<<30, AvailOK: true, AvailHard: true})
	g.tick()
	clk.add(pollInterval)
	p.set(Signal{Avail: r + 768<<20, AvailOK: true, AvailHard: true}) // 1.25 GiB per poll: the next one is too late
	g.tick()
	if len(p.killed) != 1 {
		t.Fatal("a decline that crosses the reserve before the next poll did not shed")
	}

	g, p, clk = testGuard(ModeAuto)
	attach(g, clk, 33, PriorityTicket, "steady")
	p.set(Signal{Avail: r + 2<<30, AvailOK: true, AvailHard: true})
	g.tick()
	clk.add(pollInterval)
	p.set(Signal{Avail: r + 1536<<20, AvailOK: true, AvailHard: true}) // 512 MiB per poll, 1.5 GiB left
	g.tick()
	if len(p.killed) != 0 {
		t.Fatal("shed with headroom for several more polls")
	}
	// Without a hard limit the same dip is debounced.
	g, p, clk = testGuard(ModeAuto)
	attach(g, clk, 34, PriorityTicket, "host")
	p.set(Signal{Avail: r - 1, AvailOK: true})
	g.tick()
	if len(p.killed) != 0 {
		t.Fatal("a host dip shed without the debounce")
	}
}

// Windows: commit headroom below the reserve sheds at once, and the victim is the
// session holding the most commit, not the most resident memory.
func TestCommitLowShedsLargestCommit(t *testing.T) {
	g, p, clk := testGuard(ModeAuto)
	p.rssBy = map[int]uint64{41: 4 << 30, 42: 200 << 20}
	p.commitBy = map[int]uint64{41: 4 << 30, 42: 30 << 30}
	attach(g, clk, 41, PriorityTicket, "resident")
	c := attach(g, clk, 42, PriorityTicket, "committer")
	r := g.baseline.Reserve
	// A JVM-sized commit leaves plenty of headroom: nothing happens.
	p.set(Signal{Avail: 6 << 30, AvailOK: true, CommitAvail: 7 << 30, CommitTotal: 44 << 30, CommitOK: true})
	g.tick()
	if len(p.killed) != 0 {
		t.Fatal("shed with commit headroom left")
	}
	p.set(Signal{Avail: 6 << 30, AvailOK: true, CommitAvail: r - 1, CommitTotal: 44 << 30, CommitOK: true})
	g.tick()
	if len(p.killed) != 1 || p.killed[0] != 42 {
		t.Fatalf("killed %v, want the largest committer 42", p.killed)
	}
	if ev := c.Shed(); ev == nil || ev.Reason != ReasonCommitLow || ev.Commit != 30<<30 || ev.Bystander {
		t.Fatalf("evidence = %+v", ev)
	}
}

// A machine policy's user reservation raises the trip floor above the survival
// reserve: memory the survival reserve alone would tolerate now sheds.
func TestUserReserveRaisesTripFloor(t *testing.T) {
	g, p, clk := testGuard(ModeAuto)
	attach(g, clk, 51, PriorityTicket, "hog")
	r := g.baseline.Reserve
	p.set(Signal{Avail: r + 1<<30, AvailOK: true, AvailHard: true})
	g.tick()
	if len(p.killed) != 0 {
		t.Fatal("shed above the survival reserve with no user reserve")
	}
	g.SetUserReserve(2 << 30)
	if st := g.Status(); st.UserReserve != 2<<30 || st.Reserve != r {
		t.Fatalf("status = reserve %d, user %d", st.Reserve, st.UserReserve)
	}
	g.tick()
	if len(p.killed) != 1 {
		t.Fatal("1 GiB above the survival reserve, under a 2 GiB user reserve: not shed")
	}
}
