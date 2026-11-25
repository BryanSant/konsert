package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store/sqlite"
)

func decodeWake(t *testing.T, env protocol.Envelope) protocol.HostWakeLockPayload {
	t.Helper()
	p, err := protocol.DecodePayload[protocol.HostWakeLockPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// expectWorkerWake waits for a fleet.update whose worker wake_lock satisfies pred.
func expectWorkerWake(t *testing.T, browser *fakeWorker, workerID string, pred func(*WorkerWakeLockView) bool) WorkerWakeLockView {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case env := <-browser.frames:
			if env.Type != protocol.MsgFleetUpdate {
				continue
			}
			var p struct {
				Kind   string           `json:"kind"`
				Worker WorkerStatusView `json:"worker"`
			}
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.Kind != "worker" || p.Worker.ID != workerID {
				continue
			}
			if pred(p.Worker.WakeLock) {
				return *p.Worker.WakeLock
			}
		case <-deadline:
			t.Fatalf("no fleet.update for %s with the expected wake_lock", workerID)
			return WorkerWakeLockView{}
		}
	}
}

func TestHubFleetWakeLockRoutesAndBroadcasts(t *testing.T) {
	th := newTestHub(t)
	browser := th.dialBrowser(t)
	w1 := th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	w2 := th.connectWorker(t, testWorkerInfo("w2", "beta"), nil, "w2")
	w1.send(protocol.MsgHostWakeLockState, protocol.HostWakeLockState{Method: "caffeinate"})
	w2.send(protocol.MsgHostWakeLockState, protocol.HostWakeLockState{Method: "systemd-inhibit"})
	expectWorkerWake(t, browser, "w1", func(v *WorkerWakeLockView) bool { return v != nil && v.Supported && !v.Active })
	// No desired state yet: the first report must not trigger a request.
	if got := w1.drain(protocol.MsgHostWakeLock, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("unexpected host.wakelock before any request: %d", len(got))
	}

	sent := th.h.SetFleetWakeLock(context.Background(), WakeLockDesire{Enabled: true, Reason: "night run"})
	if strings.Join(sent, ",") != "w1,w2" {
		t.Fatalf("sent to %v, want w1,w2", sent)
	}
	for _, w := range []*fakeWorker{w1, w2} {
		if p := decodeWake(t, w.expect(protocol.MsgHostWakeLock, 3*time.Second)); !p.Enabled || p.Reason != "night run" {
			t.Fatalf("payload = %+v", p)
		}
	}
	w1.send(protocol.MsgHostWakeLockState, protocol.HostWakeLockState{Active: true, Method: "caffeinate", Reason: "night run"})
	v := expectWorkerWake(t, browser, "w1", func(v *WorkerWakeLockView) bool { return v != nil && v.Active })
	if v.Desired == nil || !*v.Desired || v.Method != "caffeinate" {
		t.Fatalf("view = %+v", v)
	}

	// A per-host override wins over the fleet wish and reaches only that host.
	th.h.SetWorkerWakeLock(context.Background(), "w2", WakeLockDesire{Enabled: false})
	if p := decodeWake(t, w2.expect(protocol.MsgHostWakeLock, 3*time.Second)); p.Enabled {
		t.Fatalf("override payload = %+v", p)
	}
	if got := w1.drain(protocol.MsgHostWakeLock, 200*time.Millisecond); len(got) != 0 {
		t.Fatal("per-host override was sent to another worker")
	}
	res, err := th.svc.FleetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, wv := range res.Workers {
		if wv.WakeLock == nil || wv.WakeLock.Desired == nil {
			t.Fatalf("fleet status %s missing wake lock: %+v", wv.ID, wv.WakeLock)
		}
		if want := wv.ID == "w1"; *wv.WakeLock.Desired != want {
			t.Fatalf("%s desired = %v, want %v", wv.ID, *wv.WakeLock.Desired, want)
		}
	}
}

