package store

import "time"

// Policy directive kinds (docs/design/autopilot.md). provider.disable is reserved
// for the next ticket: the column accepts it, the API does not create it.
const (
	DirectiveMachinePause    = "machine.pause"
	DirectiveMachineThrottle = "machine.throttle"
	DirectiveMachineDrain    = "machine.drain"
	DirectiveAutopilotPause  = "autopilot.pause"
	DirectiveProviderDisable = "provider.disable"
)

// Who set a directive.
const (
	ProvenanceUser      = "user"
	ProvenanceCommander = "commander"
	ProvenanceSystem    = "system"
)

// DirectiveParams is the kind-specific payload. Throttle uses MaxConcurrency (0 drops
// the machine), CPUCeilingPct and HeavyOK. Pause uses SuspendRunning. Pointers keep an
// omitted cap distinct from zero. CPU ceiling and heavy_ok are stored and forwarded;
// enforcement of those two waits on an OS envelope that does not exist yet.
type DirectiveParams struct {
	MaxConcurrency *int  `json:"max_concurrency,omitempty"`
	CPUCeilingPct  *int  `json:"cpu_ceiling_pct,omitempty"`
	HeavyOK        *bool `json:"heavy_ok,omitempty"`
	SuspendRunning bool  `json:"suspend_running,omitzero"`
}

// Directive is one typed, timed, attributed policy entry.
type Directive struct {
	ID         string          `json:"id"`
	SID        string          `json:"sid"`
	Kind       string          `json:"kind"`
	Target     string          `json:"target"`
	Params     DirectiveParams `json:"params"`
	Until      *time.Time      `json:"until,omitzero"`
	Provenance string          `json:"provenance"`
	Actor      string          `json:"actor,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	EndedAt    *time.Time      `json:"ended_at,omitzero"`
	EndedBy    string          `json:"ended_by,omitempty"`
}

// ActiveAt reports whether the directive still applies at t: not ended, and until
// is unset or still ahead of t. A row whose until has passed is inactive even
// before the reaper stamps ended_at.
func (d Directive) ActiveAt(t time.Time) bool {
	if d.EndedAt != nil {
		return false
	}
	if d.Until != nil && !t.Before(*d.Until) {
		return false
	}
	return true
}

// DirectiveFilter selects directives, newest first. Zero values mean no filter.
// ActiveOnly keeps rows that ActiveAt(Now) would accept (Now zero means the wall clock).
// OpenOnly keeps every row with ended_at unset, including ones whose until has passed.
type DirectiveFilter struct {
	Target     string
	Kind       string
	ActiveOnly bool
	OpenOnly   bool
	Now        time.Time
	Limit      int
}
