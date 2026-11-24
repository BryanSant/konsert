// Package install registers konsertd to start automatically as the current user: a
// systemd user unit on Linux, a LaunchAgent on macOS, and a logon Scheduled Task (or,
// with --service, a native service) on Windows.
//
// Everything that touches the system goes through a Runner (external commands) and an
// Env (home, config and data directories), so the generators and the install/uninstall
// sequences are unit-tested without calling launchctl, systemctl or schtasks.
package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BaseName is the name of the default (unnamed) instance: the unit, task and service
// name, and the stem of its config and log files.
const BaseName = "konsertd"

// LaunchdLabelPrefix is the launchd label of the default instance.
const LaunchdLabelPrefix = "dev.konsert.konsertd"

// Runner runs an external command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, stdin string, name string, args ...string) (string, error)
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	s := out.String()
	if err != nil {
		msg := strings.TrimSpace(s)
		if msg == "" {
			return s, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return s, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return s, nil
}

// Env is the per-user environment the installer works in. Tests point every directory
// at a temp dir.
type Env struct {
	Home string
	// ConfigDir holds the per-user konsertd config (0600): ~/.config/konsert on Linux,
	// ~/Library/Application Support/konsert on macOS, %LOCALAPPDATA%\konsert on Windows.
	ConfigDir string
	// LogDir holds log files for mechanisms without a journal (macOS, Windows).
	LogDir string
	// UnitDir is where definitions live: ~/.config/systemd/user, ~/Library/LaunchAgents,
	// or a staging dir for the task XML on Windows.
	UnitDir string
	// UID is the numeric user ID (launchd gui/<uid> domain); User is the login name.
	UID  int
	User string
	// UserSID is the Windows account SID the logon task runs as.
	UserSID string
	// SystemRoot is %SystemRoot% (conhost.exe lives in its System32).
	SystemRoot string
	// PATH is persisted into launchd/systemd definitions, whose default PATH lacks
	// Homebrew, ~/.local/bin and friends where agent CLIs live.
	PATH string
}

// Spec describes what to register.
type Spec struct {
	// Instance is "" for the default daemon, or a slug from InstanceFor(--name).
	Instance string
	// Exe is the absolute path of the konsertd binary.
	Exe string
	// Config is the YAML written to ConfigPath (0600 / user-only ACL).
	Config []byte
	// ExtraArgs are run flags without a config-file equivalent, appended to
	// `run --config <path>`.
	ExtraArgs []string
	// NoStart registers without starting now.
	NoStart bool
	// NoLinger skips `loginctl enable-linger` on Linux.
	NoLinger bool
}

// Status reports an installed instance.
type Status struct {
	Mechanism      string
	Name           string
	DefinitionPath string
	ConfigPath     string
	LogPath        string
	Installed      bool
	Running        bool
	PID            int
	Detail         string
}

// Installer installs, removes and inspects konsertd registrations.
type Installer struct {
	Runner Runner
	Env    Env
	Out    io.Writer
}

// backend is one OS mechanism.
type backend interface {
	install(ctx context.Context, in *Installer, spec Spec) error
	uninstall(ctx context.Context, in *Installer, instance string) error
	status(ctx context.Context, in *Installer, instance string) (Status, error)
	// render returns the definition file content for --dry-run.
	render(in *Installer, spec Spec) (path string, content []byte, err error)
}

// InstanceFor maps a worker --name to an instance slug. Like the worker-id file, the
// default daemon (no name, or the hostname) gets the plain names; any other name gets a
// distinct, filesystem- and service-safe suffix.
func InstanceFor(name, hostname string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, strings.TrimSpace(hostname)) {
		return ""
	}
	if short, _, ok := strings.Cut(hostname, "."); ok && strings.EqualFold(name, short) {
		return ""
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	return slug
}

// unitBase is the instance's base name: konsertd or konsertd-<instance>.
func unitBase(instance string) string {
	if instance == "" {
		return BaseName
	}
	return BaseName + "-" + instance
}

// ConfigPath is where the instance's persisted config lives.
func (in *Installer) ConfigPath(instance string) string {
	return filepath.Join(in.Env.ConfigDir, unitBase(instance)+".yaml")
}

// LogPath is the instance's log file (unused on Linux, which logs to the journal).
func (in *Installer) LogPath(instance string) string {
	return filepath.Join(in.Env.LogDir, unitBase(instance)+".log")
}

// runArgs is the argv after the binary: run --config <path> [extra...].
func runArgs(configPath string, extra []string, logPath string) []string {
	args := []string{"run", "--config", configPath}
	if logPath != "" {
		args = append(args, "--log-file", logPath)
	}
	return append(args, extra...)
}

func (in *Installer) printf(format string, a ...any) {
	if in.Out != nil {
		fmt.Fprintf(in.Out, format, a...)
	}
}

// Install writes the config and registers the instance with backend b.
func (in *Installer) install(ctx context.Context, b backend, spec Spec) error {
	if !filepath.IsAbs(spec.Exe) {
		return fmt.Errorf("konsertd binary path %q is not absolute", spec.Exe)
	}
	if err := WriteSecretFile(in.ConfigPath(spec.Instance), spec.Config); err != nil {
		return err
	}
	in.printf("Wrote config %s (readable only by you)\n", in.ConfigPath(spec.Instance))
	return b.install(ctx, in, spec)
}

// Install registers spec with this OS's default mechanism.
func (in *Installer) Install(ctx context.Context, spec Spec) error {
	b, err := defaultBackend()
	if err != nil {
		return err
	}
	return in.install(ctx, b, spec)
}

// Uninstall removes the instance and its config file.
func (in *Installer) Uninstall(ctx context.Context, instance string) error {
	b, err := defaultBackend()
	if err != nil {
		return err
	}
	return in.uninstall(ctx, b, instance)
}

func (in *Installer) uninstall(ctx context.Context, b backend, instance string) error {
	if err := b.uninstall(ctx, in, instance); err != nil {
		return err
	}
	return in.removeConfig(instance)
}

func (in *Installer) removeConfig(instance string) error {
	p := in.ConfigPath(instance)
	if err := os.Remove(p); err == nil {
		in.printf("Removed config %s\n", p)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", p, err)
	}
	return nil
}

// Status inspects the instance.
func (in *Installer) Status(ctx context.Context, instance string) (Status, error) {
	b, err := defaultBackend()
	if err != nil {
		return Status{}, err
	}
	return b.status(ctx, in, instance)
}

// Render returns the definition that Install would write, for --dry-run.
func (in *Installer) Render(spec Spec) (string, []byte, error) {
	b, err := defaultBackend()
	if err != nil {
		return "", nil, err
	}
	return b.render(in, spec)
}

// writeFile writes a non-secret definition file atomically.
func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// removeFile deletes path, ignoring a missing file.
func removeFile(path string) (bool, error) {
	err := os.Remove(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
