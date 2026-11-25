package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// EnsureTaxonomyDefaults guarantees that the built-in Default Space and Default Project always exist.
func (s *SQLiteStore) EnsureTaxonomyDefaults(ctx context.Context) error {
	now := formatTime(time.Now().UTC())
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO workspaces (id, sid, name, is_default, created_at)
		VALUES (?, ?, 'Default', 1, ?)
	`, store.DefaultWorkspaceID, ids.DefaultWorkspaceSID, now); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO projects (id, sid, workspace_id, name, is_default, created_at)
		VALUES (?, ?, ?, 'Default', 1, ?)
	`, store.DefaultProjectID, ids.DefaultProjectSID, store.DefaultWorkspaceID, now); err != nil {
		return err
	}
	return nil
}

func (s *SQLiteStore) GetTaxonomy(ctx context.Context) (store.TaxonomySnapshot, error) {
	ws, err := s.ListWorkspaces(ctx)
	if err != nil {
		return store.TaxonomySnapshot{}, err
	}
	projects, err := s.ListProjects(ctx, "")
	if err != nil {
		return store.TaxonomySnapshot{}, err
	}
	repos, err := s.ListRepos(ctx, "", "")
	if err != nil {
		return store.TaxonomySnapshot{}, err
	}
	worktrees, err := s.ListWorktrees(ctx, "")
	if err != nil {
		return store.TaxonomySnapshot{}, err
	}
	return store.TaxonomySnapshot{
		Workspaces: ws,
		Projects:   projects,
		Repos:      repos,
		Worktrees:  worktrees,
	}, nil
}

// --- Workspaces (Spaces) ---

func (s *SQLiteStore) CreateWorkspace(ctx context.Context, ws store.Workspace) error {
	if ws.CreatedAt.IsZero() {
		ws.CreatedAt = time.Now().UTC()
	}
	if err := s.assignIDs(ctx, ids.Workspace, &ws.ID, &ws.SID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workspaces (id, sid, name, is_default, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, ws.ID, ws.SID, ws.Name, boolToInt(ws.IsDefault), formatTime(ws.CreatedAt))
	return err
}

func (s *SQLiteStore) GetWorkspace(ctx context.Context, id string) (store.Workspace, error) {
	var ws store.Workspace
	var isDef int
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, sid, name, is_default, created_at FROM workspaces WHERE id = ?
	`, id).Scan(&ws.ID, &ws.SID, &ws.Name, &isDef, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Workspace{}, store.ErrNotFound
	}
	if err != nil {
		return store.Workspace{}, err
	}
	ws.IsDefault = isDef != 0
	ws.CreatedAt = parseTime(createdAt)
	return ws, nil
}

func (s *SQLiteStore) ListWorkspaces(ctx context.Context) ([]store.Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, sid, name, is_default, created_at
		FROM workspaces ORDER BY is_default DESC, created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []store.Workspace{}
	for rows.Next() {
		var ws store.Workspace
		var isDef int
		var createdAt string
		if err := rows.Scan(&ws.ID, &ws.SID, &ws.Name, &isDef, &createdAt); err != nil {
			return nil, err
		}
		ws.IsDefault = isDef != 0
		ws.CreatedAt = parseTime(createdAt)
		out = append(out, ws)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteWorkspace(ctx context.Context, id string) error {
	if id == store.DefaultWorkspaceID {
		return store.ErrProtectedDefault
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Re-home any tasks pointing at this workspace to Default Space / Default Project.
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET workspace_id = ?, project_id = ?, repo_id = '', worktree_id = ''
		WHERE workspace_id = ?
	`, store.DefaultWorkspaceID, store.DefaultProjectID, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ? AND is_default = 0`, id)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		_ = tx.Rollback()
		return store.ErrNotFound
	}
	return tx.Commit()
}

// --- Projects ---

func (s *SQLiteStore) CreateProject(ctx context.Context, p store.Project) error {
	if p.WorkspaceID == "" {
		p.WorkspaceID = store.DefaultWorkspaceID
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if err := s.assignIDs(ctx, ids.Project, &p.ID, &p.SID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (id, sid, workspace_id, name, is_default, delivery_mode, merge_style, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, p.ID, p.SID, p.WorkspaceID, p.Name, boolToInt(p.IsDefault), p.DeliveryMode, p.MergeStyle, formatTime(p.CreatedAt))
	return err
}

func (s *SQLiteStore) GetProject(ctx context.Context, id string) (store.Project, error) {
	var p store.Project
	var isDef int
	var createdAt string
	var headlineAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, sid, workspace_id, name, is_default, headline, headline_at, delivery_mode, merge_style, created_at FROM projects WHERE id = ?
	`, id).Scan(&p.ID, &p.SID, &p.WorkspaceID, &p.Name, &isDef, &p.Headline, &headlineAt, &p.DeliveryMode, &p.MergeStyle, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Project{}, store.ErrNotFound
	}
	if err != nil {
		return store.Project{}, err
	}
	p.IsDefault = isDef != 0
	p.CreatedAt = parseTime(createdAt)
	p.HeadlineAt = parseOptTime(headlineAt)
	return p, nil
}

