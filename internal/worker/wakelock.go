package worker

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// DefaultWakeLockGrace is how long konsertd keeps a host wake lock after losing its hub
// connection. Reconnect backoff tops out at 30 s, so a short hub restart or network blip
// keeps the host awake, while a hub that is gone for good stops pinning the host.
const DefaultWakeLockGrace = 3 * time.Minute

// defaultWakeReason is used when the hub sends an empty reason.
const defaultWakeReason = "Konsert Keep Awake"

// wakeHolder is one held OS inhibitor.
type wakeHolder interface {
	// Release drops the inhibitor and waits until it is gone. It is called at most once.
	Release() error
	// Done is closed when the inhibitor ends on its own (e.g. the helper process died).
	Done() <-chan struct{}
	// Err describes why the inhibitor ended on its own, once Done is closed.
	Err() error
}

// wakeBackend acquires OS inhibitors on one platform.
type wakeBackend interface {
	// Method names the mechanism ("caffeinate", "systemd-inhibit", ...).
	Method() string
	// Acquire takes a new inhibitor. It must not leave anything behind on error.
	Acquire(reason string) (wakeHolder, error)
}

// errWakeLockUnsupported is returned by the backend on platforms with no mechanism.
var errWakeLockUnsupported = errors.New("host keep-awake is not supported on this platform")

// WakeLockManager holds at most one host wake lock on behalf of the hub. All methods are
// safe for concurrent use and idempotent: enabling an active lock or disabling an inactive
// one changes nothing.
type WakeLockManager struct {
	backend  wakeBackend
	logger   *slog.Logger
	onChange func(protocol.HostWakeLockState)

	// opMu serializes acquire/release so the OS side matches the order of requests;
	// mu guards the fields below and is never held across a backend call.
	opMu   sync.Mutex
	mu     sync.Mutex
	holder wakeHolder
	state  protocol.HostWakeLockState
	grace  *time.Timer
	closed bool
}

// NewWakeLockManager returns a manager using backend (nil selects the platform default).
// onChange, if non-nil, is called with the new state after every change; it must not block.
func NewWakeLockManager(backend wakeBackend, logger *slog.Logger, onChange func(protocol.HostWakeLockState)) *WakeLockManager {
	if backend == nil {
		backend = defaultWakeBackend()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &WakeLockManager{
		backend:  backend,
		logger:   logger,
		onChange: onChange,
		state:    protocol.HostWakeLockState{Method: backend.Method()},
	}
}

// State returns a copy of the current state.
func (m *WakeLockManager) State() protocol.HostWakeLockState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copyWakeState(m.state)
}

// Set enables or disables the wake lock and returns the resulting state. Failures are
// reported in the state's Error field rather than returned.
func (m *WakeLockManager) Set(enabled bool, reason string) protocol.HostWakeLockState {
	if enabled {
		return m.acquire(reason)
	}
	return m.release("")
}

// Shutdown releases any held lock and refuses later enables. konsertd calls it on exit.
func (m *WakeLockManager) Shutdown() {
	m.mu.Lock()
	m.closed = true
	m.stopGraceLocked()
	m.mu.Unlock()
	m.release("")
}

// Disconnected starts the grace timer after the hub connection drops: if no Connected
// call arrives within grace, the lock is released.
func (m *WakeLockManager) Disconnected(grace time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holder == nil || m.grace != nil || m.closed {
		return
	}
	if grace <= 0 {
		grace = DefaultWakeLockGrace
	}
	// t is written under mu before the callback can take mu, so the callback reads the
	// final value. A Connected (or a newer Disconnected) replaces m.grace, which makes a
	// timer that already fired a no-op.
	var t *time.Timer
	t = time.AfterFunc(grace, func() {
		m.mu.Lock()
		if m.grace != t {
			m.mu.Unlock()
			return
		}
		m.grace = nil
		m.mu.Unlock()
		m.logger.Info("releasing host wake lock: hub connection lost for longer than the grace period", "grace", grace)
		m.release("released: hub connection lost for longer than " + grace.String())
	})
	m.grace = t
}

// Connected cancels a pending grace-period release.
func (m *WakeLockManager) Connected() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopGraceLocked()
}

func (m *WakeLockManager) stopGraceLocked() {
	if m.grace != nil {
		m.grace.Stop()
		m.grace = nil
	}
}

