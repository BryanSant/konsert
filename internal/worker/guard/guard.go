package guard

import (
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"
)

// Priority orders sessions for shedding: the lowest goes first, then the most
// recently started.
type Priority int

const (
	// PriorityTicket is autonomous ticket work: requeued elsewhere when shed.
	PriorityTicket Priority = iota
	// PriorityTask is a dispatched agent task.
	PriorityTask
	// PriorityInteractive is work a person is driving (a user chat). It is shed
	// only when nothing else is left.
	PriorityInteractive
)

// Watchdog timing (docs/design/guardrails.md).
const (
	pollInterval   = 250 * time.Millisecond
	lowDebounce    = 500 * time.Millisecond
	stallDebounce  = 5 * time.Second
	stallThreshold = 20.0 // % of time all worker tasks stalled on memory (PSI full avg10)
	shedSettle     = 2 * time.Second
	// hardLookahead: under a hard limit the watchdog sheds when, at the current rate of
	// decline, available memory would cross the reserve before the next poll: by then a
	// fast allocator may already have met the OOM killer.
	hardLookahead = pollInterval
)

// Shed reasons recorded in Evidence.Reason.
const (
	ReasonBelowReserve = "host_memory_below_reserve"
	ReasonCritical     = "kernel_memory_pressure_critical"
	ReasonPoolStall    = "worker_pool_memory_stall"
	ReasonSwapGrowth   = "swap_growth"
)

// swapBudget is how much swap agent work may add once it starts: min(25% RAM, 2 GiB),
// the same cap the Linux slice's MemorySwapMax sets. Past it the host is paging work
// out to disk (macOS keeps "available" memory looking healthy while it does).
func swapBudget(total uint64) uint64 { return min(total/4, 2<<30) }

// Swap growth alone doesn't say whose memory it is: macOS rarely gives swap back, so a
// person's own apps can add gigabytes over a long session. It counts only while the host
// is also short: kernel pressure WARN or worse, or available memory within
// swapNearReserve × the reserve. After swapRebase of calm (normal pressure, available
// memory healthy) the baseline moves up to the swap in use then, so growth from before
// the calm is not blamed on later work.
const (
	swapNearReserve = 2
	swapRebase      = 2 * time.Minute
)

// PressureLevel is the kernel's own pressure verdict where the OS has one (macOS).
type PressureLevel int

const (
	LevelUnknown PressureLevel = iota
	LevelNormal
	LevelWarn
	LevelCritical
)

// Signal is one watchdog sample.
type Signal struct {
	// Avail is available physical memory in bytes; AvailOK says it was read.
	Avail   uint64
	AvailOK bool
	// AvailHard: Avail is headroom under a hard limit (a container's memory.max), where
	// running out means an OOM kill at once, with no reclaim or swap to slow it.
	AvailHard bool
	Level     PressureLevel
	// PoolStall is the worker pool's PSI "full" avg10 (Linux scope mode); < 0 unknown.
	PoolStall float64
	// SwapUsed is swap in use; SwapOK says the platform watches it (macOS, and Linux
	// without scopes, where no MemorySwapMax caps the workers).
	SwapUsed uint64
	SwapOK   bool
}

// Evidence describes one shed, for the session's owner, the digest and logs.
type Evidence struct {
	PID            int
	Owner          string
	Kind           string
	Reason         string
	RSS            uint64 // the shed session's resident memory at freeze time (0 unknown)
	HostAvail      uint64 // host available memory at the trip
	Reserve        uint64
	ActiveSessions int // managed sessions running when the shed happened, the victim included
	// HostUsed is the host's memory in use at the trip (total − available; 0 unknown).
	HostUsed uint64
	// Bystander marks a victim that held little of the memory in use (under
	// 1/culpritShare of it): the machine was full for other reasons, so the shed
	// says nothing about how much memory this work needs.
	Bystander bool
	At        time.Time
}

// culpritShare: a shed victim holding at least 1/culpritShare of the host's used
// memory is treated as the cause of the pressure; below that it is a bystander.
const culpritShare = 4

