package observe

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Registry defaults.
const (
	DefaultPollInterval = 2 * time.Second
	// DefaultGrace holds a newly found session back before its first report, so the
	// chat manager or a hook can mark it managed first (a managed chat's session file
	// appears before its session_ready reaches the daemon's bookkeeping).
	DefaultGrace = 3 * time.Second
	// DefaultLookback is how far back the first discovery after start looks: sessions
	// idle for longer are history, not activity.
	DefaultLookback = 24 * time.Hour
)

// Config sets up a Registry.
type Config struct {
	Sources []Source
	// Managed reports a session Syre runs itself (a chat's agent session id, a hook from
	// inside a managed task, a managed process). Such sessions are never reported.
	Managed func(Session) bool
	// Emit receives updates, in order per session.
	Emit func(Update)
	// OffsetsPath is where read offsets persist ("" keeps them in memory only).
	OffsetsPath string

	PollInterval time.Duration
	// Grace is how long a new session waits before its first report (0: DefaultGrace;
	// negative: none).
	Grace    time.Duration
	Lookback time.Duration
	Now      func() time.Time
	Logger   *slog.Logger
}

// Registry polls the sources and reports observed sessions incrementally.
type Registry struct {
	cfg     Config
	offsets *offsetStore

	mu       sync.Mutex
	sessions map[string]*tracked
	since    time.Time // discovery watermark
}

type tracked struct {
	s         Session
	firstSeen time.Time
	offset    int64
	readSize  int64 // the size and mtime when last read
	readMod   time.Time
	reported  bool
	managed   bool
}

// New returns a registry; offsets persisted by an earlier run are loaded.
func New(cfg Config) *Registry {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.Grace < 0 {
		cfg.Grace = 0
	} else if cfg.Grace == 0 {
		cfg.Grace = DefaultGrace
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = DefaultLookback
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Managed == nil {
		cfg.Managed = func(Session) bool { return false }
	}
	r := &Registry{cfg: cfg, sessions: map[string]*tracked{}, since: cfg.Now().Add(-cfg.Lookback)}
	r.offsets = loadOffsets(cfg.OffsetsPath, cfg.Logger)
	return r
}

// Run polls until ctx ends.
func (r *Registry) Run(ctx context.Context) {
	t := time.NewTicker(r.cfg.PollInterval)
	defer t.Stop()
	for {
		r.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// discoverySlack re-lists a little before the watermark: a transcript written in the
// same instant as the last discovery is not missed.
const discoverySlack = 2 * time.Second

// Poll runs one discovery and read pass.
func (r *Registry) Poll(ctx context.Context) {
	now := r.cfg.Now()
	r.mu.Lock()
	since := r.since
	r.mu.Unlock()
	for _, src := range r.cfg.Sources {
		found, err := src.Discover(ctx, since.Add(-discoverySlack))
		if err != nil {
			r.cfg.Logger.Debug("observe: discover failed", "agent", src.Agent(), "err", err)
			continue
		}
		for _, s := range found {
			r.track(s, now)
		}
	}
	r.mu.Lock()
	r.since = now
	list := make([]*tracked, 0, len(r.sessions))
	for _, t := range r.sessions {
		list = append(list, t)
	}
	r.mu.Unlock()

	for _, t := range list {
		r.step(ctx, t, now)
	}
	r.prune(now)
	r.offsets.flush()
}

// prune forgets sessions whose transcript hasn't changed within the lookback: they are
// history now. Their offsets stay, so one that resumes continues where it was.
func (r *Registry) prune(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, t := range r.sessions {
		if !t.s.ModTime.IsZero() && now.Sub(t.s.ModTime) > r.cfg.Lookback {
			delete(r.sessions, k)
		}
	}
}

// track records a discovered session (or refreshes what discovery knows about it).
func (r *Registry) track(s Session, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.sessions[s.Key()]; ok {
		t.s = s
		return
	}
	t := &tracked{s: s, firstSeen: now}
	if o, ok := r.offsets.get(s.Key()); ok && o.Path == s.Path {
		t.offset = o.Offset
		if s.Size < o.Offset {
			t.offset = 0 // rewritten or truncated: read it again from the start
		}
		// A session read before this run is not new: no grace, it was reported then.
		t.firstSeen = now.Add(-r.cfg.Grace)
	}
	r.sessions[s.Key()] = t
}

func (r *Registry) step(ctx context.Context, t *tracked, now time.Time) {
	s := t.s
	if t.managed {
		return
	}
	if r.cfg.Managed(s) {
		t.managed = true
		r.offsets.drop(s.Key())
		if t.reported {
			r.emit(Update{Kind: UpdateRetract, Session: s})
		}
		return
	}
	if now.Sub(t.firstSeen) < r.cfg.Grace {
		return // give the managed markers time to arrive
	}
	if t.reported && s.Size == t.readSize && s.ModTime.Equal(t.readMod) {
		return // unchanged since the last read
	}
	if s.Size < t.offset {
		t.offset = 0
	}
	evs, next, err := r.source(s.Agent).Read(ctx, s, t.offset)
	if err != nil {
		r.cfg.Logger.Debug("observe: read failed", "session", s.Key(), "err", err)
		return
	}
	// Only a read that reached the end counts as caught up; a capped read goes again.
	if next >= s.Size {
		t.readSize, t.readMod = s.Size, s.ModTime
	}
	if next != t.offset {
		t.offset = next
		r.offsets.set(s.Key(), offsetRec{Path: s.Path, Offset: next})
	}
	if len(evs) > 0 || !t.reported {
		t.reported = true
		r.emit(Update{Kind: UpdateEvents, Session: s, Events: evs})
	}
}

func (r *Registry) source(agent string) Source {
	for _, src := range r.cfg.Sources {
		if src.Agent() == agent {
			return src
		}
	}
	return nil
}

func (r *Registry) emit(u Update) {
	if r.cfg.Emit != nil {
		r.cfg.Emit(u)
	}
}

// Sessions lists the sessions reported so far and not retracted.
func (r *Registry) Sessions() []Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Session
	for _, t := range r.sessions {
		if t.reported && !t.managed {
			out = append(out, t.s)
		}
	}
	return out
}
