package controller

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BryanSant/konsert/internal/hubkey"
	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/linkwatch"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/sshutil"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/BryanSant/konsert/internal/worker"
	"github.com/coder/websocket"
)

// Transports (KON-K1B60). One worker protocol, three ways to carry it:
//
//   - ssh: machines the hub added over SSH. konsertd serves the hub on its local socket;
//     the hub keeps one long-lived `ssh` per machine, authenticated with the hub's own
//     key, whose forced command (`konsertd attach`) bridges the session to that socket.
//     SSH is the auth: no machine credential, and the hub's port never has to be
//     reachable from the machine.
//   - local: the hub's own machine. The hub dials konsertd's socket directly.
//   - outbound: machines the hub cannot reach (joined with a code). konsertd dials
//     /ws/worker with its machine credential, as before.
//
// For ssh and local the hub is the one dialing: an attach loop per machine re-dials with
// backoff whenever the link drops, and the daemon re-sends its live state in
// worker.hello, so agent sessions (which live in the daemon) survive dropped links and
// hub restarts.
const (
	TransportSSH      = "ssh"
	TransportLocal    = "local"
	TransportOutbound = "outbound"
)

// transportFor is the transport of a machine with the given launch mode.
func transportFor(launch string) string {
	switch launch {
	case store.LaunchSSH:
		return TransportSSH
	case store.LaunchLocal:
		return TransportLocal
	}
	return TransportOutbound
}

type workerTransportKey struct{}

func withWorkerTransport(ctx context.Context, t string) context.Context {
	return context.WithValue(ctx, workerTransportKey{}, t)
}

func workerTransportFrom(ctx context.Context) string {
	if t, _ := ctx.Value(workerTransportKey{}).(string); t != "" {
		return t
	}
	return TransportOutbound
}

// --- the hub's identity and SSH key ---------------------------------------------

// HubKeyFileName is the hub's SSH private key, next to the database (0600); the public
// key is HubKeyFileName + ".pub".
const HubKeyFileName = "konsert-hub-ssh-key"

// hubIDSetting is the settings key of the hub's id, the marker of its authorized_keys
// lines (konsert-hub:<id>).
const hubIDSetting = "hub_id"

// hubKeyPath is where the hub key lives: next to the database, or "" without one
// (in-memory stores have nowhere to keep it).
func (s *Server) hubKeyPath() string {
	if s.hubKeyFile != "" {
		return s.hubKeyFile
	}
	s.cfgMu.RLock()
	db := strings.TrimSpace(s.cfg.DBPath)
	s.cfgMu.RUnlock()
	if db == "" || db == ":memory:" || strings.HasPrefix(db, "file::memory:") {
		return ""
	}
	return filepath.Join(filepath.Dir(db), HubKeyFileName)
}

// hubID returns the hub's id, creating it on first use.
func (s *Server) hubID(ctx context.Context) (string, error) {
	s.hubIDMu.Lock()
	defer s.hubIDMu.Unlock()
	saved, err := s.store.GetAllSettings(ctx)
	if err != nil {
		return "", err
	}
	if id := saved[hubIDSetting]; hubkey.ValidHubID(id) {
		return id, nil
	}
	id := ids.UUID()
	if err := s.store.SetSettings(ctx, map[string]string{hubIDSetting: id}); err != nil {
		return "", err
	}
	return id, nil
}

// hubKeyGrant returns the hub key bootstrap allows on SSH machines, generating the key
// the first time.
func (s *Server) hubKeyGrant(ctx context.Context) (*sshutil.HubKeyGrant, error) {
	path := s.hubKeyPath()
	if path == "" {
		return nil, errors.New("the hub has no database directory to keep its SSH key in")
	}
	id, err := s.hubID(ctx)
	if err != nil {
		return nil, err
	}
	pub, err := hubkey.LoadOrCreate(path, hubkey.Marker(id))
	if err != nil {
		return nil, err
	}
	return &sshutil.HubKeyGrant{HubID: id, PublicKey: pub}, nil
}

// --- attach loops -----------------------------------------------------------------

// attacher keeps one attach loop per ssh or local machine.
type attacher struct {
	s *Server

	// dialSSH and dialLocal open the byte stream to a machine's konsertd socket; tests
	// replace them. dialSSH's error carries ssh's own message when it fails early.
	dialSSH   func(ctx context.Context, target, keyPath, workerID string) (net.Conn, error)
	dialLocal func(ctx context.Context, workerID string) (net.Conn, error)

	minBackoff, maxBackoff time.Duration
	// watchEvery and watchAddrs override the wake / network watcher (tests).
	watchEvery time.Duration
	watchAddrs func() (string, error)
	watchOnce  sync.Once

	mu    sync.Mutex
	loops map[string]*attachLoop
}