// LaunchSpec describes a session about to start.
type LaunchSpec struct {
	Binary string
	Args   []string
	// Owner identifies the session to its owner's shed subscriber (task id,
	// "verify:<chat>", ...). Engines that don't know their chat leave it empty and
	// match on PID instead.
	Owner    string
	Kind     string // "task", "chat", "verify": for logs and evidence
	Priority Priority
	// Headless marks unattended work (ticket runs). Windows adds UI restrictions.
	Headless bool
}

// Launch is a prepared start: run Binary/Args (possibly wrapped, e.g. in a systemd
// scope), then call Attach with the child's PID.
type Launch struct {
	Binary string
	Args   []string

	g    *Guard
	spec LaunchSpec
	unit string // Linux transient scope unit name
}

// Session is one managed process tree.
type Session struct {
	g        *Guard
	pid      int
	spec     LaunchSpec
	started  time.Time
	unit     string
	plat     any // platform state (cgroup path, job handle, ...)
	mu       sync.Mutex
	shed     *Evidence
	released bool
	usage    Usage
	bg       bool
	oomBase  uint64 // kernel OOM-kill count at attach
	oomOK    bool
}

// Usage is what a released session reports.
type Usage struct {
	PeakRSS uint64
	Shed    *Evidence
	// OOMKilled: the root died of SIGKILL while the kernel's OOM killer fired (a
	// container's or the host's), and the guard did not shed it.
	OOMKilled bool
}

type throttleState struct {
	on     bool
	cpuPct int // hard CPU ceiling; 0 none
}

// Status is the guard's view for facts and doctor.
type Status struct {
	Mode         Mode
	ModeSource   string // auto, config or hub
	BaseMode     Mode   // the mode without hub directives (config or auto)
	BaseSource   string
	Reserve      uint64
	TotalRAM     uint64
	Desktop      bool
	Mechanism    string
	Throttled    bool
	Sessions     int
	ProtectNotes []string
}

// Guard owns the managed sessions on this machine and the watchdog.
type Guard struct {
	plat   platform
	logger *slog.Logger
	now    func() time.Time

	mu         sync.Mutex
	baseline   Baseline
	configMode Mode
	hubMode    Mode
	throttle   throttleState
	sessions   []*Session
	lastPeak   map[string]uint64 // owner -> peak of its most recent released session
	lastOOM    map[string]bool   // owner -> its most recent released session was OOM-killed
	running    bool
	wake       chan struct{}
	notes      []string

	// watchdog state (guarded by mu)
	lowSince    time.Time
	stallSince  time.Time
	swapSince   time.Time
	settleUntil time.Time
	swapBase    uint64 // swap in use when the current busy period (or calm) began
	swapBaseOK  bool
	calmSince   time.Time // when the host last turned calm (no pressure, memory healthy)
	prevAvail   uint64    // the last sample's available memory, for the hard-limit trend
	prevAt      time.Time
}

// Config sets up a Guard.
type Config struct {
	Baseline Baseline
	// Mode is the operator's configured mode (syred config / --guardrails); auto or
	// empty resolves from the baseline.
	Mode   Mode
	Logger *slog.Logger
}

// New returns a Guard on the host's native platform and applies control-plane
// self-protection.
func New(cfg Config) *Guard {
	g := newGuard(nativePlatform(), cfg)
	g.notes = g.plat.selfProtect()
	g.reconfigure()
	return g
}

func newGuard(p platform, cfg Config) *Guard {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	mode := cfg.Mode
	if mode == "" {
		mode = ModeAuto
	}
	return &Guard{
		plat:       p,
		logger:     logger,
		now:        time.Now,
		baseline:   cfg.Baseline,
		configMode: mode,
		lastPeak:   map[string]uint64{},
		lastOOM:    map[string]bool{},
		wake:       make(chan struct{}, 1),
	}
}

var (
	defaultMu    sync.Mutex
	defaultGuard *Guard
)

// Default is the process-wide guard. Before syred installs one (SetDefault) it is a
// passthrough guard: sessions are tracked and cleaned up (Job Objects on Windows),
// nothing is watched or limited. Tests get exactly that.
func Default() *Guard {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultGuard == nil {
		defaultGuard = newGuard(nativePlatform(), Config{Mode: ModePassthrough})
	}
	return defaultGuard
}

