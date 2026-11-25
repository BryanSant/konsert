package store

import "time"

// Human control and attribution (PRD §3 "The user is always in charge", §5.8a).

// Actor kinds: who caused a state change.
const (
	ActorUser      = "user"
	ActorCommander = "commander"
	ActorAgent     = "agent"
	ActorScheduler = "scheduler"
	ActorSystem    = "system"
	ActorUnknown   = "unknown"
)

// Cause sources (Cause.Source) for user actions; other actors use free-form sources
// such as "mcp", "worker", "reaper".
const (
	SourceUI       = "ui"
	SourceCLI      = "cli"
	SourceTerminal = "terminal"
	SourceSSH      = "ssh"
)

// Confidence of an attribution.
const (
	ConfidenceCertain = "certain"
	ConfidenceHigh    = "high"
	ConfidenceLikely  = "likely"
	ConfidenceUnknown = "unknown"
)

// Cause attributes a state change: who did it, through which path, how sure Syre is,
// and what it correlates with (an open request, a terminal command, a hold).
type Cause struct {
	Actor string `json:"actor"`
	// Source is the path the action took: ui, cli, terminal, ssh for users; mcp, worker,
	// reaper, ... for others.
	Source string `json:"source,omitempty"`
	// ActorID identifies the actor: the authenticated user, the chat id of an agent or
	// commander chat, the task id of an agent task.
	ActorID    string `json:"actor_id,omitempty"`
	Confidence string `json:"confidence"`
	RequestID  string `json:"request_id,omitempty"`
	CommandID  string `json:"command_id,omitempty"`
	HoldID     string `json:"hold_id,omitempty"`
}

// IsZero reports whether no actor is set.
func (c Cause) IsZero() bool { return c.Actor == "" }

// Fleet event kinds (FleetEvent.Kind).
const (
	EventKindStatus   = "status"   // a task or chat status transition (Action: "task" | "chat")
	EventKindAction   = "action"   // an intervention: cancel, suspend, resume, drain, pause, command, ...
	EventKindHold     = "hold"     // Action: created | released
	EventKindRequest  = "request"  // Action: created | done | declined | canceled | linked
	EventKindVeto     = "veto"     // Action: the veto reason
	EventKindNotice   = "notice"   // Syre told an agent about something (Action: delivered)
	EventKindQuestion = "question" // Action: answered | delegated | decided | overridden
	EventKindPulse    = "pulse"    // Actionable autopilot/board pulse trigger (docs/design/autopilot.md)
)

// FleetEvent is one attributed event in the fleet timeline: every status transition and
// every intervention, hold and request. It feeds the "Your actions" timeline, the
// recent_events MCP tool and the notes delivered to affected agents.
type FleetEvent struct {
	ID         int64     `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	Kind       string    `json:"kind"`
	Action     string    `json:"action,omitempty"`
	Cause      Cause     `json:"cause"`
	WorkerID   string    `json:"worker_id,omitempty"`
	TaskID     string    `json:"task_id,omitempty"`
	ChatID     string    `json:"chat_id,omitempty"`
	FromStatus string    `json:"from_status,omitempty"`
	ToStatus   string    `json:"to_status,omitempty"`
	// Summary is one human sentence ("The user stopped nginx on host-1 via a Syre terminal").
	Summary  string `json:"summary"`
	MetaJSON string `json:"meta_json,omitempty"`
}

// FleetEventFilter selects fleet events, newest first. Zero values mean no filter.
type FleetEventFilter struct {
	WorkerID string
	TaskID   string
	ChatID   string
	Kind     string
	Actor    string
	// Notable drops routine status transitions not caused by a user (agent turns
	// starting and ending, scheduler placement) and keeps everything else.
	Notable  bool
	Since    time.Time
	AfterID  int64
	BeforeID int64
	Limit    int
}

// Hold scopes.
const (
	HoldScopeSession  = "session"
	HoldScopeMachine  = "machine"
	HoldScopeResource = "resource"
)

// Resource names something on a machine a hold or request is about.
type Resource struct {
	Kind string `json:"kind"`           // service | process | container | port | path
	Name string `json:"name,omitempty"` // nginx, postgres, my-container
	Port int    `json:"port,omitzero"`
}

// Hold blocks automatic actions on a scope after an intervention no agent asked for:
// no retries, re-dispatch, automatic restarts or commander steering there until the
// user releases it. Holds never suspend anything by themselves.
type Hold struct {
	ID        string    `json:"id"`
	SID       string    `json:"sid"`
	ScopeKind string    `json:"scope_kind"`
	WorkerID  string    `json:"worker_id,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
	ChatID    string    `json:"chat_id,omitempty"`
	Resource  *Resource `json:"resource,omitzero"`
	Summary   string    `json:"summary"`
	Cause     Cause     `json:"cause"`
	CreatedAt time.Time `json:"created_at"`
	// ReleasedAt is nil while the hold is active.
	ReleasedAt  *time.Time `json:"released_at,omitzero"`
	ReleasedBy  string     `json:"released_by,omitempty"`
	ReleaseNote string     `json:"release_note,omitempty"`
}

// Active reports whether the hold has not been released.
func (h Hold) Active() bool { return h.ReleasedAt == nil }

// HoldFilter selects holds, newest first.
type HoldFilter struct {
	ActiveOnly bool
	WorkerID   string
	TaskID     string
	ChatID     string
	Limit      int
}

// User request statuses.
const (
	RequestOpen     = "open"
	RequestDone     = "done"
	RequestDeclined = "declined"
	RequestCanceled = "canceled"
)

// UserRequest is something an agent asked the user to do on a machine ("restart nginx; I
// don't have sudo"). A matching user action resolves it instead of placing a hold.
type UserRequest struct {
	ID          string     `json:"id"`
	SID         string     `json:"sid"`
	ChatID      string     `json:"chat_id,omitempty"`
	TaskID      string     `json:"task_id,omitempty"`
	WorkerID    string     `json:"worker_id"`
	Resource    *Resource  `json:"resource,omitzero"`
	Description string     `json:"description"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitzero"`
	ResolvedBy  string     `json:"resolved_by,omitempty"`
	Resolution  string     `json:"resolution,omitempty"`
	CommandID   string     `json:"command_id,omitempty"`
}

// UserRequestFilter selects requests, newest first.
type UserRequestFilter struct {
	Status   string
	WorkerID string
	ChatID   string
	Limit    int
}

// Suspension owners (Task.SuspendedBy, ChatSession.SuspendedBy): Resume after Pause
// everything restores only what the pause suspended.
const (
	SuspendedByUser  = "user"
	SuspendedByPause = "pause"
)

// FleetPause is the persisted fleet-wide Pause everything state.
type FleetPause struct {
	Paused   bool       `json:"paused"`
	PausedAt *time.Time `json:"paused_at,omitzero"`
	PausedBy string     `json:"paused_by,omitempty"`
}
