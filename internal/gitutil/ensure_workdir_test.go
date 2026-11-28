package gitutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureHomeWorkdir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	exists := func(rel string) bool {
		st, err := os.Stat(filepath.Join(home, filepath.FromSlash(rel)))
		return err == nil && st.IsDir()
	}

	if err := EnsureHomeWorkdir("~/src/new", nil); err != nil || !exists("src/new") {
		t.Fatalf("no roots: created=%v err=%v", exists("src/new"), err)
	}
	// Inside an allowed root: created; outside: left alone (validation refuses it later).
	roots := []string{filepath.Join(home, "work")}
	if err := os.MkdirAll(roots[0], 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureHomeWorkdir("~/work/a", roots); err != nil || !exists("work/a") {
		t.Fatalf("inside root: created=%v err=%v", exists("work/a"), err)
	}
	if err := EnsureHomeWorkdir("~/other", roots); err != nil || exists("other") {
		t.Fatalf("outside root: created=%v err=%v", exists("other"), err)
	}
	// Never escapes home or touches absolute paths.
	for _, w := range []string{"~/../escape", "~/", "/tmp/abs", "~", "src"} {
		if err := EnsureHomeWorkdir(w, nil); err != nil {
			t.Fatalf("%q: %v", w, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(home), "escape")); err == nil {
		t.Fatal("created a folder outside home")
	}
}
