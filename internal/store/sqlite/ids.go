package sqlite

import (
	"context"
	"fmt"

	"github.com/BryanSant/konsert/internal/ids"
)

// idTables maps each user-facing ID kind to the table whose primary key it is.
var idTables = map[ids.Kind]string{
	ids.Task:      "tasks",
	ids.Chat:      "chat_sessions",
	ids.Machine:   "workers",
	ids.Workspace: "workspaces",
	ids.Project:   "projects",
	ids.Repo:      "repos",
	ids.Worktree:  "worktrees",
	ids.Space:     "spaces",
}

// NewID mints a unique ID of the given kind, checking candidates against its table.
func (s *SQLiteStore) NewID(ctx context.Context, kind ids.Kind) (string, error) {
	table, ok := idTables[kind]
	if !ok {
		return "", fmt.Errorf("unknown id kind %q", kind)
	}
	return ids.New(kind, func(id string) (bool, error) {
		var n int
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM `+table+` WHERE id = ?`, id).Scan(&n)
		return n > 0, err
	})
}
