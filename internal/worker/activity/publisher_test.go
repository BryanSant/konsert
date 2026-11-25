package activity

import (
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// fakeClock drives a Publisher deterministically: timers fire only when advanced past.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	was := !t.stopped
	t.stopped = true
	return was
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

// advance moves time forward by d, firing due timers in order.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		sort.Slice(c.timers, func(i, j int) bool { return c.timers[i].at.Before(c.timers[j].at) })
		var next *fakeTimer
		for _, t := range c.timers {
			if !t.stopped && !t.at.After(end) {
				next = t
				break
			}
		}
		if next == nil {
			c.now = end
			c.mu.Unlock()
			return
		}
		next.stopped = true
		c.now = next.at
		c.mu.Unlock()
		next.f()
	}
}

type sent struct {
	at time.Time
	a  protocol.SessionActivity
}

func newTestPublisher() (*Publisher, *fakeClock, *[]sent) {
	clk := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	out := &[]sent{}
	p := NewPublisher(PublisherOptions{Now: clk.Now, AfterFunc: clk.AfterFunc}, func(a protocol.SessionActivity) {
		mu.Lock()
		*out = append(*out, sent{clk.Now(), a})
		mu.Unlock()
	})
	return p, clk, out
}

func act(state, tool string) protocol.SessionActivity {
	return protocol.SessionActivity{ChatID: "c1", State: state, Tool: tool}
}

func states(out []sent) []string {
	var s []string
	for _, x := range out {
		s = append(s, x.a.State+":"+x.a.Tool)
	}
	return s
}

func TestPublisherFirstIsImmediateTransientIsDebounced(t *testing.T) {
	p, clk, out := newTestPublisher()
	p.Update(act(protocol.ActivityQueued, ""))
	if len(*out) != 1 {
		t.Fatalf("first update should publish at once, got %d", len(*out))
	}
	clk.advance(time.Second)
	p.Update(act(protocol.ActivityWorking, "Bash"))
	if len(*out) != 1 {
		t.Fatal("a transient state must wait for the debounce")
	}
	clk.advance(DefaultDebounce)
	if got := states(*out); len(got) != 2 || got[1] != "working:Bash" {
		t.Fatalf("published %v", got)
	}
}

func TestPublisherSuppressesBlips(t *testing.T) {
	p, clk, out := newTestPublisher()
	p.Update(act(protocol.ActivityWorking, "Bash"))
	clk.advance(time.Second)
	// Tool ends, the model thinks for 50 ms, the next tool starts.
	p.Update(act(protocol.ActivityThinking, ""))
	clk.advance(50 * time.Millisecond)
	p.Update(act(protocol.ActivityWorking, "Read"))
	clk.advance(time.Second)
	got := states(*out)
	if len(got) != 2 || got[1] != "working:Read" {
		t.Fatalf("published %v, want the blip skipped", got)
	}
	// And back to what is already shown publishes nothing.
	p.Update(act(protocol.ActivityThinking, ""))
	clk.advance(100 * time.Millisecond)
	p.Update(act(protocol.ActivityWorking, "Read"))
	clk.advance(time.Second)
	if len(*out) != 2 {
		t.Fatalf("returning to the shown state published %v", states(*out))
	}
}

func TestPublisherSpacingAndHumanStates(t *testing.T) {
	p, clk, out := newTestPublisher()
	p.Update(act(protocol.ActivityThinking, ""))
	clk.advance(100 * time.Millisecond)
	// Asking is published as soon as the minimum interval allows (not debounced).
	p.Update(act(protocol.ActivityAsking, ""))
	clk.advance(DefaultMinInterval - 100*time.Millisecond - time.Millisecond)
	if len(*out) != 1 {
		t.Fatal("published before the minimum interval")
	}
	clk.advance(time.Millisecond)
	if got := states(*out); len(got) != 2 || got[1] != "asking:" {
		t.Fatalf("published %v", got)
	}
	clk.advance(time.Second)
	p.Update(act(protocol.ActivityDone, ""))
	if len(*out) != 3 {
		t.Fatal("done after a quiet second should publish at once")
	}
}

