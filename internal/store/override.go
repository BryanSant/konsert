package store

import "time"

// Override requests (break-glass) and veto acknowledgement (PRD §5.8a).

// Override scopes: what an approved override lifts for its duration.
const (
	// OverrideScopeMachineSlots lets a machine run ExtraSlots more sessions at once than
	// its slot limit (max_concurrent).
	OverrideScopeMachineSlots = "machine_slots"
	// OverrideScopeHold stops one hold (HoldID) from blocking automatic actions.
	OverrideScopeHold = "hold"
	// OverrideScopeSession stops the session holds on one task (TaskID) or chat
	// (TargetChatID) from blocking automatic actions.
	OverrideScopeSession = "session"
	// OverrideScopeMachine stops the machine and resource holds on a machine (WorkerID)
	// from blocking automatic actions.
	OverrideScopeMachine = "machine"
)

// Override statuses.
const (
	OverridePending  = "pending"
	OverrideApproved = "approved"
	OverrideDenied   = "denied"
	OverrideExpired  = "expired"
)

// Override is a time-boxed exception to a limit or hold that an orchestrator asked the
// user for ("allow a 2nd session on mac for 30m"). Once approved Konsert enforces it
// until ExpiresAt and records whether it was actually used.
type Override struct {
	ID  string `json:"id"`
	SID string `json:"sid"`
	// ChatID is the requesting orchestrator chat ("" when requested over REST).
	ChatID       string `json:"chat_id,omitempty"`
	ScopeKind    string `json:"scope_kind"`
	WorkerID     string `json:"worker_id,omitempty"`
	TaskID       string `json:"task_id,omitempty"`
	TargetChatID string `json:"target_chat_id,omitempty"`
	HoldID       string `json:"hold_id,omitempty"`
	ExtraSlots   int    `json:"extra_slots,omitzero"`
	// DurationSeconds is how long the override lasts once approved.
	DurationSeconds int    `json:"duration_seconds"`
	Reason          string `json:"reason"`
	// Consequences is Konsert's plain description of what approving allows.
	Consequences string     `json:"consequences"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	DecidedAt    *time.Time `json:"decided_at,omitzero"`
	DecidedBy    string     `json:"decided_by,omitempty"`
	DecisionNote string     `json:"decision_note,omitempty"`
	ApprovedAt   *time.Time `json:"approved_at,omitzero"`
	ExpiresAt    *time.Time `json:"expires_at,omitzero"`
	// EndedAt is when an approved override expired.
	EndedAt *time.Time `json:"ended_at,omitzero"`
	// UseCount counts the actions the override let through (measured).
	UseCount    int        `json:"use_count"`
	FirstUsedAt *time.Time `json:"first_used_at,omitzero"`
	LastUsedAt  *time.Time `json:"last_used_at,omitzero"`
}

// Used reports whether the override was exercised at least once.
func (o Override) Used() bool { return o.UseCount > 0 }

// ActiveAt reports whether the override is approved and not yet expired at t.
func (o Override) ActiveAt(t time.Time) bool {
	return o.Status == OverrideApproved && o.ExpiresAt != nil && t.Before(*o.ExpiresAt)
}

// OverrideFilter selects overrides, newest first. Zero values mean no filter.
type OverrideFilter struct {
	Status string
	ChatID string
	Limit  int
}

// Veto records a veto until the agent it was told to acknowledges it (its next turn
// after the veto was delivered ends normally), it escalates, or the user dismisses it.
type Veto struct {
	ID       string `json:"id"`
	SID      string `json:"sid"`
	Reason   string `json:"reason"`
	Note     string `json:"note,omitempty"`
	Summary  string `json:"summary"`
	WorkerID string `json:"worker_id,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
	// FleetEventID is the veto's fleet event (the one delivered to agents).
	FleetEventID int64     `json:"fleet_event_id"`
	CreatedAt    time.Time `json:"created_at"`
	// DeliveredAt/DeliveredChatID/DeliveredTurnID: the latest agent turn the veto was
	// told to (in a note prepended to its prompt).
	DeliveredAt     *time.Time `json:"delivered_at,omitzero"`
	DeliveredChatID string     `json:"delivered_chat_id,omitempty"`
	DeliveredTurnID string     `json:"delivered_turn_id,omitempty"`
	// AckedAt is when that turn ended normally: the agent acknowledged the veto.
	AckedAt     *time.Time `json:"acked_at,omitzero"`
	AckedChatID string     `json:"acked_chat_id,omitempty"`
	// EscalatedAt is when the veto went unacknowledged past the deadline.
	EscalatedAt *time.Time `json:"escalated_at,omitzero"`
	DismissedAt *time.Time `json:"dismissed_at,omitzero"`
	DismissedBy string     `json:"dismissed_by,omitempty"`
}

// Open reports whether the veto still waits for acknowledgement (or a dismissal).
func (v Veto) Open() bool { return v.AckedAt == nil && v.DismissedAt == nil }

// VetoFilter selects vetoes, newest first.
type VetoFilter struct {
	OpenOnly bool
	Since    time.Time
	Limit    int
}