// SetProjectHeadline sets (or clears) a project's headline.
func (s *SQLiteStore) SetProjectHeadline(ctx context.Context, id, headline string, at time.Time) error {
	var atVal any
	if headline != "" {
		atVal = formatTime(at.UTC())
	}
	return s.execOne(ctx, `UPDATE projects SET headline = ?, headline_at = ? WHERE id = ?`, headline, atVal, id)
}

// UpdateProjectDelivery updates a project's delivery mode and merge style overrides.
func (s *SQLiteStore) UpdateProjectDelivery(ctx context.Context, id, deliveryMode, mergeStyle string) error {
	return s.execOne(ctx, `UPDATE projects SET delivery_mode = ?, merge_style = ? WHERE id = ?`, deliveryMode, mergeStyle, id)
}

// parseOptTime parses a nullable timestamp column.
func parseOptTime(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parseTime(v.String)
	return &t
}

func (s *SQLiteStore) ListProjects(ctx context.Context, workspaceID string) ([]store.Project, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if workspaceID != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, sid, workspace_id, name, is_default, headline, headline_at, delivery_mode, merge_style, created_at
			FROM projects WHERE workspace_id = ? ORDER BY is_default DESC, created_at ASC
		`, workspaceID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, sid, workspace_id, name, is_default, headline, headline_at, delivery_mode, merge_style, created_at
			FROM projects ORDER BY is_default DESC, created_at ASC
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []store.Project{}
	for rows.Next() {
		var p store.Project
		var isDef int
		var createdAt string
		var headlineAt sql.NullString
		if err := rows.Scan(&p.ID, &p.SID, &p.WorkspaceID, &p.Name, &isDef, &p.Headline, &headlineAt, &p.DeliveryMode, &p.MergeStyle, &createdAt); err != nil {
			return nil, err
		}
		p.IsDefault = isDef != 0
		p.CreatedAt = parseTime(createdAt)
		p.HeadlineAt = parseOptTime(headlineAt)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteProject(ctx context.Context, id string) error {
	if id == store.DefaultProjectID {
		return store.ErrProtectedDefault
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET project_id = ?, repo_id = '', worktree_id = ''
		WHERE project_id = ?
	`, store.DefaultProjectID, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ? AND is_default = 0`, id)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		_ = tx.Rollback()
		return store.ErrNotFound
	}
	return tx.Commit()
}

// --- Repos ---

func (s *SQLiteStore) CreateRepo(ctx context.Context, r store.Repo) error {
	if r.ProjectID == "" {
		r.ProjectID = store.DefaultProjectID
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if err := s.assignIDs(ctx, ids.Repo, &r.ID, &r.SID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO repos (id, sid, project_id, worker_id, name, path, remote_url, delivery_mode, merge_style, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, r.ID, r.SID, r.ProjectID, r.WorkerID, r.Name, r.Path, r.RemoteURL, r.DeliveryMode, r.MergeStyle, formatTime(r.CreatedAt))
	return err
}

func (s *SQLiteStore) GetRepo(ctx context.Context, id string) (store.Repo, error) {
	var r store.Repo
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, sid, project_id, worker_id, name, path, remote_url, delivery_mode, merge_style, created_at
		FROM repos WHERE id = ?
	`, id).Scan(&r.ID, &r.SID, &r.ProjectID, &r.WorkerID, &r.Name, &r.Path, &r.RemoteURL, &r.DeliveryMode, &r.MergeStyle, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Repo{}, store.ErrNotFound
	}
	if err != nil {
		return store.Repo{}, err
	}
	r.CreatedAt = parseTime(createdAt)
	return r, nil
}

