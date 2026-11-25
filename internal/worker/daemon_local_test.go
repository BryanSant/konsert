package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/drivers"
	"github.com/coder/websocket"
)

const localTestID = "01890a5d-ac96-774b-bcce-b302099a8057"

// shortSocketPath returns a socket path short enough for sun_path (t.TempDir can exceed
// macOS's 104 bytes).
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ks")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "run", "d.sock")
}

// startLocalDaemon runs a daemon without a controller URL on a short socket path.
func startLocalDaemon(t *testing.T) (*Daemon, string, chan error) {
	t.Helper()
	return startLocalDaemonWith(t, nil)
}

// startLocalDaemonWith is startLocalDaemon with setup applied before the daemon runs.
func startLocalDaemonWith(t *testing.T, setup func(*Daemon)) (*Daemon, string, chan error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("AF_UNIX tests run on POSIX")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv(StateDirEnv, t.TempDir())
	cfg := config.WorkerConfig{WorkerID: localTestID, Name: "local", EnableMockDriver: true, Tags: map[string]string{}}
	d := NewDaemon(cfg, drivers.DefaultRegistry, nil)
	d.HeartbeatInterval = time.Hour
	d.FactsInterval = time.Hour
	d.SocketPath = shortSocketPath(t)
	if setup != nil {
		setup(d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		done <- d.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(15 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	waitUntil(t, 5*time.Second, "socket", func() bool { _, err := os.Stat(d.SocketPath); return err == nil })
	return d, d.SocketPath, done
}

// attachLocal attaches to the daemon at path like the hub does (a WebSocket over the
// socket's byte stream) and collects its text envelopes.
func attachLocal(t *testing.T, path, hubID string) *hubConn {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
		DisableKeepAlives: true,
	}}
	h := http.Header{}
	if hubID != "" {
		h.Set(protocol.HubIDHeader, hubID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://konsertd"+protocol.AttachPath, &websocket.DialOptions{HTTPClient: hc, HTTPHeader: h})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	c.SetReadLimit(64 << 20)
	conn := &hubConn{conn: c, msgs: make(chan protocol.Envelope, 1024), done: make(chan struct{})}
	go func() {
		defer close(conn.done)
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			if env, err := protocol.DecodeEnvelope(data); err == nil {
				if env.Type == protocol.MsgWorkerHello {
					resumeSpool(c, env)
				}
				select {
				case conn.msgs <- env:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() { _ = c.CloseNow() })
	return conn
}

func helloOf(t *testing.T, c *hubConn) protocol.WorkerHelloPayload {
	t.Helper()
	env := c.waitFor(t, 10*time.Second, "worker.hello", func(protocol.Envelope) bool { return true })
	if env.Type != protocol.MsgWorkerHello {
		t.Fatalf("first message %s, want worker.hello", env.Type)
	}
	h, err := protocol.DecodePayload[protocol.WorkerHelloPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Without a controller URL the daemon serves the hub on its socket. A task keeps running
// when the link drops, and the next attach reports it in worker.hello (the hub resyncs).
func TestLocalDaemonKeepsTasksAcrossAttaches(t *testing.T) {
	_, path, _ := startLocalDaemon(t)
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir must be private: %v %v", fi.Mode(), err)
	}
	c1 := attachLocal(t, path, "")
	if h := helloOf(t, c1); h.Worker.ID != localTestID {
		t.Fatalf("hello id %s", h.Worker.ID)
	}
	const id = "t-local1"
	c1.send(t, protocol.MsgTaskStart, "", protocol.TaskStartPayload{
		TaskID: id, Agent: "mock", Workdir: t.TempDir(), Prompt: "WAIT_INPUT", Mode: protocol.TaskModeInteractive,
	})
	c1.waitFor(t, 10*time.Second, "running", func(e protocol.Envelope) bool {
		p, err := protocol.DecodePayload[protocol.TaskStatusPayload](e)
		return e.Type == protocol.MsgTaskStatus && err == nil && p.TaskID == id && p.Status == protocol.TaskStatusRunning
	})

	// The link drops without a close handshake (a killed ssh).
	_ = c1.conn.CloseNow()
	<-c1.done

	c2 := attachLocal(t, path, "")
	h := helloOf(t, c2)
	found := false
	for _, at := range h.ActiveTasks {
		found = found || at.TaskID == id
	}
	if !found {
		t.Fatalf("task not reported after re-attach: %+v", h.ActiveTasks)
	}
	// It is still alive: steer it to exit and get its final status on the new link.
	c2.send(t, protocol.MsgTaskSteer, "", protocol.TaskSteerPayload{TaskID: id, InputText: "exit", AppendNewline: true})
	c2.waitFor(t, 10*time.Second, "final status", func(e protocol.Envelope) bool {
		p, err := protocol.DecodePayload[protocol.TaskStatusPayload](e)
		return e.Type == protocol.MsgTaskStatus && err == nil && p.TaskID == id && protocol.IsTerminalTaskStatus(p.Status)
	})
}

// A newer attach replaces an older one (a half-open link the hub gave up on).
func TestLocalDaemonNewerAttachReplacesOlder(t *testing.T) {
	_, path, _ := startLocalDaemon(t)
	c1 := attachLocal(t, path, "")
	helloOf(t, c1)
	c2 := attachLocal(t, path, "")
	helloOf(t, c2)
	select {
	case <-c1.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the older attach was not closed")
	}
	select {
	case <-c2.done:
		t.Fatal("the newer attach must stay up")
	case <-time.After(200 * time.Millisecond):
	}
}

// Removal (close 4001) records the removal and uninstalls this identity, passing the
// removing hub's id (its key grant goes too), then stops the daemon.
func TestLocalDaemonRemovalUninstallsItself(t *testing.T) {
	type call struct{ hubID string }
	calls := make(chan call, 2)
	_, path, done := startLocalDaemonWith(t, func(d *Daemon) {
		d.selfRemove = func(_ context.Context, hubID string) (bool, error) {
			calls <- call{hubID}
			return false, nil
		}
	})
	c := attachLocal(t, path, "hub1")
	helloOf(t, c)
	_ = c.conn.Close(websocket.StatusCode(protocol.WSCloseWorkerRemoved), "worker removed from fleet")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after removal = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not stop after removal")
	}
	select {
	case got := <-calls:
		if got.hubID != "hub1" {
			t.Fatalf("self-removal for hub %q, want hub1", got.hubID)
		}
	default:
		t.Fatal("the daemon did not uninstall itself on 4001")
	}
	if hub, ok := ReadRemovedMarker("", localTestID); !ok || hub != "hub1" {
		t.Fatalf("removed marker = %q, %v; want hub1 (the fake removal leaves it)", hub, ok)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind: %v", err)
	}
	ClearRemovedMarker("", localTestID)
}

// A daemon that starts with a removal marker (it was stopped mid-removal) finishes the
// removal instead of serving.
func TestLocalDaemonFinishesInterruptedRemoval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AF_UNIX tests run on POSIX")
	}
	if err := WriteRemovedMarker("", localTestID, "hub9"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ClearRemovedMarker("", localTestID) })
	cfg := config.WorkerConfig{WorkerID: localTestID, Name: "local", Tags: map[string]string{}}
	d := NewDaemon(cfg, drivers.DefaultRegistry, nil)
	d.SocketPath = shortSocketPath(t)
	var got string
	d.selfRemove = func(_ context.Context, hubID string) (bool, error) {
		got = hubID
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got != "hub9" {
		t.Fatalf("finished removal for hub %q, want hub9", got)
	}
	if _, err := os.Stat(d.SocketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a removed daemon must not listen")
	}
}

// A second daemon with the same identity does not steal a live socket; a stale socket
// file is replaced.
func TestListenLocalRefusesLiveSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX")
	}
	path := shortSocketPath(t)
	ln, err := listenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := listenLocal(path); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("second listen: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket file should remain for this test: %v", err)
	}
	ln2, err := listenLocal(path)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	_ = ln2.Close()
	if _, err := listenLocal(strings.Repeat("x", 200) + "/d.sock"); err == nil {
		t.Fatal("an overlong path must be refused")
	}
}

