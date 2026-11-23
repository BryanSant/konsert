package sqlite

import (
	"context"
	"strings"
	"testing"
)

const taskCols = `id, external_id, worker_id, space_id, workspace_id, project_id, repo_id, worktree_id,
	agent, model, session_kind, workdir, effective_dir, branch, mode, origin, permission_mode,
	orphan_policy, prompt, status, summary, git_summary_json, exit_code,
	created_at, started_at, completed_at`

// TestHotListQueriesUseIndexes asserts the hot list queries (mirroring ListTasks
// and ListChatSessions) walk an index and never need a temp B-tree sort.
func TestHotListQueriesUseIndexes(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cases := []struct {
		name, query string
		args        []any
	}{
		{"ListTasks unfiltered", `SELECT ` + taskCols + ` FROM tasks ORDER BY created_at DESC LIMIT ?`, []any{50}},
		{"ListTasks by worker", `SELECT ` + taskCols + ` FROM tasks WHERE worker_id = ? ORDER BY created_at DESC LIMIT ?`, []any{"w", 50}},
		{"ListTasks by status", `SELECT ` + taskCols + ` FROM tasks WHERE status = ? ORDER BY created_at DESC LIMIT ?`, []any{"queued", 50}},
		{"ListChatSessions", `SELECT ` + chatSessionCols + ` FROM chat_sessions ORDER BY updated_at DESC LIMIT ?`, []any{50}},
	}
	for _, c := range cases {
		rows, err := st.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+c.query, c.args...)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		joined := strings.Join(plan, " | ")
		if strings.Contains(joined, "TEMP B-TREE") || !strings.Contains(joined, "USING INDEX") {
			t.Errorf("%s: plan does not use an index without sorting: %s", c.name, joined)
		}
	}
}
