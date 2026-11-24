package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/config"
	"gopkg.in/yaml.v3"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPrepareGeneratesAndPersistsTokensWhenUnset(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultControllerConfig()
	cfg.DBPath = filepath.Join(dir, "konsert.db")

	var out bytes.Buffer
	if err := prepareServeConfig(&cfg, &out, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if cfg.AdminToken == "" || cfg.FleetToken == "" {
		t.Fatalf("expected generated tokens, got admin=%q fleet=%q", cfg.AdminToken, cfg.FleetToken)
	}
	if cfg.ListenAddr != ":8420" {
		t.Fatalf("authenticated default listen = %q, want :8420", cfg.ListenAddr)
	}
	if !strings.Contains(out.String(), cfg.AdminToken) || !strings.Contains(out.String(), cfg.FleetToken) {
		t.Fatal("generated tokens must be printed once")
	}

	path := filepath.Join(dir, tokensFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("tokens file mode = %o, want 600", perm)
	}
	var saved persistedTokens
	raw, _ := os.ReadFile(path)
	if err := yaml.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.AdminToken != cfg.AdminToken || saved.FleetToken != cfg.FleetToken {
		t.Fatal("persisted tokens differ from the in-memory config")
	}

	// A second start reuses the persisted tokens and prints nothing.
	cfg2 := config.DefaultControllerConfig()
	cfg2.DBPath = cfg.DBPath
	out.Reset()
	if err := prepareServeConfig(&cfg2, &out, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if cfg2.AdminToken != cfg.AdminToken || cfg2.FleetToken != cfg.FleetToken {
		t.Fatal("restart must reuse persisted tokens")
	}
	if out.Len() != 0 {
		t.Fatalf("tokens must not be printed again, got %q", out.String())
	}
}

func TestPrepareKeepsExplicitTokensAndFillsOnlyMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultControllerConfig()
	cfg.DBPath = filepath.Join(dir, "konsert.db")
	cfg.AdminToken = "my-admin"
	var out bytes.Buffer
	if err := prepareServeConfig(&cfg, &out, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if cfg.AdminToken != "my-admin" {
		t.Fatal("explicit admin token must win")
	}
	if cfg.FleetToken == "" {
		t.Fatal("missing fleet token must be generated")
	}
	if strings.Contains(out.String(), "my-admin") {
		t.Fatal("explicit token must not be printed")
	}

	// Both set: no file is written.
	cfg2 := config.DefaultControllerConfig()
	cfg2.DBPath = filepath.Join(t.TempDir(), "konsert.db")
	cfg2.AdminToken, cfg2.FleetToken = "a", "f"
	if err := prepareServeConfig(&cfg2, &out, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg2.DBPath), tokensFileName)); !os.IsNotExist(err) {
		t.Fatal("no tokens file expected when both tokens are configured")
	}
}

func TestPrepareNoAuthDefaultsToLoopback(t *testing.T) {
	cfg := config.DefaultControllerConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "konsert.db")
	cfg.NoAuth = true
	var out bytes.Buffer
	if err := prepareServeConfig(&cfg, &out, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:8420" {
		t.Fatalf("--no-auth default listen = %q, want 127.0.0.1:8420", cfg.ListenAddr)
	}
	if cfg.AdminToken != "" || cfg.FleetToken != "" {
		t.Fatal("--no-auth must not generate tokens")
	}

	// An explicit non-loopback bind is kept but warned about.
	var logBuf bytes.Buffer
	cfg.ListenAddr = ":8420"
	if err := prepareServeConfig(&cfg, &out, slog.New(slog.NewTextHandler(&logBuf, nil))); err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8420" || !strings.Contains(logBuf.String(), "--no-auth on :8420") {
		t.Fatalf("expected loud warning for non-loopback --no-auth, got %q", logBuf.String())
	}
}

func TestIsLoopbackListenAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8420": true,
		"[::1]:8420":     true,
		"localhost:8420": true,
		":8420":          false,
		"0.0.0.0:8420":   false,
		"10.0.0.5:8420":  false,
	}
	for addr, want := range cases {
		if got := isLoopbackListenAddr(addr); got != want {
			t.Errorf("isLoopbackListenAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}
