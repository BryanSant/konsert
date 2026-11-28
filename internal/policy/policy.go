// Package policy checks new work against a machine's policy (protocol.MachinePolicy).
// The hub runs Check before it admits work to a machine and the daemon runs it again
// before it starts the work, with the same rules, so a hub that is wrong (stale
// metrics, an old version) still cannot place work a machine's owner ruled out.
package policy

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// State is what Check needs to know about the machine at admission. Unknown values
// are negative (load, RAM) or zero (CPUs): rules that need them don't apply.
type State struct {
	Work string // protocol.Work*
	// Hosted is the managed sessions already running, the new one not included.
	Hosted int
	// LoadAvg is the 1-minute load average; CPUs the core count.
	LoadAvg float64
	CPUs    int
	// AvailRAMMB is the memory available now; SurvivalReserveMB the guardrails'.
	AvailRAMMB        int
	SurvivalReserveMB int
	Now               time.Time
	// Lifted are rules an approved break-glass override suspends for now.
	Lifted map[string]bool
	// Automatic marks work no person started now (the scheduler, the Commander, an
	// agent); UserIdleS is how long since someone used the machine's keyboard or mouse
	// (< 0 unknown). An interactive machine holds automatic work while it is in use.
	Automatic bool
	UserIdleS int64
}

// InteractiveAwayS is how long the user of an interactive machine must be away before
// it takes automatic work again.
const InteractiveAwayS = 5 * 60

// Rules named in a Violation.
const (
	RuleRole        = "role"
	RuleAllowedWork = "allowed_work"
	RuleQuietHours  = "quiet_hours"
	RuleMaxSessions = "max_sessions"
	RuleMaxLoad     = "max_load"
	RuleReserveCPU  = "reserve_cpu"
	RuleFreeRAM     = "min_free_ram"
	RuleUserActive  = "user_active"
)

// Violation is a policy rule new work would break.
type Violation struct {
	Rule    string
	Message string
}

func (v *Violation) Error() string { return v.Message }

// Check returns the first rule the work would break, or nil.
func Check(p *protocol.MachinePolicy, st State) *Violation {
	if p == nil {
		return nil
	}
	// A rule an override lifted reports nothing: the next rule decides.
	v := func(rule, format string, args ...any) *Violation {
		if st.Lifted[rule] {
			return nil
		}
		return &Violation{Rule: rule, Message: "machine policy: " + fmt.Sprintf(format, args...)}
	}
	return check(p, st, v)
}

// Rules lists every rule name Check can report (what an override may lift).
var Rules = []string{RuleRole, RuleAllowedWork, RuleQuietHours, RuleMaxSessions, RuleMaxLoad, RuleReserveCPU, RuleFreeRAM, RuleUserActive}

func check(p *protocol.MachinePolicy, st State, v func(rule, format string, args ...any) *Violation) *Violation {
	if p.Role == protocol.MachineRoleControlOnly {
		if vi := v(RuleRole, "this machine is control-only and runs no work"); vi != nil {
			return vi
		}
	}
	if p.Role == protocol.MachineRoleInteractive && st.Automatic && st.UserIdleS >= 0 && st.UserIdleS < InteractiveAwayS {
		if vi := v(RuleUserActive, "someone is using this machine (last input %ds ago): automatic work waits until they have been away %d minutes",
			st.UserIdleS, InteractiveAwayS/60); vi != nil {
			return vi
		}
	}
	if len(p.AllowedWork) > 0 && st.Work != "" && !slices.Contains(p.AllowedWork, st.Work) {
		if vi := v(RuleAllowedWork, "this machine runs only %s work, not %s", strings.Join(p.AllowedWork, ", "), st.Work); vi != nil {
			return vi
		}
	}
	if w, ok := InQuietHours(p, st.Now); ok {
		if vi := v(RuleQuietHours, "quiet hours (%s to %s %s): no new work", w.Start, w.End, p.TimeZone); vi != nil {
			return vi
		}
	}
	if p.MaxSessions > 0 && st.Hosted >= p.MaxSessions {
		if vi := v(RuleMaxSessions, "at most %d sessions at once (%d running)", p.MaxSessions, st.Hosted); vi != nil {
			return vi
		}
	}
	if p.MaxLoad > 0 && st.LoadAvg > p.MaxLoad {
		if vi := v(RuleMaxLoad, "load average %.1f is above %.1f", st.LoadAvg, p.MaxLoad); vi != nil {
			return vi
		}
	}
	if p.ReserveCPU > 0 && st.CPUs > 0 && st.LoadAvg >= 0 {
		if limit := float64(st.CPUs) - p.ReserveCPU; st.LoadAvg > limit {
			if vi := v(RuleReserveCPU, "load average %.1f leaves less than %g of %d cores for the user", st.LoadAvg, p.ReserveCPU, st.CPUs); vi != nil {
				return vi
			}
		}
	}
	if keep := p.MinFreeRAMMB + p.ReserveRAMMB; keep > 0 && st.AvailRAMMB >= 0 {
		if floor := st.SurvivalReserveMB + keep; st.AvailRAMMB < floor {
			if vi := v(RuleFreeRAM, "%d MB available, below the %d MB kept free (%d survival reserve + %d policy)",
				st.AvailRAMMB, floor, st.SurvivalReserveMB, keep); vi != nil {
				return vi
			}
		}
	}
	return nil
}