func TestAttachRequest(t *testing.T) {
	const a, b = "01890a5d-ac96-774b-bcce-b302099a8057", "01890a5d-ac96-774b-bcce-b302099a8058"
	cases := []struct {
		req   string
		allow []string
		want  string
		err   bool
	}{
		{"konsertd attach " + a, []string{a, b}, a, false},
		{"konsertd attach " + strings.ToUpper(b), []string{a, b}, b, false},
		{a, nil, a, false},
		{"konsertd attach " + b, []string{a}, "", true},
		{"", []string{a}, a, false},
		{"konsertd attach", []string{a}, a, false},
		{"konsertd attach ../../etc/passwd", []string{a}, a, false},
		{"", []string{a, b}, "", true},
		{"", nil, "", true},
	}
	for _, c := range cases {
		got, err := AttachRequest(c.req, c.allow)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("AttachRequest(%q, %v) = %q, %v", c.req, c.allow, got, err)
		}
	}
}

// Attach copies both directions and ends when the daemon side closes.
func TestAttachBridgesStdio(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX")
	}
	path := shortSocketPath(t)
	ln, err := listenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 5)
		_, _ = io.ReadFull(c, buf)
		_, _ = c.Write(append([]byte("echo:"), buf...))
		_ = c.Close()
	}()
	var out bytes.Buffer
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("hello")) }()
	if err := Attach(context.Background(), path, pr, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "echo:hello" {
		t.Fatalf("out = %q", out.String())
	}
	if err := Attach(context.Background(), path+".missing", strings.NewReader(""), io.Discard); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("missing socket: %v", err)
	}
}

// resumeSpool answers worker.hello like the hub: resume the spool from the start (a
// test hub applies everything it is sent).
func resumeSpool(c *websocket.Conn, env protocol.Envelope) {
	hello, err := protocol.DecodePayload[protocol.WorkerHelloPayload](env)
	if err != nil || hello.Spool == nil {
		return
	}
	if raw, err := protocol.NewEnvelope(protocol.MsgWorkerSpoolResume, "", protocol.SpoolResume{Epoch: hello.Spool.Epoch}); err == nil {
		_ = c.Write(context.Background(), websocket.MessageText, raw)
	}
}
