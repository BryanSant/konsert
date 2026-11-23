package sqlite

import (
	"context"
	"strings"
	"testing"
)

// TestPragmasAppliedOnEveryConnection verifies connection-scoped pragmas are set
// on fresh pool connections, not just the one that ran during Open.
func TestPragmasAppliedOnEveryConnection(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Allow more connections and hold several at once so the pool must dial new ones.
	st.db.SetMaxOpenConns(3)
	defer st.db.SetMaxOpenConns(1)
	for i := 0; i < 3; i++ {
		conn, err := st.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()

		var fk, busy int
		var journal, syncMode string
		if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&syncMode); err != nil {
			t.Fatal(err)
		}
		if fk != 1 || busy != 5000 || !strings.EqualFold(journal, "wal") || syncMode != "1" {
			t.Fatalf("conn %d: foreign_keys=%d busy_timeout=%d journal_mode=%s synchronous=%s", i, fk, busy, journal, syncMode)
		}
	}
}

func TestBuildDSN(t *testing.T) {
	if got := buildDSN("/var/x.db"); !strings.HasPrefix(got, "/var/x.db?_pragma=") || strings.Count(got, "_pragma=") != len(connPragmas) {
		t.Fatalf("unexpected DSN %q", got)
	}
	if got := buildDSN("file:x.db?mode=rwc"); !strings.HasPrefix(got, "file:x.db?mode=rwc&_pragma=") {
		t.Fatalf("unexpected DSN %q", got)
	}
}
