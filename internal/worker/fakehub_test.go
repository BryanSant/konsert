package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/drivers"
	"github.com/coder/websocket"
)

// fakeHub is a minimal hub endpoint: each worker connection is handed to the test.
type fakeHub struct {
	srv    *httptest.Server
	conns  chan *hubConn
	reject atomic.Bool // when set, connection attempts get HTTP 503
	// silent: new connections go silent after worker.hello (the hub stops reading and
	// writing, as a half-open link or a stopped hub process would), until the test ends.
	silent atomic.Bool
	// pingEvery, when set, makes the hub ping every connection that often (as the real
	// hub does when it is quiet).
	pingEvery atomic.Int64 // nanoseconds
	// noResume: the hub never answers worker.hello's spool section (spooled messages are
	// then never sent).
	noResume atomic.Bool
	// applied is the highest spool seq received (the resume point of the next connection,
	// as the real hub tracks it); resumeFrom, when set, overrides it once.
	applied    atomic.Uint64
	resumeFrom atomic.Int64
	stop       chan struct{}
}

// hubConn is one accepted worker connection with its decoded text envelopes.
type hubConn struct {
	conn *websocket.Conn
	msgs chan protocol.Envelope
	done chan struct{}
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{conns: make(chan *hubConn, 8), stop: make(chan struct{})}
	h.resumeFrom.Store(-1)
	h.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.reject.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(64 << 20)
		hc := &hubConn{conn: c, msgs: make(chan protocol.Envelope, 1024), done: make(chan struct{})}
		h.conns <- hc
		defer close(hc.done)
		if h.silent.Load() {
			if _, data, err := c.Read(context.Background()); err == nil {
				if env, err := protocol.DecodeEnvelope(data); err == nil {
					hc.msgs <- env
				}
			}
			<-h.stop
			return
		}
		if every := time.Duration(h.pingEvery.Load()); every > 0 {
			pctx, pcancel := context.WithCancel(context.Background())
			defer pcancel()
			go func() {
				t := time.NewTicker(every)
				defer t.Stop()
				for {
					select {
					case <-pctx.Done():
						return
					case <-t.C:
						ctx, cancel := context.WithTimeout(pctx, time.Second)
						_ = c.Ping(ctx)
						cancel()
					}
				}
			}()
		}
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			if env, err := protocol.DecodeEnvelope(data); err == nil {
				if env.Type == protocol.MsgWorkerHello && !h.noResume.Load() {
					h.resume(c, env)
				}
				if env.Seq != 0 {
					for {
						cur := h.applied.Load()
						if env.Seq <= cur || h.applied.CompareAndSwap(cur, env.Seq) {
							break
						}
					}
				}
				select {
				case hc.msgs <- env:
				default:
				}
			}
		}
	}))
	h.srv.Start()
	t.Cleanup(func() {
		close(h.stop)
		h.srv.CloseClientConnections()
		h.srv.Close()
	})
	return h
}

// resume answers worker.hello's spool section like the real hub.
func (h *fakeHub) resume(c *websocket.Conn, env protocol.Envelope) {
	hello, err := protocol.DecodePayload[protocol.WorkerHelloPayload](env)
	if err != nil || hello.Spool == nil {
		return
	}
	applied := h.applied.Load()
	if from := h.resumeFrom.Swap(-1); from >= 0 {
		applied = uint64(from)
	}
	raw, err := protocol.NewEnvelope(protocol.MsgWorkerSpoolResume, "", protocol.SpoolResume{Epoch: hello.Spool.Epoch, AppliedSeq: applied})
	if err == nil {
		_ = c.Write(context.Background(), websocket.MessageText, raw)
	}
}

func (h *fakeHub) wsURL() string {
	return "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/ws/worker"
}

func (h *fakeHub) accept(t *testing.T) *hubConn {
	t.Helper()
	select {
	case c := <-h.conns:
		return c
	case <-time.After(10 * time.Second):
		t.Fatal("worker never connected to fake hub")
		return nil
	}
}

func (c *hubConn) send(t *testing.T, msgType, id string, payload any) {
	t.Helper()
	raw, err := protocol.NewEnvelope(msgType, id, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatalf("hub write %s: %v", msgType, err)
	}
}

// waitFor returns the first envelope matching pred, failing after timeout.
func (c *hubConn) waitFor(t *testing.T, timeout time.Duration, what string, pred func(protocol.Envelope) bool) protocol.Envelope {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case env := <-c.msgs:
			if pred(env) {
				return env
			}
		case <-deadline:
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
			return protocol.Envelope{}
		}
	}
}

func isType(msgType string) func(protocol.Envelope) bool {
	return func(e protocol.Envelope) bool { return e.Type == msgType }
}

// startTestDaemon runs a Daemon against the fake hub with a private HOME.
func startTestDaemon(t *testing.T, hub *fakeHub, mutate func(*config.WorkerConfig)) (*Daemon, context.CancelFunc) {
	t.Helper()
	return startTestDaemonWith(t, hub, mutate, nil)
}

// startTestDaemonWith is startTestDaemon with a hook to adjust the daemon before it runs.
func startTestDaemonWith(t *testing.T, hub *fakeHub, mutate func(*config.WorkerConfig), prepare func(*Daemon)) (*Daemon, context.CancelFunc) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	// A private state directory: each test daemon has its own spool.
	t.Setenv(StateDirEnv, t.TempDir())
	cfg := config.WorkerConfig{
		ControllerURL:    hub.wsURL(),
		WorkerID:         "test-worker",
		Name:             "test-worker",
		EnableMockDriver: true,
		Tags:             map[string]string{},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	if len(cfg.CheckoutRoots) == 0 {
		cfg.CheckoutRoots = []string{t.TempDir()}
	}
	d := NewDaemon(cfg, drivers.DefaultRegistry, nil)
	d.HeartbeatInterval = time.Hour
	d.FactsInterval = time.Hour
	if prepare != nil {
		prepare(d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	return d, cancel
}
