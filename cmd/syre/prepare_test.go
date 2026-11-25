package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/config"
	"gopkg.in/yaml.v3"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPrepareGeneratesAndPersistsTokensSilently(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultControllerConfig()
	cfg.DBPath = filepath.Join(dir, "syre.db")

	var logBuf bytes.Buffer
	if err := prepareServeConfig(&cfg, slog.New(slog.NewTextHandler(&logBuf, nil))); err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryKey == "" || !cfg.RecoveryKeyInTokensFile {
		t.Fatalf("expected a generated recovery key, got %q", cfg.RecoveryKey)
	}
	if cfg.ListenAddr != ":8420" {
		t.Fatalf("authenticated default listen = %q, want :8420", cfg.ListenAddr)
	}
	if strings.Contains(logBuf.String(), cfg.RecoveryKey) {
		t.Fatal("generated tokens must never be printed")
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
	if saved.RecoveryKey != cfg.RecoveryKey || !strings.Contains(string(raw), "recovery_key:") || strings.Contains(string(raw), "fleet") {
		t.Fatal("persisted tokens differ from the in-memory config")
	}

	// A second start reuses the persisted tokens.
	cfg2 := config.DefaultControllerConfig()
	cfg2.DBPath = cfg.DBPath
	if err := prepareServeConfig(&cfg2, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if cfg2.RecoveryKey != cfg.RecoveryKey || !cfg2.RecoveryKeyInTokensFile {
		t.Fatal("restart must reuse persisted tokens")
	}

	// Rotation rewrites only the recovery key.
	if err := rotateRecoveryKeyInFile(path, "new-key"); err != nil {
		t.Fatal(err)
	}
	if got, _ := readTokensFile(path); got.RecoveryKey != "new-key" {
		t.Fatalf("after rotation: %+v", got)
	}
}

func TestPrepareKeepsExplicitTokensAndFillsOnlyMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultControllerConfig()
	cfg.DBPath = filepath.Join(dir, "syre.db")
	cfg.RecoveryKey = "my-key"
	if err := prepareServeConfig(&cfg, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryKey != "my-key" || cfg.RecoveryKeyInTokensFile {
		t.Fatal("explicit recovery key must win and is not file-managed")
	}

	// Both set: no file is written.
	cfg2 := config.DefaultControllerConfig()
	cfg2.DBPath = filepath.Join(t.TempDir(), "syre.db")
	cfg2.RecoveryKey = "a"
	if err := prepareServeConfig(&cfg2, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg2.DBPath), tokensFileName)); !os.IsNotExist(err) {
		t.Fatal("no tokens file expected when both tokens are configured")
	}
}

func TestPrepareNoAuthDefaultsToLoopback(t *testing.T) {
	cfg := config.DefaultControllerConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "syre.db")
	cfg.NoAuth = true
	if err := prepareServeConfig(&cfg, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:8420" {
		t.Fatalf("--no-auth default listen = %q, want 127.0.0.1:8420", cfg.ListenAddr)
	}
	if cfg.RecoveryKey != "" {
		t.Fatal("--no-auth must not generate tokens")
	}

	// An explicit non-loopback bind is kept but warned about.
	var logBuf bytes.Buffer
	cfg.ListenAddr = ":8420"
	if err := prepareServeConfig(&cfg, slog.New(slog.NewTextHandler(&logBuf, nil))); err != nil {
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
