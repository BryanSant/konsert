//go:build darwin || linux

package install

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
)

// DefaultEnv returns the current user's environment.
func DefaultEnv() (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return Env{}, fmt.Errorf("cannot determine your home directory: %v", err)
	}
	env := Env{
		Home:       home,
		UID:        os.Getuid(),
		PATH:       CleanPATH(os.Getenv("PATH"), os.PathListSeparator),
		ManagedBin: filepath.Join(home, ".local", "bin", "konsertd"),
	}
	if u, err := user.Current(); err == nil {
		env.User = u.Username
	} else {
		env.User = os.Getenv("USER")
	}
	if runtime.GOOS == "darwin" {
		env.ConfigDir = filepath.Join(home, "Library", "Application Support", "konsert")
		env.LogDir = filepath.Join(home, "Library", "Logs", "konsert")
		env.UnitDir = filepath.Join(home, "Library", "LaunchAgents")
		return env, nil
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" || !filepath.IsAbs(cfg) {
		cfg = filepath.Join(home, ".config")
	}
	env.ConfigDir = filepath.Join(cfg, "konsert")
	env.LogDir = filepath.Join(home, ".konsert", "logs")
	env.UnitDir = filepath.Join(cfg, "systemd", "user")
	return env, nil
}

func defaultBackend() (backend, error) {
	if runtime.GOOS == "darwin" {
		return launchdBackend{}, nil
	}
	return systemdBackend{}, nil
}
