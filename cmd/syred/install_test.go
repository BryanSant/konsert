package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// parseInstall parses args with install's flag set (which includes every run flag).
func parseInstall(t *testing.T, args ...string) *pflag.FlagSet {
	t.Helper()
	cmd := newInstallCmd()
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return cmd.Flags()
}

func TestBuildInstallConfigPersistsRunFlags(t *testing.T) {
	fs := parseInstall(t,
		"--controller", "wss://hub.example/ws/worker",
		"--name", "builder",
		"--sandbox",
		"--sandbox-allow-shell",
		"--tag", "gpu=yes", "--tag", "zone = a ",
		"--allowed-roots", "/src,/work",
		"--max-concurrent", "4",
		"--no-linger", "--dry-run", // install-only flags: never persisted
	)
	cfg, extra, err := buildInstallConfig(fs)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControllerURL != "wss://hub.example/ws/worker" || cfg.Name != "builder" ||
		!cfg.Sandbox || cfg.MaxConcurrent != 4 || cfg.Tags["gpu"] != "yes" || cfg.Tags["zone"] != "a" ||
		strings.Join(cfg.AllowedRoots, ",") != "/src,/work" {
		t.Errorf("cfg = %+v", cfg)
	}
	if strings.Join(extra, " ") != "--sandbox-allow-shell=true" {
		t.Errorf("extra args = %v", extra)
	}
	// Unset flags (e.g. --controller's default, --isolation) are not persisted.
	cfg2, _, _ := buildInstallConfig(parseInstall(t))
	if cfg2.ControllerURL != "" || cfg2.Isolation != "" || cfg2.MaxConcurrent != 0 {
		t.Errorf("defaults leaked into the persisted config: %+v", cfg2)
	}
}

func TestBuildInstallConfigMergesConfigFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.yaml")
	if err := os.WriteFile(src, []byte("controller_url: wss://from-file/ws/worker\nname: filename\ntags:\n  a: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := parseInstall(t, "--config", src, "--name", "override", "--tag", "c=d")
	cfg, extra, err := buildInstallConfig(fs)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControllerURL != "wss://from-file/ws/worker" || cfg.Name != "override" || cfg.Tags["a"] != "b" || cfg.Tags["c"] != "d" {
		t.Errorf("cfg = %+v", cfg)
	}
	if len(extra) != 0 {
		t.Errorf("--config must not be passed through: %v", extra)
	}

	// --no-start is install's own flag, never persisted or passed through.
	cfg, extra, _ = buildInstallConfig(parseInstall(t, "--no-start"))
	if len(extra) != 0 || cfg.ControllerURL != "" {
		t.Errorf("--no-start leaked: cfg=%+v extra=%v", cfg, extra)
	}
	// The hub no longer hands credentials to install: the flag is gone.
	if err := newInstallCmd().Flags().Parse([]string{"--credential-stdin"}); err == nil {
		t.Error("--credential-stdin must not exist")
	}
}

func TestBuildInstallConfigRejectsBadTag(t *testing.T) {
	if _, _, err := buildInstallConfig(parseInstall(t, "--tag", "novalue")); err == nil {
		t.Fatal("expected an error for --tag without =")
	}
}

func TestValidateInstalledConfig(t *testing.T) {
	if cfg, err := installedRunConfig([]byte("controller_url: ws://h/ws/worker\n")); err != nil || cfg.MaxConcurrent != 2 || cfg.Name == "" {
		t.Errorf("valid config rejected or defaults missing: %+v %v", cfg, err)
	}
	if _, err := installedRunConfig([]byte("isolation: bogus\n")); err == nil {
		t.Error("invalid isolation accepted")
	}
}

func TestOpenLogFileRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "syred.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, logRotateSize+1), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if fi, err := os.Stat(path + ".1"); err != nil || fi.Size() != logRotateSize+1 {
		t.Errorf("old log not rotated: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Size() > 200 {
		t.Errorf("new log size = %d", fi.Size())
	}
}
