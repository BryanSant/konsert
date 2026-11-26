package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/KanArtist/ka/pkg/kaspec"
)

var (
	// ErrInvalidWork is returned when a work-model record fails validation
	// (invalid kind, status, priority, hierarchy nesting, missing required fields, etc.).
	ErrInvalidWork = errors.New("invalid work item")
	// ErrCycle is returned when adding a blocked_by edge or changing a parent would create a cycle.
	ErrCycle = errors.New("dependency cycle detected")
	// ErrDuplicate is returned when a unique constraint (e.g. repo remote_url per project) is violated.
	ErrDuplicate = errors.New("duplicate record")
)

// Ticket kinds (ka vocabulary: containers initiative/epic + work ticket kinds).
const (
	TicketKindInitiative = kaspec.KindInitiative
	TicketKindEpic       = kaspec.KindEpic
	TicketKindFeature    = "feature"
	TicketKindBug        = "bug"
	TicketKindChore      = "chore"
	TicketKindSpike      = "spike"
)

var TicketKinds = []string{
	TicketKindInitiative,
	TicketKindEpic,
	TicketKindFeature,
	TicketKindBug,
	TicketKindChore,
	TicketKindSpike,
}

// Ticket statuses (docs/design/work.md).
const (
	TicketStatusBacklog  = "backlog"
	TicketStatusReady    = "ready"
	TicketStatusRunning  = "running"
	TicketStatusNeedsYou = "needs_you"
	TicketStatusReview   = "review"
	TicketStatusDone     = "done"
	TicketStatusFailed   = "failed"
	TicketStatusCanceled = "canceled"
)

// Attempt statuses stored on attempts.status. An empty status means the attempt
// is still open (a turn has been dispatched and has not finished).
const (
	AttemptReview    = "review"
	AttemptNeedsYou  = "needs_you"
	AttemptFailed    = "failed"
	AttemptCompleted = "completed"
	AttemptCanceled  = "canceled"
)

// ValidAttemptStatus reports whether s may be stored on an attempt row.
// The empty string is the open (in-flight) status.
func ValidAttemptStatus(s string) bool {
	switch s {
	case "", AttemptReview, AttemptNeedsYou, AttemptFailed, AttemptCompleted, AttemptCanceled:
		return true
	default:
		return false
	}
}

var TicketStatuses = []string{
	TicketStatusBacklog,
	TicketStatusReady,
	TicketStatusRunning,
	TicketStatusNeedsYou,
	TicketStatusReview,
	TicketStatusDone,
	TicketStatusFailed,
	TicketStatusCanceled,
}

// TicketPriorities reuses ka's fixed priority vocabulary (P0..P3).
var TicketPriorities = kaspec.Priorities

// AcceptanceItem reuses kaspec.AcceptanceItem ({Text, Done}).
type AcceptanceItem = kaspec.AcceptanceItem

// ValidTicketKind reports whether k is a valid ticket kind.
func ValidTicketKind(k string) bool {
	return slices.Contains(TicketKinds, k)
}

// ValidTicketStatus reports whether s is a valid ticket status.
func ValidTicketStatus(s string) bool {
	return slices.Contains(TicketStatuses, s)
}

// ValidTicketPriority reports whether p is a valid ka priority (P0..P3).
func ValidTicketPriority(p string) bool {
	return slices.Contains(TicketPriorities, p)
}

// IsContainerKind reports whether k is a container kind (initiative or epic).
func IsContainerKind(k string) bool {
	return kaspec.IsContainerKind(k)
}

// ValidateTicketHierarchy checks ka's parent/child kind rules:
//   - initiative cannot have a parent;
//   - epic may have no parent or an initiative parent;
//   - other tickets may have an initiative, epic, or another ticket as parent.
func ValidateTicketHierarchy(childKind, parentKind string) error {
	if parentKind == "" {
		return nil
	}
	if msg := kaspec.HierarchyProblem(childKind, parentKind); msg != "" {
		return fmt.Errorf("%w: %s", ErrInvalidWork, msg)
	}
	return nil
}

// Ticket orphan policies: what happens when a ticket's lease expires (machine lost).
const (
	TicketOrphanPolicyRequeue = "requeue"
	TicketOrphanPolicyFail    = "fail"
)

// DefaultTicketLeaseTTL is the default duration of a ticket lease (docs/design/work.md).
const DefaultTicketLeaseTTL = 10 * time.Minute

// ValidTicketOrphanPolicy reports whether p is a valid ticket orphan policy.
func ValidTicketOrphanPolicy(p string) bool {
	return p == TicketOrphanPolicyRequeue || p == TicketOrphanPolicyFail
}

