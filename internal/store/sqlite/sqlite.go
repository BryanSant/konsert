package sqlite

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	_ "modernc.org/sqlite"
)

// SQLiteStore implements store.Store backed by modernc.org/sqlite in WAL mode.
type SQLiteStore struct {
	db *sql.DB

	// auditInserts counts AppendAudit calls so retention pruning runs on the
	// first insert after Open and then every auditPruneEvery inserts.
	auditInserts atomic.Uint64
}

// connPragmas are applied by the driver to every new connection in the pool (via
// the DSN), not once on whichever connection happens to run an Exec. busy_timeout
// and foreign_keys are connection-scoped, so setting them with a single Exec left
// any replacement connection (e.g. after a bad-conn error) without them.
var connPragmas = []string{
	"auto_vacuum(2)", // incremental: retention reclaims space in small steps (only applies to a new file)
	"busy_timeout(5000)",
	"foreign_keys(1)",
	"journal_mode(WAL)",
	"synchronous(NORMAL)",
}

// buildDSN appends the per-connection pragmas to a database path for modernc.org/sqlite.
func buildDSN(path string) string {
	q := url.Values{}
	for _, p := range connPragmas {
		q.Add("_pragma", p)
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + q.Encode()
}

// Open opens (or creates) a SQLite database at path, applies WAL pragmas, and runs migrations.
func Open(ctx context.Context, path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", buildDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// Single-writer connection prevents SQLITE_BUSY under concurrent goroutines.
	// A separate read-only pool was considered and deferred: it would touch every
	// query site (including task CRUD other work is extending) for limited gain
	// at the hub's current load.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	if err := runMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
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

// timeLayout is a fixed-width UTC timestamp layout. Unlike time.RFC3339Nano it
// never strips trailing fractional zeros, so stored values sort correctly as
// text (RFC3339Nano would order "...05.5Z" before "...05Z" because '.' < 'Z').
// Every write goes through formatTime; migration 008 rewrites legacy rows.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// parseTime accepts both the fixed-width layout and legacy RFC3339(Nano) values.
// parseTimePtr parses a nullable timestamp column (nil when NULL or empty).
func parseTimePtr(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parseTime(v.String)
	return &t
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
	if w.SID == "" {
		// Keep the sid of a known machine; mint one the first time it is seen.
		err := s.db.QueryRowContext(ctx, `SELECT sid FROM workers WHERE id = ?`, w.ID).Scan(&w.SID)
		if errors.Is(err, sql.ErrNoRows) {
			w.SID, err = s.NewSID(ctx, ids.Machine)
		}
		if err != nil {
			return err
		}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO workers (id, sid, name, host_group, os, arch, isolation, permission_mode, agents_json, tags_json, max_concurrent, status, last_seen_at, facts_json, facts_updated_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			facts_updated_at = COALESCE(excluded.facts_updated_at, workers.facts_updated_at),
			version = excluded.version
	`, w.ID, w.SID, w.Name, w.HostGroup, w.OS, w.Arch, w.Isolation, string(w.PermissionMode),
		string(agentsJSON), string(tagsJSON), w.MaxConcurrent, w.Status, formatTime(w.LastSeenAt), w.FactsJSON, factsUpdated, w.Version)
	return err
}

func (s *SQLiteStore) TouchWorker(ctx context.Context, id string, seenAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE workers SET status = 'online', last_seen_at = ? WHERE id = ?
	`, formatTime(seenAt), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// MarkWorkerOffline flips worker id from online to offline and reports whether it did.
func (s *SQLiteStore) MarkWorkerOffline(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE workers SET status = 'offline' WHERE id = ? AND status = 'online'`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *SQLiteStore) EnsureWorker(ctx context.Context, w store.Worker) (store.Worker, error) {
	if strings.TrimSpace(w.ID) == "" {
		return store.Worker{}, errors.New("worker id is required")
	}
	if w.HostGroup == "" {
		w.HostGroup = w.Name
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
	// Keep the sid of a known machine (the conflict clause never touches it); mint one
	// for a new row.
	sid := ""
	err := s.db.QueryRowContext(ctx, `SELECT sid FROM workers WHERE id = ?`, w.ID).Scan(&sid)
	if errors.Is(err, sql.ErrNoRows) {
		sid, err = s.NewSID(ctx, ids.Machine)
	}
	if err != nil {
		return store.Worker{}, err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO workers (id, sid, name, host_group, os, arch, isolation, permission_mode, agents_json, tags_json, max_concurrent, status, last_seen_at,
			launch, ssh_target, controller_url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '[]', '{}', ?, 'offline', ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			launch = excluded.launch,
			ssh_target = excluded.ssh_target,
			controller_url = excluded.controller_url
	`, w.ID, sid, w.Name, w.HostGroup, w.OS, w.Arch, w.Isolation, string(w.PermissionMode), w.MaxConcurrent,
		formatTime(w.LastSeenAt), w.Launch, w.SSHTarget, w.ControllerURL); err != nil {
		return store.Worker{}, err
	}
	return s.GetWorker(ctx, w.ID)
}

func (s *SQLiteStore) SetWorkerLaunchState(ctx context.Context, id, state, errMsg string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE workers SET launch_state = ?, launch_error = ?, launch_at = ? WHERE id = ?
	`, state, errMsg, formatTime(at), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
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

// workerColumns is the column list scanWorker reads, in order.
const workerColumns = `id, sid, name, host_group, os, arch, isolation, permission_mode, agents_json, tags_json, max_concurrent, status, last_seen_at, facts_json, facts_updated_at, version,
		launch, ssh_target, controller_url, launch_state, launch_error, launch_at, drained, drained_at`

func scanWorker(scanner interface{ Scan(...any) error }) (store.Worker, error) {
	var (
		w            store.Worker
		perm         string
		agentsJSON   string
		tagsJSON     string
		lastSeen     string
		factsJSON    string
		factsUpdated sql.NullString
		launchAt     sql.NullString
		drained      int
		drainedAt    sql.NullString
	)
	if err := scanner.Scan(
		&w.ID, &w.SID, &w.Name, &w.HostGroup, &w.OS, &w.Arch, &w.Isolation, &perm,
		&agentsJSON, &tagsJSON, &w.MaxConcurrent, &w.Status, &lastSeen, &factsJSON, &factsUpdated, &w.Version,
		&w.Launch, &w.SSHTarget, &w.ControllerURL, &w.LaunchState, &w.LaunchError, &launchAt, &drained, &drainedAt,
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
	if launchAt.Valid && launchAt.String != "" {
		lt := parseTime(launchAt.String)
		w.LaunchAt = &lt
	}
	w.Drained = drained != 0
	if drainedAt.Valid && drainedAt.String != "" {
		dt := parseTime(drainedAt.String)
		w.DrainedAt = &dt
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
		SELECT `+workerColumns+`
		FROM workers WHERE id = ?
	`, id)
	return scanWorker(row)
}

func (s *SQLiteStore) ListWorkers(ctx context.Context) ([]store.Worker, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+workerColumns+`
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
	// Clear task references to the spaces, repos and worktrees about to be deleted
	// (repos cascade to worktrees), mirroring DeleteRepo, so no task dangles.
	for _, q := range []string{
		`UPDATE tasks SET space_id = '' WHERE space_id IN (SELECT id FROM spaces WHERE worker_id = ?)`,
		`UPDATE tasks SET repo_id = '', worktree_id = '' WHERE repo_id IN (SELECT id FROM repos WHERE worker_id = ?)`,
		`UPDATE tasks SET worktree_id = '' WHERE worktree_id IN (SELECT wt.id FROM worktrees wt JOIN repos r ON r.id = wt.repo_id WHERE r.worker_id = ?)`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM spaces WHERE worker_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM repos WHERE worker_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_accounts WHERE worker_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM machine_vitals WHERE worker_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM machine_vitals_rollup WHERE worker_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM machine_credentials WHERE worker_id = ?`, id); err != nil {
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

// MarkStaleWorkersOffline flips every online worker whose last heartbeat is older
// than cutoff to offline and returns exactly the IDs it changed. It is a single
// atomic UPDATE ... RETURNING, so a heartbeat that lands concurrently can never
// cause a live worker's ID to be reported as reaped.
func (s *SQLiteStore) MarkStaleWorkersOffline(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE workers SET status = 'offline'
		WHERE status = 'online' AND last_seen_at < ?
		RETURNING id
	`, formatTime(cutoff))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		stale = append(stale, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return stale, nil
}

func (s *SQLiteStore) CreateSpace(ctx context.Context, sp store.Space) error {
	if sp.CreatedAt.IsZero() {
		sp.CreatedAt = time.Now().UTC()
	}
	if err := s.assignIDs(ctx, ids.Space, &sp.ID, &sp.SID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO spaces (id, sid, worker_id, name, path, branch, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, sp.ID, sp.SID, sp.WorkerID, sp.Name, sp.Path, sp.Branch, formatTime(sp.CreatedAt))
	return err
}

func (s *SQLiteStore) GetSpace(ctx context.Context, id string) (store.Space, error) {
	var sp store.Space
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, sid, worker_id, name, path, branch, created_at
		FROM spaces WHERE id = ?
	`, id).Scan(&sp.ID, &sp.SID, &sp.WorkerID, &sp.Name, &sp.Path, &sp.Branch, &createdAt)
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
			SELECT id, sid, worker_id, name, path, branch, created_at
			FROM spaces WHERE worker_id = ? ORDER BY created_at ASC
		`, workerID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, sid, worker_id, name, path, branch, created_at
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
		if err := rows.Scan(&sp.ID, &sp.SID, &sp.WorkerID, &sp.Name, &sp.Path, &sp.Branch, &createdAt); err != nil {
			return nil, err
		}
		sp.CreatedAt = parseTime(createdAt)
		out = append(out, sp)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteSpace(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET space_id = '' WHERE space_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM spaces WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return tx.Commit()
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
	if err := s.assignIDs(ctx, ids.Task, &t.ID, &t.SID); err != nil {
		return err
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
			id, sid, worker_id, space_id, workspace_id, project_id, repo_id, worktree_id,
			agent, model, session_kind, workdir, effective_dir, branch, mode, origin, permission_mode,
			orphan_policy, prompt, status, summary, git_summary_json, exit_code,
			cause_json, suspended_by, parent_chat_id, title, ticket_id, ticket_title,
			created_at, started_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, t.ID, t.SID, workerID, t.SpaceID, t.WorkspaceID, t.ProjectID, t.RepoID, t.WorktreeID,
		t.Agent, t.Model, t.SessionKind, t.Workdir, t.EffectiveDir, t.Branch, t.Mode, t.Origin, string(t.PermissionMode),
		t.OrphanPolicy, t.Prompt, t.Status, t.Summary, t.GitSummaryJSON, t.ExitCode,
		causeJSON(t.Cause), t.SuspendedBy, t.ParentChatID, t.Title, t.TicketID, t.TicketTitle,
		formatTime(t.CreatedAt), startedAt, completedAt)
	return err
}

// UpsertExternalTask creates or updates a passively discovered / hook-reported session row.
func (s *SQLiteStore) UpsertExternalTask(ctx context.Context, t store.Task) (string, string, error) {
	if t.WorkerID == "" || t.ExternalID == "" {
		return "", "", fmt.Errorf("external task needs worker_id and external_id")
	}
	t.ID, t.SID = "", ""
	prev := ""
	err := s.db.QueryRowContext(ctx, `SELECT id, sid, status FROM tasks WHERE worker_id = ? AND external_id = ?`, t.WorkerID, t.ExternalID).Scan(&t.ID, &t.SID, &prev)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.assignIDs(ctx, ids.Task, &t.ID, &t.SID)
	}
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.Mode == "" {
		t.Mode = protocol.TaskModeInteractive
	}
	if t.Origin == "" {
		t.Origin = protocol.TaskOriginExternal
	}
	if t.SessionKind == "" {
		t.SessionKind = "chat"
	}
	if t.PermissionMode == "" {
		t.PermissionMode = store.PermissionFullAccess
	}
	if t.OrphanPolicy == "" {
		t.OrphanPolicy = protocol.OrphanPolicyFail
	}
	if t.WorkspaceID == "" {
		t.WorkspaceID = store.DefaultWorkspaceID
	}
	if t.ProjectID == "" {
		t.ProjectID = store.DefaultProjectID
	}
	var startedAt any = formatTime(now)
	var completedAt any
	if protocol.IsTerminalTaskStatus(t.Status) {
		completedAt = formatTime(now)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tasks (
			id, sid, external_id, worker_id, space_id, workspace_id, project_id, repo_id, worktree_id,
			agent, model, session_kind, workdir, effective_dir, branch, mode, origin, permission_mode,
			orphan_policy, prompt, status, summary, git_summary_json, exit_code, cause_json,
			created_at, started_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			cause_json = CASE WHEN excluded.status != tasks.status AND excluded.cause_json != '' THEN excluded.cause_json ELSE tasks.cause_json END,
			workspace_id = excluded.workspace_id,
			project_id = excluded.project_id,
			repo_id = CASE WHEN excluded.repo_id != '' THEN excluded.repo_id ELSE tasks.repo_id END,
			worktree_id = CASE WHEN excluded.worktree_id != '' THEN excluded.worktree_id ELSE tasks.worktree_id END,
			model = CASE WHEN excluded.model != '' THEN excluded.model ELSE tasks.model END,
			effective_dir = CASE WHEN excluded.effective_dir != '' THEN excluded.effective_dir ELSE tasks.effective_dir END,
			branch = CASE WHEN excluded.branch != '' THEN excluded.branch ELSE tasks.branch END,
			status = excluded.status,
			summary = CASE WHEN excluded.summary != '' THEN excluded.summary ELSE tasks.summary END,
			git_summary_json = CASE WHEN excluded.git_summary_json != '' THEN excluded.git_summary_json ELSE tasks.git_summary_json END,
			completed_at = CASE WHEN excluded.completed_at IS NOT NULL THEN excluded.completed_at ELSE tasks.completed_at END,
			compacted_at = CASE WHEN excluded.completed_at IS NULL THEN NULL ELSE tasks.compacted_at END
	`, t.ID, t.SID, t.ExternalID, t.WorkerID, t.SpaceID, t.WorkspaceID, t.ProjectID, t.RepoID, t.WorktreeID,
		t.Agent, t.Model, t.SessionKind, t.Workdir, t.EffectiveDir, t.Branch, t.Mode, t.Origin, string(t.PermissionMode),
		t.OrphanPolicy, t.Prompt, t.Status, t.Summary, t.GitSummaryJSON, t.ExitCode, causeJSON(t.Cause),
		formatTime(t.CreatedAt), startedAt, completedAt)
	if err != nil {
		return "", "", err
	}
	return t.ID, prev, nil
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

func (s *SQLiteStore) UpdateTaskStatus(ctx context.Context, id, status, summary, gitSummaryJSON string, exitCode *int, cause store.Cause) (string, error) {
	now := formatTime(time.Now().UTC())
	var startedExpr, completedExpr any
	if status == protocol.TaskStatusRunning {
		startedExpr = now
	}
	if protocol.IsTerminalTaskStatus(status) {
		completedExpr = now
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var prev string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id).Scan(&prev); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", store.ErrNotFound
		}
		return "", err
	}
	cj := causeJSON(&cause)
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status = ?,
		    summary = CASE WHEN ? != '' THEN ? ELSE summary END,
		    git_summary_json = CASE WHEN ? != '' THEN ? ELSE git_summary_json END,
		    exit_code = COALESCE(?, exit_code),
		    cause_json = CASE WHEN ? != '' THEN ? ELSE cause_json END,
		    suspended_by = CASE WHEN ? = 'suspended' THEN suspended_by ELSE '' END,
		    started_at = COALESCE(started_at, ?),
		    completed_at = COALESCE(?, completed_at),
		    compacted_at = CASE WHEN ? IS NULL THEN NULL ELSE compacted_at END
		WHERE id = ?
	`, status, summary, summary, gitSummaryJSON, gitSummaryJSON, exitCode,
		cj, cj, status, startedExpr, completedExpr, completedExpr, id); err != nil {
		return "", err
	}
	return prev, tx.Commit()
}

// SetTaskSuspendedBy records who suspended a task.
func (s *SQLiteStore) SetTaskSuspendedBy(ctx context.Context, id, by string) error {
	return s.execOne(ctx, `UPDATE tasks SET suspended_by = ? WHERE id = ?`, by, id)
}

// SetTaskParentChat records the chat that dispatched a task.
func (s *SQLiteStore) SetTaskParentChat(ctx context.Context, id, chatID string) error {
	return s.execOne(ctx, `UPDATE tasks SET parent_chat_id = ? WHERE id = ?`, chatID, id)
}

// SetTaskNarration sets (or clears) a task's one-line narration.
func (s *SQLiteStore) SetTaskNarration(ctx context.Context, id, narration string, at time.Time) error {
	var atVal any
	if narration != "" {
		atVal = formatTime(at.UTC())
	}
	return s.execOne(ctx, `UPDATE tasks SET narration = ?, narration_at = ? WHERE id = ?`, narration, atVal, id)
}

// SetTaskTicket records the ka ticket a task works on.
func (s *SQLiteStore) SetTaskTicket(ctx context.Context, id, ticketID, ticketTitle string) error {
	return s.execOne(ctx, `UPDATE tasks SET ticket_id = ?, ticket_title = ? WHERE id = ?`, ticketID, ticketTitle, id)
}

// execOne runs an UPDATE that must touch exactly one row (else ErrNotFound).
func (s *SQLiteStore) execOne(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
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
		cause       string
		createdAt   string
		startedAt   sql.NullString
		completedAt sql.NullString
		narrationAt sql.NullString
		keep        int
		compactedAt sql.NullString
	)
	if err := scanner.Scan(
		&t.ID, &t.SID, &t.ExternalID, &workerID, &t.SpaceID, &t.WorkspaceID, &t.ProjectID, &t.RepoID, &t.WorktreeID,
		&t.Agent, &t.Model, &t.SessionKind, &t.Workdir, &t.EffectiveDir, &t.Branch, &t.Mode, &t.Origin, &perm,
		&t.OrphanPolicy, &t.Prompt, &t.Status, &t.Summary, &t.GitSummaryJSON, &exitCode,
		&cause, &t.SuspendedBy, &t.ParentChatID,
		&t.Title, &t.Narration, &narrationAt, &t.TicketID, &t.TicketTitle,
		&keep, &compactedAt, &createdAt, &startedAt, &completedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Task{}, store.ErrNotFound
		}
		return store.Task{}, err
	}
	t.WorkerID = workerID.String
	t.PermissionMode = store.PermissionMode(perm)
	t.Cause = parseCause(cause)
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
	if narrationAt.Valid && narrationAt.String != "" {
		nt := parseTime(narrationAt.String)
		t.NarrationAt = &nt
	}
	t.Keep = keep != 0
	t.CompactedAt = parseTimePtr(compactedAt)
	return t, nil
}

func (s *SQLiteStore) GetTask(ctx context.Context, id string) (store.Task, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, sid, external_id, worker_id, space_id, workspace_id, project_id, repo_id, worktree_id,
		       agent, model, session_kind, workdir, effective_dir, branch, mode, origin, permission_mode,
		       orphan_policy, prompt, status, summary, git_summary_json, exit_code,
		       cause_json, suspended_by, parent_chat_id,
		       title, narration, narration_at, ticket_id, ticket_title,
		       keep, compacted_at, created_at, started_at, completed_at
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
		SELECT id, sid, external_id, worker_id, space_id, workspace_id, project_id, repo_id, worktree_id,
		       agent, model, session_kind, workdir, effective_dir, branch, mode, origin, permission_mode,
		       orphan_policy, prompt, status, summary, git_summary_json, exit_code,
		       cause_json, suspended_by, parent_chat_id,
		       title, narration, narration_at, ticket_id, ticket_title,
		       keep, compacted_at, created_at, started_at, completed_at
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

// chatSessionCols is the column list scanned by scanChatSession.
const chatSessionCols = `id, sid, title, provider, model, created_at, updated_at,
	backend, worker_id, agent, account_id, effort, approval_mode, cwd,
	agent_session_id, status, fleet_tools, last_error_json, mcp_token_hash,
	cause_json, suspended_by, suspended_prev, notes_cursor, question_id, fork_of`

// chatSessionSelectCols adds the retention columns (written by retention and
// SetChatKeep only) to chatSessionCols.
const chatSessionSelectCols = chatSessionCols + `, keep, compacted_at, compacted_before`

func scanChatSession(scanner interface{ Scan(...any) error }) (store.ChatSession, error) {
	var cs store.ChatSession
	var cAt, uAt string
	var fleet int
	var cause string
	var keep int
	var compactedAt, compactedBefore sql.NullString
	if err := scanner.Scan(&cs.ID, &cs.SID, &cs.Title, &cs.Provider, &cs.Model, &cAt, &uAt,
		&cs.Backend, &cs.WorkerID, &cs.Agent, &cs.AccountID, &cs.Effort, &cs.ApprovalMode, &cs.Cwd,
		&cs.AgentSessionID, &cs.Status, &fleet, &cs.LastErrorJSON, &cs.MCPTokenHash,
		&cause, &cs.SuspendedBy, &cs.SuspendedPrev, &cs.NotesCursor, &cs.QuestionID, &cs.ForkOf,
		&keep, &compactedAt, &compactedBefore); err != nil {
		return store.ChatSession{}, err
	}
	cs.Keep = keep != 0
	cs.CompactedAt = parseTimePtr(compactedAt)
	cs.CompactedBefore = parseTimePtr(compactedBefore)
	cs.Cause = parseCause(cause)
	cs.FleetTools = fleet != 0
	cs.CreatedAt = parseTime(cAt)
	cs.UpdatedAt = parseTime(uAt)
	return cs, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *SQLiteStore) CreateChatSession(ctx context.Context, cs store.ChatSession) error {
	now := time.Now().UTC()
	if cs.CreatedAt.IsZero() {
		cs.CreatedAt = now
	}
	if cs.UpdatedAt.IsZero() {
		cs.UpdatedAt = now
	}
	if cs.Backend == "" {
		cs.Backend = store.ChatBackendAgent
	}
	if err := s.assignIDs(ctx, ids.Chat, &cs.ID, &cs.SID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_sessions (`+chatSessionCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, cs.ID, cs.SID, cs.Title, cs.Provider, cs.Model, formatTime(cs.CreatedAt), formatTime(cs.UpdatedAt),
		cs.Backend, cs.WorkerID, cs.Agent, cs.AccountID, cs.Effort, cs.ApprovalMode, cs.Cwd,
		cs.AgentSessionID, cs.Status, boolInt(cs.FleetTools), cs.LastErrorJSON, cs.MCPTokenHash,
		causeJSON(cs.Cause), cs.SuspendedBy, cs.SuspendedPrev, cs.NotesCursor, cs.QuestionID, cs.ForkOf)
	return err
}

