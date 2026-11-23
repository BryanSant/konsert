//go:build !windows

package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteSecretFileIsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "konsert")
	path := filepath.Join(dir, "konsertd.yaml")
	if err := WriteSecretFile(path, []byte("fleet_token: x\n")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %o, want 600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm()&0o077 != 0 {
		t.Errorf("config dir mode = %o, want no group/other access", di.Mode().Perm())
	}

	// Overwriting an existing, wider file still ends at 0600.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteSecretFile(path, []byte("fleet_token: y\n")); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("rewritten config mode = %o, want 600", fi.Mode().Perm())
	}
	if data, _ := os.ReadFile(path); string(data) != "fleet_token: y\n" {
		t.Errorf("content = %q", data)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".konsertd-*.tmp")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}
