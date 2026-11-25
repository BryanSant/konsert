package sqlite

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
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

func parseOptTime(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parseTime(v.String)
	return &t
}

func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}

// EnsureDefaultProject guarantees that the built-in Default Project always exists.
func (s *SQLiteStore) EnsureDefaultProject(ctx context.Context) error {
	now := formatTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO projects (id, sid, name, is_default, created_at)
		VALUES (?, ?, 'Default', 1, ?)
	`, store.DefaultProjectID, ids.DefaultProjectSID, now)
	return err
}

// --- Projects ---

func marshalAutonomy(a store.AutonomyPolicy) (string, error) {
	b, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalAutonomy(raw string) store.AutonomyPolicy {
	var a store.AutonomyPolicy
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &a)
	}
	return a
}

const projectColumns = `id, sid, name, is_default, headline, headline_at, verify_cmd, autonomy_json, delivery_mode, merge_style, created_at`

func scanProject(scanner interface{ Scan(...any) error }) (store.Project, error) {
	var (
		p            store.Project
		isDef        int
		headlineAt   sql.NullString
		autonomyJSON string
		createdAt    string
	)
	if err := scanner.Scan(
		&p.ID, &p.SID, &p.Name, &isDef, &p.Headline, &headlineAt,
		&p.VerifyCmd, &autonomyJSON, &p.DeliveryMode, &p.MergeStyle, &createdAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Project{}, store.ErrNotFound
		}
		return store.Project{}, err
	}
	p.IsDefault = isDef != 0
	p.HeadlineAt = parseOptTime(headlineAt)
	p.Autonomy = unmarshalAutonomy(autonomyJSON)
	p.CreatedAt = parseTime(createdAt)
	return p, nil
}

func (s *SQLiteStore) CreateProject(ctx context.Context, p store.Project) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return fmt.Errorf("%w: project name is required", store.ErrInvalidWork)
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if err := s.assignIDs(ctx, ids.Project, &p.ID, &p.SID); err != nil {
		return err
	}
	autonomyJSON, err := marshalAutonomy(p.Autonomy)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO projects (id, sid, name, is_default, verify_cmd, autonomy_json, delivery_mode, merge_style, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.ID, p.SID, p.Name, boolToInt(p.IsDefault), p.VerifyCmd, autonomyJSON, p.DeliveryMode, p.MergeStyle, formatTime(p.CreatedAt))
	return err
}

func (s *SQLiteStore) GetProject(ctx context.Context, id string) (store.Project, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id)
	return scanProject(row)
}

func (s *SQLiteStore) UpdateProject(ctx context.Context, p store.Project) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return fmt.Errorf("%w: project name is required", store.ErrInvalidWork)
	}
	autonomyJSON, err := marshalAutonomy(p.Autonomy)
	if err != nil {
		return err
	}
	return s.execOne(ctx, `
		UPDATE projects
		SET name = ?, verify_cmd = ?, autonomy_json = ?, delivery_mode = ?, merge_style = ?
		WHERE id = ?
	`, p.Name, p.VerifyCmd, autonomyJSON, p.DeliveryMode, p.MergeStyle, p.ID)
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

func (s *SQLiteStore) ListProjects(ctx context.Context) ([]store.Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY is_default DESC, created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []store.Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
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
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET project_id = ?, repo_id = ''
		WHERE project_id = ?
	`, store.DefaultProjectID, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ? AND is_default = 0`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return tx.Commit()
}

// --- Repos (per-project registry keyed by remote_url) ---

const repoColumns = `id, sid, project_id, name, remote_url, default_branch, verify_cmd, delivery_mode, merge_style, created_at`

func scanRepo(scanner interface{ Scan(...any) error }) (store.Repo, error) {
	var (
		r         store.Repo
		createdAt string
	)
	if err := scanner.Scan(
		&r.ID, &r.SID, &r.ProjectID, &r.Name, &r.RemoteURL,
		&r.DefaultBranch, &r.VerifyCmd, &r.DeliveryMode, &r.MergeStyle, &createdAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Repo{}, store.ErrNotFound
		}
		return store.Repo{}, err
	}
	r.CreatedAt = parseTime(createdAt)
	return r, nil
}

func (s *SQLiteStore) CreateRepo(ctx context.Context, r store.Repo) error {
	if r.ProjectID == "" {
		r.ProjectID = store.DefaultProjectID
	}
	r.RemoteURL = strings.TrimSpace(r.RemoteURL)
	if r.RemoteURL == "" {
		return fmt.Errorf("%w: remote_url is required", store.ErrInvalidWork)
	}
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return fmt.Errorf("%w: repo name is required", store.ErrInvalidWork)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if err := s.assignIDs(ctx, ids.Repo, &r.ID, &r.SID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO repos (id, sid, project_id, name, remote_url, default_branch, verify_cmd, delivery_mode, merge_style, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, r.ID, r.SID, r.ProjectID, r.Name, r.RemoteURL, r.DefaultBranch, r.VerifyCmd, r.DeliveryMode, r.MergeStyle, formatTime(r.CreatedAt))
	if isUniqueConstraint(err) {
		return fmt.Errorf("%w: repo %q already registered in project", store.ErrDuplicate, r.RemoteURL)
	}
	return err
}

func (s *SQLiteStore) GetRepo(ctx context.Context, id string) (store.Repo, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+repoColumns+` FROM repos WHERE id = ?`, id)
	return scanRepo(row)
}

func (s *SQLiteStore) UpdateRepo(ctx context.Context, r store.Repo) error {
	r.RemoteURL = strings.TrimSpace(r.RemoteURL)
	if r.RemoteURL == "" {
		return fmt.Errorf("%w: remote_url is required", store.ErrInvalidWork)
	}
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return fmt.Errorf("%w: repo name is required", store.ErrInvalidWork)
	}
	err := s.execOne(ctx, `
		UPDATE repos
		SET name = ?, remote_url = ?, default_branch = ?, verify_cmd = ?, delivery_mode = ?, merge_style = ?
		WHERE id = ?
	`, r.Name, r.RemoteURL, r.DefaultBranch, r.VerifyCmd, r.DeliveryMode, r.MergeStyle, r.ID)
	if isUniqueConstraint(err) {
		return fmt.Errorf("%w: repo %q already registered in project", store.ErrDuplicate, r.RemoteURL)
	}
	return err
}

// UpdateRepoDelivery updates a repo's delivery mode, merge style, and remote URL.
func (s *SQLiteStore) UpdateRepoDelivery(ctx context.Context, id, deliveryMode, mergeStyle, remoteURL string) error {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return fmt.Errorf("%w: remote_url is required", store.ErrInvalidWork)
	}
	err := s.execOne(ctx, `UPDATE repos SET delivery_mode = ?, merge_style = ?, remote_url = ? WHERE id = ?`, deliveryMode, mergeStyle, remoteURL, id)
	if isUniqueConstraint(err) {
		return fmt.Errorf("%w: repo %q already registered in project", store.ErrDuplicate, remoteURL)
	}
	return err
}

func (s *SQLiteStore) ListRepos(ctx context.Context, projectID string) ([]store.Repo, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if projectID != "" {
		rows, err = s.db.QueryContext(ctx, `SELECT `+repoColumns+` FROM repos WHERE project_id = ? ORDER BY created_at ASC`, projectID)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT `+repoColumns+` FROM repos ORDER BY created_at ASC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []store.Repo{}
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteRepo(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET repo_id = '' WHERE repo_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM repos WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return tx.Commit()
}

// --- Tickets ---

const ticketColumns = `id, sid, project_id, parent_id, kind, title, body, acceptance_json, labels_json, priority, status, hints_json, verify_cmd, autonomy_json, created_at, updated_at`

func scanTicket(scanner interface{ Scan(...any) error }) (store.Ticket, error) {
	var (
		t              store.Ticket
		parentID       sql.NullString
		acceptanceJSON string
		labelsJSON     string
		hintsJSON      string
		autonomyJSON   string
		createdAt      string
		updatedAt      string
	)
	if err := scanner.Scan(
		&t.ID, &t.SID, &t.ProjectID, &parentID, &t.Kind, &t.Title, &t.Body,
		&acceptanceJSON, &labelsJSON, &t.Priority, &t.Status,
		&hintsJSON, &t.VerifyCmd, &autonomyJSON, &createdAt, &updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Ticket{}, store.ErrNotFound
		}
		return store.Ticket{}, err
	}
	t.ParentID = parentID.String
	_ = json.Unmarshal([]byte(acceptanceJSON), &t.Acceptance)
	if t.Acceptance == nil {
		t.Acceptance = []store.AcceptanceItem{}
	}
	_ = json.Unmarshal([]byte(labelsJSON), &t.Labels)
	if t.Labels == nil {
		t.Labels = []string{}
	}
	if strings.TrimSpace(hintsJSON) != "" {
		_ = json.Unmarshal([]byte(hintsJSON), &t.Hints)
	}
	t.Autonomy = unmarshalAutonomy(autonomyJSON)
	t.CreatedAt = parseTime(createdAt)
	t.UpdatedAt = parseTime(updatedAt)
	return t, nil
}

func (s *SQLiteStore) validateParentInTx(ctx context.Context, tx *sql.Tx, ticketID, projectID, childKind, parentID string) error {
	if parentID == "" {
		return nil
	}
	if ticketID != "" && parentID == ticketID {
		return fmt.Errorf("%w: ticket cannot be its own parent", store.ErrCycle)
	}
	var parentProject, parentKind string
	err := tx.QueryRowContext(ctx, `SELECT project_id, kind FROM tickets WHERE id = ?`, parentID).Scan(&parentProject, &parentKind)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: parent ticket %s not found", store.ErrInvalidWork, parentID)
	}
	if err != nil {
		return err
	}
	if parentProject != projectID {
		return fmt.Errorf("%w: parent ticket belongs to a different project", store.ErrInvalidWork)
	}
	if err := store.ValidateTicketHierarchy(childKind, parentKind); err != nil {
		return err
	}
	if ticketID != "" {
		// Ensure parentID is not a descendant of ticketID (no parent cycle).
		var hit int
		err := tx.QueryRowContext(ctx, `
			WITH RECURSIVE anc(id) AS (
				SELECT parent_id FROM tickets WHERE id = ? AND parent_id IS NOT NULL
				UNION
				SELECT t.parent_id FROM tickets t JOIN anc a ON t.id = a.id WHERE t.parent_id IS NOT NULL
			)
			SELECT 1 FROM anc WHERE id = ? LIMIT 1
		`, parentID, ticketID).Scan(&hit)
		if err == nil {
			return fmt.Errorf("%w: parent %s is a descendant of %s", store.ErrCycle, parentID, ticketID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) CreateTicket(ctx context.Context, t store.Ticket) (store.Ticket, error) {
	if err := store.NormalizeAndValidateTicket(&t); err != nil {
		return store.Ticket{}, err
	}
	if t.ProjectID == "" {
		t.ProjectID = store.DefaultProjectID
	}
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = t.CreatedAt
	}
	if err := s.assignIDs(ctx, ids.Ticket, &t.ID, &t.SID); err != nil {
		return store.Ticket{}, err
	}
	acceptanceJSON, err := json.Marshal(t.Acceptance)
	if err != nil {
		return store.Ticket{}, err
	}
	labelsJSON, err := json.Marshal(t.Labels)
	if err != nil {
		return store.Ticket{}, err
	}
	hintsJSON, err := json.Marshal(t.Hints)
	if err != nil {
		return store.Ticket{}, err
	}
	autonomyJSON, err := marshalAutonomy(t.Autonomy)
	if err != nil {
		return store.Ticket{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Ticket{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var projExists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, t.ProjectID).Scan(&projExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Ticket{}, fmt.Errorf("%w: project %s not found", store.ErrNotFound, t.ProjectID)
		}
		return store.Ticket{}, err
	}
	if err := s.validateParentInTx(ctx, tx, t.ID, t.ProjectID, t.Kind, t.ParentID); err != nil {
		return store.Ticket{}, err
	}
	var parentVal any
	if t.ParentID != "" {
		parentVal = t.ParentID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tickets (
			id, sid, project_id, parent_id, kind, title, body,
			acceptance_json, labels_json, priority, status,
			hints_json, verify_cmd, autonomy_json, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, t.ID, t.SID, t.ProjectID, parentVal, t.Kind, t.Title, t.Body,
		string(acceptanceJSON), string(labelsJSON), t.Priority, t.Status,
		string(hintsJSON), t.VerifyCmd, autonomyJSON, formatTime(t.CreatedAt), formatTime(t.UpdatedAt)); err != nil {
		return store.Ticket{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.Ticket{}, err
	}
	return t, nil
}

func (s *SQLiteStore) GetTicket(ctx context.Context, id string) (store.Ticket, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE id = ?`, id)
	return scanTicket(row)
}