// SetDefault installs g as the process-wide guard.
func SetDefault(g *Guard) {
	defaultMu.Lock()
	defaultGuard = g
	defaultMu.Unlock()
}

// effectiveLocked resolves hub directive > config > auto.
func (g *Guard) effectiveLocked() (Mode, string) {
	if g.hubMode != "" && g.hubMode != ModeAuto {
		return g.hubMode, "hub"
	}
	m, src := g.baseLocked()
	return m, src
}

func (g *Guard) baseLocked() (Mode, string) {
	if g.configMode != "" && g.configMode != ModeAuto {
		return g.configMode, "config"
	}
	if g.baseline.AutoMode != "" {
		return g.baseline.AutoMode, "auto"
	}
	return ModePassthrough, "auto"
}

// Mode is the effective guardrail mode.
func (g *Guard) Mode() Mode {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, _ := g.effectiveLocked()
	return m
}

// SetBaseline replaces the host baseline (fresh machine facts).
func (g *Guard) SetBaseline(b Baseline) {
	g.mu.Lock()
	changed := g.baseline != b
	g.baseline = b
	g.mu.Unlock()
	if changed {
		g.reconfigure()
	}
}

// Describe resolves the status for cfg without installing anything or touching the
// OS beyond probing the mechanism (syred doctor).
func Describe(cfg Config) Status {
	return newGuard(nativePlatform(), cfg).Status()
}

// SetHubMode applies the mode from the newest active machine.guardrails directive
// ("" or auto clears it).
func (g *Guard) SetHubMode(m Mode) {
	g.mu.Lock()
	changed := g.hubMode != m
	g.hubMode = m
	g.mu.Unlock()
	if changed {
		g.reconfigure()
	}
}

// SetThrottle applies an active machine.throttle: lower CPU weight / background
// priority for workers, and a hard CPU ceiling when cpuPct is 1-99.
func (g *Guard) SetThrottle(on bool, cpuPct int) {
	if cpuPct < 0 || cpuPct >= 100 {
		cpuPct = 0
	}
	t := throttleState{on: on || cpuPct > 0, cpuPct: cpuPct}
	g.mu.Lock()
	changed := g.throttle != t
	g.throttle = t
	sessions := slices.Clone(g.sessions)
	g.mu.Unlock()
	if !changed {
		return
	}
	g.reconfigure()
	for _, s := range sessions {
		g.plat.throttle(s, t)
	}
	g.kick()
}

func (g *Guard) reconfigure() {
	g.mu.Lock()
	mode, _ := g.effectiveLocked()
	b := g.baseline
	t := g.throttle
	g.mu.Unlock()
	g.plat.configure(mode, b, t)
}

// Status reports the guard's configuration.
func (g *Guard) Status() Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, src := g.effectiveLocked()
	bm, bsrc := g.baseLocked()
	return Status{
		Mode:         m,
		ModeSource:   src,
		BaseMode:     bm,
		BaseSource:   bsrc,
		Reserve:      g.baseline.Reserve,
		TotalRAM:     g.baseline.TotalRAM,
		Desktop:      g.baseline.Desktop,
		Mechanism:    g.plat.mechanism(m),
		Throttled:    g.throttle.on,
		Sessions:     len(g.sessions),
		ProtectNotes: slices.Clone(g.notes),
	}
}

var (
	subMu   sync.Mutex
	subs    = map[int]func(Evidence){}
	nextSub int
)

// OnShed registers fn to run (synchronously, before the kill) for every shed by any
// guard in this process, so owners can subscribe before syred installs its guard.
// It returns a function that unregisters it.
func OnShed(fn func(Evidence)) func() {
	subMu.Lock()
	id := nextSub
	nextSub++
	subs[id] = fn
	subMu.Unlock()
	return func() {
		subMu.Lock()
		delete(subs, id)
		subMu.Unlock()
	}
}

// Announce tells the shed subscribers about a shed. The guard calls it before each
// kill; tests call it to simulate one.
func Announce(ev Evidence) {
	subMu.Lock()
	fns := make([]func(Evidence), 0, len(subs))
	for _, fn := range subs {
		fns = append(fns, fn)
	}
	subMu.Unlock()
	for _, fn := range fns {
		fn(ev)
	}
}

