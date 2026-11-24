package install

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/config"
)

// fakeRunner records commands instead of running them. fail maps a command prefix
// ("systemctl --user show-environment") to an error; out maps a prefix to its output.
type fakeRunner struct {
	calls []string
	fail  map[string]error
	out   map[string]string
}

func (f *fakeRunner) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	for p, err := range f.fail {
		if strings.HasPrefix(line, p) {
			return "", err
		}
	}
	for p, o := range f.out {
		if strings.HasPrefix(line, p) {
			return o, nil
		}
	}
	return "", nil
}

func (f *fakeRunner) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// testInstaller returns an installer whose directories all live in a temp dir.
func testInstaller(t *testing.T) (*Installer, *fakeRunner) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	r := &fakeRunner{fail: map[string]error{}, out: map[string]string{}}
	return &Installer{
		Runner: r,
		Out:    &bytes.Buffer{},
		Env: Env{
			Home:       filepath.Join(root, "home"),
			ConfigDir:  filepath.Join(root, "config", "konsert"),
			LogDir:     filepath.Join(root, "logs"),
			UnitDir:    filepath.Join(root, "units"),
			UID:        501,
			User:       "alice",
			UserSID:    "S-1-5-21-1-2-3-1001",
			SystemRoot: filepath.Join(root, "Windows"),
			PATH:       "/opt/homebrew/bin:/usr/bin",
		},
	}, r
}

func testSpec(instance string) Spec {
	exe, _ := filepath.Abs(filepath.Join("testdata", "konsertd"))
	return Spec{
		Instance:  instance,
		Exe:       exe,
		Config:    []byte("controller_url: ws://hub/ws/worker\nname: s3cret\n"),
		ExtraArgs: []string{"--sandbox-allow-shell=true"},
	}
}

func TestInstanceFor(t *testing.T) {
	cases := []struct{ name, host, want string }{
		{"", "box", ""},
		{"box", "box", ""},
		{"BOX", "box.example.com", ""},
		{"install-test", "box", "install-test"},
		{"My Sandbox!", "box", "my-sandbox"},
		{"a__b", "box", "a-b"},
	}
	for _, c := range cases {
		if got := InstanceFor(c.name, c.host); got != c.want {
			t.Errorf("InstanceFor(%q, %q) = %q, want %q", c.name, c.host, got, c.want)
		}
	}
	if LaunchdLabel("") != "dev.konsert.konsertd" || LaunchdLabel("x") != "dev.konsert.konsertd.x" {
		t.Errorf("labels: %q %q", LaunchdLabel(""), LaunchdLabel("x"))
	}
	if SystemdUnit("x") != "konsertd-x.service" || TaskName("") != `\konsertd` {
		t.Errorf("names: %q %q", SystemdUnit("x"), TaskName(""))
	}
}

func TestRenderPlist(t *testing.T) {
	p := RenderPlist("dev.konsert.konsertd.t", []string{"/bin/konsertd", "run", "--config", "/Users/a/Library/Application Support/konsert/k&d.yaml"},
		map[string]string{"PATH": "/opt/homebrew/bin:/usr/bin"}, "/Users/a", "/Users/a/Library/Logs/konsert/konsertd-t.log")
	s := string(p)
	for _, want := range []string{
		"<string>dev.konsert.konsertd.t</string>",
		"<string>/Users/a/Library/Application Support/konsert/k&amp;d.yaml</string>",
		"<key>RunAtLoad</key>\n  <true/>",
		"<key>KeepAlive</key>\n  <true/>",
		"<key>PATH</key>\n    <string>/opt/homebrew/bin:/usr/bin</string>",
		"<key>StandardErrorPath</key>\n  <string>/Users/a/Library/Logs/konsert/konsertd-t.log</string>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "s3cret") || strings.Contains(s, "token") {
		t.Errorf("plist must not carry a token:\n%s", s)
	}
	// Well-formed XML.
	if err := xml.Unmarshal(p, new(struct{ XMLName xml.Name })); err != nil {
		t.Fatalf("plist is not well-formed XML: %v", err)
	}
}

func TestRenderUnit(t *testing.T) {
	u := string(RenderUnit("Konsert worker daemon (t)",
		[]string{"/home/a/bin/konsertd", "run", "--config", "/home/a/.config/konsert/konsertd-t.yaml", "--tag=x=100%"},
		map[string]string{"PATH": "/home/a/.local/bin:/usr/bin"}, "/home/a"))
	for _, want := range []string{
		"Description=Konsert worker daemon (t)\n",
		`ExecStart="/home/a/bin/konsertd" "run" "--config" "/home/a/.config/konsert/konsertd-t.yaml" "--tag=x=100%%"` + "\n",
		`Environment="PATH=/home/a/.local/bin:/usr/bin"` + "\n",
		"WorkingDirectory=/home/a\n",
		"Restart=always\n",
		"KillMode=mixed\n",
		"WantedBy=default.target\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit missing %q:\n%s", want, u)
		}
	}
	if got := systemdQuote(`a"b\c$d`); got != `"a\"b\\c$$d"` {
		t.Errorf("systemdQuote = %s", got)
	}
}

