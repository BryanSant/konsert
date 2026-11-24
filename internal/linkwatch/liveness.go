// Package linkwatch keeps the hub <-> konsertd link honest (KON-G8WA3).
//
// Liveness: each side records when it last sent and last received anything on a link.
// When a side has sent nothing for QuietAfter it sends a keepalive (the hub a WebSocket
// ping, konsertd a small vitals heartbeat); regular traffic suppresses them. When a side
// has received nothing for DeadAfter the link is dead: konsertd redials, the hub drops
// the connection and marks the machine offline. Both sides keep the link busy at least
// every QuietAfter, so a live link never goes DeadAfter without a frame either way.
//
// Watcher: a tick that arrives far past its interval (the machine slept) or a change in
// the machine's network addresses means the link is probably gone; the dialer drops it
// and redials at once instead of waiting DeadAfter.
package linkwatch

import (
	"context"
	"sync"
	"time"
)

// Default liveness intervals.
const (
	DefaultQuietAfter = 15 * time.Second
	DefaultDeadAfter  = 45 * time.Second
)

// Config sets a link's liveness intervals; zero values take the defaults.
type Config struct {
	// QuietAfter is how long a side may send nothing before it sends a keepalive.
	QuietAfter time.Duration
	// DeadAfter is how long a side may receive nothing before the link counts as dead.
	DeadAfter time.Duration
}

func (c Config) withDefaults() Config {
	if c.QuietAfter <= 0 {
		c.QuietAfter = DefaultQuietAfter
	}
	if c.DeadAfter <= 0 {
		c.DeadAfter = DefaultDeadAfter
	}
	return c
}

// CheckEvery is how often Monitor checks the link: a fifth of QuietAfter, so a dead link
// is noticed within DeadAfter plus that.
func (c Config) CheckEvery() time.Duration {
	c = c.withDefaults()
	return max(c.QuietAfter/5, time.Millisecond)
}

// Action is what a liveness check asks the link owner to do.
type Action int

const (
	// None: the link is fine.
	None Action = iota
	// Keepalive: this side has been quiet for QuietAfter; send something.
	Keepalive
	// Dead: nothing was received for DeadAfter; drop the link.
	Dead
)

// Liveness tracks one link's last send and receive times. Safe for concurrent use.
type Liveness struct {
	cfg Config
	now func() time.Time

	mu       sync.Mutex
	lastSend time.Time
	lastRecv time.Time
}

// New starts tracking a link that was just established. now is the clock (time.Now when
// nil); tests pass a fake.
func New(cfg Config, now func() time.Time) *Liveness {
	if now == nil {
		now = time.Now
	}
	t := now()
	return &Liveness{cfg: cfg.withDefaults(), now: now, lastSend: t, lastRecv: t}
}

// Config returns the effective intervals.
func (l *Liveness) Config() Config { return l.cfg }

// Sent records that this side wrote a frame.
func (l *Liveness) Sent() {
	t := l.now()
	l.mu.Lock()
	l.lastSend = t
	l.mu.Unlock()
}

// Received records that a frame (data, ping or pong) arrived from the peer.
func (l *Liveness) Received() {
	t := l.now()
	l.mu.Lock()
	l.lastRecv = t
	l.mu.Unlock()
}

// LastReceived is when the peer was last heard from.
func (l *Liveness) LastReceived() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastRecv
}

// Check decides what to do now. A Keepalive answer counts as a send, so a keepalive that
// takes a while to reach the wire is not asked for again on the next check.
func (l *Liveness) Check() Action { return l.CheckAt(l.now()) }

// CheckAt is Check at time t.
func (l *Liveness) CheckAt(t time.Time) Action {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.Sub(l.lastRecv) >= l.cfg.DeadAfter {
		return Dead
	}
	if t.Sub(l.lastSend) >= l.cfg.QuietAfter {
		l.lastSend = t
		return Keepalive
	}
	return None
}

// Run checks the link on every tick (at the tick's time) until ctx ends or the link is
// dead: keepalive is called when this side has been quiet (it must not block for long),
// dead once when the link died, after which Run returns.
func (l *Liveness) Run(ctx context.Context, ticks <-chan time.Time, keepalive, dead func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticks:
			switch l.CheckAt(t) {
			case Keepalive:
				keepalive()
			case Dead:
				dead()
				return
			}
		}
	}
}

// Monitor runs l on a real ticker (every cfg.CheckEvery()).
func (l *Liveness) Monitor(ctx context.Context, keepalive, dead func()) {
	t := time.NewTicker(l.cfg.CheckEvery())
	defer t.Stop()
	l.Run(ctx, t.C, keepalive, dead)
}
