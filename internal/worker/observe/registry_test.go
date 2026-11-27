package observe

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// fakeSource keeps transcripts in memory: one event per complete line.
type fakeSource struct {
	mu    sync.Mutex
	files map[string]string // session id -> transcript
	mod   map[string]time.Time
	reads int
}

func newFake() *fakeSource {
	return &fakeSource{files: map[string]string{}, mod: map[string]time.Time{}}
}

func (f *fakeSource) Agent() string { return "fake" }

func (f *fakeSource) write(id, text string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[id] += text
	f.mod[id] = at
}

func (f *fakeSource) truncate(id, text string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[id] = text
	f.mod[id] = at
}

func (f *fakeSource) Discover(_ context.Context, since time.Time) ([]Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Session
	for id, body := range f.files {
		if f.mod[id].Before(since) {
			continue
		}
		out = append(out, Session{Agent: "fake", ID: id, Path: "/fake/" + id, Size: int64(len(body)), ModTime: f.mod[id]})
	}
	return out, nil
}

func (f *fakeSource) Read(_ context.Context, s Session, offset int64) ([]protocol.AgentChatEvent, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	body := f.files[s.ID][offset:]
	end := strings.LastIndexByte(body, '\n') + 1 // complete records only
	var evs []protocol.AgentChatEvent
	for line := range strings.SplitSeq(body[:end], "\n") {
		if line != "" {
			evs = append(evs, protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, Text: line})
		}
	}
	return evs, offset + int64(end), nil
}

type sink struct {
	mu sync.Mutex
	us []Update
}

func (s *sink) emit(u Update) {
	s.mu.Lock()
	s.us = append(s.us, u)
	s.mu.Unlock()
}