func TestRenderTaskXML(t *testing.T) {
	args := WindowsJoinArgs([]string{"--headless", `C:\Program Files\konsert\konsertd.exe`, "run", "--config", `C:\Users\a\AppData\Local\konsert\konsertd.yaml`})
	x := RenderTaskXML(TaskXMLParams{
		URI: `\konsertd`, Description: "d & e", UserSID: "S-1-5-21-1-2-3-1001",
		Command: `C:\Windows\System32\conhost.exe`, Arguments: args, WorkDir: `C:\Users\a`,
		StartBoundary: "2026-01-02T03:04:05",
	})
	var task struct {
		Triggers struct {
			Logon struct {
				UserID string `xml:"UserId"`
			} `xml:"LogonTrigger"`
			Time struct {
				Start      string `xml:"StartBoundary"`
				Repetition struct {
					Interval string `xml:"Interval"`
				} `xml:"Repetition"`
			} `xml:"TimeTrigger"`
		} `xml:"Triggers"`
		Principal struct {
			UserID    string `xml:"UserId"`
			LogonType string `xml:"LogonType"`
			RunLevel  string `xml:"RunLevel"`
		} `xml:"Principals>Principal"`
		Settings struct {
			Multiple  string `xml:"MultipleInstancesPolicy"`
			Hidden    bool   `xml:"Hidden"`
			TimeLimit string `xml:"ExecutionTimeLimit"`
			Battery   bool   `xml:"DisallowStartIfOnBatteries"`
			Restart   struct {
				Count int `xml:"Count"`
			} `xml:"RestartOnFailure"`
		} `xml:"Settings"`
		Exec struct {
			Command   string `xml:"Command"`
			Arguments string `xml:"Arguments"`
		} `xml:"Actions>Exec"`
	}
	// encoding/xml refuses an encoding="UTF-16" declaration on UTF-8 text; parse the body.
	body := x[strings.Index(x, "<Task "):]
	if err := xml.Unmarshal([]byte(body), &task); err != nil {
		t.Fatalf("task XML: %v\n%s", err, x)
	}
	if task.Principal.LogonType != "InteractiveToken" || task.Principal.RunLevel != "LeastPrivilege" {
		t.Errorf("principal = %+v", task.Principal)
	}
	if task.Principal.UserID != "S-1-5-21-1-2-3-1001" || task.Triggers.Logon.UserID != "S-1-5-21-1-2-3-1001" {
		t.Errorf("user = %+v / %+v", task.Principal, task.Triggers.Logon)
	}
	if task.Triggers.Time.Repetition.Interval != "PT1M" || task.Triggers.Time.Start != "2026-01-02T03:04:05" || task.Settings.Multiple != "IgnoreNew" {
		t.Errorf("keep-alive trigger settings = %+v %+v", task.Triggers.Time, task.Settings)
	}
	if !task.Settings.Hidden || task.Settings.TimeLimit != "PT0S" || task.Settings.Battery || task.Settings.Restart.Count == 0 {
		t.Errorf("settings = %+v", task.Settings)
	}
	if task.Exec.Command != `C:\Windows\System32\conhost.exe` {
		t.Errorf("command = %q", task.Exec.Command)
	}
	if want := `--headless "C:\Program Files\konsert\konsertd.exe" run --config C:\Users\a\AppData\Local\konsert\konsertd.yaml`; task.Exec.Arguments != want {
		t.Errorf("arguments = %q, want %q", task.Exec.Arguments, want)
	}
	if !strings.Contains(x, "d &amp; e") {
		t.Errorf("description not escaped")
	}
}

func TestEncodeUTF16(t *testing.T) {
	b := EncodeUTF16("a\né")
	want := []byte{0xFF, 0xFE, 'a', 0, '\r', 0, '\n', 0, 0xE9, 0}
	if !bytes.Equal(b, want) {
		t.Errorf("EncodeUTF16 = % x, want % x", b, want)
	}
}

