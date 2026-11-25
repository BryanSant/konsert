package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// spawnRecorder records the detached commands a self-removal starts.
func spawnRecorder(in *Installer) *[]string {
	var spawned []string
	in.Spawn = func(name string, args ...string) error {
		spawned = append(spawned, strings.Join(append([]string{name}, args...), " "))
		return nil
	}
	return &spawned
}

// The daemon removing its own LaunchAgent removes the plist and config first, then
// starts `launchctl bootout` detached: run synchronously, bootout would wait for the
// very process running it to exit.
func TestLaunchdUninstallSelf(t *testing.T) {
	in, r := testInstaller(t)
	spawned := spawnRecorder(in)
	if err := in.install(context.Background(), launchdBackend{}, testSpec("")); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(in.Env.UnitDir, "dev.syre.syred.plist")
	r.calls = nil
	if err := in.uninstallSelf(context.Background(), launchdBackend{}, ""); err != nil {
		t.Fatal(err)
	}
	if fileExists(plist) || fileExists(in.ConfigPath("")) {
		t.Error("self-removal left the plist or config behind")
	}
	if r.called("launchctl") {
		t.Errorf("launchctl must not run synchronously from the daemon: %v", r.calls)
	}
	if !slices.Equal(*spawned, []string{"launchctl bootout gui/501/dev.syre.syred"}) {
		t.Errorf("spawned = %v", *spawned)
	}
}

// systemd: disabled without --now, unit removed, then a non-blocking stop (a pending
// stop job keeps Restart=always from restarting the exiting daemon).
func TestSystemdUninstallSelf(t *testing.T) {
	in, r := testInstaller(t)
	r.out["loginctl show-user"] = "yes\n"
	if err := in.install(context.Background(), systemdBackend{}, testSpec("box")); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(in.Env.UnitDir, "syred-box.service")
	r.calls = nil
	if err := in.uninstallSelf(context.Background(), systemdBackend{}, "box"); err != nil {
		t.Fatal(err)
	}
	if fileExists(unit) || fileExists(in.ConfigPath("box")) {
		t.Error("self-removal left the unit or config behind")
	}
	if r.called("systemctl --user disable --now") || r.called("systemctl --user stop syred-box.service") {
		t.Errorf("a blocking stop from inside the unit: %v", r.calls)
	}
	want := []string{"systemctl --user disable syred-box.service", "systemctl --user daemon-reload", "systemctl --user stop --no-block syred-box.service"}
	last := -1
	for _, w := range want {
		i := slices.Index(r.calls, w)
		if i < 0 || i < last {
			t.Fatalf("calls = %v, want %v in order", r.calls, want)
		}
		last = i
	}
}

// Windows: deleting the task removes both triggers, including the every-minute
// keep-alive that would restart syred; /End is not called (it would kill the
// daemon doing the removal).
func TestSchtasksUninstallSelf(t *testing.T) {
	in, r := testInstaller(t)
	conhost := in.Env.SystemRoot + `\System32\conhost.exe`
	if err := writeFile(conhost, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := in.install(context.Background(), schtasksBackend{}, testSpec("")); err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	if err := in.uninstallSelf(context.Background(), schtasksBackend{}, ""); err != nil {
		t.Fatal(err)
	}
	if !r.called(`schtasks /Delete /TN \syred /F`) || r.called("schtasks /End") {
		t.Errorf("calls = %v", r.calls)
	}
	if fileExists(in.ConfigPath("")) {
		t.Error("config left behind")
	}

	// No task (already removed): nothing to delete, still no error.
	r.calls = nil
	r.fail["schtasks /Query"] = errors.New("ERROR: The system cannot find the file specified.")
	if err := in.uninstallSelf(context.Background(), schtasksBackend{}, ""); err != nil || r.called("schtasks /Delete") {
		t.Errorf("err = %v, calls = %v", err, r.calls)
	}
}

// A container without a systemd user manager must fail before writing the config: on
// a home directory shared with its host, the config file is the host's.
func TestSystemdInstallChecksUserManagerBeforeWritingConfig(t *testing.T) {
	in, r := testInstaller(t)
	host := []byte("name: host\n")
	if err := WriteSecretFile(in.ConfigPath(""), host); err != nil {
		t.Fatal(err)
	}
	r.fail["systemctl --user show-environment"] = errors.New("Failed to connect to bus")
	if err := in.install(context.Background(), systemdBackend{}, testSpec("")); err == nil {
		t.Fatal("install without a user manager must fail")
	}
	if got, _ := os.ReadFile(in.ConfigPath("")); string(got) != string(host) {
		t.Fatalf("the host's config was overwritten: %q", got)
	}
}

func TestInstalledConfigs(t *testing.T) {
	in, _ := testInstaller(t)
	for _, n := range []string{"syred.yaml", "syred-box.yaml", "syred-box.task.xml", "other.yaml"} {
		if err := writeFile(filepath.Join(in.Env.ConfigDir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := in.InstalledConfigs()
	if err != nil {
		t.Fatal(err)
	}
	var instances []string
	for _, c := range got {
		instances = append(instances, c.Instance)
	}
	slices.Sort(instances)
	if !slices.Equal(instances, []string{"", "box"}) {
		t.Fatalf("instances = %q", instances)
	}

	in.Env.ConfigDir = filepath.Join(t.TempDir(), "missing")
	if got, err := in.InstalledConfigs(); err != nil || len(got) != 0 {
		t.Fatalf("missing dir: %v, %v", got, err)
	}
}

func TestNohupBase(t *testing.T) {
	for in, want := range map[string]string{
		"bocabox-1":   "syred-nohup-bocabox-1",
		"BocaBox":     "syred-nohup-bocabox",
		"my box/x":    "syred-nohup-my_box_x",
		"":            "syred-nohup-worker",
		"host.ts.net": "syred-nohup-host.ts.net",
	} {
		if got := NohupBase(in); got != want {
			t.Errorf("NohupBase(%q) = %q, want %q", in, got, want)
		}
	}
}
