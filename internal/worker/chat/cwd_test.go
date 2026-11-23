package chat

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.Mkdir(filepath.Join(home, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{"": home, "~": home, "~/src": filepath.Join(home, "src")} {
		got, err := ResolveCwd(in)
		if err != nil || got != want {
			t.Errorf("ResolveCwd(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// Seen live: the orchestrator chat (cwd "~") failed with "claude is not installed".
	if _, err := ResolveCwd(filepath.Join(home, "missing")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing dir error = %v", err)
	}
}

func TestChdirFailureIsNotNotInstalled(t *testing.T) {
	chdir := &fs.PathError{Op: "chdir", Path: "/nope", Err: fs.ErrNotExist}
	if IsNotInstalled(chdir) {
		t.Fatal("a missing working directory was classified as a missing binary")
	}
	if !IsNotInstalled(exec.ErrNotFound) || !IsNotInstalled(&fs.PathError{Op: "fork/exec", Path: "/opt/claude", Err: fs.ErrNotExist}) {
		t.Fatal("a missing binary must still be not-installed")
	}
}
