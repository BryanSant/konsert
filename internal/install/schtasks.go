package install

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

// schtasksBackend registers a Task Scheduler task that starts konsertd at the current
// user's logon, in the user's interactive session (LogonType InteractiveToken: no stored
// password, full access to the user's profile, Credential Manager and DPAPI), with a
// limited (non-elevated) token.
//
// A console program started by Task Scheduler in an interactive session gets a visible
// console window (on Windows 11 possibly a Windows Terminal tab). To avoid it, the task
// runs `conhost.exe --headless konsertd.exe run ...`: conhost is itself a GUI-subsystem
// program, so starting it shows nothing, and in headless mode it hosts a console for
// konsertd without ever creating a window. konsertd still has a real console, so the
// git and agent CLI child processes it starts inherit that hidden console instead of
// each popping up a window of their own (which is what a windowsgui build of konsertd
// would cause). Default-terminal delegation to Windows Terminal does not apply because
// conhost is launched explicitly.
//
// conhost --headless exits 0 whatever konsertd's exit code was, so Task Scheduler's
// RestartOnFailure never sees a failure. Instead a second trigger fires every minute,
// indefinitely, from install time on, with MultipleInstancesPolicy IgnoreNew: while
// konsertd runs each firing is ignored, and if it has exited the next one starts it
// again. (A repetition on the logon trigger itself would only begin at the next logon.)
// Like the logon trigger, it only runs while the user is logged on.
type schtasksBackend struct{}

// TaskName is the Task Scheduler task name of an instance (in the root folder, where a
// standard user may create tasks).
func TaskName(instance string) string { return `\` + unitBase(instance) }

// TaskXMLParams are the inputs to RenderTaskXML.
type TaskXMLParams struct {
	URI         string
	Description string
	UserSID     string
	Command     string
	Arguments   string
	WorkDir     string
	// StartBoundary is the local time (2006-01-02T15:04:05) the every-minute
	// keep-alive trigger starts from.
	StartBoundary string
}

// RenderTaskXML renders the Task Scheduler definition (UTF-8 text; see EncodeUTF16 for
// what schtasks reads).
func RenderTaskXML(p TaskXMLParams) string {
	esc := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	work := ""
	if p.WorkDir != "" {
		work = "\n      <WorkingDirectory>" + esc(p.WorkDir) + "</WorkingDirectory>"
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Author>konsertd install</Author>
    <Description>` + esc(p.Description) + `</Description>
    <URI>` + esc(p.URI) + `</URI>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + esc(p.UserSID) + `</UserId>
    </LogonTrigger>
    <TimeTrigger>
      <Enabled>true</Enabled>
      <StartBoundary>` + esc(p.StartBoundary) + `</StartBoundary>
      <Repetition>
        <Interval>PT1M</Interval>
        <StopAtDurationEnd>false</StopAtDurationEnd>
      </Repetition>
    </TimeTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + esc(p.UserSID) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>true</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>5</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + esc(p.Command) + `</Command>
      <Arguments>` + esc(p.Arguments) + `</Arguments>` + work + `
    </Exec>
  </Actions>
</Task>
`
}

// EncodeUTF16 encodes s as UTF-16LE with a byte order mark, the encoding schtasks
// /Create /XML expects (it rejects some UTF-8 files).
func EncodeUTF16(s string) []byte {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", "\r\n")
	u := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+2*len(u))
	out[0], out[1] = 0xFF, 0xFE
	for _, c := range u {
		out = append(out, byte(c), byte(c>>8))
	}
	return out
}

// WindowsJoinArgs quotes argv for CreateProcess the way the Microsoft C runtime parses
// it (the same rules as syscall.EscapeArg, available here on every GOOS for tests).
func WindowsJoinArgs(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = windowsEscapeArg(a)
	}
	return strings.Join(parts, " ")
}

func windowsEscapeArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			slashes++
		case '"':
			for ; slashes > 0; slashes-- {
				b.WriteByte('\\')
			}
			b.WriteByte('\\')
		default:
			slashes = 0
		}
		b.WriteByte(c)
	}
	for ; slashes > 0; slashes-- {
		b.WriteByte('\\')
	}
	b.WriteByte('"')
	return b.String()
}

func (schtasksBackend) conhost(in *Installer) string {
	root := in.Env.SystemRoot
	if root == "" {
		root = `C:\Windows`
	}
	return root + `\System32\conhost.exe`
}

