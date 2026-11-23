package install

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteSecretFile atomically writes data (which may hold the fleet token) to path so
// only the current user can read it: mode 0600 in a 0700 directory on Unix, a protected
// DACL granting only the user and SYSTEM on Windows. The permissions are applied to the
// temp file before any byte is written, so the secret is never briefly readable by others.
func WriteSecretFile(path string, data []byte) error {
	return writeRestricted(path, data, restrictFile)
}

// writeRestricted writes data atomically after applying restrict to the empty temp file.
func writeRestricted(path string, data []byte, restrict func(string) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".konsertd-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	cleanup := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := restrict(tmp); err != nil {
		return cleanup(fmt.Errorf("restrict %s: %w", tmp, err))
	}
	if _, err := f.Write(data); err != nil {
		return cleanup(fmt.Errorf("write %s: %w", tmp, err))
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}