func newAttacher(s *Server) *attacher {
	return &attacher{
		s: s,
		dialSSH: func(ctx context.Context, target, keyPath, workerID string) (net.Conn, error) {
			return sshutil.DialSSH(ctx, target, keyPath, workerID)
		},
		dialLocal: func(ctx context.Context, workerID string) (net.Conn, error) {
			path := worker.LocalSocketPath("", workerID)
			if path == "" {
				return nil, errors.New("no local socket path")
			}
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
		minBackoff: time.Second,
		maxBackoff: 30 * time.Second,
		loops:      map[string]*attachLoop{},
	}
}

// ensure starts w's attach loop if w uses the ssh or local transport and has none.
func (a *attacher) ensure(w store.Worker) {
	if t := transportFor(w.Launch); t != TransportSSH && t != TransportLocal {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.loops[w.ID]; ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	loop := &attachLoop{cancel: cancel, redial: make(chan struct{}, 1)}
	a.loops[w.ID] = loop
	if !a.s.hub.goBackground(func(hctx context.Context) {
		stop := context.AfterFunc(hctx, cancel)
		defer stop()
		defer func() {
			cancel()
			a.mu.Lock()
			if a.loops[w.ID] == loop {
				delete(a.loops, w.ID)
				a.s.hub.restarts.forget(w.ID)
			}
			a.mu.Unlock()
		}()
		a.run(ctx, w.ID, loop)
	}) {
		delete(a.loops, w.ID)
		cancel()
	}
}

type attachLoop struct {
	cancel context.CancelFunc
	// redial asks the loop to drop its link (or its wait) and dial again at once.
	redial chan struct{}
	// attemptAt is when the current dial attempt started (unix ns, wall clock); 0
	// while the loop waits between attempts.
	attemptAt atomic.Int64
}

// attemptStart is when the loop's current attempt started (zero: none under way).
func (l *attachLoop) attemptStart() time.Time {
	if ns := l.attemptAt.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return time.Time{}
}

// redialAll asks every attach loop whose link predates c to redial now: the hub's
// machine woke from sleep or its network changed, so SSH links are probably dead (local
// socket links are not affected, and a redial costs them nothing but a fresh
// worker.hello). A loop that already dialed again after the change (its link noticed
// the drop before the watcher's tick) is left alone, so it does not reconnect twice.
func (a *attacher) redialAll(c linkwatch.Change) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for id, l := range a.loops {
		if !c.Predates(l.attemptStart()) {
			a.s.logger.Debug(c.Reason+": link already re-dialed since the change", "worker_id", id)
			continue
		}
		n++
		select {
		case l.redial <- struct{}{}:
		default:
		}
	}
	if n > 0 {
		a.s.logger.Info(c.Reason + ": re-dialing machine links")
	}
}

// watch redials every link on a wake from sleep or a network change until ctx ends.
func (a *attacher) watch(ctx context.Context) {
	w := &linkwatch.Watcher{Every: a.watchEvery, Addrs: a.watchAddrs}
	w.Watch(ctx, a.redialAll)
}

// stop ends workerID's attach loop (and its link), if any.
func (a *attacher) stop(workerID string) {
	a.mu.Lock()
	loop := a.loops[workerID]
	delete(a.loops, workerID)
	a.mu.Unlock()
	a.s.hub.restarts.forget(workerID)
	if loop != nil {
		loop.cancel()
	}
}

// poke asks workerID's attach loop to dial now instead of waiting out its backoff (a
// start just ran, so the daemon is coming up).
func (a *attacher) poke(workerID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l := a.loops[workerID]; l != nil {
		select {
		case l.redial <- struct{}{}:
		default:
		}
	}
}

// running reports whether workerID has an attach loop.
func (a *attacher) running(workerID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.loops[workerID]
	return ok
}

// run attaches to workerID until ctx ends or the machine is removed or no longer uses
// a hub-dialed transport, re-dialing with backoff (reset after a link that lasted).
func (a *attacher) run(ctx context.Context, workerID string, loop *attachLoop) {
	redial := loop.redial
	backoff := a.minBackoff
	failures := 0
	for ctx.Err() == nil {
		// Drop any redial request that came in while nothing was up.
		select {
		case <-redial:
		default:
		}
		w, err := a.s.store.GetWorker(ctx, workerID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) || ctx.Err() != nil {
				return
			}
		} else {
			t := transportFor(w.Launch)
			if t != TransportSSH && t != TransportLocal {
				return
			}
			start := time.Now()
			loop.attemptAt.Store(start.UnixNano())
			attemptCtx, cancelAttempt := context.WithCancel(ctx)
			var redialed atomic.Bool
			attemptDone := make(chan struct{})
			go func() {
				select {
				case <-redial:
					redialed.Store(true)
					cancelAttempt()
				case <-attemptDone:
				}
			}()
			up, err := a.attachOnce(attemptCtx, w)
			loop.attemptAt.Store(0)
			close(attemptDone)
			cancelAttempt()
			if ctx.Err() != nil {
				return
			}
			if redialed.Load() {
				// Wake or network change: dial again at once.
				backoff, failures = a.minBackoff, 0
				continue
			}
			if up && time.Since(start) > 10*time.Second {
				backoff = a.minBackoff
			}
			// A daemon that is just starting refuses the first dials: warn once it has
			// failed a few times in a row, then stay quiet until it comes back.
			if !up {
				failures++
				// A daemon that is down on a reachable host is restarted (restart.go).
				a.s.onAttachFailed(ctx, w, t, err)
			}
			switch {
			case up:
				failures = 0
				a.s.logger.Info("link to "+w.Name+" dropped; re-dialing", "transport", t, "err", err)
			case failures == 3:
				a.s.logger.Warn("cannot attach to "+w.Name, "transport", t, "err", err)
			default:
				a.s.logger.Debug("cannot attach to "+w.Name, "transport", t, "err", err)
			}
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff/4+1)))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-redial:
			timer.Stop()
			backoff, failures = a.minBackoff, 0
			continue
		case <-timer.C:
		}
		backoff = min(backoff*2, a.maxBackoff)
	}
}

