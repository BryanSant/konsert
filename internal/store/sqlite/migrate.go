package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

// ErrIncompatibleSchema means the database was created by an older pre-1.0 schema. Pre-1.0
// there are no upgrade paths: delete the database and start fresh. Each applied migration
// records the SHA-256 of its file, so editing 001_schema.sql in place also refuses
// databases created by the previous version of it.
var ErrIncompatibleSchema = errors.New("made by an older Syre; delete it and restart")

//go:embed migrations/*.sql
var migrationsFS embed.FS

func runMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL,
			checksum TEXT NOT NULL DEFAULT ''
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	// Pre-1.0 the schema is squashed into one migration and old databases are not upgraded.
	// A database that recorded a migration we no longer ship predates the current schema.
	// So does one whose recorded checksum differs from the file we ship (the schema was
	// edited in place), or whose schema_migrations predates checksums.
	sums := map[string]string{}
	for _, f := range files {
		content, err := fs.ReadFile(migrationsFS, "migrations/"+f)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", f, err)
		}
		sum := sha256.Sum256(content)
		sums[f] = hex.EncodeToString(sum[:])
	}
	rows, err := db.QueryContext(ctx, `SELECT name, checksum FROM schema_migrations`)
	if err != nil {
		if strings.Contains(err.Error(), "no such column") {
			return ErrIncompatibleSchema
		}
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	stale := false
	for rows.Next() {
		var name, sum string
		if err := rows.Scan(&name, &sum); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		if want, ok := sums[name]; !ok || sum != want {
			stale = true
		}
	}
	_ = rows.Close()
	if stale {
		return ErrIncompatibleSchema
	}

	for _, name := range files {
		var exists int
		err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations WHERE name = ?`, name).Scan(&exists)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists > 0 {
			continue
		}

		content, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx for %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(content)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (name, applied_at, checksum) VALUES (?, ?, ?)`,
			name, formatTime(time.Now()), sums[name],
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}