func TestWindowsJoinArgs(t *testing.T) {
	got := WindowsJoinArgs([]string{"plain", "", "with space", `C:\dir with space\`, `q"uote`})
	want := `plain "" "with space" "C:\dir with space\\" "q\"uote"`
	if got != want {
		t.Errorf("WindowsJoinArgs = %s, want %s", got, want)
	}
}

func TestMarshalWorkerConfigIsSparse(t *testing.T) {
	data, err := MarshalWorkerConfig(config.WorkerConfig{
		ControllerURL: "wss://hub/ws/worker",
		Credential:    "kmc_never_persisted",
		Sandbox:       true,
		Tags:          map[string]string{"gpu": "yes"},
		AllowedRoots:  []string{"/src"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{"controller_url: wss://hub/ws/worker", "sandbox: true", "gpu: \"yes\"", "- /src"} {
		if !strings.Contains(s, want) {
			t.Errorf("config missing %q:\n%s", want, s)
		}
	}
	// Unset fields must be absent so run's defaults (hostname as name, 2 slots) apply.
	for _, absent := range []string{"kmc_never_persisted", "credential", "name:", "max_concurrent:", "isolation:", "worker_id:", "enable_mock_driver:"} {
		if strings.Contains(s, absent) {
			t.Errorf("config should omit %q:\n%s", absent, s)
		}
	}
	back, err := ParseWorkerConfig(data)
	if err != nil || back.Tags["gpu"] != "yes" {
		t.Errorf("round trip = %+v, %v", back, err)
	}
}

func TestCleanPATH(t *testing.T) {
	if runtime.GOOS != "windows" { // "/a" is drive-relative on Windows
		if got := CleanPATH("/a:.::/b:/a:rel/x:/c", ':'); got != "/a:/b:/c" {
			t.Errorf("CleanPATH = %q", got)
		}
	}
	if got := CleanPATH(`C:\a;.;C:\a;\\srv\share;D:/x`, ';'); got != `C:\a;\\srv\share;D:/x` {
		t.Errorf("CleanPATH windows = %q", got)
	}
}

func TestLaunchdInstallUninstall(t *testing.T) {
	in, r := testInstaller(t)
	spec := testSpec("install-test")
	if err := in.install(context.Background(), launchdBackend{}, spec); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(in.Env.UnitDir, "dev.konsert.konsertd.install-test.plist")
	data, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<string>"+in.ConfigPath("install-test")+"</string>") ||
		!strings.Contains(string(data), "<string>--sandbox-allow-shell=true</string>") {
		t.Errorf("plist args wrong:\n%s", data)
	}
	if strings.Contains(string(data), "s3cret") {
		t.Error("config leaked into the plist")
	}
	if !r.called("launchctl bootstrap gui/501 " + plist) {
		t.Errorf("calls = %v", r.calls)
	}
	cfg, err := os.ReadFile(in.ConfigPath("install-test"))
	if err != nil || !strings.Contains(string(cfg), "name: s3cret") {
		t.Errorf("config = %q, %v", cfg, err)
	}

	r.out["launchctl print"] = "gui/501/dev.konsert.konsertd.install-test = {\n\tstate = running\n\tpid = 4242\n}"
	st, err := in.status(context.Background(), launchdBackend{}, "install-test")
	if err != nil || !st.Installed || !st.Running || st.PID != 4242 {
		t.Errorf("status = %+v, %v", st, err)
	}

	if err := in.uninstall(context.Background(), launchdBackend{}, "install-test"); err != nil {
		t.Fatal(err)
	}
	if !r.called("launchctl bootout gui/501/dev.konsert.konsertd.install-test") {
		t.Errorf("calls = %v", r.calls)
	}
	if fileExists(plist) || fileExists(in.ConfigPath("install-test")) {
		t.Error("uninstall left files behind")
	}
}

// status/uninstall helpers that go through an explicit backend (tests only).
func (in *Installer) status(ctx context.Context, b backend, instance string) (Status, error) {
	return b.status(ctx, in, instance)
}

func TestSystemdInstallUninstall(t *testing.T) {
	in, r := testInstaller(t)
	r.out["loginctl show-user"] = "no\n"
	spec := testSpec("")
	if err := in.install(context.Background(), systemdBackend{}, spec); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(in.Env.UnitDir, "konsertd.service")
	data, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"run" "--config" `+systemdQuote(in.ConfigPath(""))+` "--sandbox-allow-shell=true"`) {
		t.Errorf("unit ExecStart wrong:\n%s", data)
	}
	if strings.Contains(string(data), "s3cret") {
		t.Error("token leaked into the unit")
	}
	for _, c := range []string{"systemctl --user daemon-reload", "systemctl --user enable konsertd.service", "systemctl --user restart konsertd.service", "loginctl enable-linger alice"} {
		if !r.called(c) {
			t.Errorf("missing call %q in %v", c, r.calls)
		}
	}

	r.out["systemctl --user show konsertd.service"] = "ActiveState=active\nSubState=running\nMainPID=99\nUnitFileState=enabled\n"
	st, err := in.status(context.Background(), systemdBackend{}, "")
	if err != nil || !st.Running || st.PID != 99 {
		t.Errorf("status = %+v, %v", st, err)
	}

	if err := in.uninstall(context.Background(), systemdBackend{}, ""); err != nil {
		t.Fatal(err)
	}
	if !r.called("systemctl --user disable --now konsertd.service") || fileExists(unit) || fileExists(in.ConfigPath("")) {
		t.Errorf("uninstall incomplete; calls = %v", r.calls)
	}
}

func TestSystemdLingerAlreadyOnOrSkipped(t *testing.T) {
	in, r := testInstaller(t)
	r.out["loginctl show-user"] = "yes\n"
	if err := in.install(context.Background(), systemdBackend{}, testSpec("")); err != nil {
		t.Fatal(err)
	}
	if r.called("loginctl enable-linger") {
		t.Error("enable-linger called although lingering was on")
	}

	in, r = testInstaller(t)
	spec := testSpec("")
	spec.NoLinger = true
	if err := in.install(context.Background(), systemdBackend{}, spec); err != nil {
		t.Fatal(err)
	}
	if r.called("loginctl") {
		t.Errorf("--no-linger still called loginctl: %v", r.calls)
	}

	in, r = testInstaller(t)
	r.fail["loginctl enable-linger"] = errors.New("access denied")
	if err := in.install(context.Background(), systemdBackend{}, testSpec("")); err != nil {
		t.Fatalf("a linger failure must not fail the install: %v", err)
	}
	if !strings.Contains(in.Out.(*bytes.Buffer).String(), "sudo loginctl enable-linger alice") {
		t.Errorf("expected the manual linger command, got:\n%s", in.Out)
	}
}

func TestSystemdNoUserManager(t *testing.T) {
	in, r := testInstaller(t)
	r.fail["systemctl --user show-environment"] = errors.New("Failed to connect to bus")
	err := in.install(context.Background(), systemdBackend{}, testSpec(""))
	if err == nil || !strings.Contains(err.Error(), "no systemd user instance") {
		t.Fatalf("err = %v", err)
	}
	if fileExists(filepath.Join(in.Env.UnitDir, "konsertd.service")) {
		t.Error("unit written without a user manager")
	}
}

func TestSchtasksInstallUninstall(t *testing.T) {
	in, r := testInstaller(t)
	conhost := in.Env.SystemRoot + `\System32\conhost.exe`
	if err := writeFile(conhost, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	spec := testSpec("install-test")
	if err := in.install(context.Background(), schtasksBackend{}, spec); err != nil {
		t.Fatal(err)
	}
	xmlPath := filepath.Join(in.Env.ConfigDir, "konsertd-install-test.task.xml")
	for _, c := range []string{
		`schtasks /Create /TN \konsertd-install-test /XML ` + xmlPath + " /F",
		`schtasks /Run /TN \konsertd-install-test`,
	} {
		if !r.called(c) {
			t.Errorf("missing call %q in %v", c, r.calls)
		}
	}
	if fileExists(xmlPath) {
		t.Error("staged task XML not removed")
	}
	_, def, err := schtasksBackend{}.render(in, spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(def), "s3cret") || !strings.Contains(string(def), "--log-file") {
		t.Errorf("task definition:\n%s", def)
	}

	r.out["schtasks /Query /TN"] = `"\konsertd-install-test","N/A","Running"` + "\r\n"
	st, err := in.status(context.Background(), schtasksBackend{}, "install-test")
	if err != nil || !st.Installed || !st.Running {
		t.Errorf("status = %+v, %v", st, err)
	}

	if err := in.uninstall(context.Background(), schtasksBackend{}, "install-test"); err != nil {
		t.Fatal(err)
	}
	if !r.called(`schtasks /Delete /TN \konsertd-install-test /F`) || fileExists(in.ConfigPath("install-test")) {
		t.Errorf("uninstall incomplete; calls = %v", r.calls)
	}
}

func TestSchtasksNeedsConhost(t *testing.T) {
	in, _ := testInstaller(t)
	err := in.install(context.Background(), schtasksBackend{}, testSpec(""))
	if err == nil || !strings.Contains(err.Error(), "conhost") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallRejectsRelativeExe(t *testing.T) {
	in, _ := testInstaller(t)
	spec := testSpec("")
	spec.Exe = "konsertd"
	if err := in.install(context.Background(), launchdBackend{}, spec); err == nil {
		t.Fatal("expected an error for a relative binary path")
	}
}