// AutonomyPolicy holds project defaults and per-ticket overrides for Autopilot.
// Pointer fields allow per-ticket overrides to distinguish unset from explicit false/0.
type AutonomyPolicy struct {
	RetryLimit         *int  `json:"retry_limit,omitzero"`
	CrossReview        *bool `json:"cross_review,omitzero"`
	EscalateOnBlocker  *bool `json:"escalate_on_blocker,omitzero"`
	ExpandScope        *bool `json:"expand_scope,omitzero"`
	OptimisticChaining *bool `json:"optimistic_chaining,omitzero"`
}

// TicketLease records the active worker lease on a running or needs_you ticket.
type TicketLease struct {
	WorkerID    string     `json:"worker_id"`
	TaskID      string     `json:"task_id,omitempty"`
	ExpiresAt   time.Time  `json:"expires_at"`
	HeartbeatAt *time.Time `json:"heartbeat_at,omitzero"`
}

// ExpiredTicketLease describes a ticket whose lease expired and was transitioned
// according to its orphan policy.
type ExpiredTicketLease struct {
	Ticket      Ticket `json:"ticket"`
	PriorStatus string `json:"prior_status"`
	WorkerID    string `json:"worker_id"`
	TaskID      string `json:"task_id,omitempty"`
}

// TicketHints are optional placement hints for Autopilot scheduling.
type TicketHints struct {
	Machine  string   `json:"machine,omitempty"`
	Agent    string   `json:"agent,omitempty"`
	Model    string   `json:"model,omitempty"`
	Effort   string   `json:"effort,omitempty"`
	OS       string   `json:"os,omitempty"`
	Arch     string   `json:"arch,omitempty"`
	GPU      bool     `json:"gpu,omitzero"`
	MinRAMGB int      `json:"min_ram_gb,omitzero"`
	Tools    []string `json:"tools,omitempty"`
}