// OwnerPeak is the peak memory of owner's live sessions, or of its most recently
// released one; 0 when unknown.
func (g *Guard) OwnerPeak(owner string) uint64 {
	if g == nil || owner == "" {
		return 0
	}
	g.mu.Lock()
	var live []*Session
	for _, s := range g.sessions {
		if s.spec.Owner == owner {
			live = append(live, s)
		}
	}
	peak := g.lastPeak[owner]
	g.mu.Unlock()
	for _, s := range live {
		peak = max(peak, g.plat.peak(s))
	}
	return peak
}

const lastPeakCap = 512

// Prepare readies a launch. Callers start l.Binary with l.Args and then call Attach.
func (g *Guard) Prepare(spec LaunchSpec) *Launch {
	l := &Launch{Binary: spec.Binary, Args: spec.Args, g: g, spec: spec}
	if g == nil {
		return l
	}
	g.mu.Lock()
	mode, _ := g.effectiveLocked()
	g.mu.Unlock()
	g.plat.wrap(l, mode)
	return l
}

// Attach registers the started process (a process-group leader on Unix).
func (l *Launch) Attach(pid int) *Session {
	if l == nil || l.g == nil || pid <= 0 {
		return nil
	}
	g := l.g
	s := &Session{g: g, pid: pid, spec: l.spec, started: g.now(), unit: l.unit}
	s.oomBase, s.oomOK = g.plat.oomKills()
	g.mu.Lock()
	mode, _ := g.effectiveLocked()
	t := g.throttle
	g.mu.Unlock()
	g.plat.attach(s, mode)
	if t.on {
		g.plat.throttle(s, t)
	}
	g.mu.Lock()
	g.sessions = append(g.sessions, s)
	g.mu.Unlock()
	g.ensureWatchdog()
	return s
}

// PID is the session's root process.
func (s *Session) PID() int {
	if s == nil {
		return 0
	}
	return s.pid
}

// Shed returns the evidence when the guard shed this session, nil otherwise.
func (s *Session) Shed() *Evidence {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shed == nil {
		return nil
	}
	ev := *s.shed
	return &ev
}

// PeakRSS is the session's peak memory so far, where the platform tracks it
// (cgroup memory.peak, sampled job working sets); 0 when unknown.
func (s *Session) PeakRSS() uint64 {
	if s == nil {
		return 0
	}
	return s.g.plat.peak(s)
}

// Kill stops the whole tree (cgroup.kill, TerminateJobObject or the process group).
// It reports false when the platform has no tree-wide kill for this session and the
// caller should fall back to its own.
func (s *Session) Kill() bool {
	if s == nil {
		return false
	}
	return s.g.plat.kill(s) == nil
}

// Release unregisters the session once its root has exited. ps, when known, supplies
// the root's rusage for a peak where the platform tracks none. Later calls return the
// same usage.
func (s *Session) Release(ps *os.ProcessState) Usage {
	if s == nil {
		return Usage{}
	}
	s.mu.Lock()
	if s.released {
		u := s.usage
		u.Shed = s.shedCopyLocked()
		s.mu.Unlock()
		return u
	}
	s.released = true
	s.mu.Unlock()
	peak := s.g.plat.peak(s)
	if peak == 0 {
		peak = rusagePeak(ps)
	}
	oom := false
	if s.oomOK && killedBySIGKILL(ps) && s.Shed() == nil {
		n, ok := s.g.plat.oomKills()
		oom = ok && n > s.oomBase
	}
	s.g.plat.release(s)
	g := s.g
	g.mu.Lock()
	g.sessions = slices.DeleteFunc(g.sessions, func(x *Session) bool { return x == s })
	if s.spec.Owner != "" {
		if len(g.lastPeak) >= lastPeakCap {
			clear(g.lastPeak)
		}
		if len(g.lastOOM) >= lastPeakCap {
			clear(g.lastOOM)
		}
		if peak > 0 {
			g.lastPeak[s.spec.Owner] = peak
		}
		if oom {
			g.lastOOM[s.spec.Owner] = true
		} else {
			delete(g.lastOOM, s.spec.Owner)
		}
	}
	g.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = Usage{PeakRSS: peak, OOMKilled: oom}
	return Usage{PeakRSS: peak, Shed: s.shedCopyLocked(), OOMKilled: oom}
}

