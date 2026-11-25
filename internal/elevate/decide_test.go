package elevate

import (
	"strings"
	"testing"
)

func TestInteractive(t *testing.T) {
	desktop := Probe{SessionID: 1, VisibleDesktop: true, Console: true}
	if !Interactive(desktop) {
		t.Fatal("a console in a visible desktop session should be interactive")
	}
	rdp := desktop
	rdp.SessionID = 3
	if !Interactive(rdp) {
		t.Error("a Remote Desktop session should be interactive")
	}
	for name, mutate := range map[string]func(*Probe){
		"ssh env":         func(p *Probe) { p.SSH = true },
		"ci":              func(p *Probe) { p.CI = true },
		"service":         func(p *Probe) { p.Service = true },
		"session 0":       func(p *Probe) { p.SessionID = 0 },
		"hidden station":  func(p *Probe) { p.VisibleDesktop = false },
		"no console":      func(p *Probe) { p.Console = false },
		"openssh session": func(p *Probe) { p.SessionID, p.VisibleDesktop, p.SSH = 0, false, true },
	} {
		p := desktop
		mutate(&p)
		if Interactive(p) {
			t.Errorf("%s: should not be interactive", name)
		}
	}
}

func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Situation
		want Action
	}{
		{"elevated", Situation{Elevated: true}, RunHere},
		{"elevated over ssh", Situation{Elevated: true, SSH: true}, RunHere},
		{"elevated, --no-elevate", Situation{Elevated: true, NoElevate: true}, RunHere},
		{"interactive", Situation{Interactive: true}, Elevate},
		{"interactive, --no-elevate", Situation{Interactive: true, NoElevate: true}, Refuse},
		{"interactive, declined", Situation{Interactive: true, Declined: true}, Refuse},
		{"ssh, standard user", Situation{SSH: true}, Refuse},
		{"no desktop", Situation{}, Refuse},
		{"no desktop, sudo inline", Situation{Sudo: SudoInline}, Refuse},
	} {
		if got := Decide(tc.s); got != tc.want {
			t.Errorf("%s: Decide = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestSudoModeFrom(t *testing.T) {
	for _, tc := range []struct {
		setting uint32
		haveSet bool
		policy  uint32
		havePol bool
		want    SudoMode
		desc    string
	}{
		{0, false, 0, false, SudoDisabled, "no key"},
		{0, true, 0, false, SudoDisabled, "disabled"},
		{1, true, 0, false, SudoNewWindow, "new window"},
		{2, true, 0, false, SudoInputClosed, "input closed"},
		{3, true, 0, false, SudoInline, "inline (verified on Windows 11 26300)"},
		{3, true, 1, true, SudoNewWindow, "policy caps inline to new window"},
		{3, true, 0, true, SudoDisabled, "policy disables"},
		{1, true, 3, true, SudoNewWindow, "policy above the setting does not raise it"},
		{7, true, 0, false, SudoDisabled, "unknown value"},
	} {
		if got := SudoModeFrom(tc.setting, tc.haveSet, tc.policy, tc.havePol); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.desc, got, tc.want)
		}
	}
}

func TestMessage(t *testing.T) {
	cmd := `konsertd.exe install --service --user=.\bob`
	for _, tc := range []struct {
		name      string
		op        Op
		s         Situation
		want      []string
		forbidden []string
	}{
		{
			name: "ssh never suggests sudo",
			op:   OpInstall, s: Situation{SSH: true, Sudo: SudoInline},
			want:      []string{"Installing a Windows service needs administrator rights.", "Over SSH, connect as an administrator", "\n  " + cmd, "Or omit --service"},
			forbidden: []string{"sudo"},
		},
		{
			name: "inline sudo suggested",
			op:   OpInstall, s: Situation{Sudo: SudoInline, NoElevate: true},
			want:      []string{"Run it with sudo:\n  sudo " + cmd},
			forbidden: []string{"Run as administrator"},
		},
		{
			name: "new-window sudo not suggested",
			op:   OpInstall, s: Situation{Sudo: SudoNewWindow},
			want:      []string{"elevated terminal", "Run as administrator", "\n  " + cmd},
			forbidden: []string{"sudo"},
		},
		{
			name: "input-closed sudo not suggested",
			op:   OpInstall, s: Situation{Sudo: SudoInputClosed},
			want:      []string{"Run as administrator"},
			forbidden: []string{"sudo"},
		},
		{
			name: "declined",
			op:   OpInstall, s: Situation{Interactive: true, Declined: true},
			want: []string{"The administrator (UAC) prompt was declined or failed.", "Run as administrator"},
		},
		{
			name: "uninstall",
			op:   OpUninstall, s: Situation{},
			want:      []string{"Removing this Windows service needs administrator rights", "Run as administrator"},
			forbidden: []string{"omit --service"},
		},
	} {
		got := Message(tc.op, tc.s, cmd)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: message lacks %q:\n%s", tc.name, w, got)
			}
		}
		for _, f := range tc.forbidden {
			if strings.Contains(got, f) {
				t.Errorf("%s: message contains %q:\n%s", tc.name, f, got)
			}
		}
	}
}

func TestCommandLine(t *testing.T) {
	got := CommandLine([]string{`C:\Program Files\konsertd.exe`, "install", "--service", `--user=.\bob`, "--no-elevate", `--tag=a="b c"`, `--config=C:\x y\`})
	want := `"C:\Program Files\konsertd.exe" install --service --user=.\bob "--tag=a=\"b c\"" "--config=C:\x y\\"`
	if got != want {
		t.Errorf("CommandLine =\n %s\nwant\n %s", got, want)
	}
}
