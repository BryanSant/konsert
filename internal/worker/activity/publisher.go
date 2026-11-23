package activity

import (
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// Publisher defaults.
const (
	// DefaultDebounce: a transient state (thinking, working, speaking) must hold this long
	// before it is published, so a tool that ends and the next one that starts 50 ms later
	// never show the "thinking" in between.
	DefaultDebounce = 300 * time.Millisecond
	// DefaultMinInterval is the shortest gap between two publishes of one session (at most
	// ~3 per second, whatever the engine streams).
	DefaultMinInterval = 300 * time.Millisecond
	// DefaultMaxWait bounds how long a debounced change can be held back by a stream of
	// further changes (a tool every 200 ms still shows up about once a second).
	DefaultMaxWait = time.Second
	// DefaultRateInterval is the shortest gap between publishes that only update the
	// output rate (tokens/s) while speaking.
	DefaultRateInterval = time.Second
)

// PublisherOptions configures a Publisher. Zero values take the defaults.
type PublisherOptions struct {
	Debounce     time.Duration
	MinInterval  time.Duration
	MaxWait      time.Duration
	RateInterval time.Duration
	// Now and AfterFunc are the clock and timer (tests replace them).
	Now       func() time.Time
	AfterFunc func(d time.Duration, f func()) Timer
}

// Timer is the part of *time.Timer the publisher uses.
type Timer interface{ Stop() bool }

// Publisher sends session activity to the hub at a bounded rate per session: changes to
// the state, tool or label are debounced (no flapping faster than Debounce) and spaced at
// least MinInterval apart; rate-only updates at most every RateInterval. The latest
// activity always wins: a held-back change is replaced, never queued, and is sent when its
// timer fires. Safe for concurrent use; send is called without the publisher's lock held.
type Publisher struct {
	opts PublisherOptions
	send func(protocol.SessionActivity)

	mu      sync.Mutex
	entries map[string]*pubEntry
}

type pubEntry struct {
	sent      protocol.SessionActivity // last published
	sentAt    time.Time
	hasSent   bool
	pending   *protocol.SessionActivity // latest unpublished activity
	firstAt   time.Time                 // when the oldest unpublished change arrived
	timer     Timer
	timerAt   time.Time
	forgotten bool
}

// NewPublisher returns a publisher that hands activities to send.
func NewPublisher(opts PublisherOptions, send func(protocol.SessionActivity)) *Publisher {
	if opts.Debounce <= 0 {
		opts.Debounce = DefaultDebounce
	}
	if opts.MinInterval <= 0 {
		opts.MinInterval = DefaultMinInterval
	}
	if opts.MaxWait <= 0 {
		opts.MaxWait = DefaultMaxWait
	}
	if opts.RateInterval <= 0 {
		opts.RateInterval = DefaultRateInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.AfterFunc == nil {
		opts.AfterFunc = func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
	}
	return &Publisher{opts: opts, send: send, entries: map[string]*pubEntry{}}
}

// transient states are debounced; the others (a human is needed, the turn ended, it
// can't run) are published as soon as the minimum interval allows.
func transient(state string) bool {
	switch state {
	case protocol.ActivityThinking, protocol.ActivityWorking, protocol.ActivitySpeaking:
		return true
	}
	return false
}

func sameView(a, b protocol.SessionActivity) bool {
	return a.State == b.State && a.Tool == b.Tool && a.Label == b.Label && a.Coarse == b.Coarse
}

// Update records the session's latest activity and publishes it now or later.
func (p *Publisher) Update(a protocol.SessionActivity) {
	if a.State == "" || (a.ChatID == "" && a.TaskID == "") {
		return
	}
	now := p.opts.Now()
	key := a.Key()
	p.mu.Lock()
	e := p.entries[key]
	if e == nil {
		e = &pubEntry{}
		p.entries[key] = e
	}
	if e.hasSent && sameView(a, e.sent) && a.TokensPerSec == e.sent.TokensPerSec {
		// Back to (or still at) what the hub shows: nothing to publish.
		e.pending = nil
		if e.timer != nil {
			e.timer.Stop()
			e.timer = nil
		}
		p.mu.Unlock()
		return
	}
	if e.pending == nil {
		e.firstAt = now
	}
	if e.pending != nil && sameView(a, *e.pending) && e.timer != nil {
		// The same state again (a speaking rate update): it is already planned; the
		// debounce measures how long a state holds, not how long the stream is quiet.
		cp := a
		e.pending = &cp
		p.mu.Unlock()
		return
	}
	cp := a
	e.pending = &cp

	var at time.Time
	switch {
	case !e.hasSent || (!sameView(a, e.sent) && !transient(a.State)):
		at = e.sentAt.Add(p.opts.MinInterval) // zero sentAt: now
	case !sameView(a, e.sent):
		at = now.Add(p.opts.Debounce)
		if lim := e.firstAt.Add(p.opts.MaxWait); at.After(lim) {
			at = lim
		}
		if min := e.sentAt.Add(p.opts.MinInterval); at.Before(min) {
			at = min
		}
	default: // only the rate changed
		at = e.sentAt.Add(p.opts.RateInterval)
		if e.timer != nil && e.timerAt.Before(at) {
			at = e.timerAt // a sooner publish is already planned
		}
	}
	if !at.After(now) {
		out := p.takeLocked(e, now)
		p.mu.Unlock()
		p.send(out)
		return
	}
	p.scheduleLocked(key, e, at, now)
	p.mu.Unlock()
}

// takeLocked marks the pending activity published and returns it.
func (p *Publisher) takeLocked(e *pubEntry, now time.Time) protocol.SessionActivity {
	out := *e.pending
	e.pending = nil
	e.sent, e.sentAt, e.hasSent = out, now, true
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	return out
}

func (p *Publisher) scheduleLocked(key string, e *pubEntry, at, now time.Time) {
	if e.timer != nil {
		if e.timerAt.Equal(at) {
			return
		}
		e.timer.Stop()
	}
	e.timerAt = at
	e.timer = p.opts.AfterFunc(at.Sub(now), func() { p.fire(key, e) })
}

func (p *Publisher) fire(key string, e *pubEntry) {
	p.mu.Lock()
	if e.forgotten || p.entries[key] != e || e.pending == nil {
		if e.timer != nil {
			e.timer = nil
		}
		p.mu.Unlock()
		return
	}
	out := p.takeLocked(e, p.opts.Now())
	p.mu.Unlock()
	p.send(out)
}

// Flush publishes key's held-back activity now (e.g. the final state of a task that
// ended, before its process is reaped).
func (p *Publisher) Flush(key string) {
	p.mu.Lock()
	e := p.entries[key]
	if e == nil || e.pending == nil {
		p.mu.Unlock()
		return
	}
	out := p.takeLocked(e, p.opts.Now())
	p.mu.Unlock()
	p.send(out)
}

// Forget drops a session (its chat was discarded): nothing more is sent for it.
func (p *Publisher) Forget(key string) {
	p.mu.Lock()
	if e := p.entries[key]; e != nil {
		e.forgotten = true
		if e.timer != nil {
			e.timer.Stop()
		}
		delete(p.entries, key)
	}
	p.mu.Unlock()
}

// Snapshot returns the latest activity of every session (published or held back), to
// re-send after a reconnect.
func (p *Publisher) Snapshot() []protocol.SessionActivity {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]protocol.SessionActivity, 0, len(p.entries))
	for _, e := range p.entries {
		switch {
		case e.pending != nil:
			out = append(out, *e.pending)
		case e.hasSent:
			out = append(out, e.sent)
		}
	}
	return out
}

// Prune drops sessions whose last activity is terminal and older than age (bounds memory
// on long-running daemons).
func (p *Publisher) Prune(age time.Duration) {
	cut := p.opts.Now().Add(-age)
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, e := range p.entries {
		if e.pending == nil && e.hasSent && protocol.ActivityTerminal(e.sent.State) && e.sentAt.Before(cut) {
			delete(p.entries, k)
		}
	}
}
