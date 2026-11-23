package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/store"
)

// Audit action names recorded in the fleet-wide audit log (KON-WP2A5).
const (
	AuditTakeControl    = "task.take_control"
	AuditReleaseControl = "task.release_control"
	AuditSteer          = "task.steer"
	AuditCancel         = "task.cancel"
	AuditGitCommit      = "git.commit"
	AuditWorkerRemove   = "worker.remove"
	AuditSettingsUpdate = "settings.update"
	AuditWakeLockFleet  = "wakelock.fleet"
	AuditWakeLockHost   = "wakelock.host"
	// Agent chat (KON-TY1XP): settings changes and answers to agent requests.
	AuditChatModel        = "chat.model"
	AuditChatApprovalMode = "chat.approval_mode"
	AuditChatAccount      = "chat.account"
	AuditChatRespond      = "chat.respond"
)

// Audit sources: the path an action arrived through.
const (
	auditSourceREST         = "rest"
	auditSourceCLI          = "cli"
	auditSourceWS           = "ws"
	auditSourceMCP          = "mcp"
	auditSourceOrchestrator = "orchestrator"
)

// clientKindHeader optionally labels first-party API clients ("cli", "mcp") so the
// audit log can show the path an action took. It is informational only: the
// recorded user always comes from authentication, never from this header.
const clientKindHeader = "X-Konsert-Client"

// auditActor identifies who performed an action and through which path.
type auditActor struct {
	User   string
	Source string
}

type auditActorKey struct{}

// withAuditActor attaches the acting identity to ctx for audit recording.
func withAuditActor(ctx context.Context, user, source string) context.Context {
	return context.WithValue(ctx, auditActorKey{}, auditActor{User: user, Source: source})
}

// withDefaultAuditActor attaches the identity only when ctx carries none yet.
func withDefaultAuditActor(ctx context.Context, user, source string) context.Context {
	if _, ok := ctx.Value(auditActorKey{}).(auditActor); ok {
		return ctx
	}
	return withAuditActor(ctx, user, source)
}

// auditActorFrom returns the identity attached to ctx, or "system" when the
// action was not initiated by an authenticated caller.
func auditActorFrom(ctx context.Context) auditActor {
	if a, ok := ctx.Value(auditActorKey{}).(auditActor); ok && a.User != "" {
		return a
	}
	return auditActor{User: "system"}
}

// restAuditSource maps the optional client-kind header to a known source label.
func restAuditSource(r *http.Request) string {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get(clientKindHeader))) {
	case auditSourceCLI:
		return auditSourceCLI
	case auditSourceMCP:
		return auditSourceMCP
	default:
		return auditSourceREST
	}
}

// recordAudit appends an audit event attributed to the actor in ctx and pushes it
// to browsers as a fleet.update of kind "audit". Failures are logged, never fatal:
// auditing must not block the control action itself.
func (h *Hub) recordAudit(ctx context.Context, e store.AuditEvent) {
	if h == nil || h.store == nil {
		return
	}
	a := auditActorFrom(ctx)
	if e.User == "" {
		e.User = a.User
	}
	if e.Source == "" {
		e.Source = a.Source
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	// Record even if the request context was canceled right after the action.
	id, err := h.store.AppendAudit(context.WithoutCancel(ctx), e)
	if err != nil {
		h.logger.Warn("record audit event failed", "action", e.Action, "err", err)
		return
	}
	e.ID = id
	h.broadcastFleetUpdate("audit", e)
}

// auditMeta renders a small metadata object as JSON ("" on failure or when empty).
func auditMeta(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// --- REST ---

// AuditListResponse is the GET /api/audit response. NextCursor, when non-zero, is
// the before_id that fetches the next (older) page.
type AuditListResponse struct {
	Events     []store.AuditEvent `json:"events"`
	NextCursor int64              `json:"next_cursor,omitempty"`
}

const (
	auditDefaultLimit = 100
	auditMaxLimit     = 500
)

// parseAuditTime accepts RFC3339 timestamps or Unix seconds.
func parseAuditTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC(), nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0).UTC(), nil
	}
	return time.Time{}, errors.New("want RFC3339 or unix seconds")
}

