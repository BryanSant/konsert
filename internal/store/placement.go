package store

import "time"

// Placement rule scopes, effects, targets and provenances (SYR-880CP).
const (
	RuleScopeAny     = "any"
	RuleScopeProject = "project"
	RuleScopeRepo    = "repo"
	RuleScopeBranch  = "branch"
	RuleScopeWorkKey = "work_key"

	RuleEffectPrefer  = "prefer"
	RuleEffectRequire = "require"
	RuleEffectAvoid   = "avoid"

	RuleTargetMachine = "machine"
	RuleTargetClass   = "class"

	RuleProvenanceUser      = "user"
	RuleProvenanceCommander = "commander"
	RuleProvenanceLearned   = "learned"
)

// PlacementRule prefers, requires or avoids a machine or machine class for a scope.
type PlacementRule struct {
	ID         string     `json:"id"`
	ScopeKind  string     `json:"scope_kind"`
	ScopeValue string     `json:"scope_value,omitempty"`
	Effect     string     `json:"effect"`
	TargetKind string     `json:"target_kind"`
	Target     string     `json:"target"`
	Provenance string     `json:"provenance"`
	Evidence   string     `json:"evidence,omitempty"`
	CreatedBy  string     `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitzero"`
	EndedAt    *time.Time `json:"ended_at,omitzero"`
}

// Active reports whether the rule applies at now.
func (r PlacementRule) Active(now time.Time) bool {
	return r.EndedAt == nil && (r.ExpiresAt == nil || now.Before(*r.ExpiresAt))
}

// PlacementLogEntry is one placement decision.
type PlacementLogEntry struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	Subject  string    `json:"subject"`
	WorkKey  string    `json:"work_key,omitempty"`
	Decider  string    `json:"decider"`
	WorkerID string    `json:"worker_id,omitempty"`
	// DetailJSON holds the candidates, rejections and rules (controller.PlacementDetail).
	DetailJSON string `json:"detail_json"`
}

// PlacementLogFilter selects log entries (newest first).
type PlacementLogFilter struct {
	Subject string
	Since   time.Time
	Limit   int
}