func TestPublisherBoundsAFlood(t *testing.T) {
	p, clk, out := newTestPublisher()
	// 2000 updates over 2 s: alternating tools every millisecond plus rate changes.
	for i := 0; i < 2000; i++ {
		a := act(protocol.ActivityWorking, []string{"Bash", "Read", "Edit"}[i%3])
		if i%5 == 0 {
			a = act(protocol.ActivitySpeaking, "")
			a.TokensPerSec = float64(i)
		}
		p.Update(a)
		clk.advance(time.Millisecond)
	}
	clk.advance(2 * time.Second)
	if n := len(*out); n > 10 || n < 2 {
		t.Fatalf("2000 updates in 2 s published %d times, want a few per second", n)
	}
	for i := 1; i < len(*out); i++ {
		if gap := (*out)[i].at.Sub((*out)[i-1].at); gap < DefaultMinInterval {
			t.Fatalf("publishes %d and %d only %v apart", i-1, i, gap)
		}
	}
	// The latest activity always lands.
	last := (*out)[len(*out)-1].a
	if last.State != protocol.ActivityWorking || last.Tool != "Read" { // i=1999: 1999%3 == 1
		t.Fatalf("last published %s:%s, want the final update working:Read", last.State, last.Tool)
	}
}

func TestPublisherRateOnlyUpdatesAtMostOncePerSecond(t *testing.T) {
	p, clk, out := newTestPublisher()
	a := act(protocol.ActivitySpeaking, "")
	p.Update(a)
	for i := 1; i <= 30; i++ {
		clk.advance(100 * time.Millisecond)
		a.TokensPerSec = float64(i)
		p.Update(a)
	}
	clk.advance(2 * time.Second)
	// 3 s of rate changes: the first publish plus about one per second.
	if n := len(*out); n < 3 || n > 5 {
		t.Fatalf("rate-only updates published %d times, want ~4", n)
	}
	if last := (*out)[len(*out)-1].a.TokensPerSec; last != 30 {
		t.Fatalf("last rate %v, want 30", last)
	}
}

func TestPublisherMaxWait(t *testing.T) {
	p, clk, out := newTestPublisher()
	p.Update(act(protocol.ActivityThinking, ""))
	clk.advance(time.Second)
	// A new tool every 200 ms never settles for the debounce, but shows within MaxWait.
	start := clk.Now()
	for i := 0; i < 10 && len(*out) == 1; i++ {
		p.Update(act(protocol.ActivityWorking, []string{"Bash", "Read"}[i%2]))
		clk.advance(200 * time.Millisecond)
	}
	if len(*out) < 2 {
		t.Fatal("never published a stream of changes")
	}
	if d := (*out)[1].at.Sub(start); d > DefaultMaxWait {
		t.Fatalf("first change published after %v, want within %v", d, DefaultMaxWait)
	}
}

func TestPublisherFlushForgetSnapshotPrune(t *testing.T) {
	p, clk, out := newTestPublisher()
	p.Update(act(protocol.ActivityThinking, ""))
	p.Update(act(protocol.ActivityWorking, "Bash"))
	p.Flush("chat:c1")
	if got := states(*out); len(got) != 2 || got[1] != "working:Bash" {
		t.Fatalf("flush published %v", got)
	}
	task := protocol.SessionActivity{TaskID: "t1", State: protocol.ActivityDone}
	p.Update(task)
	if snap := p.Snapshot(); len(snap) != 2 {
		t.Fatalf("snapshot has %d sessions, want 2", len(snap))
	}
	p.Update(act(protocol.ActivitySpeaking, ""))
	p.Forget("chat:c1")
	clk.advance(time.Second)
	if len(*out) != 3 {
		t.Fatalf("a forgotten session published: %v", states(*out))
	}
	clk.advance(2 * time.Hour)
	p.Prune(time.Hour)
	if snap := p.Snapshot(); len(snap) != 0 {
		t.Fatalf("prune kept %v", snap)
	}
}

func TestPublisherShowsAShortReply(t *testing.T) {
	p, clk, out := newTestPublisher()
	p.Update(act(protocol.ActivityThinking, ""))
	clk.advance(time.Second)
	// A reply streamed for 600 ms (a delta every 50 ms), then the turn goes on.
	a := act(protocol.ActivitySpeaking, "")
	for i := 0; i < 12; i++ {
		a.TokensPerSec = float64(10 + i)
		p.Update(a)
		clk.advance(50 * time.Millisecond)
	}
	p.Update(act(protocol.ActivityThinking, ""))
	clk.advance(time.Second)
	got := states(*out)
	if len(got) != 3 || got[1] != "speaking:" || got[2] != "thinking:" {
		t.Fatalf("published %v, want the 600 ms reply shown", got)
	}
}
