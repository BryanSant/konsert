package worker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/install"
)

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// populatePerIdentityFiles creates all per-identity state files for workerID.
func populatePerIdentityFiles(t *testing.T, stateDir, workerID string) []string {
	t.Helper()
	files := []string{
		CredentialPath(stateDir, workerID),
		LinkStatusPath(stateDir, workerID),
		LocalSocketPath(stateDir, workerID),
		filepath.Join(spoolDir(stateDir, workerID), "meta.json"),
		filepath.Join(scrollbackDir(stateDir, workerID), "task1.log"),
	}
	for _, f := range files {
		mustWriteFile(t, f, "data")
	}
	return files
}

// populateSharedFiles creates all shared syred files (hooks, hook-events.jsonl,
// orphaned spool, usage cache, shell-integration, and managed binary).
func populateSharedFiles(t *testing.T, home, stateDir, managedBin string) {
	t.Helper()
	mustWriteFile(t, managedBin, "#!/bin/sh\n")
	mustWriteFile(t, managedBin+".old", "old")
	mustWriteFile(t, filepath.Join(stateDir, "hook-events.jsonl"), "{}\n")
	mustWriteFile(t, filepath.Join(stateDir, "usage-cache.json"), "{}\n")
	mustWriteFile(t, filepath.Join(stateDir, "shell-integration", "init.sh"), "# sh\n")
	mustWriteFile(t, filepath.Join(stateDir, "spool", "spool-orphaned-hub1-20261124T000000Z", "meta.json"), "{}\n")
	// Pre-existing Claude settings with a user setting that must survive hook removal.
	claudeSettings := filepath.Join(home, ".claude", "settings.json")
	mustWriteFile(t, claudeSettings, "{\n  \"theme\": \"dark\"\n}\n")
	if _, err := InstallAgentHooks(home, managedBin); err != nil {
		t.Fatalf("InstallAgentHooks: %v", err)
	}
}

// Single identity: uninstall removes everything syred created (service, config, logs,
// identity file, per-identity state, shared files, hooks, managed binary, empty dirs)
// while preserving non-Syre entries in ~/.claude/settings.json.
func TestRemoveIdentitySingleIdentityRemovesEverything(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	stateDir := filepath.Join(home, ".syre")
	cfgDir := filepath.Join(home, ".config", "syre")
	logDir := filepath.Join(home, "Library", "Logs", "syre")
	unitDir := filepath.Join(home, "Library", "LaunchAgents")
	managedBin := filepath.Join(home, ".local", "bin", "syred")

	a := &sharedAccount{
		t:        t,
		state:    stateDir,
		runner:   &recRunner{},
		procArgs: map[int]string{},
	}
	a.in = &install.Installer{
		Runner: a.runner,
		Out:    &bytes.Buffer{},
		Spawn:  func(string, ...string) error { return nil },
		Env: install.Env{
			Home:       home,
			ConfigDir:  cfgDir,
			LogDir:     logDir,
			UnitDir:    unitDir,
			ManagedBin: managedBin,
			UID:        501,
			User:       "me",
		},
	}

	id := a.identity("solo")
	cfgPath := a.service("", "solo")
	logPath := a.in.LogPath("")
	mustWriteFile(t, logPath, "log\n")
	mustWriteFile(t, logPath+".1", "old log\n")
	perIDFiles := populatePerIdentityFiles(t, stateDir, id)
	populateSharedFiles(t, home, stateDir, managedBin)

	u := &Uninstaller{
		Installer:    a.in,
		StateDir:     stateDir,
		LockWait:     300 * time.Millisecond,
		DropHubGrant: func(string, string) (string, bool, error) { return "", false, nil },
		Executable:   func() (string, error) { return managedBin, nil },
		OtherRunning: func(string) (bool, error) { return false, nil },
		ServiceConfigs: func() ([]install.InstalledConfig, error) {
			return nil, nil
		},
	}

	rep, err := u.RemoveIdentity(context.Background(), id, "hub1")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Found {
		t.Fatalf("expected Found=true, rep=%+v", rep)
	}

	// Everything syred created must be gone.
	mustBeGone := append([]string{
		cfgPath,
		logPath,
		logPath + ".1",
		filepath.Join(stateDir, workerIDFilename("solo", "")),
		filepath.Join(stateDir, "hook-events.jsonl"),
		filepath.Join(stateDir, "usage-cache.json"),
		filepath.Join(stateDir, "shell-integration"),
		filepath.Join(stateDir, "spool"),
		filepath.Join(stateDir, "scrollback"),
		filepath.Join(stateDir, "run"),
		filepath.Join(stateDir, "credentials"),
		filepath.Join(stateDir, "hooks"),
		stateDir,
		logDir,
		cfgDir,
		managedBin,
		managedBin + ".old",
		filepath.Join(home, ".gemini", "config", "skills", "syre-telemetry", "SKILL.md"),
	}, perIDFiles...)
	for _, p := range mustBeGone {
		if fileExists(p) {
			t.Errorf("leftover not removed: %s", p)
		}
	}

	// ~/.claude/settings.json still exists with the user's setting and without Syre hooks.
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("claude settings.json missing: %v", err)
	}
	if !strings.Contains(string(raw), `"theme": "dark"`) || strings.Contains(string(raw), "syred") {
		t.Errorf("claude settings.json unexpected: %s", raw)
	}

	lines := strings.Join(rep.Lines(), "\n")
	if !strings.Contains(lines, "Removed the syred binary "+managedBin) {
		t.Errorf("report missing binary removal:\n%s", lines)
	}
	if !strings.Contains(lines, "Removed "+stateDir) {
		t.Errorf("report missing state dir removal:\n%s", lines)
	}
}

