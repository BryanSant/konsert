package guard

// platform is the per-OS half of the guard. Every method must be safe to call on a
// session whose process has already exited.
type platform interface {
	// mechanism names how sessions are contained in mode m, for facts and doctor
	// ("systemd-scope", "process-group", "job-object", ...).
	mechanism(m Mode) string
	// configure applies machine-wide settings for a mode (Linux slice properties).
	configure(m Mode, b Baseline, t throttleState)
	// wrap may rewrite l.Binary/l.Args before start (Linux: run inside a scope).
	wrap(l *Launch, m Mode)
	// attach runs right after start: Job Object, priority, cgroup discovery.
	attach(s *Session, m Mode)
	// release frees platform state after the root exited.
	release(s *Session)
	// sample reads the watchdog signal.
	sample() Signal
	// freeze stops the whole tree from running (and allocating).
	freeze(s *Session) error
	// kill stops the whole tree for good.
	kill(s *Session) error
	// rss is the tree's resident memory now (0 unknown).
	rss(s *Session) uint64
	// peak is the tree's peak memory so far (0 unknown).
	peak(s *Session) uint64
	// background moves the tree to (or back from) background priority.
	background(s *Session, on bool)
	// throttle applies machine.throttle to a running session.
	throttle(s *Session, t throttleState)
	// selfProtect raises syred's own standing where the OS allows it unprivileged and
	// returns notes for doctor (applied and refused steps).
	selfProtect() []string
}
