package auth

import (
	"sync"
	"time"
)

// LoginLimiter throttles repeated failed logins per client key (usually the peer IP)
// with exponential backoff. The first FreeAttempts failures are not delayed; each
// further failure doubles the lockout, starting at BaseDelay and capped at MaxDelay.
// A successful login clears the key.
type LoginLimiter struct {
	FreeAttempts int
	BaseDelay    time.Duration
	MaxDelay     time.Duration
	// Now is the clock; tests may replace it.
	Now func() time.Time

	mu      sync.Mutex
	entries map[string]*loginEntry
}

type loginEntry struct {
	failures    int
	lockedUntil time.Time
	lastSeen    time.Time
}

// staleAfter is how long an idle entry is kept before it is pruned.
const staleAfter = time.Hour

// pruneThreshold is the entry count above which stale entries are swept.
const pruneThreshold = 1024

// NewLoginLimiter returns a limiter allowing 5 free attempts, then backing off from
// 1s up to 5 minutes.
func NewLoginLimiter() *LoginLimiter {
	return &LoginLimiter{
		FreeAttempts: 5,
		BaseDelay:    time.Second,
		MaxDelay:     5 * time.Minute,
		Now:          time.Now,
		entries:      make(map[string]*loginEntry),
	}
}

// Allow reports whether key may attempt a login now; if not, it returns how long to wait.
func (l *LoginLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	e := l.entries[key]
	if e == nil {
		return true, 0
	}
	if wait := e.lockedUntil.Sub(now); wait > 0 {
		return false, wait
	}
	return true, 0
}

// Failure records a failed login for key and returns the resulting lockout (0 if none).
func (l *LoginLimiter) Failure(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	if l.entries == nil {
		l.entries = make(map[string]*loginEntry)
	}
	if len(l.entries) >= pruneThreshold {
		l.pruneLocked(now)
	}
	e := l.entries[key]
	if e == nil {
		e = &loginEntry{}
		l.entries[key] = e
	}
	e.failures++
	e.lastSeen = now
	over := e.failures - l.FreeAttempts
	if over <= 0 {
		return 0
	}
	delay := l.BaseDelay
	for i := 1; i < over && delay < l.MaxDelay; i++ {
		delay *= 2
	}
	if delay > l.MaxDelay {
		delay = l.MaxDelay
	}
	e.lockedUntil = now.Add(delay)
	return delay
}

// Success clears any failure history for key.
func (l *LoginLimiter) Success(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

func (l *LoginLimiter) pruneLocked(now time.Time) {
	for k, e := range l.entries {
		if now.Sub(e.lastSeen) > staleAfter && now.After(e.lockedUntil) {
			delete(l.entries, k)
		}
	}
}

// WindowLimiter allows at most Limit events per key in each fixed Window (for
// unauthenticated requests that create rows, such as CLI login codes).
type WindowLimiter struct {
	Limit  int
	Window time.Duration
	// Now is the clock; tests may replace it.
	Now func() time.Time

	mu      sync.Mutex
	windows map[string]*window
}

type window struct {
	start time.Time
	count int
}

// NewWindowLimiter returns a limiter allowing limit events per key per window.
func NewWindowLimiter(limit int, per time.Duration) *WindowLimiter {
	return &WindowLimiter{Limit: limit, Window: per, Now: time.Now, windows: make(map[string]*window)}
}

// Allow records an event for key and reports whether it is within the limit.
func (l *WindowLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	if len(l.windows) >= pruneThreshold {
		for k, w := range l.windows {
			if now.Sub(w.start) >= l.Window {
				delete(l.windows, k)
			}
		}
	}
	w := l.windows[key]
	if w == nil || now.Sub(w.start) >= l.Window {
		w = &window{start: now}
		l.windows[key] = w
	}
	if w.count >= l.Limit {
		return false
	}
	w.count++
	return true
}