// Ticket is a work item in Syre's work model (docs/design/work.md):
// ka fields plus Syre runtime fields. Attempt history is one row per loop
// iteration in the attempts table; Ticket.Attempt is the latest attempt number.
type Ticket struct {
	ID           string           `json:"id"`
	SID          string           `json:"sid"`
	ProjectID    string           `json:"project_id"`
	ParentID     string           `json:"parent_id,omitempty"`
	Kind         string           `json:"kind"`
	Title        string           `json:"title"`
	Body         string           `json:"body,omitempty"`
	Acceptance   []AcceptanceItem `json:"acceptance"`
	Labels       []string         `json:"labels"`
	Priority     string           `json:"priority"`
	Status       string           `json:"status"`
	OrphanPolicy string           `json:"orphan_policy"`
	Hints        TicketHints      `json:"hints,omitzero"`
	VerifyCmd    string           `json:"verify_cmd,omitempty"`
	Autonomy     AutonomyPolicy   `json:"autonomy,omitzero"`
	Attempt      int              `json:"attempt,omitzero"`
	Lease        *TicketLease     `json:"lease,omitzero"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

// Attempt is one loop iteration of a ticket or an ad-hoc task.
// Machine is filled by REST and inspect from the worker name; it is not stored.
type Attempt struct {
	ID              string     `json:"id"`
	TicketID        string     `json:"ticket_id,omitempty"`
	TaskID          string     `json:"task_id"`
	ChatID          string     `json:"chat_id,omitempty"`
	Attempt         int        `json:"attempt"`
	WorkerID        string     `json:"worker_id,omitempty"`
	AccountID       string     `json:"account_id,omitempty"`
	Agent           string     `json:"agent,omitempty"`
	Model           string     `json:"model,omitempty"`
	Machine         string     `json:"machine,omitempty"`
	QueuedAt        *time.Time `json:"queued_at,omitzero"`
	StartedAt       *time.Time `json:"started_at,omitzero"`
	EndedAt         *time.Time `json:"ended_at,omitzero"`
	QueueWaitS      *int64     `json:"queue_wait_s,omitzero"`
	DurationS       *int64     `json:"duration_s,omitzero"`
	ExitCode        *int       `json:"exit_code,omitzero"`
	Status          string     `json:"status,omitempty"`
	FailureCategory string     `json:"failure_category,omitempty"`
	VerifyCmd       string     `json:"verify_cmd,omitempty"`
	VerifyExit      *int       `json:"verify_exit,omitzero"`
	VerifyPassed    *int       `json:"verify_passed,omitzero"`
	VerifyFailed    *int       `json:"verify_failed,omitzero"`
	TokensIn        int64      `json:"tokens_in,omitzero"`
	TokensOut       int64      `json:"tokens_out,omitzero"`
	CostUSD         *float64   `json:"cost_usd,omitzero"`
	GitJSON         string     `json:"git_json,omitempty"`
	// Substitution records that placement ran this attempt on a different agent
	// than the one requested, because a provider.disable was in force.
	Substitution string `json:"substitution,omitempty"`
}

// NormalizeAndValidateTicket fills default values for omitted fields and validates invariants.
func NormalizeAndValidateTicket(t *Ticket) error {
	t.Title = strings.TrimSpace(t.Title)
	if t.Title == "" {
		return fmt.Errorf("%w: title is required", ErrInvalidWork)
	}
	t.Kind = strings.ToLower(strings.TrimSpace(t.Kind))
	if t.Kind == "" {
		t.Kind = TicketKindFeature
	}
	if !ValidTicketKind(t.Kind) {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidWork, t.Kind)
	}
	t.Priority = strings.ToUpper(strings.TrimSpace(t.Priority))
	if t.Priority == "" {
		t.Priority = "P2"
	}
	if !ValidTicketPriority(t.Priority) {
		return fmt.Errorf("%w: unknown priority %q", ErrInvalidWork, t.Priority)
	}
	t.Status = strings.ToLower(strings.TrimSpace(t.Status))
	if t.Status == "" {
		t.Status = TicketStatusBacklog
	}
	if !ValidTicketStatus(t.Status) {
		return fmt.Errorf("%w: unknown status %q", ErrInvalidWork, t.Status)
	}
	t.OrphanPolicy = strings.ToLower(strings.TrimSpace(t.OrphanPolicy))
	if t.OrphanPolicy == "" {
		t.OrphanPolicy = TicketOrphanPolicyRequeue
	} else if t.OrphanPolicy == "failed" {
		t.OrphanPolicy = TicketOrphanPolicyFail
	}
	if !ValidTicketOrphanPolicy(t.OrphanPolicy) {
		return fmt.Errorf("%w: unknown orphan_policy %q", ErrInvalidWork, t.OrphanPolicy)
	}
	if t.Acceptance == nil {
		t.Acceptance = []AcceptanceItem{}
	}
	if t.Labels == nil {
		t.Labels = []string{}
	}
	return nil
}

// TicketFilter filters ListTickets queries. Zero fields mean no filter.
type TicketFilter struct {
	ProjectID string
	ParentID  string
	// RootOnly, when true and ParentID is empty, returns only top-level tickets (parent_id IS NULL).
	RootOnly bool
	Status   string
	Kind     string
	Limit    int
}

// TicketEdge represents a blocked_by DAG edge: TicketID is blocked by BlockedByID.
type TicketEdge struct {
	TicketID    string    `json:"ticket_id"`
	BlockedByID string    `json:"blocked_by_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// TicketRepoBinding binds a ticket to one of its project's registered repos.
type TicketRepoBinding struct {
	TicketID     string `json:"ticket_id"`
	RepoID       string `json:"repo_id"`
	BaseBranch   string `json:"base_branch,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`
}

// TaskWorktree records one provisioned git worktree reported back on a task.
type TaskWorktree struct {
	RepoName     string `json:"repo_name"`
	CheckoutPath string `json:"checkout_path"`
	WorktreePath string `json:"worktree_path"`
	Branch       string `json:"branch,omitempty"`
}

// ExternalRef links a Syre ticket to an external tracker item (ka, GitHub, Jira, etc.).
type ExternalRef struct {
	TicketID   string `json:"ticket_id"`
	Tracker    string `json:"tracker"`
	ExternalID string `json:"external_id"`
	URL        string `json:"url,omitempty"`
	Version    string `json:"version,omitempty"`
}

// TicketRollup is the computed progress summary for an initiative or epic (never stored).
type TicketRollup struct {
	TicketID string `json:"ticket_id"`
	Total    int    `json:"total"`
	Done     int    `json:"done"`
	Running  int    `json:"running"`
	Blocked  int    `json:"blocked"`
}

// TicketDigestRaw is the latest non-empty task digest JSON for one ticket.
// The API turns it into a one-line summary for board cards.
type TicketDigestRaw struct {
	TicketID   string
	DigestJSON string
}

// TicketDigestLine is the board's one-line view of a ticket's latest run digest.
type TicketDigestLine struct {
	TicketID string `json:"ticket_id"`
	Summary  string `json:"summary,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Machine  string `json:"machine,omitempty"`
	Attempt  int    `json:"attempt,omitzero"`
}

// MaxTicketCommentChars is the maximum length (in runes) of a ticket comment body.
const MaxTicketCommentChars = 500

// TicketComment is a short progress note or comment posted on a ticket.
type TicketComment struct {
	ID        string    `json:"id"`
	TicketID  string    `json:"ticket_id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskFollowup is a proposed follow-up ticket recorded by an agent session via ticket_followup.
type TaskFollowup struct {
	Title     string    `json:"title"`
	Why       string    `json:"why,omitempty"`
	Scope     string    `json:"scope,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
}
