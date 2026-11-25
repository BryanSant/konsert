package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A daemon the hub removed uninstalls its own registration (KON-AV0YT). The usual
// sequence stops the service first, which would kill the very process running it, or
// (launchd) block until it exits. The self-removal sequences instead make the
// registration inert first (files gone, task deleted, unit disabled) and stop the job
// last, without waiting, so the daemon exits on its own and nothing restarts it.

// selfUninstaller is implemented by backends that can remove the registration of the
// daemon running the call.
type selfUninstaller interface {
	uninstallSelf(ctx context.Context, in *Installer, instance string) error
}

// UninstallSelf removes the instance and its config file from inside the daemon that
// instance runs. It returns once nothing will start the daemon again; the daemon should
// then exit (launchd and systemd also stop it shortly after).
func (in *Installer) UninstallSelf(ctx context.Context, instance string) error {
	b, err := defaultBackend()
	if err != nil {
		return err
	}
	return in.uninstallSelf(ctx, b, instance)
}

func (in *Installer) uninstallSelf(ctx context.Context, b backend, instance string) error {
	su, ok := b.(selfUninstaller)
	if !ok {
		return in.uninstall(ctx, b, instance)
	}
	if err := su.uninstallSelf(ctx, in, instance); err != nil {
		return err
	}
	return in.removeConfig(instance)
}

// spawn starts a command without waiting for it, detached from this process (its own
// session), so it survives this process being stopped.
func (in *Installer) spawn(name string, args ...string) error {
	if in.Spawn != nil {
		return in.Spawn(name, args...)
	}
	return spawnDetached(name, args...)
}

// launchd: the plist goes first, then `launchctl bootout` runs detached. bootout stops
// the job (this daemon) and unloads it; run synchronously it would wait for this very
// process to exit.
func (l launchdBackend) uninstallSelf(_ context.Context, in *Installer, instance string) error {
	path := l.plistPath(in, instance)
	removed, err := removeFile(path)
	if err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	if removed {
		in.printf("Removed %s\n", path)
	}
	label := LaunchdLabel(instance)
	if err := in.spawn("launchctl", "bootout", in.domain()+"/"+label); err != nil {
		return fmt.Errorf("unload LaunchAgent: %w", err)
	}
	in.printf("Unloading %s (launchctl bootout)\n", label)
	return nil
}

// systemd: disable (no --now), remove the unit, reload, then queue the stop without
// waiting. A unit with a pending stop job is not restarted when its process exits.
func (s systemdBackend) uninstallSelf(ctx context.Context, in *Installer, instance string) error {
	unit := SystemdUnit(instance)
	path := s.unitPath(in, instance)
	manager := s.checkUserManager(ctx, in) == nil
	if manager {
		if _, err := in.Runner.Run(ctx, "", "systemctl", "--user", "disable", unit); err == nil {
			in.printf("Disabled %s\n", unit)
		}
	}
	removed, err := removeFile(path)
	if err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	if removed {
		in.printf("Removed %s\n", path)
	}
	if !manager {
		return nil
	}
	_, _ = in.Runner.Run(ctx, "", "systemctl", "--user", "daemon-reload")
	if _, err := in.Runner.Run(ctx, "", "systemctl", "--user", "stop", "--no-block", unit); err != nil {
		return fmt.Errorf("stop %s: %w", unit, err)
	}
	in.printf("Stopping %s\n", unit)
	return nil
}

// schtasks: deleting the task removes both triggers (logon and the every-minute
// keep-alive) without ending the running instance (no /End: that would kill this
// process before it finished).
func (s schtasksBackend) uninstallSelf(ctx context.Context, in *Installer, instance string) error {
	name := TaskName(instance)
	if _, err := in.Runner.Run(ctx, "", "schtasks", "/Query", "/TN", name); err != nil {
		in.printf("No scheduled task %s\n", name)
	} else {
		if _, err := in.Runner.Run(ctx, "", "schtasks", "/Delete", "/TN", name, "/F"); err != nil {
			return fmt.Errorf("delete scheduled task: %w", err)
		}
		in.printf("Deleted scheduled task %s\n", name)
	}
	_, _ = removeFile(s.xmlPath(in, instance))
	return nil
}

// precheck lets a backend refuse before Install writes anything.
type prechecker interface {
	precheck(ctx context.Context, in *Installer) error
}

// The config file is shared by every host on a shared home directory (a container and
// its host): a container without a systemd user manager must fail before it overwrites
// its host's config.
func (s systemdBackend) precheck(ctx context.Context, in *Installer) error {
	return s.checkUserManager(ctx, in)
}

// InstalledConfig is one instance config `konsertd install` wrote.
type InstalledConfig struct {
	Instance string
	Path     string
}

// InstalledConfigs lists the instance configs in the config directory (konsertd.yaml,
// konsertd-<instance>.yaml): every instance `konsertd install` registered and
// `konsertd uninstall` has not removed.
func (in *Installer) InstalledConfigs() ([]InstalledConfig, error) {
	entries, err := os.ReadDir(in.Env.ConfigDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []InstalledConfig
	for _, e := range entries {
		instance, ok := instanceOfConfig(e.Name())
		if e.IsDir() || !ok {
			continue
		}
		out = append(out, InstalledConfig{Instance: instance, Path: filepath.Join(in.Env.ConfigDir, e.Name())})
	}
	return out, nil
}

// instanceOfConfig maps an instance config file name (konsertd.yaml,
// konsertd-<instance>.yaml) to its instance.
func instanceOfConfig(name string) (string, bool) {
	stem, ok := strings.CutSuffix(name, ".yaml")
	switch {
	case !ok:
		return "", false
	case stem == BaseName:
		return "", true
	case strings.HasPrefix(stem, BaseName+"-"):
		return strings.TrimPrefix(stem, BaseName+"-"), true
	}
	return "", false
}

// NohupBase is the stem of the nohup daemon files for a worker name, in the state dir:
// <base>.yaml (its config) and <base>.pid. Keyed by name, so hosts sharing a home
// directory (a container and its siblings) never share them.
func NohupBase(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	slug := strings.Trim(b.String(), "-_.")
	if slug == "" {
		slug = "worker"
	}
	return "konsertd-nohup-" + slug
}