// attachOnce opens one link to w's konsertd and serves it until it ends. up reports
// whether the WebSocket was established.
func (a *attacher) attachOnce(ctx context.Context, w store.Worker) (up bool, err error) {
	t := transportFor(w.Launch)
	var conn net.Conn
	switch t {
	case TransportSSH:
		key := a.s.hubKeyPath()
		if key == "" {
			return false, errors.New("the hub has no SSH key")
		}
		conn, err = a.dialSSH(ctx, w.SSHTarget, key, w.ID)
	default:
		if conn, err = a.dialLocal(ctx, w.ID); err != nil {
			err = errDaemonSocket{err}
		}
	}
	if err != nil {
		return false, err
	}
	defer conn.Close()

	hubID, err := a.s.hubID(ctx)
	if err != nil {
		return false, err
	}
	var once sync.Once
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			var c net.Conn
			once.Do(func() { c = conn })
			if c == nil {
				return nil, errors.New("the link carries one connection")
			}
			return c, nil
		},
		DisableKeepAlives: true,
	}}
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	ws, _, err := websocket.Dial(dialCtx, "ws://konsertd"+protocol.AttachPath, &websocket.DialOptions{
		HTTPClient: hc,
		HTTPHeader: http.Header{protocol.HubIDHeader: []string{hubID}, protocol.TransportHeader: []string{t}},
	})
	cancel()
	if err != nil {
		return false, linkError(conn, err)
	}

	a.s.onAttached(ctx, w.ID)
	// The link is bound to this machine: a hello claiming another identity is refused.
	sctx := withBoundMachine(ctx, boundMachine{workerID: w.ID, via: t})
	sctx = withWorkerTransport(sctx, t)
	a.s.hub.ServeWorkerConn(sctx, ws)
	return true, linkError(conn, errors.New("link closed"))
}

// linkError prefers the ssh process's own message (e.g. "Permission denied
// (publickey)") over the generic stream error it caused.
func linkError(conn net.Conn, err error) error {
	if cc, ok := conn.(*sshutil.CommandConn); ok {
		_ = cc.Close()
		if msg := cc.Err(); msg != "" {
			return fmt.Errorf("%s", msg)
		}
	}
	return err
}

// startAttachers starts the attach loop of every ssh and local machine.
func (s *Server) startAttachers(ctx context.Context) {
	s.attach.watchOnce.Do(func() {
		s.hub.goBackground(s.attach.watch)
	})
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		s.logger.Warn("list machines to attach failed", "err", err)
		return
	}
	for _, w := range workers {
		s.attach.ensure(w)
	}
}
