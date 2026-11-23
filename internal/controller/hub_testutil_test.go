package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/BryanSant/konsert/internal/store/sqlite"
	"github.com/coder/websocket"
)

// testHub is a Hub backed by a real SQLite store and served over httptest so
// fake workers and browser clients can talk to it through real WebSockets.
type testHub struct {
	h      *Hub
	st     *sqlite.SQLiteStore
	svc    *FleetService
	ts     *httptest.Server
	dbPath string
}

func newTestHub(t *testing.T) *testHub {
	t.Helper()
	return newTestHubAt(t, filepath.Join(t.TempDir(), "hub.db"))
}

func newTestHubAt(t *testing.T, dbPath string) *testHub {
	t.Helper()
	st, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHub(st, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/worker", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		h.ServeWorkerConn(r.Context(), conn)
	})
	mux.HandleFunc("/ws/client", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		h.ServeBrowserConn(r.Context(), conn, "tester")
	})
	th := &testHub{h: h, st: st, svc: NewFleetService(st, h), ts: httptest.NewServer(mux), dbPath: dbPath}
	t.Cleanup(th.stop)
	return th
}

func (th *testHub) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = th.h.Shutdown(ctx)
	th.ts.Close()
	_ = th.st.Close()
}

func (th *testHub) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(th.ts.URL, "http") + path
}

// fakeWorker is a minimal konsertd stand-in speaking the worker protocol.
type fakeWorker struct {
	t      *testing.T
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	frames chan protocol.Envelope
	done   chan struct{}

	mu       sync.Mutex
	closeErr error
}

func testWorkerInfo(id, name string) protocol.WorkerInfo {
	return protocol.WorkerInfo{
		ID:            id,
		Name:          name,
		HostGroup:     name,
		OS:            "linux",
		Arch:          "amd64",
		Isolation:     "host",
		Agents:        []string{"mock"},
		MaxConcurrent: 4,
	}
}

// dialWorker connects and sends worker.hello without waiting for registration.
func (th *testHub) dialWorker(t *testing.T, info protocol.WorkerInfo, active []protocol.ActiveTaskInfo) *fakeWorker {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, _, err := websocket.Dial(ctx, th.wsURL("/ws/worker"), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	conn.SetReadLimit(20 * 1024 * 1024)
	fw := &fakeWorker{t: t, conn: conn, ctx: ctx, cancel: cancel, frames: make(chan protocol.Envelope, 1024), done: make(chan struct{})}
	go func() {
		defer close(fw.done)
		for {
			mt, data, err := conn.Read(ctx)
			if err != nil {
				fw.mu.Lock()
				fw.closeErr = err
				fw.mu.Unlock()
				return
			}
			if mt != websocket.MessageText {
				continue
			}
			env, err := protocol.DecodeEnvelope(data)
			if err != nil {
				continue
			}
			select {
			case fw.frames <- env:
			default:
			}
		}
	}()
	t.Cleanup(fw.close)
	fw.send(protocol.MsgWorkerHello, protocol.WorkerHelloPayload{Worker: info, ActiveTasks: active})
	return fw
}

// connectWorker dials and waits until the hub registers the worker under wantID.
func (th *testHub) connectWorker(t *testing.T, info protocol.WorkerInfo, active []protocol.ActiveTaskInfo, wantID string) *fakeWorker {
	t.Helper()
	fw := th.dialWorker(t, info, active)
	waitFor(t, 5*time.Second, "worker "+wantID+" to connect", func() bool { return th.h.IsWorkerConnected(wantID) })
	return fw
}

func (fw *fakeWorker) send(msgType string, payload any) {
	fw.t.Helper()
	raw, err := protocol.NewEnvelope(msgType, "", payload)
	if err != nil {
		fw.t.Fatal(err)
	}
	if err := fw.conn.Write(fw.ctx, websocket.MessageText, raw); err != nil {
		fw.t.Logf("fake worker write %s: %v", msgType, err)
	}
}

// expect returns the next frame of msgType, skipping others, or fails after timeout.
func (fw *fakeWorker) expect(msgType string, timeout time.Duration) protocol.Envelope {
	fw.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case env := <-fw.frames:
			if env.Type == msgType {
				return env
			}
		case <-deadline:
			fw.t.Fatalf("timed out waiting for %s", msgType)
			return protocol.Envelope{}
		}
	}
}

// drain collects frames of msgType arriving within d.
func (fw *fakeWorker) drain(msgType string, d time.Duration) []protocol.Envelope {
	var out []protocol.Envelope
	deadline := time.After(d)
	for {
		select {
		case env := <-fw.frames:
			if env.Type == msgType {
				out = append(out, env)
			}
		case <-deadline:
			return out
		}
	}
}

// waitClosed waits for the hub to close the connection and returns the close status.
func (fw *fakeWorker) waitClosed(timeout time.Duration) websocket.StatusCode {
	fw.t.Helper()
	select {
	case <-fw.done:
	case <-time.After(timeout):
		fw.t.Fatal("timed out waiting for hub to close the worker connection")
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return websocket.CloseStatus(fw.closeErr)
}

func (fw *fakeWorker) close() {
	_ = fw.conn.Close(websocket.StatusNormalClosure, "")
	fw.cancel()
}

func startedTaskIDs(t *testing.T, envs []protocol.Envelope) []string {
	t.Helper()
	ids := make([]string, 0, len(envs))
	for _, env := range envs {
		p, err := protocol.DecodePayload[protocol.TaskStartPayload](env)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.TaskID)
	}
	return ids
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mustTask(t *testing.T, st store.Store, id string) store.Task {
	t.Helper()
	task, err := st.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}