// Shared home with two identities: removing the first removes only its files and keeps
// all shared files; removing the second (last) removes the shared files too.
func TestRemoveIdentitySharedHomeAndLastIdentity(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	stateDir := filepath.Join(home, ".syre")
	cfgDir := filepath.Join(home, ".config", "syre")
	logDir := filepath.Join(home, "Library", "Logs", "syre")
	unitDir := filepath.Join(home, "Library", "LaunchAgents")
	managedBin := filepath.Join(home, ".local", "bin", "syred")

	a := &sharedAccount{
		t:        t,
		state:    stateDir,
		runner:   &recRunner{},
		procArgs: map[int]string{},
	}
	a.in = &install.Installer{
		Runner: a.runner,
		Out:    &bytes.Buffer{},
		Spawn:  func(string, ...string) error { return nil },
		Env: install.Env{
			Home:       home,
			ConfigDir:  cfgDir,
			LogDir:     logDir,
			UnitDir:    unitDir,
			ManagedBin: managedBin,
			UID:        501,
			User:       "me",
		},
	}

	hostID := a.identity("bocabox")
	boxID := a.identity("bocabox-1")
	hostCfg := a.service("", "bocabox")
	hostLog := a.in.LogPath("")
	mustWriteFile(t, hostLog, "host log\n")
	boxCfg := a.nohup("bocabox-1", 5101)
	boxLog := strings.TrimSuffix(boxCfg, ".yaml") + ".log"
	mustWriteFile(t, boxLog, "box log\n")

	hostFiles := populatePerIdentityFiles(t, stateDir, hostID)
	boxFiles := populatePerIdentityFiles(t, stateDir, boxID)
	populateSharedFiles(t, home, stateDir, managedBin)

	newU := func() *Uninstaller {
		return &Uninstaller{
			Installer:      a.in,
			StateDir:       stateDir,
			LockWait:       300 * time.Millisecond,
			DropHubGrant:   func(string, string) (string, bool, error) { return "", false, nil },
			ProcArgs:       func(pid int) (string, error) { return a.procArgs[pid], nil },
			Kill:           func(pid int) error { a.killed = append(a.killed, pid); return nil },
			ServiceConfigs: func() ([]install.InstalledConfig, error) { return nil, nil },
			Executable:     func() (string, error) { return managedBin, nil },
			OtherRunning:   func(string) (bool, error) { return false, nil },
		}
	}

	// 1. Remove bocabox-1: only bocabox-1's files go; bocabox and shared files stay.
	rep1, err := newU().RemoveIdentity(context.Background(), boxID, "hub1")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append([]string{boxCfg, boxLog, filepath.Join(stateDir, workerIDFilename("bocabox-1", ""))}, boxFiles...) {
		if fileExists(p) {
			t.Errorf("box1 leftover not removed: %s", p)
		}
	}
	mustStay := append([]string{
		hostCfg,
		hostLog,
		filepath.Join(stateDir, workerIDFilename("bocabox", "")),
		managedBin,
		filepath.Join(stateDir, "hook-events.jsonl"),
		filepath.Join(stateDir, "usage-cache.json"),
		filepath.Join(stateDir, "shell-integration", "init.sh"),
		filepath.Join(stateDir, "spool", "spool-orphaned-hub1-20261124T000000Z", "meta.json"),
		filepath.Join(stateDir, "hooks", "notify.sh"),
	}, hostFiles...)
	for _, p := range mustStay {
		if !fileExists(p) {
			t.Errorf("shared or other-identity file was prematurely removed: %s", p)
		}
	}
	kept1 := strings.Join(rep1.Kept, "\n")
	if !strings.Contains(kept1, "still used by bocabox") {
		t.Errorf("expected kept reason naming bocabox, got:\n%s", kept1)
	}

	// 2. Remove bocabox (last identity): everything including shared files and binary goes.
	rep2, err := newU().RemoveIdentity(context.Background(), hostID, "hub1")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append([]string{
		hostCfg, hostLog, managedBin, stateDir, logDir, cfgDir,
		filepath.Join(home, ".gemini", "config", "skills", "syre-telemetry", "SKILL.md"),
	}, hostFiles...) {
		if fileExists(p) {
			t.Errorf("last-identity leftover not removed: %s", p)
		}
	}
	if !strings.Contains(strings.Join(rep2.Lines(), "\n"), "Removed the syred binary "+managedBin) {
		t.Errorf("last-identity report missing binary removal:\n%s", strings.Join(rep2.Lines(), "\n"))
	}
}

