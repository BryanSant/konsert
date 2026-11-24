package worker

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
)

// fastLink shortens the daemon's liveness intervals for tests.
func fastLink(d *Daemon) {
	d.LinkQuiet = 50 * time.Millisecond
	d.LinkDead = 250 * time.Millisecond
}

// TestDaemonDetectsSilentHub: a hub that stops answering (a half-open link, a stopped hub
// process) is declared dead after LinkDead and the daemon redials.
func TestDaemonDetectsSilentHub(t *testing.T) {
	hub := newFakeHub(t)
	hub.silent.Store(true)
	startTestDaemonWith(t, hub, nil, fastLink)
	first := hub.accept(t)
	first.waitFor(t, 5*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))
	silentAt := time.Now()
	hub.silent.Store(false)
	second := hub.accept(t)
	gap := time.Since(silentAt)
	second.waitFor(t, 5*time.Second, "worker.hello on the new link", isType(protocol.MsgWorkerHello))
	// Dead after 250 ms (+ one 10 ms check), then the first backoff (1-1.25 s).
	if gap < 250*time.Millisecond || gap > 4*time.Second {
		t.Fatalf("redialed %v after the hub went silent", gap)
	}
}

// TestDaemonKeepsLiveLinkAndSendsIdleHeartbeats: a hub that pings when quiet keeps the
// link up indefinitely, and an idle daemon sends a vitals heartbeat whenever it has been
// quiet for LinkQuiet.
func TestDaemonKeepsLiveLinkAndSendsIdleHeartbeats(t *testing.T) {
	hub := newFakeHub(t)
	hub.pingEvery.Store(int64(40 * time.Millisecond))
	startTestDaemonWith(t, hub, nil, fastLink)
	c := hub.accept(t)
	c.waitFor(t, 5*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))
	deadline := time.Now().Add(1500 * time.Millisecond) // 6x LinkDead
	beats := 0
	for time.Now().Before(deadline) {
		select {
		case env := <-c.msgs:
			if env.Type == protocol.MsgWorkerHeartbeat {
				beats++
				p, err := protocol.DecodePayload[protocol.WorkerHeartbeatPayload](env)
				if err != nil || p.Metrics == nil {
					t.Fatalf("heartbeat without metrics: %v", err)
				}
			}
		case <-c.done:
			t.Fatal("a live link was dropped")
		case <-time.After(50 * time.Millisecond):
		}
	}
	// HeartbeatInterval is an hour here: these are liveness heartbeats (one per ~50-60 ms
	// of quiet).
	if beats < 10 {
		t.Fatalf("only %d idle heartbeats in 1.5 s", beats)
	}
	select {
	case <-hub.conns:
		t.Fatal("daemon redialed a live link")
	default:
	}
}

// TestDaemonRedialsOnNetworkChange: a change in the machine's addresses drops a working
// link and redials at once, without waiting for the backoff.
func TestDaemonRedialsOnNetworkChange(t *testing.T) {
	hub := newFakeHub(t)
	hub.pingEvery.Store(int64(40 * time.Millisecond))
	var addr atomic.Value
	addr.Store("192.168.1.10")
	startTestDaemonWith(t, hub, nil, func(d *Daemon) {
		fastLink(d)
		d.LinkDead = time.Minute // only the watcher may end the link
		d.WatchEvery = 10 * time.Millisecond
		d.WatchAddrs = func() (string, error) { return addr.Load().(string), nil }
	})
	c := hub.accept(t)
	c.waitFor(t, 5*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))
	time.Sleep(50 * time.Millisecond)
	changedAt := time.Now()
	addr.Store("10.0.0.20")
	second := hub.accept(t)
	second.waitFor(t, 5*time.Second, "worker.hello on the new link", isType(protocol.MsgWorkerHello))
	// No backoff sleep: well under the 1 s first backoff.
	if gap := time.Since(changedAt); gap > 900*time.Millisecond {
		t.Fatalf("redialed %v after the network change", gap)
	}
}

// TestDaemonWritesLinkStatus: the state file `konsertd status` reads follows the link.
func TestDaemonWritesLinkStatus(t *testing.T) {
	const id = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	hub := newFakeHub(t)
	hub.pingEvery.Store(int64(40 * time.Millisecond))
	d, _ := startTestDaemonWith(t, hub, func(c *config.WorkerConfig) { c.WorkerID = id }, fastLink)
	c := hub.accept(t)
	c.waitFor(t, 5*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))
	waitStatus := func(want string) LinkStatus {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			st, err := ReadLinkStatus("", id)
			if err == nil && st.State == want {
				return st
			}
			if time.Now().After(deadline) {
				t.Fatalf("link state %q (err %v), want %q", st.State, err, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	st := waitStatus(LinkConnected)
	if st.Transport != "outbound" || st.WorkerID != id || st.Peer != hub.wsURL() {
		t.Fatalf("connected status %+v", st)
	}
	hub.reject.Store(true)
	_ = c.conn.CloseNow()
	st = waitStatus(LinkDisconnected)
	if st.NextAttempt.IsZero() || st.LastError == "" || st.Attempts < 1 {
		t.Fatalf("disconnected status %+v", st)
	}
	if got := d.link.status(); got.State != LinkDisconnected {
		t.Fatalf("tracker state %q", got.State)
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Split(strings.TrimSpace(s.b.String()), "\n")
}

// TestLinkTrackerLogsQuietly: the first failure, at most one summary a minute, one line
// when the link is back.
func TestLinkTrackerLogsQuietly(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	tr := newLinkTracker(logger, "", "w", "w", "outbound")
	tr.now = func() time.Time { return now }
	tr.up("outbound", "ws://hub")
	boom := errors.New("dial tcp: connection refused")
	// A 10-minute outage, one attempt every 30 s.
	for range 20 {
		tr.failed(boom, 30*time.Second)
		now = now.Add(30 * time.Second)
	}
	tr.up("outbound", "ws://hub")
	lines := buf.lines()
	var lost, still, back int
	for _, l := range lines {
		switch {
		case strings.Contains(l, "hub link lost"):
			lost++
		case strings.Contains(l, "still disconnected"):
			still++
		case strings.Contains(l, "reconnected to the hub after 10m0s (20 attempts)"):
			back++
		}
	}
	if lost != 1 || still < 9 || still > 10 || back != 1 || len(lines) != 2+lost+still {
		t.Fatalf("lost %d, still %d, back %d in %d lines:\n%s", lost, still, back, len(lines), strings.Join(lines, "\n"))
	}
	if st := tr.status(); st.State != LinkConnected || st.Attempts != 0 || st.LastError != "" {
		t.Fatalf("status after reconnect %+v", st)
	}
}
