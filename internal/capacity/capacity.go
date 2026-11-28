// Package capacity estimates how much more work a machine can take (SYR-0AANJ;
// docs/prd.md §5.5). Headroom replaces slot counting as the scheduler's measure:
//
//	RAM headroom = available physical RAM − survival reserve − policy reservation
//	               − Σ expected remaining growth of the running managed sessions
//
// A running session's expected peak comes from its work profile (unknown work counts
// as a small class); what it holds now is already outside "available", so only the
// growth still to come counts. Other load, the user's and observed sessions', is in
// the available figure. Expected peaks are estimates for placement, never limits.
//
// CPU headroom is the cores left after the load and the policy's reserved cores. A
// machine cooling down after a memory-pressure shed is down-weighted until it ends.
package capacity

import (
	"math"
	"time"
)

// SmallClassPeakMB is the peak assumed for work with no profile yet: a typical agent
// session with a modest build.
const SmallClassPeakMB = 1024

// ShedWeight is the weight of a machine cooling down after a shed.
const ShedWeight = 0.25

// Session is a running managed session on the machine.
type Session struct {
	Subject string `json:"subject"` // "task:<id>" or "chat:<id>"
	WorkKey string `json:"work_key,omitempty"`
	// ExpectedPeakMB is the profile's estimate (0: no profile, the small class counts).
	ExpectedPeakMB uint64 `json:"expected_peak_mb,omitzero"`
	// RSSMB is what the session's processes hold now.
	RSSMB uint64 `json:"rss_mb"`
}

// Inputs is what is known about one machine.
type Inputs struct {
	// TotalRAMMB and AvailRAMMB are from the last heartbeat (Avail < 0: unknown).
	TotalRAMMB, AvailRAMMB int
	SurvivalReserveMB      int
	// PolicyReserveMB is what the machine policy keeps free on top (min free + the
	// user's reservation).
	PolicyReserveMB int
	// CPUs is the core count; LoadAvg the 1-minute load (< 0 unknown, 0 on Windows,
	// where CPUPercent stands in); ReserveCPU the policy's cores kept for the user.
	CPUs       int
	LoadAvg    float64
	CPUPercent float64
	ReserveCPU float64
	Sessions   []Session
	// ShedCooldownUntil is when the machine's shed cooldown ends (zero: none).
	ShedCooldownUntil time.Time
	Now               time.Time
}

// Headroom is a machine's estimated room for new work.
type Headroom struct {
	// RAMMB is the memory a new session can still grow into (negative: over).
	RAMMB int `json:"ram_mb"`
	// CPU is the free cores (negative: oversubscribed); nil when the load is unknown.
	CPU *float64 `json:"cpu,omitzero"`
	// The terms of RAMMB.
	AvailRAMMB        int `json:"avail_ram_mb"`
	SurvivalReserveMB int `json:"survival_reserve_mb"`
	PolicyReserveMB   int `json:"policy_reserve_mb,omitzero"`
	ExpectedGrowthMB  int `json:"expected_growth_mb,omitzero"`
	// Sessions are the running managed sessions with their estimates.
	Sessions []Session `json:"sessions,omitempty"`
	// Weight scales the machine in placement: 1, or ShedWeight while cooling down.
	Weight            float64    `json:"weight"`
	ShedCooldownUntil *time.Time `json:"shed_cooldown_until,omitzero"`
}

// Compute estimates the headroom; ok is false when the memory figures are unknown.
func Compute(in Inputs) (Headroom, bool) {
	if in.AvailRAMMB < 0 || in.TotalRAMMB <= 0 {
		return Headroom{}, false
	}
	h := Headroom{AvailRAMMB: in.AvailRAMMB, SurvivalReserveMB: in.SurvivalReserveMB,
		PolicyReserveMB: in.PolicyReserveMB, Sessions: in.Sessions, Weight: 1}
	for _, s := range in.Sessions {
		peak := s.ExpectedPeakMB
		if peak == 0 {
			peak = SmallClassPeakMB
		}
		if peak > s.RSSMB {
			h.ExpectedGrowthMB += int(peak - s.RSSMB)
		}
	}
	h.RAMMB = in.AvailRAMMB - in.SurvivalReserveMB - in.PolicyReserveMB - h.ExpectedGrowthMB
	if in.CPUs > 0 {
		busy := -1.0
		switch {
		case in.LoadAvg > 0:
			busy = in.LoadAvg
		case in.LoadAvg == 0 && in.CPUPercent > 0: // Windows: no load average
			busy = in.CPUPercent / 100 * float64(in.CPUs)
		case in.LoadAvg == 0:
			busy = 0
		}
		if busy >= 0 {
			free := math.Round((float64(in.CPUs)-busy-max(0, in.ReserveCPU))*10) / 10
			h.CPU = &free
		}
	}
	if !in.ShedCooldownUntil.IsZero() && in.Now.Before(in.ShedCooldownUntil) {
		h.Weight = ShedWeight
		until := in.ShedCooldownUntil
		h.ShedCooldownUntil = &until
	}
	return h, true
}

// Fits reports whether work expected to peak at peakMB (0: unknown, the small class)
// fits in the headroom.
func (h Headroom) Fits(peakMB uint64) bool {
	if peakMB == 0 {
		peakMB = SmallClassPeakMB
	}
	return h.RAMMB >= int(peakMB)
}
