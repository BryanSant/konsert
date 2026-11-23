package install

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// launchdBackend installs a per-user LaunchAgent in ~/Library/LaunchAgents, loaded into
// the gui/<uid> domain so the daemon has the user's keychain and login session.
type launchdBackend struct{}

// LaunchdLabel is the launchd label of an instance.
func LaunchdLabel(instance string) string {
	if instance == "" {
		return LaunchdLabelPrefix
	}
	return LaunchdLabelPrefix + "." + instance
}

func (launchdBackend) plistPath(in *Installer, instance string) string {
	return filepath.Join(in.Env.UnitDir, LaunchdLabel(instance)+".plist")
}

func (in *Installer) domain() string { return "gui/" + strconv.Itoa(in.Env.UID) }

// RenderPlist renders the LaunchAgent plist.
func RenderPlist(label string, argv []string, env map[string]string, workDir, logPath string) []byte {
	var b bytes.Buffer
	esc := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + esc(label) + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range argv {
		b.WriteString("    <string>" + esc(a) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	if len(env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, k := range sortedKeys(env) {
			b.WriteString("    <key>" + esc(k) + "</key>\n    <string>" + esc(env[k]) + "</string>\n")
		}
		b.WriteString("  </dict>\n")
	}
	if workDir != "" {
		b.WriteString("  <key>WorkingDirectory</key>\n  <string>" + esc(workDir) + "</string>\n")
	}
	b.WriteString(`  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Interactive</string>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>StandardOutPath</key>
  <string>` + esc(logPath) + `</string>
  <key>StandardErrorPath</key>
  <string>` + esc(logPath) + `</string>
</dict>
</plist>
`)
	return b.Bytes()
}

func (l launchdBackend) render(in *Installer, spec Spec) (string, []byte, error) {
	argv := append([]string{spec.Exe}, runArgs(in.ConfigPath(spec.Instance), spec.ExtraArgs, "")...)
	env := map[string]string{}
	if in.Env.PATH != "" {
		env["PATH"] = in.Env.PATH
	}
	return l.plistPath(in, spec.Instance), RenderPlist(LaunchdLabel(spec.Instance), argv, env, in.Env.Home, in.LogPath(spec.Instance)), nil
}

func (l launchdBackend) install(ctx context.Context, in *Installer, spec Spec) error {
	path, content, err := l.render(in, spec)
	if err != nil {
		return err
	}
	if err := mkdirAll(in.Env.LogDir); err != nil {
		return err
	}
	label := LaunchdLabel(spec.Instance)
	// Unload a previous version first; bootstrap refuses an already-loaded label.
	_, _ = in.Runner.Run(ctx, "", "launchctl", "bootout", in.domain()+"/"+label)
	if err := writeFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	in.printf("Wrote LaunchAgent %s\n", path)
	if spec.NoStart {
		in.printf("Not loaded now (--no-start); launchd starts it at your next login.\n")
		return nil
	}
	if _, err := in.Runner.Run(ctx, "", "launchctl", "bootstrap", in.domain(), path); err != nil {
		return fmt.Errorf("load LaunchAgent: %w", err)
	}
	in.printf("Loaded %s into %s; logs: %s\n", label, in.domain(), in.LogPath(spec.Instance))
	return nil
}

func (l launchdBackend) uninstall(ctx context.Context, in *Installer, instance string) error {
	label := LaunchdLabel(instance)
	if _, err := in.Runner.Run(ctx, "", "launchctl", "bootout", in.domain()+"/"+label); err == nil {
		in.printf("Stopped and unloaded %s\n", label)
	}
	path := l.plistPath(in, instance)
	removed, err := removeFile(path)
	if err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	if removed {
		in.printf("Removed %s\n", path)
	} else {
		in.printf("No LaunchAgent at %s\n", path)
	}
	return nil
}

var (
	launchdStateRe = regexp.MustCompile(`(?m)^\s*state = (\S+)`)
	launchdPIDRe   = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)
)

func (l launchdBackend) status(ctx context.Context, in *Installer, instance string) (Status, error) {
	label := LaunchdLabel(instance)
	st := Status{
		Mechanism:      "launchd LaunchAgent (" + in.domain() + ")",
		Name:           label,
		DefinitionPath: l.plistPath(in, instance),
		ConfigPath:     in.ConfigPath(instance),
		LogPath:        in.LogPath(instance),
	}
	st.Installed = fileExists(st.DefinitionPath)
	out, err := in.Runner.Run(ctx, "", "launchctl", "print", in.domain()+"/"+label)
	if err != nil {
		st.Detail = "not loaded"
		return st, nil
	}
	if m := launchdStateRe.FindStringSubmatch(out); m != nil {
		st.Detail = "state " + m[1]
		st.Running = m[1] == "running"
	}
	if m := launchdPIDRe.FindStringSubmatch(out); m != nil {
		st.PID, _ = strconv.Atoi(m[1])
	}
	if !st.Installed && strings.TrimSpace(out) != "" {
		st.Detail += " (loaded, but the plist file is missing)"
	}
	return st, nil
}
