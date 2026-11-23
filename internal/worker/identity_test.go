package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/BryanSant/konsert/internal/ids"
)

func TestResolveStableWorkerIDPersistsAcrossRestarts(t *testing.T) {
	dir := t.TempDir()

	id1 := ResolveStableWorkerID(dir, "my-box", "my-box", "host")
	id2 := ResolveStableWorkerID(dir, "my-box", "my-box", "host")
	if id1 == "" || id1 != id2 {
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

	// Verify file permissions are restrictive
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) < 2 {
		t.Fatalf("expected persisted ID files in %s, got %v err=%v", dir, entries, err)
	}
	info, err := os.Stat(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("expected 0600 perms on worker-id file, got %o", info.Mode().Perm())
	}
}

func TestResolveStableWorkerIDIsUUIDv7AndReplacesOlderIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, workerIDFilename("", "host"))
	if err := os.WriteFile(path, []byte("75cca373-29b6-4692-840b-bd942267949f\n"), 0o600); err != nil {
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
