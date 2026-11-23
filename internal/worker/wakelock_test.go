package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

// fakeWakeBackend records acquisitions and hands out controllable holders.
type fakeWakeBackend struct {
	mu       sync.Mutex
	err      error
	acquired int
	released int
	reasons  []string
	holders  []*fakeWakeHolder
}

func (b *fakeWakeBackend) Method() string { return "fake" }

func (b *fakeWakeBackend) Acquire(reason string) (wakeHolder, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	b.acquired++
	b.reasons = append(b.reasons, reason)
	h := &fakeWakeHolder{b: b, done: make(chan struct{})}
	b.holders = append(b.holders, h)
	return h, nil
}

func (b *fakeWakeBackend) counts() (acquired, released int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.acquired, b.released
}

func (b *fakeWakeBackend) last() *fakeWakeHolder {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.holders[len(b.holders)-1]
}

type fakeWakeHolder struct {
	b    *fakeWakeBackend
	once sync.Once
	done chan struct{}
	err  error
}

func (h *fakeWakeHolder) Release() error {
	h.once.Do(func() {
		h.b.mu.Lock()
		h.b.released++
		h.b.mu.Unlock()
		close(h.done)
	})
	return nil
}

// die simulates the helper ending on its own.
func (h *fakeWakeHolder) die(err error) {
	h.once.Do(func() {
		h.err = err
		close(h.done)
	})
}

func (h *fakeWakeHolder) Done() <-chan struct{} { return h.done }
func (h *fakeWakeHolder) Err() error            { return h.err }

// stateRecorder collects onChange notifications.
type stateRecorder struct {
	mu     sync.Mutex
	states []protocol.HostWakeLockState
}

func (r *stateRecorder) record(st protocol.HostWakeLockState) {
	r.mu.Lock()
	r.states = append(r.states, st)
	r.mu.Unlock()
}

func (r *stateRecorder) snapshot() []protocol.HostWakeLockState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]protocol.HostWakeLockState(nil), r.states...)
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWakeLockSetIsIdempotent(t *testing.T) {
	b := &fakeWakeBackend{}
	rec := &stateRecorder{}
	m := NewWakeLockManager(b, nil, rec.record)

	if st := m.State(); st.Active || st.Method != "fake" {
		t.Fatalf("initial state = %+v, want inactive fake", st)
	}
	st := m.Set(true, "long run")
	if !st.Active || st.Reason != "long run" || st.Since == nil || st.Error != "" {
		t.Fatalf("after enable state = %+v", st)
	}
	again := m.Set(true, "other reason")
	if !wakeStateEqual(st, again) {
		t.Fatalf("second enable changed state: %+v -> %+v", st, again)
	}
	if a, r := b.counts(); a != 1 || r != 0 {
		t.Fatalf("acquired=%d released=%d after double enable, want 1/0", a, r)
	}

	if st := m.Set(false, ""); st.Active {
		t.Fatalf("after disable state = %+v", st)
	}
	m.Set(false, "")
	if a, r := b.counts(); a != 1 || r != 1 {
		t.Fatalf("acquired=%d released=%d after double disable, want 1/1", a, r)
	}
	if n := len(rec.snapshot()); n != 2 {
		t.Fatalf("got %d notifications, want 2 (enable, disable): %+v", n, rec.snapshot())
	}
}

func TestWakeLockDefaultReason(t *testing.T) {
	b := &fakeWakeBackend{}
	m := NewWakeLockManager(b, nil, nil)
	m.Set(true, "   ")
	if b.reasons[0] != defaultWakeReason {
		t.Fatalf("reason = %q, want default", b.reasons[0])
	}
}

func TestWakeLockAcquireErrorIsReported(t *testing.T) {
	b := &fakeWakeBackend{err: errors.New("systemd-inhibit not found on PATH")}
	rec := &stateRecorder{}
	m := NewWakeLockManager(b, nil, rec.record)

	st := m.Set(true, "")
	if st.Active || !strings.Contains(st.Error, "systemd-inhibit not found") {
		t.Fatalf("state = %+v, want inactive with error", st)
	}
	if n := len(rec.snapshot()); n != 1 {
		t.Fatalf("notifications = %d, want 1", n)
	}
	// Disabling clears the error.
	if st := m.Set(false, ""); st.Error != "" {
		t.Fatalf("after disable state = %+v, want error cleared", st)
	}
	// Once the tool appears, enabling works.
	b.mu.Lock()
	b.err = nil
	b.mu.Unlock()
	if st := m.Set(true, ""); !st.Active {
		t.Fatalf("enable after fix: %+v", st)
	}
}

func TestWakeLockHolderDeathIsReported(t *testing.T) {
	b := &fakeWakeBackend{}
	rec := &stateRecorder{}
	m := NewWakeLockManager(b, nil, rec.record)
	m.Set(true, "")
	b.last().die(errors.New("caffeinate exited: signal: killed"))

	waitUntil(t, 2*time.Second, "lost lock to be reported", func() bool { return !m.State().Active })
	if st := m.State(); !strings.Contains(st.Error, "signal: killed") {
		t.Fatalf("state = %+v, want the helper's error", st)
	}
	// Enabling again takes a fresh lock.
	if st := m.Set(true, ""); !st.Active {
		t.Fatalf("re-enable: %+v", st)
	}
	if a, _ := b.counts(); a != 2 {
		t.Fatalf("acquired = %d, want 2", a)
	}
}