// auditFilterFromQuery parses GET /api/audit query parameters.
func auditFilterFromQuery(q map[string][]string) (store.AuditFilter, error) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	f := store.AuditFilter{
		WorkerID: get("worker_id"),
		TaskID:   get("task_id"),
		User:     get("user"),
		Action:   get("action"),
		Limit:    auditDefaultLimit,
	}
	var err error
	if f.Since, err = parseAuditTime(get("since")); err != nil {
		return f, errors.New("invalid since: " + err.Error())
	}
	if f.Until, err = parseAuditTime(get("until")); err != nil {
		return f, errors.New("invalid until: " + err.Error())
	}
	parseID := func(name string) (int64, error) {
		v := get(name)
		if v == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, errors.New("invalid " + name)
		}
		return n, nil
	}
	// "cursor" is an alias of before_id (the next_cursor of a previous page).
	if f.BeforeID, err = parseID("before_id"); err != nil {
		return f, err
	}
	if f.BeforeID == 0 {
		if f.BeforeID, err = parseID("cursor"); err != nil {
			return f, err
		}
	}
	if f.AfterID, err = parseID("after_id"); err != nil {
		return f, err
	}
	if v := get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return f, errors.New("invalid limit")
		}
		f.Limit = min(n, auditMaxLimit)
	}
	return f, nil
}

// listAudit runs a filtered query and computes the next-page cursor.
func listAudit(ctx context.Context, st store.Store, f store.AuditFilter) (AuditListResponse, error) {
	events, err := st.ListAudit(ctx, f)
	if err != nil {
		return AuditListResponse{}, err
	}
	if events == nil {
		events = []store.AuditEvent{}
	}
	resp := AuditListResponse{Events: events}
	if f.Limit > 0 && len(events) == f.Limit {
		resp.NextCursor = events[len(events)-1].ID
	}
	return resp, nil
}

// handleListAudit serves GET /api/audit.
func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	f, err := auditFilterFromQuery(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	resp, err := listAudit(r.Context(), s.store, f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleTaskControl serves POST /api/tasks/{id}/control {"enabled": bool}: the
// PWA's Take Control (Interactive) toggle. The hub does not gate PTY input on it;
// this is the audit signal that an operator took or released interactive control.
func (s *Server) handleTaskControl(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled is required"})
		return
	}
	t, err := s.store.GetTask(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	action, msg := AuditReleaseControl, "Released interactive control"
	if *body.Enabled {
		action, msg = AuditTakeControl, "Took interactive control"
	}
	s.hub.recordAudit(r.Context(), store.AuditEvent{
		Action:   action,
		WorkerID: t.WorkerID,
		TaskID:   t.ID,
		Message:  msg,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task_id": t.ID, "enabled": *body.Enabled})
}

// --- MCP ---

// AuditBackend is implemented by MCP backends that can read the audit log.
type AuditBackend interface {
	ListAudit(ctx context.Context, in ListAuditInput) (AuditListResponse, error)
}

// ListAuditInput is the read-only MCP list_audit tool input.
type ListAuditInput struct {
	WorkerID string `json:"worker_id,omitempty" jsonschema:"only events on this worker/host ID"`
	TaskID   string `json:"task_id,omitempty" jsonschema:"only events for this task ID"`
	User     string `json:"user,omitempty" jsonschema:"only events by this user"`
	Action   string `json:"action,omitempty" jsonschema:"only this action, e.g. task.steer, task.cancel, git.commit"`
	Since    string `json:"since,omitempty" jsonschema:"RFC3339 lower time bound"`
	Until    string `json:"until,omitempty" jsonschema:"RFC3339 upper time bound"`
	BeforeID int64  `json:"before_id,omitempty" jsonschema:"page cursor: next_cursor of the previous call"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max events (default 100, max 500)"`
}

func (in ListAuditInput) query() map[string][]string {
	q := map[string][]string{}
	set := func(k, v string) {
		if v != "" {
			q[k] = []string{v}
		}
	}
	set("worker_id", in.WorkerID)
	set("task_id", in.TaskID)
	set("user", in.User)
	set("action", in.Action)
	set("since", in.Since)
	set("until", in.Until)
	if in.BeforeID > 0 {
		set("before_id", strconv.FormatInt(in.BeforeID, 10))
	}
	if in.Limit > 0 {
		set("limit", strconv.Itoa(in.Limit))
	}
	return q
}

// ListAudit implements AuditBackend on the local FleetService.
func (s *FleetService) ListAudit(ctx context.Context, in ListAuditInput) (AuditListResponse, error) {
	f, err := auditFilterFromQuery(in.query())
	if err != nil {
		return AuditListResponse{}, err
	}
	return listAudit(ctx, s.store, f)
}
