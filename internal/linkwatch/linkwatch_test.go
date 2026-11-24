package linkwatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestLivenessCheck(t *testing.T) {
	clk := newFakeClock()
	l := New(Config{}, clk.Now)
	step := func(d time.Duration, want Action) {
		t.Helper()
		clk.Advance(d)
		if got := l.Check(); got != want {
			t.Fatalf("at +%v: got %v, want %v", d, got, want)
		}
	}
	step(14*time.Second, None)
	step(time.Second, Keepalive) // quiet for 15 s
	step(3*time.Second, None)    // the keepalive counts as a send
	l.Received()
	l.Sent()
	step(14*time.Second, None)
	step(time.Second, Keepalive)
	// Traffic out keeps keepalives away, but only traffic in keeps the link alive.
	for range 9 {
		clk.Advance(3 * time.Second)
		l.Sent()
		if got := l.Check(); got != None {
			t.Fatalf("got %v while sending", got)
		}
	}
	step(3*time.Second, Dead) // 15 + 30 = 45 s without a frame from the peer
}

func TestLivenessRunPingsWhenQuietAndDiesWhenSilent(t *testing.T) {
	clk := newFakeClock()
	l := New(Config{QuietAfter: 15 * time.Second, DeadAfter: 45 * time.Second}, clk.Now)
	ticks := make(chan time.Time) // unbuffered: a send returns once the previous tick was handled
	keepalives := make(chan time.Time, 16)
	dead := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(context.Background(), ticks, func() { keepalives <- time.Time{} }, func() { close(dead) })
	}()
	tick := func(d time.Duration) {
		clk.Advance(d)
		ticks <- clk.Now()
	}
	// The peer answers for the first minute (a pong to every keepalive), then goes silent.
	for range 20 {
		tick(3 * time.Second)
		l.Received()
	}
	tick(0)
	if n := len(keepalives); n != 4 { // 60 s quiet in 15 s steps
		t.Fatalf("keepalives during the first minute = %d, want 4", n)
	}
	// Then the peer goes silent: the link dies 45 s after the last frame, noticed on the
	// first 3 s tick at or past that.
	lastFrame := clk.Now()
	for {
		clk.Advance(3 * time.Second)
		select {
		case ticks <- clk.Now():
			if clk.Now().Sub(lastFrame) > time.Minute {
				t.Fatal("silent link never declared dead")
			}
		case <-dead:
			// Dead on the previous tick.
			if got := clk.Now().Add(-3 * time.Second).Sub(lastFrame); got < 45*time.Second || got >= 48*time.Second {
				t.Fatalf("dead after %v of silence, want 45-48 s", got)
			}
			<-done
			return
		}
	}
}

func TestLivenessRunStopsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		New(Config{}, nil).Run(ctx, make(chan time.Time), func() {}, func() { t.Error("dead") })
		close(done)
	}()
	cancel()
	<-done
}

func TestWatcherWakeAndNetwork(t *testing.T) {
	addr := "192.168.1.5"
	var addrErr error
	w := &Watcher{Every: 5 * time.Second, Slack: 10 * time.Second, Addrs: func() (string, error) { return addr, addrErr }}
	t0 := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	now := t0
	step := func(d time.Duration, want string) {
		t.Helper()
		prev := now
		now = now.Add(d)
		got := w.Step(now)
		if got.Reason != want {
			t.Fatalf("at %v: got %q, want %q", now.Sub(t0), got.Reason, want)
		}
		if want != "" && !got.Since.Equal(prev) {
			t.Fatalf("at %v: since %v, want the previous tick %v", now.Sub(t0), got.Since, prev)
		}
	}
	step(0, "") // baseline
	step(5*time.Second, "")
	step(14*time.Second, "") // late, within slack
	step(16*time.Second, ReasonWake)
	step(5*time.Second, "")
	addr = "10.0.0.7"
	step(5*time.Second, ReasonNetwork)
	step(5*time.Second, "")
	// A failed read keeps the old fingerprint: no change, then none when it comes back.
	addrErr = errors.New("netlink busy")
	addr = "garbage"
	step(5*time.Second, "")
	addrErr, addr = nil, "10.0.0.7"
	step(5*time.Second, "")
	// Wake and a new network at once report the wake.
	addr = "172.16.0.2"
	step(2*time.Hour, ReasonWake)
	step(5*time.Second, "")
}

func TestWatcherRun(t *testing.T) {
	addr := make(chan string, 1)
	addr <- "a"
	cur := "a"
	w := &Watcher{Every: time.Second, Addrs: func() (string, error) {
		select {
		case cur = <-addr:
		default:
		}
		return cur, nil
	}}
	clk := newFakeClock()
	ticks := make(chan time.Time)
	got := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx, clk.Now(), ticks, func(c Change) { got <- c.Reason })
		close(done)
	}()
	clk.Advance(time.Second)
	ticks <- clk.Now()
	addr <- "b"
	clk.Advance(time.Second)
	ticks <- clk.Now()
	clk.Advance(time.Minute) // slept
	ticks <- clk.Now()
	clk.Advance(time.Second)
	ticks <- clk.Now() // returns once the previous tick was handled
	cancel()
	<-done
	close(got)
	var reasons []string
	for r := range got {
		reasons = append(reasons, r)
	}
	if len(reasons) != 2 || reasons[0] != ReasonNetwork || reasons[1] != ReasonWake {
		t.Fatalf("reasons = %q", reasons)
	}
}

// A change only drops links whose attempt started before it: one that redialed after
// the change (it noticed the dead link first) is kept, so it does not reconnect twice.
func TestChangePredates(t *testing.T) {
	since := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	c := Change{Reason: ReasonWake, Since: since}
	if !c.Predates(time.Time{}) {
		t.Fatal("no attempt under way: redial")
	}
	if !c.Predates(since.Add(-2 * time.Hour)) {
		t.Fatal("a link from before the sleep must be redialed")
	}
	if !c.Predates(since) {
		t.Fatal("a link made at the last tick before the change predates it")
	}
	if c.Predates(since.Add(time.Millisecond)) {
		t.Fatal("a link made after the change must be kept")
	}
	// Monotonic readings are ignored (they stop while the machine sleeps).
	now := time.Now()
	if (Change{Since: now.Add(-time.Second)}).Predates(now) {
		t.Fatal("monotonic reading confused the comparison")
	}
}

func TestAddrFingerprint(t *testing.T) {
	fp, err := AddrFingerprint()
	if err != nil {
		t.Skip("no interface addresses here:", err)
	}
	fp2, _ := AddrFingerprint()
	if fp != fp2 {
		t.Fatalf("fingerprint not stable: %q vs %q", fp, fp2)
	}
}

func BenchmarkAddrFingerprint(b *testing.B) {
	for b.Loop() {
		_, _ = AddrFingerprint()
	}
}
