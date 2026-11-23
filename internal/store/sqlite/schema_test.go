package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/store"
)

func TestOpenRefusesOlderSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", buildDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL);
		INSERT INTO schema_migrations VALUES ('001_init.sql', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(ctx, path); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("Open(old db) err = %v, want ErrIncompatibleSchema", err)
	}
}

func TestNewIDAndDefaults(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tax, err := st.GetTaxonomy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tax.Workspaces) != 1 || tax.Workspaces[0].ID != store.DefaultWorkspaceID || len(tax.Projects) != 1 || tax.Projects[0].ID != store.DefaultProjectID {
		t.Fatalf("defaults = %+v", tax)
	}
	for kind := range idTables {
		id, err := st.NewID(ctx, kind)
		if err != nil || !ids.Valid(kind, id) {
			t.Fatalf("NewID(%s) = %q, %v", kind, id, err)
		}
	}
	if _, err := st.NewID(ctx, "NOPE"); err == nil {
		t.Fatal("NewID(unknown kind) succeeded")
	}
}
