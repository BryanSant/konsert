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
)

// Shed reasons recorded in Evidence.Reason.
const (
	ReasonBelowReserve = "host_memory_below_reserve"
	ReasonCritical     = "kernel_memory_pressure_critical"
	ReasonPoolStall    = "worker_pool_memory_stall"
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
	Level   PressureLevel
	// PoolStall is the worker pool's PSI "full" avg10 (Linux scope mode); < 0 unknown.
	PoolStall float64
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
	At             time.Time
}

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
}

// Usage is what a released session reports.
type Usage struct {
	PeakRSS uint64
	Shed    *Evidence
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
	running    bool
	wake       chan struct{}
	notes      []string

	// watchdog state (guarded by mu)
	lowSince    time.Time
	stallSince  time.Time
	settleUntil time.Time
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
// (cgroup memory.peak, Job Object peak); 0 when unknown.
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
	s.g.plat.release(s)
	g := s.g
	g.mu.Lock()
	g.sessions = slices.DeleteFunc(g.sessions, func(x *Session) bool { return x == s })
	if s.spec.Owner != "" && peak > 0 {
		if len(g.lastPeak) >= lastPeakCap {
			clear(g.lastPeak)
		}
		g.lastPeak[s.spec.Owner] = peak
	}
	g.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = Usage{PeakRSS: peak}
	return Usage{PeakRSS: peak, Shed: s.shedCopyLocked()}
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
			g.lowSince, g.stallSince = time.Time{}, time.Time{}
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
	victim := pickVictim(sessions)
	if victim == nil {
		return
	}
	g.shed(victim, reason, sig, reserve, len(sessions))
	g.mu.Lock()
	g.settleUntil = now.Add(shedSettle)
	g.lowSince, g.stallSince = time.Time{}, time.Time{}
	g.mu.Unlock()
}

// tripLocked applies the debounce rules and returns a shed reason, or "".
func (g *Guard) tripLocked(sig Signal, reserve uint64, now time.Time) string {
	low := sig.AvailOK && reserve > 0 && sig.Avail < reserve
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
	if now.Before(g.settleUntil) {
		return ""
	}
	switch {
	case sig.Level == LevelCritical:
		return ReasonCritical
	case low && now.Sub(g.lowSince) >= lowDebounce:
		return ReasonBelowReserve
	case stall && now.Sub(g.stallSince) >= stallDebounce:
		return ReasonPoolStall
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

// pickVictim chooses the lowest-priority, most recently started session that has not
// been shed already.
func pickVictim(sessions []*Session) *Session {
	var best *Session
	for _, s := range sessions {
		s.mu.Lock()
		gone := s.shed != nil || s.released
		s.mu.Unlock()
		if gone {
			continue
		}
		if best == nil || s.spec.Priority < best.spec.Priority ||
			(s.spec.Priority == best.spec.Priority && s.started.After(best.started)) {
			best = s
		}
	}
	return best
}

// shed freezes the victim, records evidence, tells subscribers, then kills it.
func (g *Guard) shed(s *Session, reason string, sig Signal, reserve uint64, active int) {
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
		"active_sessions", active)
}