// OwnerOOMKilled reports whether owner's most recently released session was killed by
// the kernel's OOM killer (Usage.OOMKilled).
func (g *Guard) OwnerOOMKilled(owner string) bool {
	if g == nil || owner == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastOOM[owner]
}

func (s *Session) shedCopyLocked() *Evidence {
	if s.shed == nil {
		return nil
	}
	ev := *s.shed
	return &ev
}

func (g *Guard) kick() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

func (g *Guard) ensureWatchdog() {
	g.mu.Lock()
	if g.running {
		g.mu.Unlock()
		return
	}
	g.running = true
	g.mu.Unlock()
	go g.loop()
}

// loop polls while managed sessions exist and the mode watches, and exits when idle
// so an idle host pays nothing.
func (g *Guard) loop() {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		g.mu.Lock()
		mode, _ := g.effectiveLocked()
		idle := len(g.sessions) == 0 || mode == ModePassthrough
		if idle {
			g.running = false
			g.lowSince, g.stallSince, g.swapSince, g.calmSince, g.prevAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}
			g.swapBaseOK = false
			g.mu.Unlock()
			return
		}
		g.mu.Unlock()
		g.tick()
		select {
		case <-t.C:
		case <-g.wake:
		}
	}
}

// tick runs one watchdog evaluation.
func (g *Guard) tick() {
	sig := g.plat.sample()
	now := g.now()

	g.mu.Lock()
	reserve := g.baseline.Reserve
	total := g.baseline.TotalRAM
	throttled := g.throttle.on
	sessions := slices.Clone(g.sessions)
	reason := g.tripLocked(sig, reserve, now)
	wantBG := throttled || sig.Level >= LevelWarn
	g.mu.Unlock()

	g.applyBackground(sessions, wantBG)
	for _, s := range sessions {
		g.plat.peak(s) // keep a running peak: a collected scope loses memory.peak at exit
	}
	if reason == "" {
		return
	}
	victim := pickVictim(sessions, g.plat.rss)
	if victim == nil {
		return
	}
	g.shed(victim, reason, sig, reserve, total, len(sessions))
	g.mu.Lock()
	g.settleUntil = now.Add(shedSettle)
	g.lowSince, g.stallSince, g.swapSince = time.Time{}, time.Time{}, time.Time{}
	if sig.SwapOK {
		g.swapBase = max(g.swapBase, sig.SwapUsed) // the victim's pages leave swap slowly
	}
	g.mu.Unlock()
}

// tripLocked applies the debounce rules and returns a shed reason, or "".
func (g *Guard) tripLocked(sig Signal, reserve uint64, now time.Time) string {
	low := sig.AvailOK && reserve > 0 && sig.Avail < reserve
	hardLow := false
	if sig.AvailOK && sig.AvailHard && reserve > 0 {
		// No cushion under a hard limit: below the reserve sheds at once, and so does a
		// decline that would get there before the next samples.
		hardLow = low
		if !g.prevAt.IsZero() && sig.Avail < g.prevAvail {
			if dt := now.Sub(g.prevAt); dt > 0 && dt <= 4*pollInterval {
				drop := float64(g.prevAvail-sig.Avail) * float64(hardLookahead) / float64(dt)
				hardLow = hardLow || float64(sig.Avail) < float64(reserve)+drop
			}
		}
	}
	if sig.AvailOK {
		g.prevAvail, g.prevAt = sig.Avail, now
	}
	if low {
		if g.lowSince.IsZero() {
			g.lowSince = now
		}
	} else {
		g.lowSince = time.Time{}
	}
	stall := sig.PoolStall >= stallThreshold
	if stall {
		if g.stallSince.IsZero() {
			g.stallSince = now
		}
	} else {
		g.stallSince = time.Time{}
	}
	swapping := false
	if sig.SwapOK {
		if !g.swapBaseOK {
			g.swapBase, g.swapBaseOK = sig.SwapUsed, true
		}
		if sig.SwapUsed < g.swapBase {
			g.swapBase = sig.SwapUsed
		}
		short := sig.Level >= LevelWarn || (sig.AvailOK && reserve > 0 && sig.Avail < swapNearReserve*reserve)
		if short {
			g.calmSince = time.Time{}
		} else {
			if g.calmSince.IsZero() {
				g.calmSince = now
			}
			if now.Sub(g.calmSince) >= swapRebase {
				g.swapBase, g.calmSince = sig.SwapUsed, now
			}
		}
		swapping = short && sig.SwapUsed-g.swapBase > swapBudget(g.baseline.TotalRAM)
	}
	if swapping {
		if g.swapSince.IsZero() {
			g.swapSince = now
		}
	} else {
		g.swapSince = time.Time{}
	}
	if now.Before(g.settleUntil) {
		return ""
	}
	switch {
	case sig.Level == LevelCritical:
		return ReasonCritical
	case hardLow, low && now.Sub(g.lowSince) >= lowDebounce:
		return ReasonBelowReserve
	case stall && now.Sub(g.stallSince) >= stallDebounce:
		return ReasonPoolStall
	case swapping && now.Sub(g.swapSince) >= lowDebounce:
		return ReasonSwapGrowth
	}
	return ""
}

