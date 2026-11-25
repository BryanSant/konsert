package hubkey

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// localPath is this user's authorized_keys file on this machine.
func localPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("cannot locate the home directory")
	}
	return AuthorizedKeysPath(runtime.GOOS, isAdmin(), home, os.Getenv("ProgramData")), nil
}

// Install allows e on this machine (syred runs this over the user's SSH during the
// hub's bootstrap) and returns the file it changed.
func Install(e Entry) (string, error) {
	path, err := localPath()
	if err != nil {
		return "", err
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return path, err
	}
	content, err := Add(string(old), e)
	if err != nil {
		return path, err
	}
	if content == string(old) {
		return path, restrictKeysFile(path)
	}
	return path, write(path, content)
}

// Uninstall removes workerID from hubID's line on this machine (all of it when
// workerID is empty). It reports whether the file changed.
func Uninstall(hubID, workerID string) (string, bool, error) {
	path, err := localPath()
	if err != nil {
		return "", false, err
	}
	old, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, false, nil
	}
	if err != nil {
		return path, false, err
	}
	content, changed := Remove(string(old), hubID, workerID)
	if !changed {
		return path, false, nil
	}
	return path, true, write(path, content)
}

// write rewrites path in place (keeping its owner, mode and any link), creating it and
// its directory private to the user when missing. Windows' administrators file gets
// the DACL sshd insists on.
func write(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return restrictKeysFile(path)
}