func (s *SQLiteStore) GetChatSession(ctx context.Context, id string) (store.ChatSession, error) {
	cs, err := scanChatSession(s.db.QueryRowContext(ctx, `SELECT `+chatSessionSelectCols+` FROM chat_sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.ChatSession{}, store.ErrNotFound
	}
	return cs, err
}

func (s *SQLiteStore) ListChatSessions(ctx context.Context, limit int) ([]store.ChatSession, error) {
	if limit <= 0 {
		limit = 50
	}
	// Hidden side-conversation chats (question_id set) are not listed.
	return s.queryChatSessions(ctx, `SELECT `+chatSessionSelectCols+` FROM chat_sessions WHERE question_id = '' ORDER BY updated_at DESC LIMIT ?`, limit)
}

// ListChatSessionsByWorker returns the agent chat sessions hosted on workerID.
func (s *SQLiteStore) ListChatSessionsByWorker(ctx context.Context, workerID string) ([]store.ChatSession, error) {
	return s.queryChatSessions(ctx, `SELECT `+chatSessionSelectCols+` FROM chat_sessions
		WHERE worker_id = ? AND backend = 'agent' ORDER BY updated_at DESC`, workerID)
}

func (s *SQLiteStore) queryChatSessions(ctx context.Context, q string, args ...any) ([]store.ChatSession, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ChatSession
	for rows.Next() {
		cs, err := scanChatSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// UpdateChatSession writes every mutable column of an existing session.
func (s *SQLiteStore) UpdateChatSession(ctx context.Context, cs store.ChatSession) error {
	if cs.Backend == "" {
		cs.Backend = store.ChatBackendAgent
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE chat_sessions SET title = ?, provider = ?, model = ?, updated_at = ?,
			backend = ?, worker_id = ?, agent = ?, account_id = ?, effort = ?, approval_mode = ?,
			cwd = ?, agent_session_id = ?, status = ?, fleet_tools = ?, last_error_json = ?,
			mcp_token_hash = ?, cause_json = ?, suspended_by = ?, suspended_prev = ?, notes_cursor = ?
		WHERE id = ?
	`, cs.Title, cs.Provider, cs.Model, formatTime(time.Now().UTC()),
		cs.Backend, cs.WorkerID, cs.Agent, cs.AccountID, cs.Effort, cs.ApprovalMode,
		cs.Cwd, cs.AgentSessionID, cs.Status, boolInt(cs.FleetTools), cs.LastErrorJSON,
		cs.MCPTokenHash, causeJSON(cs.Cause), cs.SuspendedBy, cs.SuspendedPrev, cs.NotesCursor, cs.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) RenameChatSession(ctx context.Context, id, title string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE chat_sessions SET title = ?, updated_at = ? WHERE id = ?
	`, strings.TrimSpace(title), formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) DeleteChatSession(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_messages WHERE session_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_events WHERE chat_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM chat_sessions WHERE id = ?`, id)
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

func (s *SQLiteStore) AppendChatMessage(ctx context.Context, m store.ChatMessage) (int64, error) {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	ts := formatTime(m.CreatedAt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chat_messages (session_id, role, content, tool_json, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, m.SessionID, m.Role, m.Content, m.ToolJSON, ts)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET updated_at = ? WHERE id = ?`, ts, m.SessionID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
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
		INSERT INTO usage_events (provider, model, account_id, input_tokens, output_tokens, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, u.Provider, u.Model, u.AccountID, u.InputTokens, u.OutputTokens, formatTime(u.CreatedAt))
	return err
}

func (s *SQLiteStore) GetUsageSummary(ctx context.Context) (store.UsageSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, model, COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(requests), 0)
		FROM (
			SELECT provider, model, input_tokens, output_tokens, 1 AS requests FROM usage_events
			UNION ALL
			SELECT provider, model, input_tokens, output_tokens, requests FROM usage_daily
		)
		GROUP BY provider, model
		ORDER BY provider ASC, model ASC
	`)
	if err != nil {
		return store.UsageSummary{}, err
	}
	defer rows.Close()

	sum := store.UsageSummary{ByProvider: []store.UsageBucket{}, Quotas: []store.SubscriptionQuota{}}
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
	if err := rows.Err(); err != nil {
		return store.UsageSummary{}, err
	}
	if quotas, qErr := s.ListSubscriptionQuotas(ctx); qErr == nil {
		sum.Quotas = quotas
	}
	return sum, nil
}

func (s *SQLiteStore) UpsertSubscriptionQuota(ctx context.Context, q store.SubscriptionQuota) error {
	if q.UpdatedAt.IsZero() {
		q.UpdatedAt = time.Now().UTC()
	}
	if q.Label == "" {
		q.Label = q.Provider
	}
	if q.Source == "" {
		q.Source = "probe"
	}
	if q.ID == "" {
		q.ID = ids.UUID() // an existing (provider, window_label) row keeps its id
	}
	var resetAt any
	if q.ResetAt != nil {
		resetAt = formatTime(*q.ResetAt)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO subscription_quotas (id, provider, label, window_label, used_percent, reset_at, source, worker_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, window_label) DO UPDATE SET
			label = excluded.label,
			used_percent = excluded.used_percent,
			reset_at = COALESCE(excluded.reset_at, subscription_quotas.reset_at),
			source = excluded.source,
			worker_id = CASE WHEN excluded.worker_id != '' THEN excluded.worker_id ELSE subscription_quotas.worker_id END,
			updated_at = excluded.updated_at
	`, q.ID, q.Provider, q.Label, q.WindowLabel, q.UsedPercent, resetAt, q.Source, q.WorkerID, formatTime(q.UpdatedAt))
	return err
}

func (s *SQLiteStore) ListSubscriptionQuotas(ctx context.Context) ([]store.SubscriptionQuota, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, provider, label, window_label, used_percent, reset_at, source, worker_id, updated_at
		FROM subscription_quotas
		ORDER BY provider ASC, window_label ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []store.SubscriptionQuota{}
	for rows.Next() {
		var q store.SubscriptionQuota
		var resetAt sql.NullString
		var updatedAt string
		if err := rows.Scan(&q.ID, &q.Provider, &q.Label, &q.WindowLabel, &q.UsedPercent, &resetAt, &q.Source, &q.WorkerID, &updatedAt); err != nil {
			return nil, err
		}
		if resetAt.Valid && resetAt.String != "" {
			rt := parseTime(resetAt.String)
			q.ResetAt = &rt
		}
		q.UpdatedAt = parseTime(updatedAt)
		out = append(out, q)
	}
	return out, rows.Err()
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
		m.ID = ids.UUID() // an existing (scope, key) row keeps its id
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
