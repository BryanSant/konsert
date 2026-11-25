//go:build windows

package install

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultEnv returns the current user's environment. Config and logs go under
// %LOCALAPPDATA%\syre, which is private to the user and does not roam.
func DefaultEnv() (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return Env{}, fmt.Errorf("cannot determine your profile directory: %v", err)
	}
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		local = filepath.Join(home, "AppData", "Local")
	}
	sid, err := currentUserSID()
	if err != nil {
		return Env{}, err
	}
	dir := filepath.Join(local, "syre")
	return Env{
		Home:       home,
		ConfigDir:  dir,
		LogDir:     filepath.Join(dir, "logs"),
		UnitDir:    dir,
		User:       os.Getenv("USERNAME"),
		UserSID:    sid,
		SystemRoot: os.Getenv("SystemRoot"),
		Elevated:   IsElevated(),
		PATH:       CleanPATH(os.Getenv("PATH"), os.PathListSeparator),
		ManagedBin: filepath.Join(local, "Programs", "syred", "syred.exe"),
	}, nil
}

func defaultBackend() (backend, error) { return schtasksBackend{}, nil }
