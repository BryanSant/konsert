package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/config"
)

func TestResolveWorkdirRootRefusesHomeUnderTest(t *testing.T) {
	if _, err := resolveWorkdirRoot("", nil); err == nil {
		t.Fatal("resolveWorkdirRoot with no roots must fail under go test so ~/src is not created")
	}
}

func TestResolveWorkdirRootDefaultUsesHomeSrc(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	got, err := resolveWorkdirRootAllowHome("", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "src")
	gs, err1 := filepath.EvalSymlinks(got)
	ws, err2 := filepath.EvalSymlinks(want)
	if err1 != nil || err2 != nil || gs != ws {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNewDaemonUsesHermeticRootsFromEnv(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SYRE_ALLOWED_ROOTS", root)
	t.Setenv("SYRE_CHECKOUT_ROOTS", root)
	d := NewDaemon(config.WorkerConfig{Name: "hermetic"}, nil, discardLogger())
	if len(d.cfg.AllowedRoots) != 1 || d.cfg.AllowedRoots[0] != root {
		t.Fatalf("AllowedRoots = %v, want [%s]", d.cfg.AllowedRoots, root)
	}
	if len(d.cfg.CheckoutRoots) != 1 || d.cfg.CheckoutRoots[0] != root {
		t.Fatalf("CheckoutRoots = %v, want [%s]", d.cfg.CheckoutRoots, root)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	homeSrc := filepath.Join(home, "src")
	for _, p := range append(append([]string{}, d.cfg.AllowedRoots...), d.cfg.CheckoutRoots...) {
		if p == homeSrc || strings.HasPrefix(p, homeSrc+string(os.PathSeparator)) {
			t.Fatalf("daemon root %q is under ~/src", p)
		}
	}
}
