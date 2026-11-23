package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	_ "modernc.org/sqlite"
)

// SQLiteStore implements store.Store backed by modernc.org/sqlite in WAL mode.
type SQLiteStore struct {
	db *sql.DB
}

// Open opens (or creates) a SQLite database at path, applies WAL pragmas, and runs migrations.
func Open(ctx context.Context, path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// Single-writer connection prevents SQLITE_BUSY under concurrent goroutines.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA foreign_keys=ON;",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("exec %s: %w", p, err)
		}
	}

	if err := runMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	st := &SQLiteStore{db: db}
	if err := st.EnsureTaxonomyDefaults(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	return st, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(v string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, v)
	}
	return t
}

func (s *SQLiteStore) UpsertWorker(ctx context.Context, w store.Worker) error {
	if w.Agents == nil {
		w.Agents = []string{}
	}
	if w.Tags == nil {
		w.Tags = map[string]string{}
	}
	if w.Isolation == "" {
		w.Isolation = "host"
	}
	if w.PermissionMode == "" {
		w.PermissionMode = store.PermissionFullAccess
	}
	if w.MaxConcurrent <= 0 {
		w.MaxConcurrent = 2
	}
	if w.LastSeenAt.IsZero() {
		w.LastSeenAt = time.Now().UTC()
	}

	agentsJSON, err := json.Marshal(w.Agents)
	if err != nil {
		return err
	}
	tagsJSON, err := json.Marshal(w.Tags)
	if err != nil {
		return err
	}

	var factsUpdated any
	if w.FactsUpdatedAt != nil {
		factsUpdated = formatTime(*w.FactsUpdatedAt)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO workers (id, name, host_group, os, arch, isolation, permission_mode, agents_json, tags_json, max_concurrent, status, last_seen_at, facts_json, facts_updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			host_group = excluded.host_group,
			os = excluded.os,
			arch = excluded.arch,
			isolation = excluded.isolation,
			permission_mode = excluded.permission_mode,
			agents_json = excluded.agents_json,
			tags_json = excluded.tags_json,
			max_concurrent = excluded.max_concurrent,
			status = excluded.status,
			last_seen_at = excluded.last_seen_at,
			facts_json = CASE WHEN excluded.facts_json != '' THEN excluded.facts_json ELSE workers.facts_json END,
			facts_updated_at = COALESCE(excluded.facts_updated_at, workers.facts_updated_at)
	`, w.ID, w.Name, w.HostGroup, w.OS, w.Arch, w.Isolation, string(w.PermissionMode),
		string(agentsJSON), string(tagsJSON), w.MaxConcurrent, w.Status, formatTime(w.LastSeenAt), w.FactsJSON, factsUpdated)
	return err
}

func (s *SQLiteStore) UpdateWorkerFacts(ctx context.Context, id, factsJSON string, updatedAt time.Time) error {
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE workers SET facts_json = ?, facts_updated_at = ? WHERE id = ?
	`, factsJSON, formatTime(updatedAt), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func scanWorker(scanner interface{ Scan(...any) error }) (store.Worker, error) {
	var (
		w            store.Worker
		perm         string
		agentsJSON   string
		tagsJSON     string
		lastSeen     string
		factsJSON    string
		factsUpdated sql.NullString
	)
	if err := scanner.Scan(
		&w.ID, &w.Name, &w.HostGroup, &w.OS, &w.Arch, &w.Isolation, &perm,
		&agentsJSON, &tagsJSON, &w.MaxConcurrent, &w.Status, &lastSeen, &factsJSON, &factsUpdated,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Worker{}, store.ErrNotFound
		}
		return store.Worker{}, err
	}
	w.PermissionMode = store.PermissionMode(perm)
	w.LastSeenAt = parseTime(lastSeen)
	w.FactsJSON = factsJSON
	if factsUpdated.Valid && factsUpdated.String != "" {
		ft := parseTime(factsUpdated.String)
		w.FactsUpdatedAt = &ft
	}
	_ = json.Unmarshal([]byte(agentsJSON), &w.Agents)
	_ = json.Unmarshal([]byte(tagsJSON), &w.Tags)
	if w.Agents == nil {
		w.Agents = []string{}
	}
	if w.Tags == nil {
		w.Tags = map[string]string{}
	}
	return w, nil
}

func (s *SQLiteStore) GetWorker(ctx context.Context, id string) (store.Worker, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, host_group, os, arch, isolation, permission_mode, agents_json, tags_json, max_concurrent, status, last_seen_at, facts_json, facts_updated_at
		FROM workers WHERE id = ?
	`, id)
	return scanWorker(row)
}

func (s *SQLiteStore) ListWorkers(ctx context.Context) ([]store.Worker, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, host_group, os, arch, isolation, permission_mode, agents_json, tags_json, max_concurrent, status, last_seen_at, facts_json, facts_updated_at
		FROM workers ORDER BY host_group ASC, name ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workers []store.Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, rows.Err()
}

func (s *SQLiteStore) DeleteWorker(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Preserve task history by unlinking worker_id before deleting the worker row.
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET worker_id = NULL WHERE worker_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM spaces WHERE worker_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM repos WHERE worker_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM workers WHERE id = ?`, id)
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

func (s *SQLiteStore) MarkStaleWorkersOffline(ctx context.Context, cutoff time.Time) ([]string, error) {
	cutoffStr := formatTime(cutoff)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM workers WHERE status = 'online' AND last_seen_at < ?
	`, cutoffStr)
	if err != nil {
		return nil, err
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		stale = append(stale, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(stale) == 0 {
		return nil, nil
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE workers SET status = 'offline' WHERE status = 'online' AND last_seen_at < ?
	`, cutoffStr)
	if err != nil {
		return nil, err
	}
	return stale, nil
}

func (s *SQLiteStore) CreateSpace(ctx context.Context, sp store.Space) error {
	if sp.CreatedAt.IsZero() {
		sp.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO spaces (id, worker_id, name, path, branch, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, sp.ID, sp.WorkerID, sp.Name, sp.Path, sp.Branch, formatTime(sp.CreatedAt))
	return err
}

func (s *SQLiteStore) GetSpace(ctx context.Context, id string) (store.Space, error) {
	var sp store.Space
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, worker_id, name, path, branch, created_at
		FROM spaces WHERE id = ?
	`, id).Scan(&sp.ID, &sp.WorkerID, &sp.Name, &sp.Path, &sp.Branch, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Space{}, store.ErrNotFound
	}
	if err != nil {
		return store.Space{}, err
	}
	sp.CreatedAt = parseTime(createdAt)
	return sp, nil
}

func (s *SQLiteStore) ListSpaces(ctx context.Context, workerID string) ([]store.Space, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if workerID != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, worker_id, name, path, branch, created_at
			FROM spaces WHERE worker_id = ? ORDER BY created_at ASC
		`, workerID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, worker_id, name, path, branch, created_at
			FROM spaces ORDER BY created_at ASC
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []store.Space{}
	for rows.Next() {
		var sp store.Space
		var createdAt string
		if err := rows.Scan(&sp.ID, &sp.WorkerID, &sp.Name, &sp.Path, &sp.Branch, &createdAt); err != nil {
			return nil, err
		}
		sp.CreatedAt = parseTime(createdAt)
		out = append(out, sp)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteSpace(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM spaces WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) CreateTask(ctx context.Context, t store.Task) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.Mode == "" {
		t.Mode = protocol.TaskModeOneshot
	}
	if t.Origin == "" {
		t.Origin = protocol.TaskOriginOrchestrator
	}
	if t.PermissionMode == "" {
		t.PermissionMode = store.PermissionFullAccess
	}
	if t.OrphanPolicy == "" {
		t.OrphanPolicy = protocol.OrphanPolicyFail
	}
	if t.Status == "" {
		t.Status = protocol.TaskStatusQueued
	}
	if t.SessionKind == "" {
		if t.Agent == "shell" {
			t.SessionKind = "terminal"
		} else {
			t.SessionKind = "chat"
		}
	}
	if t.WorkspaceID == "" {
		t.WorkspaceID = store.DefaultWorkspaceID
	}
	if t.ProjectID == "" {
		t.ProjectID = store.DefaultProjectID
	}

	var workerID any
	if t.WorkerID != "" {
		workerID = t.WorkerID
	}
	var startedAt, completedAt any
	if t.StartedAt != nil {
		startedAt = formatTime(*t.StartedAt)
	}
	if t.CompletedAt != nil {
		completedAt = formatTime(*t.CompletedAt)
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (
			id, worker_id, space_id, workspace_id, project_id, repo_id, worktree_id,
			agent, model, session_kind, workdir, effective_dir, branch, mode, origin, permission_mode,
			orphan_policy, prompt, status, summary, git_summary_json, exit_code,
			created_at, started_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, t.ID, workerID, t.SpaceID, t.WorkspaceID, t.ProjectID, t.RepoID, t.WorktreeID,
		t.Agent, t.Model, t.SessionKind, t.Workdir, t.EffectiveDir, t.Branch, t.Mode, t.Origin, string(t.PermissionMode),
		t.OrphanPolicy, t.Prompt, t.Status, t.Summary, t.GitSummaryJSON, t.ExitCode,
		formatTime(t.CreatedAt), startedAt, completedAt)
	return err
}

func (s *SQLiteStore) UpdateTaskAssignment(ctx context.Context, id, workerID, effectiveDir string) error {
	var wVal any
	if workerID != "" {
		wVal = workerID
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET worker_id = COALESCE(?, worker_id),
		    effective_dir = CASE WHEN ? != '' THEN ? ELSE effective_dir END
		WHERE id = ?
	`, wVal, effectiveDir, effectiveDir, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) UpdateTaskStatus(ctx context.Context, id, status, summary, gitSummaryJSON string, exitCode *int) error {
	now := formatTime(time.Now().UTC())
	var startedExpr, completedExpr any
	if status == protocol.TaskStatusRunning {
		startedExpr = now
	}
	if protocol.IsTerminalTaskStatus(status) {
		completedExpr = now
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET status = ?,
		    summary = CASE WHEN ? != '' THEN ? ELSE summary END,
		    git_summary_json = CASE WHEN ? != '' THEN ? ELSE git_summary_json END,
		    exit_code = COALESCE(?, exit_code),
		    started_at = COALESCE(started_at, ?),
		    completed_at = COALESCE(?, completed_at)
		WHERE id = ?
	`, status, summary, summary, gitSummaryJSON, gitSummaryJSON, exitCode, startedExpr, completedExpr, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func scanTask(scanner interface{ Scan(...any) error }) (store.Task, error) {
	var (
		t           store.Task
		workerID    sql.NullString
		perm        string
		exitCode    sql.NullInt64
		createdAt   string
		startedAt   sql.NullString
		completedAt sql.NullString
	)
	if err := scanner.Scan(
		&t.ID, &workerID, &t.SpaceID, &t.WorkspaceID, &t.ProjectID, &t.RepoID, &t.WorktreeID,
		&t.Agent, &t.Model, &t.SessionKind, &t.Workdir, &t.EffectiveDir, &t.Branch, &t.Mode, &t.Origin, &perm,
		&t.OrphanPolicy, &t.Prompt, &t.Status, &t.Summary, &t.GitSummaryJSON, &exitCode,
		&createdAt, &startedAt, &completedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Task{}, store.ErrNotFound
		}
		return store.Task{}, err
	}
	t.WorkerID = workerID.String
	t.PermissionMode = store.PermissionMode(perm)
	if exitCode.Valid {
		ec := int(exitCode.Int64)
		t.ExitCode = &ec
	}
	t.CreatedAt = parseTime(createdAt)
	if startedAt.Valid && startedAt.String != "" {
		st := parseTime(startedAt.String)
		t.StartedAt = &st
	}
	if completedAt.Valid && completedAt.String != "" {
		ct := parseTime(completedAt.String)
		t.CompletedAt = &ct
	}
	return t, nil
}

func (s *SQLiteStore) GetTask(ctx context.Context, id string) (store.Task, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, worker_id, space_id, workspace_id, project_id, repo_id, worktree_id,
		       agent, model, session_kind, workdir, effective_dir, branch, mode, origin, permission_mode,
		       orphan_policy, prompt, status, summary, git_summary_json, exit_code,
		       created_at, started_at, completed_at
		FROM tasks WHERE id = ?
	`, id)
	return scanTask(row)
}

func (s *SQLiteStore) ListTasks(ctx context.Context, status, workerID string, limit int) ([]store.Task, error) {
	if limit <= 0 {
		limit = 50
	}
	var conds []string
	var args []any
	if status != "" {
		conds = append(conds, "status = ?")
		args = append(args, status)
	}
	if workerID != "" {
		conds = append(conds, "worker_id = ?")
		args = append(args, workerID)
	}

	query := `
		SELECT id, worker_id, space_id, workspace_id, project_id, repo_id, worktree_id,
		       agent, model, session_kind, workdir, effective_dir, branch, mode, origin, permission_mode,
		       orphan_policy, prompt, status, summary, git_summary_json, exit_code,
		       created_at, started_at, completed_at
		FROM tasks`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []store.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (s *SQLiteStore) AppendEvent(ctx context.Context, e store.TaskEvent) (int64, error) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO task_events (task_id, kind, message, meta_json, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, e.TaskID, e.Kind, e.Message, e.MetaJSON, formatTime(e.CreatedAt))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *SQLiteStore) ListEvents(ctx context.Context, taskID string, afterID int64, limit int) ([]store.TaskEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, kind, message, meta_json, created_at
		FROM task_events
		WHERE task_id = ? AND id > ?
		ORDER BY id ASC
		LIMIT ?
	`, taskID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []store.TaskEvent
	for rows.Next() {
		var e store.TaskEvent
		var ts string
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Kind, &e.Message, &e.MetaJSON, &ts); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(ts)
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *SQLiteStore) CreateChatSession(ctx context.Context, cs store.ChatSession) error {
	now := time.Now().UTC()
	if cs.CreatedAt.IsZero() {
		cs.CreatedAt = now
	}
	if cs.UpdatedAt.IsZero() {
		cs.UpdatedAt = now
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_sessions (id, title, provider, model, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, cs.ID, cs.Title, cs.Provider, cs.Model, formatTime(cs.CreatedAt), formatTime(cs.UpdatedAt))
	return err
}

func (s *SQLiteStore) GetChatSession(ctx context.Context, id string) (store.ChatSession, error) {
	var cs store.ChatSession
	var cAt, uAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, title, provider, model, created_at, updated_at
		FROM chat_sessions WHERE id = ?
	`, id).Scan(&cs.ID, &cs.Title, &cs.Provider, &cs.Model, &cAt, &uAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ChatSession{}, store.ErrNotFound
	}
	if err != nil {
		return store.ChatSession{}, err
	}
	cs.CreatedAt = parseTime(cAt)
	cs.UpdatedAt = parseTime(uAt)
	return cs, nil
}

func (s *SQLiteStore) ListChatSessions(ctx context.Context, limit int) ([]store.ChatSession, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, provider, model, created_at, updated_at
		FROM chat_sessions ORDER BY updated_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.ChatSession
	for rows.Next() {
		var cs store.ChatSession
		var cAt, uAt string
		if err := rows.Scan(&cs.ID, &cs.Title, &cs.Provider, &cs.Model, &cAt, &uAt); err != nil {
			return nil, err
		}
		cs.CreatedAt = parseTime(cAt)
		cs.UpdatedAt = parseTime(uAt)
		out = append(out, cs)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) AppendChatMessage(ctx context.Context, m store.ChatMessage) (int64, error) {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	ts := formatTime(m.CreatedAt)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_messages (session_id, role, content, tool_json, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, m.SessionID, m.Role, m.Content, m.ToolJSON, ts)
	if err != nil {
		return 0, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE chat_sessions SET updated_at = ? WHERE id = ?`, ts, m.SessionID)
	return res.LastInsertId()
}

func (s *SQLiteStore) ListChatMessages(ctx context.Context, sessionID string) ([]store.ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, role, content, tool_json, created_at
		FROM chat_messages WHERE session_id = ? ORDER BY id ASC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.ChatMessage
	for rows.Next() {
		var m store.ChatMessage
		var ts string
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.ToolJSON, &ts); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(ts)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) RecordUsage(ctx context.Context, u store.UsageEvent) error {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_events (provider, model, input_tokens, output_tokens, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, u.Provider, u.Model, u.InputTokens, u.OutputTokens, formatTime(u.CreatedAt))
	return err
}

func (s *SQLiteStore) GetUsageSummary(ctx context.Context) (store.UsageSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, model, COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COUNT(1)
		FROM usage_events
		GROUP BY provider, model
		ORDER BY provider ASC, model ASC
	`)
	if err != nil {
		return store.UsageSummary{}, err
	}
	defer rows.Close()

	sum := store.UsageSummary{ByProvider: []store.UsageBucket{}}
	for rows.Next() {
		var b store.UsageBucket
		if err := rows.Scan(&b.Provider, &b.Model, &b.InputTokens, &b.OutputTokens, &b.Requests); err != nil {
			return store.UsageSummary{}, err
		}
		sum.TotalInputTokens += b.InputTokens
		sum.TotalOutputTokens += b.OutputTokens
		sum.TotalRequests += b.Requests
		sum.ByProvider = append(sum.ByProvider, b)
	}
	return sum, rows.Err()
}

func (s *SQLiteStore) GetAllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *SQLiteStore) SetSettings(ctx context.Context, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	ts := formatTime(time.Now().UTC())
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		`, k, v, ts); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) UpsertMemory(ctx context.Context, m store.MemoryEntry) error {
	if m.Scope == "" {
		m.Scope = "fleet"
	}
	if m.ID == "" {
		m.ID = m.Scope + "::" + m.Key
	}
	if m.MetaJSON == "" {
		m.MetaJSON = "{}"
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO orchestrator_memory (id, scope, mem_key, summary, meta_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope, mem_key) DO UPDATE SET
			summary = excluded.summary,
			meta_json = excluded.meta_json,
			updated_at = excluded.updated_at
	`, m.ID, m.Scope, m.Key, m.Summary, m.MetaJSON, formatTime(m.UpdatedAt))
	return err
}

func (s *SQLiteStore) ListMemory(ctx context.Context, scope string, limit int) ([]store.MemoryEntry, error) {
	if limit <= 0 {
		limit = 30
	}
	var (
		rows *sql.Rows
		err  error
	)
	if scope != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, scope, mem_key, summary, meta_json, updated_at
			FROM orchestrator_memory WHERE scope = ?
			ORDER BY updated_at DESC LIMIT ?
		`, scope, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, scope, mem_key, summary, meta_json, updated_at
			FROM orchestrator_memory
			ORDER BY updated_at DESC LIMIT ?
		`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []store.MemoryEntry{}
	for rows.Next() {
		var e store.MemoryEntry
		var ts string
		if err := rows.Scan(&e.ID, &e.Scope, &e.Key, &e.Summary, &e.MetaJSON, &ts); err != nil {
			return nil, err
		}
		e.UpdatedAt = parseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteMemory(ctx context.Context, scope, key string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM orchestrator_memory WHERE scope = ? AND mem_key = ?`, scope, key)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