func (s schtasksBackend) params(in *Installer, spec Spec) TaskXMLParams {
	args := append([]string{"--headless", spec.Exe}, runArgs(in.ConfigPath(spec.Instance), spec.ExtraArgs, in.LogPath(spec.Instance))...)
	desc := "Konsert worker daemon (konsertd), started at logon by `konsertd install`."
	if spec.Instance != "" {
		desc = "Konsert worker daemon (konsertd, " + spec.Instance + "), started at logon by `konsertd install`."
	}
	return TaskXMLParams{
		URI:         TaskName(spec.Instance),
		Description: desc,
		UserSID:     in.Env.UserSID,
		Command:     s.conhost(in),
		Arguments:   WindowsJoinArgs(args),
		WorkDir:     in.Env.Home,
		// A minute in the past, so the first keep-alive firing is due at once.
		StartBoundary: time.Now().Add(-time.Minute).Format("2006-01-02T15:04:05"),
	}
}

func (s schtasksBackend) render(in *Installer, spec Spec) (string, []byte, error) {
	if in.Env.UserSID == "" {
		return "", nil, fmt.Errorf("current user SID unknown")
	}
	return TaskName(spec.Instance), []byte(RenderTaskXML(s.params(in, spec))), nil
}

func (s schtasksBackend) xmlPath(in *Installer, instance string) string {
	return filepath.Join(in.Env.ConfigDir, unitBase(instance)+".task.xml")
}

func (s schtasksBackend) install(ctx context.Context, in *Installer, spec Spec) error {
	_, content, err := s.render(in, spec)
	if err != nil {
		return err
	}
	if !fileExists(s.conhost(in)) {
		return fmt.Errorf("%s not found: the hidden logon task needs conhost.exe --headless (Windows 10 1809 or later); "+
			"use `konsertd install --service` or run konsertd from a terminal instead", s.conhost(in))
	}
	if err := mkdirAll(in.Env.LogDir); err != nil {
		return err
	}
	name := TaskName(spec.Instance)
	xmlPath := s.xmlPath(in, spec.Instance)
	if err := writeFile(xmlPath, EncodeUTF16(string(content)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", xmlPath, err)
	}
	defer func() { _, _ = removeFile(xmlPath) }()
	// Stop a running previous version so the new definition and config take effect.
	_, _ = in.Runner.Run(ctx, "", "schtasks", "/End", "/TN", name)
	if _, err := in.Runner.Run(ctx, "", "schtasks", "/Create", "/TN", name, "/XML", xmlPath, "/F"); err != nil {
		return fmt.Errorf("register scheduled task: %w", err)
	}
	in.printf("Registered scheduled task %s (at logon of the current user, hidden, not elevated)\n", name)
	if spec.NoStart {
		in.printf("Not started now (--no-start); it starts at your next logon.\n")
		return nil
	}
	if _, err := in.Runner.Run(ctx, "", "schtasks", "/Run", "/TN", name); err != nil {
		return fmt.Errorf("start scheduled task: %w", err)
	}
	in.printf("Started %s; logs: %s\n", name, in.LogPath(spec.Instance))
	in.printf("(An InteractiveToken task only runs while you are logged on to Windows; from an SSH session with no desktop logon it starts at your next logon.)\n")
	return nil
}

func (s schtasksBackend) uninstall(ctx context.Context, in *Installer, instance string) error {
	name := TaskName(instance)
	if _, err := in.Runner.Run(ctx, "", "schtasks", "/Query", "/TN", name); err != nil {
		in.printf("No scheduled task %s\n", name)
	} else {
		_, _ = in.Runner.Run(ctx, "", "schtasks", "/End", "/TN", name)
		if _, err := in.Runner.Run(ctx, "", "schtasks", "/Delete", "/TN", name, "/F"); err != nil {
			return fmt.Errorf("delete scheduled task: %w", err)
		}
		in.printf("Stopped and deleted scheduled task %s\n", name)
	}
	_, _ = removeFile(s.xmlPath(in, instance))
	return nil
}

func (s schtasksBackend) status(ctx context.Context, in *Installer, instance string) (Status, error) {
	name := TaskName(instance)
	st := Status{
		Mechanism:      "Task Scheduler logon task",
		Name:           name,
		DefinitionPath: name,
		ConfigPath:     in.ConfigPath(instance),
		LogPath:        in.LogPath(instance),
	}
	out, err := in.Runner.Run(ctx, "", "schtasks", "/Query", "/TN", name, "/FO", "CSV", "/NH")
	if err != nil {
		return st, nil
	}
	st.Installed = true
	recs, _ := csv.NewReader(strings.NewReader(strings.TrimSpace(out))).ReadAll()
	for _, rec := range recs {
		if len(rec) >= 3 && strings.EqualFold(rec[0], name) {
			st.Detail = rec[2]
			st.Running = strings.EqualFold(rec[2], "Running")
		}
	}
	return st, nil
}