// ExtraReserveMB is the memory the policy keeps free on top of the survival reserve:
// the guardrails' watchdog trips at survival reserve + this.
func ExtraReserveMB(p *protocol.MachinePolicy) int {
	if p == nil {
		return 0
	}
	return max(0, p.MinFreeRAMMB) + max(0, p.ReserveRAMMB)
}

var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// InQuietHours reports the quiet window now falls in, if any.
func InQuietHours(p *protocol.MachinePolicy, now time.Time) (protocol.QuietWindow, bool) {
	if p == nil || len(p.QuietHours) == 0 || now.IsZero() {
		return protocol.QuietWindow{}, false
	}
	loc, err := time.LoadLocation(p.TimeZone)
	if err != nil || p.TimeZone == "" {
		return protocol.QuietWindow{}, false // Validate refuses this; never guess a zone
	}
	t := now.In(loc)
	mins := t.Hour()*60 + t.Minute()
	for _, w := range p.QuietHours {
		start, err1 := clock(w.Start)
		end, err2 := clock(w.End)
		if err1 != nil || err2 != nil || start == end {
			continue
		}
		// The day the window that covers now started on.
		startDay := t.Weekday()
		var in bool
		if start < end {
			in = mins >= start && mins < end
		} else { // past midnight
			switch {
			case mins >= start:
				in = true
			case mins < end:
				in = true
				startDay = (startDay + 6) % 7
			}
		}
		if in && (len(w.Days) == 0 || slices.Contains(w.Days, dayNames[startDay])) {
			return w, true
		}
	}
	return protocol.QuietWindow{}, false
}

func clock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not a time of day (HH:MM)", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Validate reports what is wrong with a policy a user is about to save.
func Validate(p *protocol.MachinePolicy) error {
	if p == nil {
		return nil
	}
	switch p.Role {
	case "", protocol.MachineRoleDedicated, protocol.MachineRoleInteractive, protocol.MachineRoleControlOnly:
	default:
		return fmt.Errorf("role %q: use dedicated, interactive or control_only", p.Role)
	}
	if p.MaxSessions < 0 || p.MaxLoad < 0 || p.ReserveCPU < 0 || p.MinFreeRAMMB < 0 || p.ReserveRAMMB < 0 {
		return fmt.Errorf("limits cannot be negative")
	}
	for _, w := range p.AllowedWork {
		if w != protocol.WorkChat && w != protocol.WorkTask && w != protocol.WorkTicket {
			return fmt.Errorf("work class %q: use chat, task or ticket", w)
		}
	}
	if len(p.QuietHours) > 0 {
		if p.TimeZone == "" {
			return fmt.Errorf("quiet hours need a time zone (e.g. America/Denver)")
		}
		if _, err := time.LoadLocation(p.TimeZone); err != nil {
			return fmt.Errorf("time zone %q: %v", p.TimeZone, err)
		}
	}
	for _, w := range p.QuietHours {
		start, err := clock(w.Start)
		if err != nil {
			return err
		}
		end, err := clock(w.End)
		if err != nil {
			return err
		}
		if start == end {
			return fmt.Errorf("quiet window %s-%s is empty", w.Start, w.End)
		}
		for _, d := range w.Days {
			if !slices.Contains(dayNames, d) {
				return fmt.Errorf("day %q: use mon, tue, wed, thu, fri, sat or sun", d)
			}
		}
	}
	return nil
}
