package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/store"
)

// idTables maps each entity kind to its table (id UUIDv7 primary key + unique sid).
var idTables = map[ids.Kind]string{
	ids.Task:      "tasks",
	ids.Chat:      "chat_sessions",
	ids.Machine:   "workers",
	ids.Workspace: "workspaces",
	ids.Project:   "projects",
	ids.Repo:      "repos",
	ids.Worktree:  "worktrees",
	ids.Space:     "spaces",
	ids.Hold:      "holds",
	ids.Request:   "user_requests",
	ids.Question:  "questions",
	ids.Override:  "override_requests",
	ids.Veto:      "vetoes",
	ids.Review:    "reviews",
}

// NewSID mints a unique short ID of the given kind, checking candidates against its
// table's sid column.
func (s *SQLiteStore) NewSID(ctx context.Context, kind ids.Kind) (string, error) {
	table, ok := idTables[kind]
	if !ok {
		return "", fmt.Errorf("unknown id kind %q", kind)
	}
	return ids.New(kind, func(sid string) (bool, error) {
		var n int
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM `+table+` WHERE sid = ?`, sid).Scan(&n)
		return n > 0, err
	})
}

// ResolveID returns the id of the kind's record named by ref, which is either its id or its
// sid. A well-formed sid of another kind, or a ref that matches nothing, is ErrNotFound.
func (s *SQLiteStore) ResolveID(ctx context.Context, kind ids.Kind, ref string) (string, error) {
	table, ok := idTables[kind]
	if !ok {
		return "", fmt.Errorf("unknown id kind %q", kind)
	}
	if ref == "" {
		return "", store.ErrNotFound
	}
	col := "id"
	if k, _, ok := ids.Parse(ref); ok {
		if k != kind {
			return "", store.ErrNotFound
		}
		col = "sid"
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM `+table+` WHERE `+col+` = ?`, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	return id, err
}

// assignIDs fills an empty id with a new UUIDv7 and an empty sid with a fresh short ID.
func (s *SQLiteStore) assignIDs(ctx context.Context, kind ids.Kind, id, sid *string) error {
	if *id == "" {
		*id = ids.UUID()
	}
	if *sid == "" {
		v, err := s.NewSID(ctx, kind)
		if err != nil {
			return err
		}
		*sid = v
	}
	return nil
}
