//go:build !windows

package hubkey

import "os"

func isAdmin() bool { return false }

// restrictKeysFile keeps authorized_keys private (sshd's StrictModes rejects a file
// others can write).
func restrictKeysFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return os.Chmod(path, 0o600)
	}
	return nil
}