func (s *SQLiteStore) UpdateTicket(ctx context.Context, t store.Ticket) (store.Ticket, error) {
	if err := store.NormalizeAndValidateTicket(&t); err != nil {
		return store.Ticket{}, err
	}
	acceptanceJSON, err := json.Marshal(t.Acceptance)
	if err != nil {
		return store.Ticket{}, err
	}
	labelsJSON, err := json.Marshal(t.Labels)
	if err != nil {
		return store.Ticket{}, err
	}
	hintsJSON, err := json.Marshal(t.Hints)
	if err != nil {
		return store.Ticket{}, err
	}
	autonomyJSON, err := marshalAutonomy(t.Autonomy)
	if err != nil {
		return store.Ticket{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Ticket{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var curProject, curKind, curSID, curCreated string
	err = tx.QueryRowContext(ctx, `SELECT sid, project_id, kind, created_at FROM tickets WHERE id = ?`, t.ID).Scan(&curSID, &curProject, &curKind, &curCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Ticket{}, store.ErrNotFound
	}
	if err != nil {
		return store.Ticket{}, err
	}
	if t.ProjectID == "" {
		t.ProjectID = curProject
	}
	if t.ProjectID != curProject {
		return store.Ticket{}, fmt.Errorf("%w: moving a ticket across projects is not supported", store.ErrInvalidWork)
	}
	if err := s.validateParentInTx(ctx, tx, t.ID, t.ProjectID, t.Kind, t.ParentID); err != nil {
		return store.Ticket{}, err
	}
	if t.Kind != curKind {
		// Validate that existing children remain valid under the new kind.
		rows, err := tx.QueryContext(ctx, `SELECT kind FROM tickets WHERE parent_id = ?`, t.ID)
		if err != nil {
			return store.Ticket{}, err
		}
		var childKinds []string
		for rows.Next() {
			var ck string
			if err := rows.Scan(&ck); err != nil {
				_ = rows.Close()
				return store.Ticket{}, err
			}
			childKinds = append(childKinds, ck)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return store.Ticket{}, err
		}
		for _, ck := range childKinds {
			if err := store.ValidateTicketHierarchy(ck, t.Kind); err != nil {
				return store.Ticket{}, err
			}
		}
	}

	now := time.Now().UTC()
	t.SID = curSID
	t.CreatedAt = parseTime(curCreated)
	t.UpdatedAt = now
	var parentVal any
	if t.ParentID != "" {
		parentVal = t.ParentID
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tickets
		SET parent_id = ?, kind = ?, title = ?, body = ?,
		    acceptance_json = ?, labels_json = ?, priority = ?, status = ?,
		    hints_json = ?, verify_cmd = ?, autonomy_json = ?, updated_at = ?
		WHERE id = ?
	`, parentVal, t.Kind, t.Title, t.Body,
		string(acceptanceJSON), string(labelsJSON), t.Priority, t.Status,
		string(hintsJSON), t.VerifyCmd, autonomyJSON, formatTime(t.UpdatedAt), t.ID); err != nil {
		return store.Ticket{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.Ticket{}, err
	}
	return t, nil
}

func (s *SQLiteStore) ListTickets(ctx context.Context, f store.TicketFilter) ([]store.Ticket, error) {
	var conds []string
	var args []any
	if f.ProjectID != "" {
		conds = append(conds, "project_id = ?")
		args = append(args, f.ProjectID)
	}
	if f.ParentID != "" {
		conds = append(conds, "parent_id = ?")
		args = append(args, f.ParentID)
	} else if f.RootOnly {
		conds = append(conds, "parent_id IS NULL")
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.Kind != "" {
		conds = append(conds, "kind = ?")
		args = append(args, f.Kind)
	}

	q := `SELECT ` + ticketColumns + ` FROM tickets`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY priority ASC, created_at ASC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []store.Ticket{}
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteTicket(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET ticket_id = '' WHERE ticket_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM tickets WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return tx.Commit()
}

// --- Ticket Edges (blocked_by DAG with cycle rejection) ---

func (s *SQLiteStore) AddTicketEdge(ctx context.Context, ticketID, blockedByID string) error {
	if ticketID == "" || blockedByID == "" {
		return fmt.Errorf("%w: ticket_id and blocked_by_id are required", store.ErrInvalidWork)
	}
	if ticketID == blockedByID {
		return fmt.Errorf("%w: ticket cannot block itself", store.ErrCycle)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM tickets WHERE id IN (?, ?)`, ticketID, blockedByID).Scan(&count); err != nil {
		return err
	}
	if count != 2 {
		return store.ErrNotFound
	}

	// Reject cycles: if blockedByID already transitively depends on ticketID, adding
	// ticketID -> blockedByID would close a cycle.
	var hit int
	err = tx.QueryRowContext(ctx, `
		WITH RECURSIVE reach(id) AS (
			SELECT blocked_by_id FROM ticket_edges WHERE ticket_id = ?
			UNION
			SELECT e.blocked_by_id FROM ticket_edges e JOIN reach r ON e.ticket_id = r.id
		)
		SELECT 1 FROM reach WHERE id = ? LIMIT 1
	`, blockedByID, ticketID).Scan(&hit)
	if err == nil {
		return fmt.Errorf("%w: adding edge %s blocked_by %s creates a cycle", store.ErrCycle, ticketID, blockedByID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO ticket_edges (ticket_id, blocked_by_id, created_at)
		VALUES (?, ?, ?)
	`, ticketID, blockedByID, formatTime(time.Now().UTC())); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) RemoveTicketEdge(ctx context.Context, ticketID, blockedByID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ticket_edges WHERE ticket_id = ? AND blocked_by_id = ?`, ticketID, blockedByID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListTicketEdges(ctx context.Context, ticketID string) ([]store.TicketEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ticket_id, blocked_by_id, created_at
		FROM ticket_edges WHERE ticket_id = ? ORDER BY created_at ASC
	`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.TicketEdge{}
	for rows.Next() {
		var e store.TicketEdge
		var createdAt string
		if err := rows.Scan(&e.TicketID, &e.BlockedByID, &createdAt); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ListTicketBlocking(ctx context.Context, blockedByID string) ([]store.TicketEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ticket_id, blocked_by_id, created_at
		FROM ticket_edges WHERE blocked_by_id = ? ORDER BY created_at ASC
	`, blockedByID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.TicketEdge{}
	for rows.Next() {
		var e store.TicketEdge
		var createdAt string
		if err := rows.Scan(&e.TicketID, &e.BlockedByID, &createdAt); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- Ticket Repo Bindings ---

func (s *SQLiteStore) PutTicketRepoBinding(ctx context.Context, b store.TicketRepoBinding) error {
	if b.TicketID == "" || b.RepoID == "" {
		return fmt.Errorf("%w: ticket_id and repo_id are required", store.ErrInvalidWork)
	}
	var ticketProj, repoProj string
	if err := s.db.QueryRowContext(ctx, `SELECT project_id FROM tickets WHERE id = ?`, b.TicketID).Scan(&ticketProj); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT project_id FROM repos WHERE id = ?`, b.RepoID).Scan(&repoProj); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	if ticketProj != repoProj {
		return fmt.Errorf("%w: repo belongs to a different project than ticket", store.ErrInvalidWork)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ticket_repo_bindings (ticket_id, repo_id, base_branch, target_branch)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(ticket_id, repo_id) DO UPDATE SET
			base_branch = excluded.base_branch,
			target_branch = excluded.target_branch
	`, b.TicketID, b.RepoID, strings.TrimSpace(b.BaseBranch), strings.TrimSpace(b.TargetBranch))
	return err
}

func (s *SQLiteStore) RemoveTicketRepoBinding(ctx context.Context, ticketID, repoID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ticket_repo_bindings WHERE ticket_id = ? AND repo_id = ?`, ticketID, repoID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListTicketRepoBindings(ctx context.Context, ticketID string) ([]store.TicketRepoBinding, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ticket_id, repo_id, base_branch, target_branch
		FROM ticket_repo_bindings WHERE ticket_id = ? ORDER BY repo_id ASC
	`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.TicketRepoBinding{}
	for rows.Next() {
		var b store.TicketRepoBinding
		if err := rows.Scan(&b.TicketID, &b.RepoID, &b.BaseBranch, &b.TargetBranch); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// --- External Refs ---

func (s *SQLiteStore) PutExternalRef(ctx context.Context, ref store.ExternalRef) error {
	ref.Tracker = strings.TrimSpace(ref.Tracker)
	ref.ExternalID = strings.TrimSpace(ref.ExternalID)
	if ref.TicketID == "" || ref.Tracker == "" || ref.ExternalID == "" {
		return fmt.Errorf("%w: ticket_id, tracker, and external_id are required", store.ErrInvalidWork)
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tickets WHERE id = ?`, ref.TicketID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO external_refs (ticket_id, tracker, external_id, url, version)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(ticket_id, tracker, external_id) DO UPDATE SET
			url = excluded.url,
			version = excluded.version
	`, ref.TicketID, ref.Tracker, ref.ExternalID, strings.TrimSpace(ref.URL), strings.TrimSpace(ref.Version))
	return err
}

func (s *SQLiteStore) RemoveExternalRef(ctx context.Context, ticketID, tracker, externalID string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM external_refs WHERE ticket_id = ? AND tracker = ? AND external_id = ?
	`, ticketID, strings.TrimSpace(tracker), strings.TrimSpace(externalID))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListExternalRefs(ctx context.Context, ticketID string) ([]store.ExternalRef, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ticket_id, tracker, external_id, url, version
		FROM external_refs WHERE ticket_id = ? ORDER BY tracker ASC, external_id ASC
	`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.ExternalRef{}
	for rows.Next() {
		var r store.ExternalRef
		if err := rows.Scan(&r.TicketID, &r.Tracker, &r.ExternalID, &r.URL, &r.Version); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- Computed Rollups (never stored) ---

// GetTicketRollup computes done/total, running, and blocked counts across all non-container
// descendant tickets of id (an initiative or epic).
func (s *SQLiteStore) GetTicketRollup(ctx context.Context, id string) (store.TicketRollup, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tickets WHERE id = ?`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.TicketRollup{}, store.ErrNotFound
		}
		return store.TicketRollup{}, err
	}
	var r store.TicketRollup
	r.TicketID = id
	err := s.db.QueryRowContext(ctx, `
		WITH RECURSIVE desc_tickets(id, kind, status) AS (
			SELECT id, kind, status FROM tickets WHERE parent_id = ?
			UNION ALL
			SELECT t.id, t.kind, t.status FROM tickets t JOIN desc_tickets d ON t.parent_id = d.id
		)
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN status = 'done' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'running' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE
				WHEN status = 'needs_you' THEN 1
				WHEN status NOT IN ('done', 'failed', 'canceled') AND EXISTS (
					SELECT 1 FROM ticket_edges e
					JOIN tickets b ON b.id = e.blocked_by_id
					WHERE e.ticket_id = desc_tickets.id AND b.status != 'done'
				) THEN 1
				ELSE 0
			END), 0)
		FROM desc_tickets
		WHERE kind NOT IN ('initiative', 'epic')
	`, id).Scan(&r.Total, &r.Done, &r.Running, &r.Blocked)
	return r, err
}
