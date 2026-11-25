package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// prepareDBFile creates the SQLite database file with mode 0600 if it does not exist
// yet. SQLite creates its -wal and -shm files with the database file's permissions,
// so this keeps all three private regardless of the process umask.
func prepareDBFile(path string) error {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create database directory: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case err == nil:
		return f.Close()
	case errors.Is(err, fs.ErrExist):
		return nil
	default:
		return fmt.Errorf("create database file: %w", err)
	}
}

// secureDBFiles restricts the database and its -wal/-shm companions to 0600. The DB
// holds the LLM API key (if set from the UI) and session data in plaintext.
func secureDBFiles(path string) error {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil
	}
	var errs []error
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if info.Mode().Perm() != 0o600 {
			if err := os.Chmod(p, 0o600); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
