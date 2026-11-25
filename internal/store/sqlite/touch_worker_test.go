package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

// A heartbeat racing a fleet removal must not resurrect the deleted worker row.
func TestTouchWorkerNeverRecreatesDeletedRow(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "touch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	old := time.Now().UTC().Add(-time.Hour)
	if err := st.UpsertWorker(ctx, store.Worker{ID: "w1", Name: "box", Status: "offline", LastSeenAt: old}); err != nil {
		t.Fatal(err)
	}
	seen := time.Now().UTC()
	if err := st.TouchWorker(ctx, "w1", seen); err != nil {
		t.Fatal(err)
	}
	w, err := st.GetWorker(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != "online" || !w.LastSeenAt.After(old) {
		t.Fatalf("touch did not mark worker online/seen: %+v", w)
	}

	if err := st.DeleteWorker(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchWorker(ctx, "w1", time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("touch on deleted worker: got %v, want ErrNotFound", err)
	}
	if _, err := st.GetWorker(ctx, "w1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted worker was recreated: %v", err)
	}
}
