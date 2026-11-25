package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/coder/websocket"
)

func TestRunServeGracefulShutdownAndDBPermissions(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultControllerConfig()
	cfg.DBPath = filepath.Join(dir, "syre.db")
	cfg.NoAuth = true
	cfg.ListenAddr = "127.0.0.1:0"

	// Simulate a world-readable DB left by an older build.
	if err := os.WriteFile(cfg.DBPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrCh := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, discardLogger(), func(a net.Addr) { addrCh <- a }) }()

	var addr net.Addr
	select {
	case addr = <-addrCh:
	case err := <-done:
		t.Fatalf("runServe exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}
	base := "http://" + addr.String()

	resp, err := http.Get(base + "/api/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %v %v", resp, err)
	}
	_ = resp.Body.Close()

	for _, p := range []string{cfg.DBPath, cfg.DBPath + "-wal", cfg.DBPath + "-shm"} {
		info, err := os.Stat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(p), perm)
		}
	}

	// A connected browser socket (a hijacked handler) must not block shutdown.
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wsCancel()
	conn, _, err := websocket.Dial(wsCtx, "ws://"+addr.String()+"/ws/client", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.CloseNow()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServe returned %v", err)
		}
	case <-time.After(shutdownTimeout + 5*time.Second):
		t.Fatal("runServe did not shut down")
	}
	if _, err := http.Get(base + "/api/health"); err == nil {
		t.Fatal("listener should be closed after shutdown")
	}
}

func TestPrepareDBFileCreatesPrivateFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "k.db")
	if err := prepareDBFile(p); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	// Idempotent on an existing file.
	if err := prepareDBFile(p); err != nil {
		t.Fatal(err)
	}
}
