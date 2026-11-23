package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/store"
)

func TestWorkerMachineIDs(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "mid.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Two rows sharing the legacy name tuple but with distinct machine IDs must
	// survive deduplication; a legacy row with no machine ID still collapses.
	now := time.Now().UTC()
	for _, w := range []store.Worker{
		{ID: "a", Name: "ubuntu", HostGroup: "ubuntu", OS: "linux", Arch: "amd64", Status: "online", LastSeenAt: now},
		{ID: "b", Name: "ubuntu", HostGroup: "ubuntu", OS: "linux", Arch: "amd64", Status: "online", LastSeenAt: now.Add(-time.Second)},
	} {
		if err := st.UpsertWorker(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetWorkerMachineID(ctx, "a", "mid-a"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWorkerMachineID(ctx, "b", "mid-b"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWorkerMachineID(ctx, "nope", "x"); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// Re-upserting a worker must not wipe its machine ID.
	if err := st.UpsertWorker(ctx, store.Worker{ID: "a", Name: "ubuntu", HostGroup: "ubuntu", OS: "linux", Arch: "amd64", Status: "online", LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	removed, err := st.DeduplicateWorkers(ctx)
	if err != nil || len(removed) != 0 {
		t.Fatalf("distinct machine IDs must not be deduplicated: removed=%v err=%v", removed, err)
	}
	mids, err := st.ListWorkerMachineIDs(ctx)
	if err != nil || mids["a"] != "mid-a" || mids["b"] != "mid-b" {
		t.Fatalf("machine IDs = %v err=%v", mids, err)
	}

	if err := st.UpsertWorker(ctx, store.Worker{ID: "c", Name: "devbox", HostGroup: "devbox", Status: "online", LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorker(ctx, store.Worker{ID: "d", Name: "devbox", HostGroup: "devbox", Status: "offline", LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	removed, err = st.DeduplicateWorkers(ctx)
	if err != nil || len(removed) != 1 || removed[0] != "d" {
		t.Fatalf("legacy duplicates should still collapse: removed=%v err=%v", removed, err)
	}
}