func (m *WakeLockManager) acquire(reason string) protocol.HostWakeLockState {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = defaultWakeReason
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return m.State()
	}
	if m.holder != nil {
		st := copyWakeState(m.state)
		m.mu.Unlock()
		return st
	}
	m.mu.Unlock()

	h, err := m.backend.Acquire(reason)

	m.mu.Lock()
	if err != nil {
		m.state = protocol.HostWakeLockState{Method: m.backend.Method(), Error: err.Error()}
		st := copyWakeState(m.state)
		m.mu.Unlock()
		m.logger.Warn("host wake lock unavailable", "method", m.backend.Method(), "err", err)
		m.notify(st)
		return st
	}
	now := time.Now().UTC()
	m.holder = h
	m.state = protocol.HostWakeLockState{Active: true, Method: m.backend.Method(), Reason: reason, Since: &now}
	st := copyWakeState(m.state)
	m.mu.Unlock()

	m.logger.Info("host wake lock acquired", "method", m.backend.Method(), "reason", reason)
	go m.watch(h)
	m.notify(st)
	return st
}

// release drops the held lock (if any). note, when non-empty, is recorded as the state's
// Error so the hub can show why the lock went away.
func (m *WakeLockManager) release(note string) protocol.HostWakeLockState {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	h := m.holder
	m.holder = nil
	m.stopGraceLocked()
	m.mu.Unlock()

	if h != nil {
		if err := h.Release(); err != nil {
			m.logger.Warn("releasing host wake lock", "method", m.backend.Method(), "err", err)
		} else {
			m.logger.Info("host wake lock released", "method", m.backend.Method())
		}
	}

	m.mu.Lock()
	changed := h != nil || m.state.Error != note
	m.state = protocol.HostWakeLockState{Method: m.backend.Method(), Error: note}
	st := copyWakeState(m.state)
	m.mu.Unlock()
	if changed {
		m.notify(st)
	}
	return st
}

// watch reports a lock that ends without being released (helper process killed, etc.).
func (m *WakeLockManager) watch(h wakeHolder) {
	<-h.Done()
	m.mu.Lock()
	if m.holder != h {
		m.mu.Unlock()
		return
	}
	m.holder = nil
	msg := "wake lock ended unexpectedly"
	if err := h.Err(); err != nil {
		msg = err.Error()
	}
	m.state = protocol.HostWakeLockState{Method: m.backend.Method(), Error: msg}
	st := copyWakeState(m.state)
	m.mu.Unlock()
	m.logger.Warn("host wake lock lost", "method", m.backend.Method(), "err", msg)
	m.notify(st)
}

func (m *WakeLockManager) notify(st protocol.HostWakeLockState) {
	if m.onChange != nil {
		m.onChange(st)
	}
}

func copyWakeState(s protocol.HostWakeLockState) protocol.HostWakeLockState {
	if s.Since != nil {
		t := *s.Since
		s.Since = &t
	}
	return s
}

// unsupportedWakeBackend is used on platforms without a mechanism.
type unsupportedWakeBackend struct{}

func (unsupportedWakeBackend) Method() string { return "" }

func (unsupportedWakeBackend) Acquire(string) (wakeHolder, error) {
	return nil, errWakeLockUnsupported
}

// wakeStateEqual reports whether two states describe the same lock.
func wakeStateEqual(a, b protocol.HostWakeLockState) bool {
	if a.Active != b.Active || a.Method != b.Method || a.Error != b.Error || a.Reason != b.Reason {
		return false
	}
	if (a.Since == nil) != (b.Since == nil) {
		return false
	}
	return a.Since == nil || a.Since.Equal(*b.Since)
}

// fallbackWakeBackend tries a full inhibitor first and, when useFallback(err) says the
// failure is a permission problem, a weaker one. Method reports which one is held.
//
// On Linux, logind lets any session block idle, but blocking sleep from a non-console
// session (SSH, a systemd user service) needs polkit's auth_admin. konsertd usually runs
// that way, so it falls back to an idle-only inhibitor rather than failing outright.
type fallbackWakeBackend struct {
	primary, fallback wakeBackend
	useFallback       func(error) bool
	fallbackMethod    string

	mu     sync.Mutex
	method string
}

func (b *fallbackWakeBackend) Method() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.method == "" {
		return b.primary.Method()
	}
	return b.method
}

func (b *fallbackWakeBackend) setMethod(m string) {
	b.mu.Lock()
	b.method = m
	b.mu.Unlock()
}

func (b *fallbackWakeBackend) Acquire(reason string) (wakeHolder, error) {
	h, err := b.primary.Acquire(reason)
	if err == nil {
		b.setMethod(b.primary.Method())
		return h, nil
	}
	if b.useFallback == nil || !b.useFallback(err) {
		b.setMethod(b.primary.Method())
		return nil, err
	}
	h, ferr := b.fallback.Acquire(reason)
	if ferr != nil {
		b.setMethod(b.primary.Method())
		return nil, err
	}
	b.setMethod(b.fallbackMethod)
	return h, nil
}
