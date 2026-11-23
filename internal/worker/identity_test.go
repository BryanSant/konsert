package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/google/uuid"
)

func TestResolveStableWorkerIDPersistsAcrossRestarts(t *testing.T) {
	dir := t.TempDir()

	id1 := ResolveStableWorkerID(dir, "my-box", "my-box", "host")
	if !ids.IsUUIDv7(id1) {
		t.Fatalf("worker id %q is not a UUIDv7", id1)
	}
	if id2 := ResolveStableWorkerID(dir, "my-box", "my-box", "host"); id1 != id2 {
		t.Fatalf("expected identical persisted worker ID across restarts, got %q vs %q", id1, id2)
	}

	// Distinct container isolation on same host gets its own stable persisted ID.
	ctrID := ResolveStableWorkerID(dir, "my-box", "my-box", "container")
	if ctrID == "" || ctrID == id1 {
		t.Fatalf("expected distinct stable ID for container isolation, got %q", ctrID)
	}
	if ctrID2 := ResolveStableWorkerID(dir, "my-box", "my-box", "container"); ctrID2 != ctrID {
		t.Fatalf("expected persisted container ID to remain stable, got %q vs %q", ctrID, ctrID2)
	}

	// One identity file per daemon (no separate machine-id file).
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected exactly two identity files in %s, got %v err=%v", dir, entries, err)
	}
	info, err := os.Stat(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("expected 0600 perms on worker-id file, got %o", info.Mode().Perm())
	}
}

// A state dir copied into a cloned VM (different machine fingerprint) gets a fresh UUIDv7.
func TestResolveStableWorkerIDRegeneratesOnOSHashChange(t *testing.T) {
	dir := t.TempDir()
	machine := "aaaa"
	orig := machineSource
	machineSource = func() string { return machine }
	t.Cleanup(func() { machineSource = orig })

	first := ResolveStableWorkerID(dir, "", "", "host")
	if again := ResolveStableWorkerID(dir, "", "", "host"); again != first {
		t.Fatalf("identity not stable on the same machine: %q then %q", first, again)
	}
	machine = "bbbb" // the state dir now lives on a clone
	cloned := ResolveStableWorkerID(dir, "", "", "host")
	if cloned == first || !ids.IsUUIDv7(cloned) {
		t.Fatalf("worker id must be a fresh UUIDv7 when the OS machine identity changes: %q then %q", first, cloned)
	}
	if again := ResolveStableWorkerID(dir, "", "", "host"); again != cloned {
		t.Fatal("regenerated worker id not persisted")
	}
}

// Anything that is not a UUIDv7 identity record is replaced.
func TestResolveStableWorkerIDReplacesInvalidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, workerIDFilename("", "host"))
	if err := os.WriteFile(path, []byte("m-7qk3m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := ResolveStableWorkerID(dir, "", "", "host")
	if !ids.IsUUIDv7(id) {
		t.Fatalf("worker id %q is not a UUIDv7", id)
	}
	if again := ResolveStableWorkerID(dir, "", "", "host"); again != id {
		t.Fatalf("replacement id not persisted: %q then %q", id, again)
	}
}

func TestDeterministicWorkerIDIsStableUUID(t *testing.T) {
	a := deterministicWorkerID("box", "grp", "host")
	u, err := uuid.Parse(a)
	if err != nil || u.Version() != 8 || u.Variant() != uuid.RFC4122 {
		t.Fatalf("fallback id %q is not a v8 RFC 9562 UUID (err=%v)", a, err)
	}
	if b := deterministicWorkerID("box", "grp", "host"); b != a {
		t.Fatalf("fallback id not deterministic: %q vs %q", a, b)
	}
}

func TestHelloCarriesWorkerID(t *testing.T) {
	hub := newFakeHub(t)
	d, _ := startTestDaemon(t, hub, nil)
	c := hub.accept(t)
	env := c.waitFor(t, 10*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))
	hello, err := protocol.DecodePayload[protocol.WorkerHelloPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Worker.ID == "" || hello.Worker.ID != d.WorkerID() {
		t.Fatalf("hello id = %q, daemon = %q", hello.Worker.ID, d.WorkerID())
	}
}

// An identity written where no fingerprint was available is replaced once one is.
func TestResolveStableWorkerIDRequiresMatchingHash(t *testing.T) {
	dir := t.TempDir()
	orig := machineSource
	t.Cleanup(func() { machineSource = orig })
	machineSource = func() string { return "" }
	unhashed := ResolveStableWorkerID(dir, "", "", "host")
	machineSource = func() string { return "hw-uuid-1" }
	if got := ResolveStableWorkerID(dir, "", "", "host"); got == unhashed {
		t.Fatal("an identity without a machine fingerprint was reused once one became available")
	}
}
