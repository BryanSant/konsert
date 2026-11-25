// Package elevate runs a syred command with administrator rights on Windows
// (SYR-28HHD): `syred install --service` (and `uninstall --service` when the
// account may not remove the service itself) relaunch themselves through UAC when
// there is someone at the desktop to approve the prompt, and relay the elevated
// process's output and exit code in the original terminal. Without a desktop (SSH,
// services, CI) they never try UAC, which would hang or fail invisibly: they print
// what to do instead.
//
// The decision logic, the message text and the parent/child pipe protocol are plain
// Go, tested on every OS; only process creation, the pipe and the environment probes
// are Windows-specific.
package elevate

import (
	"strings"
)

// SudoMode is the Windows sudo setting (Settings > System > For developers > Enable
// sudo), from HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Sudo value Enabled
// (REG_DWORD). Verified on Windows 11 build 26300: Enabled=3 while `sudo config` says
// "Sudo is currently in Inline mode on this machine". The values are those of sudo's
// own enum (microsoft/sudo): 0 disabled, 1 forceNewWindow, 2 disableInput, 3 normal
// (inline). A policy value of the same name under
// HKLM\SOFTWARE\Policies\Microsoft\Windows\Sudo caps the mode an administrator may
// choose.
type SudoMode uint32

const (
	SudoDisabled     SudoMode = 0
	SudoNewWindow    SudoMode = 1
	SudoInputClosed  SudoMode = 2
	SudoInline       SudoMode = 3
	sudoHighestKnown          = SudoInline
)

func (m SudoMode) String() string {
	switch m {
	case SudoDisabled:
		return "disabled"
	case SudoNewWindow:
		return "new window"
	case SudoInputClosed:
		return "input closed"
	case SudoInline:
		return "inline"
	}
	return "unknown"
}

// SudoModeFrom computes the effective mode from the registry values: the setting
// (absent: disabled), capped by the policy when one is set. Unknown values count as
// disabled, so a future mode never makes the message suggest sudo by mistake.
func SudoModeFrom(setting uint32, haveSetting bool, policy uint32, havePolicy bool) SudoMode {
	if !haveSetting || setting > uint32(sudoHighestKnown) {
		return SudoDisabled
	}
	m := SudoMode(setting)
	if havePolicy && policy < uint32(m) {
		m = SudoMode(policy)
	}
	return m
}

// Probe is what the interactivity rule looks at.
type Probe struct {
	// SSH is set when SSH_CONNECTION, SSH_CLIENT or SSH_TTY is in the environment.
	SSH bool
	// CI is set when the CI environment variable is (GitHub Actions, GitLab, ...).
	CI bool
	// Service is set when the SCM started this process.
	Service bool
	// SessionID is the process's Terminal Services session; 0 is the non-interactive
	// services session (where OpenSSH sessions also run).
	SessionID uint32
	// VisibleDesktop is set when the process's window station is visible (WSF_VISIBLE,
	// the test .NET's Environment.UserInteractive uses): WinSta0 of a logged-on user.
	VisibleDesktop bool
	// Console is set when the process has a console (GetConsoleWindow), where the
	// password prompt and the relayed output go.
	Console bool
}

// Interactive is the rule for when a UAC prompt can be shown and answered: someone is
// at an interactive desktop session (the console or a Remote Desktop session; the
// session need not be the active console one, UAC works over RDP) with a visible
// window station, the process has a console, and it was not started over SSH, by the
// service manager or by CI. OpenSSH sessions run in session 0 without a visible window
// station, so they fail on both counts; the SSH variables also cover SSH
// servers that run sessions some other way.
func Interactive(p Probe) bool {
	return !p.SSH && !p.CI && !p.Service && p.SessionID != 0 && p.VisibleDesktop && p.Console
}

// Situation is everything the decision depends on.
type Situation struct {
	Elevated    bool
	Interactive bool
	SSH         bool
	// NoElevate is --no-elevate: never prompt, print the message instead.
	NoElevate bool
	Sudo      SudoMode
	// Declined is set after the user declined the UAC prompt (or it failed).
	Declined bool
}

// Action is what a command needing administrator rights does.
type Action int

const (
	// RunHere: already elevated, go ahead.
	RunHere Action = iota
	// Elevate: relaunch through UAC and relay the result.
	Elevate
	// Refuse: print Message and fail.
	Refuse
)

func (a Action) String() string {
	switch a {
	case RunHere:
		return "run here"
	case Elevate:
		return "elevate"
	}
	return "refuse"
}

// Decide picks the action. A declined prompt is never retried.
func Decide(s Situation) Action {
	switch {
	case s.Elevated:
		return RunHere
	case s.Interactive && !s.NoElevate && !s.Declined:
		return Elevate
	}
	return Refuse
}

// Op is the command needing administrator rights.
type Op int

const (
	OpInstall Op = iota
	OpUninstall
)

// Message is the short explanation printed instead of elevating. command is the user's
// command line (without --no-elevate), for the suggestion to run it again elevated.
// Over SSH sudo is never suggested: it needs the same UAC prompt, which nobody can
// answer there. Elsewhere sudo is suggested only in inline mode: "new window" loses the
// output and "input closed" breaks the password prompt.
func Message(op Op, s Situation, command string) string {
	var b strings.Builder
	if s.Declined {
		b.WriteString("The administrator (UAC) prompt was declined or failed. ")
	}
	switch op {
	case OpInstall:
		b.WriteString("Installing a Windows service needs administrator rights.")
	default:
		b.WriteString("Removing this Windows service needs administrator rights: this account was not granted the right to remove it.")
	}
	switch {
	case s.SSH:
		b.WriteString(" Over SSH, connect as an administrator (OpenSSH gives administrators an elevated session), or run it in a terminal at the desktop, which asks for approval:\n  " + command)
	case s.Sudo == SudoInline:
		b.WriteString(" Run it with sudo:\n  sudo " + command)
	default:
		b.WriteString(" Run it from an elevated terminal (right-click Terminal or PowerShell, Run as administrator):\n  " + command)
	}
	if op == OpInstall {
		b.WriteString("\nOr omit --service to install a logon task as the current user, which needs no elevation.")
	}
	return b.String()
}

// CommandLine renders argv for the message, dropping --no-elevate and quoting
// arguments with spaces or quotes the way the Windows command line splits them.
func CommandLine(argv []string) string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		if a == "--no-elevate" || strings.HasPrefix(a, "--no-elevate=") {
			continue
		}
		out = append(out, quoteArg(a))
	}
	return strings.Join(out, " ")
}

func quoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			slashes++
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
		}
		slashes = 0
		b.WriteRune(r)
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}
