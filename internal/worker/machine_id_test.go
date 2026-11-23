package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/google/uuid"
)

func TestResolveMachineIDStableAndPerIdentity(t *testing.T) {
	dir := t.TempDir()
	a := ResolveMachineID(dir, "", "host")
	if _, err := uuid.Parse(a); err != nil {
		t.Fatalf("machine id %q is not a UUID", a)
	}
	if again := ResolveMachineID(dir, "", "host"); again != a {
		t.Fatalf("machine id not stable across restarts: %q vs %q", a, again)
	}
	if other := ResolveMachineID(dir, "box-2", "container"); other == a || other == "" {
		t.Fatalf("a separately named daemon must get its own machine id, got %q", other)
	}
	info, err := os.Stat(filepath.Join(dir, "machine-id"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("machine-id perms = %o, want 0600", info.Mode().Perm())
	}
}

// A state dir copied into a cloned VM (different OS machine-id) gets a fresh machine id.
func TestResolveMachineIDRegeneratesOnClone(t *testing.T) {
	dir := t.TempDir()
	osID := filepath.Join(t.TempDir(), "machine-id")
	orig := osMachineIDPaths
	osMachineIDPaths = []string{osID}
	t.Cleanup(func() { osMachineIDPaths = orig })

	if err := os.WriteFile(osID, []byte("aaaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := ResolveMachineID(dir, "", "host")
	if err := os.WriteFile(osID, []byte("bbbb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cloned := ResolveMachineID(dir, "", "host")
	if cloned == first {
		t.Fatal("machine id must change when the OS machine identity changes")
	}
	if again := ResolveMachineID(dir, "", "host"); again != cloned {
		t.Fatal("regenerated machine id not persisted")
	}
}

func TestHelloCarriesMachineID(t *testing.T) {
	hub := newFakeHub(t)
	d, _ := startTestDaemon(t, hub, nil)
	c := hub.accept(t)
	env := c.waitFor(t, 10*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))
	hello, err := protocol.DecodePayload[protocol.WorkerHelloPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Worker.MachineID == "" || hello.Worker.MachineID != d.MachineID() {
		t.Fatalf("hello machine_id = %q, daemon = %q", hello.Worker.MachineID, d.MachineID())
	}
}