func TestHubReappliesWakeLockOnReconnect(t *testing.T) {
	th := newTestHub(t)
	w := th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	w.send(protocol.MsgHostWakeLockState, protocol.HostWakeLockState{Method: "caffeinate"})
	waitFor(t, 3*time.Second, "state report", func() bool {
		v := th.h.workerWakeLockView("w1")
		return v != nil && v.Supported
	})
	th.h.SetWorkerWakeLock(context.Background(), "w1", WakeLockDesire{Enabled: true, Reason: "r"})
	w.expect(protocol.MsgHostWakeLock, 3*time.Second)

	// The worker restarts (lock lost) and reconnects: the hub asks again.
	w.close()
	waitFor(t, 3*time.Second, "disconnect", func() bool { return !th.h.IsWorkerConnected("w1") })
	w2 := th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	w2.send(protocol.MsgHostWakeLockState, protocol.HostWakeLockState{Method: "caffeinate"})
	if p := decodeWake(t, w2.expect(protocol.MsgHostWakeLock, 3*time.Second)); !p.Enabled || p.Reason != "r" {
		t.Fatalf("re-applied payload = %+v", p)
	}
	// If the worker cannot take the lock, the hub does not retry in a loop.
	w2.send(protocol.MsgHostWakeLockState, protocol.HostWakeLockState{Method: "caffeinate", Error: "boom"})
	if got := w2.drain(protocol.MsgHostWakeLock, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("hub re-sent host.wakelock %d times after a failure report", len(got))
	}

	// A worker that reconnects still holding the lock (grace period) is left alone.
	w2.close()
	waitFor(t, 3*time.Second, "disconnect", func() bool { return !th.h.IsWorkerConnected("w1") })
	w3 := th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	w3.send(protocol.MsgHostWakeLockState, protocol.HostWakeLockState{Active: true, Method: "caffeinate"})
	if got := w3.drain(protocol.MsgHostWakeLock, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("hub re-sent an enable to a worker that already holds the lock")
	}
}

func TestWakeLockREST(t *testing.T) {
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	th := &testHub{h: srv.Hub(), st: st, svc: srv.Service(), ts: ts}
	t.Cleanup(th.stop)

	w := th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")

	post := func(path, body string, headers map[string]string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.UnmarshalRead(resp.Body, &out)
		return resp.StatusCode, out
	}
	jsonHdr := map[string]string{"Content-Type": "application/json", "X-Syre-Request": "1"}

	// CSRF guard: a form post never reaches the handler.
	if code, _ := post("/api/wakelock", `enabled=true`, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); code != http.StatusUnsupportedMediaType {
		t.Fatalf("form post status = %d", code)
	}
	if code, _ := post("/api/wakelock", `{}`, jsonHdr); code != http.StatusBadRequest {
		t.Fatalf("missing enabled status = %d", code)
	}

	code, out := post("/api/wakelock", `{"enabled":true}`, jsonHdr)
	if code != http.StatusOK {
		t.Fatalf("fleet enable status = %d %v", code, out)
	}
	if p := decodeWake(t, w.expect(protocol.MsgHostWakeLock, 3*time.Second)); !p.Enabled || p.Reason != defaultHostWakeReason {
		t.Fatalf("payload = %+v", p)
	}

	code, out = post("/api/workers/w1/wakelock", `{"enabled":false,"reason":"done"}`, jsonHdr)
	if code != http.StatusOK || out["sent"] != true {
		t.Fatalf("worker disable = %d %v", code, out)
	}
	if p := decodeWake(t, w.expect(protocol.MsgHostWakeLock, 3*time.Second)); p.Enabled {
		t.Fatalf("payload = %+v", p)
	}
	if code, _ := post("/api/workers/nope/wakelock", `{"enabled":true}`, jsonHdr); code != http.StatusNotFound {
		t.Fatalf("unknown worker status = %d", code)
	}

	resp, err := http.Get(ts.URL + "/api/wakelock")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status WakeLockStatus
	if err := json.UnmarshalRead(resp.Body, &status); err != nil {
		t.Fatal(err)
	}
	if status.Fleet == nil || !status.Fleet.Enabled || len(status.Workers) != 1 || !status.Workers[0].Online ||
		status.Workers[0].WorkerWakeLockView == nil || *status.Workers[0].Desired {
		t.Fatalf("status = %+v", status)
	}
}
