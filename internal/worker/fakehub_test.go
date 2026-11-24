package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/drivers"
	"github.com/coder/websocket"
)

// fakeHub is a minimal hub endpoint: each worker connection is handed to the test.
type fakeHub struct {
	srv    *httptest.Server
	conns  chan *hubConn
	reject atomic.Bool // when set, connection attempts get HTTP 503
}

// hubConn is one accepted worker connection with its decoded text envelopes.
type hubConn struct {
	conn *websocket.Conn
	msgs chan protocol.Envelope
	done chan struct{}
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{conns: make(chan *hubConn, 8)}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			if env, err := protocol.DecodeEnvelope(data); err == nil {
				select {
				case hc.msgs <- env:
				default:
				}
			}
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
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