// texts are the event texts reported for id, in order; retracts appear as "<retract>".
func (s *sink) texts(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, u := range s.us {
		if u.Session.ID != id {
			continue
		}
		if u.Kind == UpdateRetract {
			out = append(out, "<retract>")
		}
		for _, e := range u.Events {
			out = append(out, e.Text)
		}
	}
	return out
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time       { return c.t }
func (c *clock) add(d time.Duration)  { c.t = c.t.Add(d) }
func newClock() *clock                { return &clock{t: time.Unix(1_800_000_000, 0)} }
func eq(a []string, b ...string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

func TestRegistryReadsIncrementallyAfterGrace(t *testing.T) {
	ctx := context.Background()
	src, out, clk := newFake(), &sink{}, newClock()
	r := New(Config{Sources: []Source{src}, Emit: out.emit, Now: clk.now, Grace: 3 * time.Second})

	src.write("s1", "hello\nworld\npart", clk.t)
	r.Poll(ctx)
	if got := out.texts("s1"); len(got) != 0 {
		t.Fatalf("reported during the grace period: %v", got)
	}
	clk.add(3 * time.Second)
	r.Poll(ctx)
	if got := out.texts("s1"); !eq(got, "hello", "world") {
		t.Fatalf("first report = %v, want the complete records only", got)
	}
	src.write("s1", "ial\nmore\n", clk.t)
	clk.add(2 * time.Second)
	r.Poll(ctx)
	if got := out.texts("s1"); !eq(got, "hello", "world", "partial", "more") {
		t.Fatalf("after appends = %v", got)
	}
	reads := src.reads
	clk.add(2 * time.Second)
	r.Poll(ctx)
	if src.reads != reads {
		t.Fatal("an unchanged transcript was read again")
	}
}

// Acceptance: managed sessions are never double-reported.
func TestRegistryNeverReportsManagedSessions(t *testing.T) {
	ctx := context.Background()
	src, out, clk := newFake(), &sink{}, newClock()
	managed := map[string]bool{}
	var mu sync.Mutex
	isManaged := func(s Session) bool { mu.Lock(); defer mu.Unlock(); return managed[s.ID] }
	r := New(Config{Sources: []Source{src}, Emit: out.emit, Managed: isManaged, Now: clk.now, Grace: 3 * time.Second})

	// A managed chat's file appears before its mark reaches the daemon: the grace
	// period covers it, and it is never reported.
	src.write("chat", "one\n", clk.t)
	r.Poll(ctx)
	clk.add(time.Second)
	mu.Lock()
	managed["chat"] = true
	mu.Unlock()
	for range 5 {
		src.write("chat", "more\n", clk.t)
		clk.add(2 * time.Second)
		r.Poll(ctx)
	}
	if got := out.texts("chat"); len(got) != 0 {
		t.Fatalf("managed session reported: %v", got)
	}

	// A mark arriving after the report retracts the session, and nothing more follows.
	src.write("late", "a\n", clk.t)
	r.Poll(ctx)
	clk.add(3 * time.Second)
	r.Poll(ctx)
	mu.Lock()
	managed["late"] = true
	mu.Unlock()
	src.write("late", "b\n", clk.t)
	clk.add(2 * time.Second)
	r.Poll(ctx)
	src.write("late", "c\n", clk.t)
	clk.add(2 * time.Second)
	r.Poll(ctx)
	if got := out.texts("late"); !eq(got, "a", "<retract>") {
		t.Fatalf("late-marked session = %v, want its first report then a retract", got)
	}
	if n := len(r.Sessions()); n != 0 {
		t.Fatalf("%d sessions listed, want none", n)
	}
}

// Offsets persist: a restarted registry continues where the last one stopped, with no
// replay and no grace delay; a rewritten transcript is read from the start.
func TestRegistryOffsetsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "observe-offsets.json")
	src, clk := newFake(), newClock()

	out1 := &sink{}
	r1 := New(Config{Sources: []Source{src}, Emit: out1.emit, Now: clk.now, OffsetsPath: path, Grace: time.Second})
	src.write("s", "one\ntwo\n", clk.t)
	r1.Poll(ctx)
	clk.add(time.Second)
	r1.Poll(ctx)
	if got := out1.texts("s"); !eq(got, "one", "two") {
		t.Fatalf("first run = %v", got)
	}

	src.write("s", "three\n", clk.t) // written while the daemon was down
	clk.add(time.Minute)
	out2 := &sink{}
	r2 := New(Config{Sources: []Source{src}, Emit: out2.emit, Now: clk.now, OffsetsPath: path, Grace: time.Second})
	r2.Poll(ctx)
	if got := out2.texts("s"); !eq(got, "three") {
		t.Fatalf("after restart = %v, want only what was new", got)
	}

	src.truncate("s", "fresh\n", clk.t) // the agent rewrote it
	clk.add(2 * time.Second)
	r2.Poll(ctx)
	if got := out2.texts("s"); !eq(got, "three", "fresh") {
		t.Fatalf("after a rewrite = %v, want it read from the start", got)
	}
}

// Sessions idle beyond the lookback are history: not reported on first start.
func TestRegistrySkipsHistory(t *testing.T) {
	ctx := context.Background()
	src, out, clk := newFake(), &sink{}, newClock()
	src.write("old", "x\n", clk.t.Add(-48*time.Hour))
	r := New(Config{Sources: []Source{src}, Emit: out.emit, Now: clk.now, Grace: time.Second, Lookback: 24 * time.Hour})
	r.Poll(ctx)
	clk.add(time.Second)
	r.Poll(ctx)
	if got := out.texts("old"); len(got) != 0 {
		t.Fatalf("a two-day-old session was reported: %v", got)
	}
}

func TestManagedSet(t *testing.T) {
	var ms ManagedSet
	ms.Mark("claude", "abc")
	ms.Mark("claude", "")
	if !ms.Has(Session{Agent: "claude", ID: "abc"}) {
		t.Fatal("marked session not managed")
	}
	if ms.Has(Session{Agent: "codex", ID: "abc"}) || ms.Has(Session{Agent: "claude", ID: ""}) {
		t.Fatal("unmarked session reported managed")
	}
}
