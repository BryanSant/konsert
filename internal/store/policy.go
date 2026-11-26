package store

import "time"

// Policy proposal statuses. A pending proposal is a Needs-you card. Approval
// creates the directive; denial drops it.
const (
	PolicyProposalPending  = "pending"
	PolicyProposalApproved = "approved"
	PolicyProposalDenied   = "denied"
)

// PolicyProposal is one Commander set_policy change held for the user when
// commander_policy_writes is confirm.
type PolicyProposal struct {
	ID        string          `json:"id"`
	SID       string          `json:"sid"`
	ChatID    string          `json:"chat_id,omitempty"`
	Kind      string          `json:"kind"`
	Target    string          `json:"target"`
	Params    DirectiveParams `json:"params"`
	Until     *time.Time      `json:"until,omitzero"`
	Reason    string          `json:"reason,omitempty"`
	Summary   string          `json:"summary"`
	Status    string          `json:"status"`
	ActorID   string          `json:"actor_id,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	DecidedAt *time.Time      `json:"decided_at,omitzero"`
	DecidedBy string          `json:"decided_by,omitempty"`
	AppliedID string          `json:"applied_id,omitempty"`
}

// PolicyProposalFilter selects proposals, newest first.
type PolicyProposalFilter struct {
	Status string
	Limit  int
}
