package controller

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/store/sqlite"
	"github.com/BryanSant/konsert/internal/worker"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

func TestVersionMismatch(t *testing.T) {
	cases := []struct {
		ctrl, worker string
		want         bool
	}{
		{"1.2.0", "1.2.0", false},
		{"v1.2.0", "1.2.0", false},
		{"1.2.0", " v1.2.0 ", false},
		{"1.2.0", "1.1.9", true},
		{"1.2.0", "", false},
		{"1.2.0", "dev", false},
		{"1.2.0", "DEV", false},
		{"dev", "1.2.0", false},
		{"", "1.2.0", false},
		{"dev", "dev", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := VersionMismatch(c.ctrl, c.worker); got != c.want {
			t.Errorf("VersionMismatch(%q, %q) = %v, want %v", c.ctrl, c.worker, got, c.want)
		}
	}
}

// TestWorkerVersionFlowsToFleetAPI runs a real konsertd daemon against a real
// Server and checks that the version from worker.hello reaches GET /api/fleet
// (and survives the heartbeat/facts re-broadcast paths via the store).
func TestWorkerVersionFlowsToFleetAPI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "konsert.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.RecoveryKey = "admin-tok"
	srv := NewServer(cfg, st, nil, "1.5.0")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	wCfg := config.DefaultWorkerConfig()
	wCfg.ControllerURL = "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/worker"
	wCfg.Credential = testMachineCredential(t, srv, "worker-ver-1")
	wCfg.WorkerID = "worker-ver-1"
	wCfg.Name = "ver-node"
	wCfg.EnableMockDriver = true
	daemon := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
	daemon.SetVersion("1.4.2")
	daemon.HeartbeatInterval = 200 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = daemon.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	waitForWorkerOnline(t, srv.Service(), "worker-ver-1")
	// Let at least one heartbeat re-read the row from the store.
	time.Sleep(400 * time.Millisecond)

	client := NewRemoteFleetClient(ts.URL, "admin-tok")
	res, err := client.FleetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.ControllerVersion != "1.5.0" {
		t.Errorf("controller_version = %q, want 1.5.0", res.ControllerVersion)
	}
	var found bool
	for _, w := range res.Workers {
		if w.ID == "worker-ver-1" {
			found = true
			if w.Version != "1.4.2" {
				t.Errorf("worker version = %q, want 1.4.2", w.Version)
			}
			if !VersionMismatch(res.ControllerVersion, w.Version) {
				t.Error("expected a version mismatch")
			}
		}
	}
	if !found {
		t.Fatal("worker missing from /api/fleet")
	}

	// The persisted version is what the store returns too.
	wk, err := st.GetWorker(ctx, "worker-ver-1")
	if err != nil || wk.Version != "1.4.2" {
		t.Errorf("stored version = %q err=%v", wk.Version, err)
	}
}
