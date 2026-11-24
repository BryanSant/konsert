package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

func unauthorizedHub(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, `{"error":"invalid machine credential"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// isolateState points konsertd's state directory at a fresh temp dir.
func isolateState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv(StateDirEnv, filepath.Join(dir, ".konsert"))
	return filepath.Join(dir, ".konsert")
}

func TestDaemonExitsClearlyWithoutCredential(t *testing.T) {
	isolateState(t)
	srv, hits := unauthorizedHub(t)
	d := NewDaemon(config.WorkerConfig{
		ControllerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/worker",
		WorkerID:      "auth-test", Name: "auth-test", Tags: map[string]string{},
	}, drivers.DefaultRegistry, discardLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := d.Run(ctx)
	if !errors.Is(err, ErrCredentialRequired) {
		t.Fatalf("expected ErrCredentialRequired, got %v", err)
	}
	if !strings.Contains(err.Error(), "konsertd join") {
		t.Fatalf("the error must point at konsertd join: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("expected a single attempt without a credential, got %d", n)
	}
}

func TestDaemonBacksOffSlowlyOnRejectedCredential(t *testing.T) {
	isolateState(t)
	srv, hits := unauthorizedHub(t)
	d := NewDaemon(config.WorkerConfig{
		ControllerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/worker",
		Credential:    "kmc_wrong", WorkerID: "auth-test-2", Name: "auth-test-2", Tags: map[string]string{},
	}, drivers.DefaultRegistry, discardLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = d.Run(ctx)
	if n := hits.Load(); n != 1 {
		t.Fatalf("a rejected credential must wait the maximum backoff before retrying; got %d attempts in 3s", n)
	}
}

// A daemon started before the machine joined picks up the credential `konsertd join`
// saves, instead of exiting.
func TestDaemonUsesCredentialSavedWhileRunning(t *testing.T) {
	isolateState(t)
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	d := NewDaemon(config.WorkerConfig{
		ControllerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/worker",
		WorkerID:      "auth-test-3", Name: "auth-test-3", Tags: map[string]string{},
	}, drivers.DefaultRegistry, discardLogger())
	if err := SaveCredential("", "auth-test-3", "kmc_joined"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	_ = d.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != "" || seen[1] != "Bearer kmc_joined" {
		t.Fatalf("authorization headers = %q, want none, then the saved credential", seen)
	}
}

func TestCredentialFile(t *testing.T) {
	state := isolateState(t)
	const id = "0196a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	if _, err := LoadCredential("", id); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("missing credential: %v", err)
	}
	if err := SaveCredential("", id, "has space"); err == nil {
		t.Fatal("saved a credential with whitespace")
	}
	if err := SaveCredential("", id, "kmc_abc"); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadCredential("", id); err != nil || got != "kmc_abc" {
		t.Fatalf("load = %q %v", got, err)
	}
	path := filepath.Join(state, "credentials", id)
	if CredentialPath("", id) != path {
		t.Fatalf("path = %q, want %q", CredentialPath("", id), path)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("credential file mode = %v %v, want 0600", fi.Mode().Perm(), err)
		}
	}
	for _, bad := range []string{"", "../x", `a\b`, "a/b", ".."} {
		if CredentialPath("", bad) != "" {
			t.Errorf("CredentialPath(%q) accepted", bad)
		}
	}
	// A daemon loads the saved credential.
	d := NewDaemon(config.WorkerConfig{WorkerID: id, Name: "rot", Tags: map[string]string{}}, drivers.DefaultRegistry, discardLogger())
	if d.credential() != "kmc_abc" {
		t.Fatalf("daemon did not load the saved credential: %q", d.credential())
	}
}