func TestWakeLockGracePeriod(t *testing.T) {
	b := &fakeWakeBackend{}
	m := NewWakeLockManager(b, nil, nil)
	m.Set(true, "")

	// Reconnect within the grace period keeps the lock.
	m.Disconnected(80 * time.Millisecond)
	m.Connected()
	time.Sleep(200 * time.Millisecond)
	if !m.State().Active {
		t.Fatal("lock released although the hub reconnected within the grace period")
	}

	// Staying disconnected releases it.
	m.Disconnected(30 * time.Millisecond)
	waitUntil(t, 2*time.Second, "grace release", func() bool { return !m.State().Active })
	if _, r := b.counts(); r != 1 {
		t.Fatalf("released = %d, want 1", r)
	}
	if st := m.State(); !strings.Contains(st.Error, "hub connection lost") {
		t.Fatalf("state = %+v, want a grace-release note", st)
	}

	// A disconnect while nothing is held starts no timer.
	m.Disconnected(10 * time.Millisecond)
	m.mu.Lock()
	pending := m.grace != nil
	m.mu.Unlock()
	if pending {
		t.Fatal("grace timer started with no lock held")
	}
}

func TestWakeLockShutdown(t *testing.T) {
	b := &fakeWakeBackend{}
	m := NewWakeLockManager(b, nil, nil)
	m.Set(true, "")
	m.Disconnected(time.Hour)
	m.Shutdown()
	if a, r := b.counts(); a != 1 || r != 1 {
		t.Fatalf("acquired=%d released=%d after shutdown, want 1/1", a, r)
	}
	if st := m.Set(true, ""); st.Active {
		t.Fatalf("enable after shutdown took a lock: %+v", st)
	}
	m.Shutdown() // idempotent
}

func TestWakeLockConcurrentToggles(t *testing.T) {
	b := &fakeWakeBackend{}
	m := NewWakeLockManager(b, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(on bool) {
			defer wg.Done()
			m.Set(on, "")
		}(i%2 == 0)
	}
	wg.Wait()
	m.Set(false, "")
	a, r := b.counts()
	if a != r {
		t.Fatalf("acquired=%d released=%d: a lock leaked", a, r)
	}
}

func TestUnsupportedWakeBackend(t *testing.T) {
	m := NewWakeLockManager(unsupportedWakeBackend{}, nil, nil)
	st := m.Set(true, "")
	if st.Active || st.Error == "" {
		t.Fatalf("state = %+v, want inactive with error", st)
	}
}

// TestDaemonHostWakeLock drives host.wakelock through a real daemon connection.
func TestDaemonHostWakeLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	hub := newFakeHub(t)
	d := NewDaemon(config.WorkerConfig{
		ControllerURL:    hub.wsURL(),
		WorkerID:         "wake-worker",
		Name:             "wake-worker",
		EnableMockDriver: true,
	}, drivers.DefaultRegistry, nil)
	d.HeartbeatInterval = time.Hour
	d.FactsInterval = time.Hour
	b := &fakeWakeBackend{}
	d.wake = NewWakeLockManager(b, nil, func(st protocol.HostWakeLockState) {
		d.sendJSON(protocol.MsgHostWakeLockState, st)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	c := hub.accept(t)
	decode := func(env protocol.Envelope) protocol.HostWakeLockState {
		st, err := protocol.DecodePayload[protocol.HostWakeLockState](env)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	// The state is reported right after hello.
	c.waitFor(t, 5*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))
	if st := decode(c.waitFor(t, 5*time.Second, "initial wake state", isType(protocol.MsgHostWakeLockState))); st.Active || st.Method != "fake" {
		t.Fatalf("initial state = %+v", st)
	}

	c.send(t, protocol.MsgHostWakeLock, "", protocol.HostWakeLockPayload{Enabled: true, Reason: "overnight run"})
	if st := decode(c.waitFor(t, 5*time.Second, "active wake state", isType(protocol.MsgHostWakeLockState))); !st.Active || st.Reason != "overnight run" {
		t.Fatalf("after enable state = %+v", st)
	}
	// An idempotent request still gets an answer.
	c.send(t, protocol.MsgHostWakeLock, "", protocol.HostWakeLockPayload{Enabled: true})
	if st := decode(c.waitFor(t, 5*time.Second, "repeat wake state", isType(protocol.MsgHostWakeLockState))); !st.Active {
		t.Fatalf("after repeat enable state = %+v", st)
	}
	if a, _ := b.counts(); a != 1 {
		t.Fatalf("acquired = %d, want 1", a)
	}

	// Daemon shutdown releases the lock.
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not stop")
	}
	if _, r := b.counts(); r != 1 {
		t.Fatalf("released = %d after shutdown, want 1", r)
	}
}