// Foreign files and ~/.syre/machine-id are never deleted, and non-empty directories
// containing them are kept with an explanation.
func TestRemoveIdentityPreservesForeignFilesAndMachineID(t *testing.T) {
	a := newSharedAccount(t)
	id := a.identity("solo")
	a.service("", "solo")
	populatePerIdentityFiles(t, a.state, id)

	machineID := filepath.Join(a.state, "machine-id")
	foreignState := filepath.Join(a.state, "my-notes.txt")
	foreignRun := filepath.Join(a.state, "run", "other.sock")
	foreignLog := filepath.Join(a.in.Env.LogDir, "other.log")
	foreignCfg := filepath.Join(a.in.Env.ConfigDir, "other.yaml")
	foreignBin := filepath.Join(a.in.Env.Home, ".local", "bin", "other-tool")
	for _, f := range []string{machineID, foreignState, foreignRun, foreignLog, foreignCfg, foreignBin} {
		mustWriteFile(t, f, "keep me")
	}
	managedBin := a.in.ManagedBinPath()
	mustWriteFile(t, managedBin, "bin")

	rep, err := a.uninstaller(false).RemoveIdentity(context.Background(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	if fileExists(managedBin) {
		t.Errorf("managed binary not removed: %s", managedBin)
	}
	for _, f := range []string{machineID, foreignState, foreignRun, foreignLog, foreignCfg, foreignBin} {
		if !fileExists(f) {
			t.Errorf("foreign/preserved file was deleted: %s", f)
		}
	}
	kept := strings.Join(rep.Kept, "\n")
	if !strings.Contains(kept, "machine-id") || !strings.Contains(kept, "not empty") {
		t.Errorf("expected kept inventory to mention machine-id and non-empty dir, got:\n%s", kept)
	}
}

// Never delete the binary if it is not the one syred installed, or if another syred
// process is still running.
func TestRemoveIdentityBinarySafety(t *testing.T) {
	t.Run("unmanaged binary is kept", func(t *testing.T) {
		a := newSharedAccount(t)
		id := a.identity("solo")
		a.service("", "solo")
		unmanaged := filepath.Join(t.TempDir(), "usr", "local", "bin", "syred")
		mustWriteFile(t, unmanaged, "homebrew binary")

		u := a.uninstaller(false)
		u.Executable = func() (string, error) { return unmanaged, nil }
		rep, err := u.RemoveIdentity(context.Background(), id, "")
		if err != nil {
			t.Fatal(err)
		}
		if !fileExists(unmanaged) {
			t.Fatalf("unmanaged binary was deleted: %s", unmanaged)
		}
		if !strings.Contains(strings.Join(rep.Kept, "\n"), "not installed by syred") {
			t.Errorf("expected kept reason for unmanaged binary, got %q", rep.Kept)
		}
	})

	t.Run("managed binary kept when another syred process is running", func(t *testing.T) {
		a := newSharedAccount(t)
		id := a.identity("solo")
		a.service("", "solo")
		managed := a.in.ManagedBinPath()
		mustWriteFile(t, managed, "bin")

		u := a.uninstaller(false)
		u.OtherRunning = func(string) (bool, error) { return true, nil }
		rep, err := u.RemoveIdentity(context.Background(), id, "")
		if err != nil {
			t.Fatal(err)
		}
		if !fileExists(managed) {
			t.Fatalf("managed binary deleted while another syred process was running")
		}
		if !strings.Contains(strings.Join(rep.Kept, "\n"), "another syred process is still running") {
			t.Errorf("expected kept reason for running process, got %q", rep.Kept)
		}
	})

	t.Run("windows locked exe schedules deletion on exit", func(t *testing.T) {
		a := newSharedAccount(t)
		id := a.identity("solo")
		a.service("", "solo")
		// Make managedBin a non-empty directory so os.Remove fails (simulating a
		// Windows sharing violation on a running exe).
		managedDir := filepath.Join(a.in.Env.Home, "AppData", "Local", "Programs", "syred")
		managed := filepath.Join(managedDir, "syred.exe")
		a.in.Env.ManagedBin = managed
		mustWriteFile(t, filepath.Join(managed, "lock"), "busy")

		var gotExe, gotDir string
		u := a.uninstaller(false)
		u.ScheduleDelete = func(exePath, emptyDir string) error {
			gotExe, gotDir = exePath, emptyDir
			return nil
		}
		rep, err := u.RemoveIdentity(context.Background(), id, "")
		if err != nil {
			t.Fatal(err)
		}
		if gotExe != managed || gotDir != managedDir {
			t.Errorf("ScheduleDelete(%q, %q), want (%q, %q)", gotExe, gotDir, managed, managedDir)
		}
		if !strings.Contains(strings.Join(rep.Removed, "\n"), "scheduled for deletion on exit") {
			t.Errorf("expected scheduled deletion in report, got %q", rep.Removed)
		}
	})
}

func TestBuildWindowsDeleteCmdLine(t *testing.T) {
	got, err := install.BuildWindowsDeleteCmdLine(
		`C:\Users\Bryan Sant\AppData\Local\Programs\syred\syred.exe`,
		`C:\Users\Bryan Sant\AppData\Local\Programs\syred`,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := `cmd.exe /c ping -n 3 127.0.0.1 >nul & del /f /q "C:\Users\Bryan Sant\AppData\Local\Programs\syred\syred.exe" & rmdir "C:\Users\Bryan Sant\AppData\Local\Programs\syred" 2>nul`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if _, err := install.BuildWindowsDeleteCmdLine(`C:\bad&path\syred.exe`, ""); err == nil {
		t.Error("expected unsafe path to be rejected")
	}
}
