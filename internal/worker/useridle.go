package worker

import (
	"context"
	"sync"
	"time"
)

// User idle time: how long since someone used this machine's keyboard or mouse, so
// heavy work can stay off machines where a person is working (docs/plan.md, Phase 6).
// Reading it may run a tool (ioreg, loginctl), so it is cached for userIdleEvery.

const userIdleEvery = 30 * time.Second

// readUserIdle is the per-OS reader (tests replace it): the idle duration, the source
// ("hid", "last_input", "logind") and whether the machine has a user session to read.
var readUserIdle = platformUserIdle

type userIdleCache struct {
	mu     sync.Mutex
	at     time.Time
	idle   time.Duration
	source string
	ok     bool
}

func (c *userIdleCache) get(ctx context.Context) (time.Duration, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) >= userIdleEvery {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		c.idle, c.source, c.ok = readUserIdle(rctx)
		cancel()
		c.at = time.Now()
	}
	// Between reads the machine stays idle at least as long as it was.
	if c.ok && c.idle > 0 {
		return c.idle + time.Since(c.at), c.source, true
	}
	return c.idle, c.source, c.ok
}
