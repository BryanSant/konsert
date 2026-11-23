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
		"--token", "flag-token",
		"--name", "builder",
		"--sandbox",
		"--sandbox-allow-shell",
		"--tag", "gpu=yes", "--tag", "zone = a ",
		"--allowed-roots", "/src,/work",
		"--max-concurrent", "4",
		"--no-linger", "--dry-run", // install-only flags: never persisted
	)
	cfg, extra, err := buildInstallConfig(fs, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControllerURL != "wss://hub.example/ws/worker" || cfg.FleetToken != "flag-token" || cfg.Name != "builder" ||
		!cfg.Sandbox || cfg.MaxConcurrent != 4 || cfg.Tags["gpu"] != "yes" || cfg.Tags["zone"] != "a" ||
		strings.Join(cfg.AllowedRoots, ",") != "/src,/work" {
		t.Errorf("cfg = %+v", cfg)
	}
	if strings.Join(extra, " ") != "--sandbox-allow-shell=true" {
		t.Errorf("extra args = %v", extra)
	}
	// Unset flags (e.g. --controller's default, --isolation) are not persisted.
	cfg2, _, _ := buildInstallConfig(parseInstall(t), func(string) string { return "" })
	if cfg2.ControllerURL != "" || cfg2.Isolation != "" || cfg2.MaxConcurrent != 0 {
		t.Errorf("defaults leaked into the persisted config: %+v", cfg2)
	}
}

func TestBuildInstallConfigMergesConfigFileAndEnvToken(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.yaml")
	if err := os.WriteFile(src, []byte("controller_url: wss://from-file/ws/worker\nname: filename\ntags:\n  a: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := parseInstall(t, "--config", src, "--name", "override", "--tag", "c=d")
	env := map[string]string{"KONSERT_FLEET_TOKEN": "env-token"}
	cfg, extra, err := buildInstallConfig(fs, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControllerURL != "wss://from-file/ws/worker" || cfg.Name != "override" || cfg.Tags["a"] != "b" || cfg.Tags["c"] != "d" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.FleetToken != "env-token" {
		t.Errorf("token = %q, want the KONSERT_FLEET_TOKEN value", cfg.FleetToken)
	}
	if len(extra) != 0 {
		t.Errorf("--config must not be passed through: %v", extra)
	}

	// An explicit --token wins over the environment.
	fs = parseInstall(t, "--token", "flag")
	cfg, _, _ = buildInstallConfig(fs, func(k string) string { return env[k] })
	if cfg.FleetToken != "flag" {
		t.Errorf("token = %q", cfg.FleetToken)
	}
}

func TestBuildInstallConfigRejectsBadTag(t *testing.T) {
	if _, _, err := buildInstallConfig(parseInstall(t, "--tag", "novalue"), func(string) string { return "" }); err == nil {
		t.Fatal("expected an error for --tag without =")
	}
}

func TestRedactToken(t *testing.T) {
	got := string(redactToken([]byte("controller_url: x\nfleet_token: s3cret\nname: y\n")))
	if strings.Contains(got, "s3cret") || !strings.Contains(got, "fleet_token: <redacted>") || !strings.Contains(got, "name: y") {
		t.Errorf("redactToken = %q", got)
	}
}

func TestValidateInstalledConfig(t *testing.T) {
	if err := validateInstalledConfig([]byte("controller_url: ws://h/ws/worker\n")); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	if err := validateInstalledConfig([]byte("isolation: bogus\n")); err == nil {
		t.Error("invalid isolation accepted")
	}
}

func TestOpenLogFileRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "konsertd.log")
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