// UpdateRepoDelivery updates a repo's delivery mode, merge style, and remote URL.
func (s *SQLiteStore) UpdateRepoDelivery(ctx context.Context, id, deliveryMode, mergeStyle, remoteURL string) error {
	return s.execOne(ctx, `UPDATE repos SET delivery_mode = ?, merge_style = ?, remote_url = ? WHERE id = ?`, deliveryMode, mergeStyle, remoteURL, id)
}

func (s *SQLiteStore) ListRepos(ctx context.Context, projectID, workerID string) ([]store.Repo, error) {
	var conds []string
	var args []any
	if projectID != "" {
		conds = append(conds, "project_id = ?")
		args = append(args, projectID)
	}
	if workerID != "" {
		conds = append(conds, "worker_id = ?")
		args = append(args, workerID)
	}
	q := `SELECT id, sid, project_id, worker_id, name, path, remote_url, delivery_mode, merge_style, created_at FROM repos`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY created_at ASC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []store.Repo{}
	for rows.Next() {
		var r store.Repo
		var createdAt string
		if err := rows.Scan(&r.ID, &r.SID, &r.ProjectID, &r.WorkerID, &r.Name, &r.Path, &r.RemoteURL, &r.DeliveryMode, &r.MergeStyle, &createdAt); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTime(createdAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteRepo(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET repo_id = '', worktree_id = '' WHERE repo_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM repos WHERE id = ?`, id)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		_ = tx.Rollback()
		return store.ErrNotFound
	}
	return tx.Commit()
}

// --- Worktrees ---

func (s *SQLiteStore) CreateWorktree(ctx context.Context, wt store.Worktree) error {
	if wt.CreatedAt.IsZero() {
		wt.CreatedAt = time.Now().UTC()
	}
	if err := s.assignIDs(ctx, ids.Worktree, &wt.ID, &wt.SID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO worktrees (id, sid, repo_id, branch, path, is_primary, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, wt.ID, wt.SID, wt.RepoID, wt.Branch, wt.Path, boolToInt(wt.IsPrimary), formatTime(wt.CreatedAt))
	return err
}

func (s *SQLiteStore) GetWorktree(ctx context.Context, id string) (store.Worktree, error) {
	var wt store.Worktree
	var isPrim int
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, sid, repo_id, branch, path, is_primary, created_at
		FROM worktrees WHERE id = ?
	`, id).Scan(&wt.ID, &wt.SID, &wt.RepoID, &wt.Branch, &wt.Path, &isPrim, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Worktree{}, store.ErrNotFound
	}
	if err != nil {
		return store.Worktree{}, err
	}
	wt.IsPrimary = isPrim != 0
	wt.CreatedAt = parseTime(createdAt)
	return wt, nil
}

func (s *SQLiteStore) ListWorktrees(ctx context.Context, repoID string) ([]store.Worktree, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if repoID != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, sid, repo_id, branch, path, is_primary, created_at
			FROM worktrees WHERE repo_id = ? ORDER BY is_primary DESC, created_at ASC
		`, repoID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, sid, repo_id, branch, path, is_primary, created_at
			FROM worktrees ORDER BY is_primary DESC, created_at ASC
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []store.Worktree{}
	for rows.Next() {
		var wt store.Worktree
		var isPrim int
		var createdAt string
		if err := rows.Scan(&wt.ID, &wt.SID, &wt.RepoID, &wt.Branch, &wt.Path, &isPrim, &createdAt); err != nil {
			return nil, err
		}
		wt.IsPrimary = isPrim != 0
		wt.CreatedAt = parseTime(createdAt)
		out = append(out, wt)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteWorktree(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET worktree_id = '' WHERE worktree_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM worktrees WHERE id = ?`, id)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		_ = tx.Rollback()
		return store.ErrNotFound
	}
	return tx.Commit()
}
