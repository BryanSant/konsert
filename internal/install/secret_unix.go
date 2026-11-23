//go:build !windows

package install

import "os"

// restrictFile makes path readable and writable by its owner only.
func restrictFile(path string) error {
	return os.Chmod(path, 0o600)
}