// applyBackground moves sessions to background priority while on (re-applied every
// tick so children started since follow) and restores them once when it turns off.
func (g *Guard) applyBackground(sessions []*Session, on bool) {
	for _, s := range sessions {
		s.mu.Lock()
		was := s.bg
		s.bg = on
		s.mu.Unlock()
		if on || was {
			g.plat.background(s, on)
		}
	}
}

// pickVictim chooses, among sessions not shed already, the lowest priority present
// and within it the session using the most memory (the most recently started on a
// tie), so a shed removes the pressure instead of an innocent small run.
func pickVictim(sessions []*Session, rss func(*Session) uint64) *Session {
	var cands []*Session
	for _, s := range sessions {
		s.mu.Lock()
		gone := s.shed != nil || s.released
		s.mu.Unlock()
		if gone {
			continue
		}
		if len(cands) > 0 && s.spec.Priority > cands[0].spec.Priority {
			continue
		}
		if len(cands) > 0 && s.spec.Priority < cands[0].spec.Priority {
			cands = cands[:0]
		}
		cands = append(cands, s)
	}
	if len(cands) <= 1 {
		if len(cands) == 0 {
			return nil
		}
		return cands[0]
	}
	var best *Session
	var bestRSS uint64
	for _, s := range cands {
		r := rss(s)
		if best == nil || r > bestRSS || (r == bestRSS && s.started.After(best.started)) {
			best, bestRSS = s, r
		}
	}
	return best
}

// shed freezes the victim, records evidence, tells subscribers, then kills it.
func (g *Guard) shed(s *Session, reason string, sig Signal, reserve, total uint64, active int) {
	if err := g.plat.freeze(s); err != nil {
		g.logger.Debug("guardrails: freeze before shed failed", "pid", s.pid, "err", err)
	}
	ev := Evidence{
		PID:            s.pid,
		Owner:          s.spec.Owner,
		Kind:           s.spec.Kind,
		Reason:         reason,
		RSS:            g.plat.rss(s),
		HostAvail:      sig.Avail,
		Reserve:        reserve,
		ActiveSessions: active,
		At:             g.now(),
	}
	if sig.AvailOK && sig.Avail < total {
		ev.HostUsed = total - sig.Avail
	}
	ev.Bystander = ev.RSS > 0 && ev.HostUsed > 0 && ev.RSS*culpritShare < ev.HostUsed
	s.mu.Lock()
	s.shed = &ev
	s.mu.Unlock()

	Announce(ev)
	if err := g.plat.kill(s); err != nil {
		g.logger.Warn("guardrails: kill of shed session failed", "pid", s.pid, "err", err)
	}
	g.logger.Warn("guardrails: shed session under memory pressure",
		"pid", s.pid, "owner", s.spec.Owner, "kind", s.spec.Kind, "reason", reason,
		"rss_mb", ev.RSS/mib, "host_avail_mb", ev.HostAvail/mib, "reserve_mb", reserve/mib,
		"active_sessions", active, "bystander", ev.Bystander)
}
