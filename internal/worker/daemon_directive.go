package worker

import (
	"fmt"
	"runtime"
	"time"

	"github.com/BryanSant/syre/internal/policy"
	"github.com/BryanSant/syre/internal/protocol"
)

// setDirectives replaces this machine's active policy. A until that has already
// passed is dropped so a late message cannot keep a dead cap in force.
func (d *Daemon) setDirectives(list []protocol.WorkerDirective) {
	now := time.Now()
	active := make([]protocol.WorkerDirective, 0, len(list))
	for _, dir := range list {
		if dir.Until != nil && !now.Before(*dir.Until) {
			continue
		}
		active = append(active, dir)
	}
	d.dirMu.Lock()
	d.directives = active
	d.dirMu.Unlock()
	d.applyGuardDirectives(active)
}

func (d *Daemon) directiveSnapshot() []protocol.WorkerDirective {
	d.dirMu.Lock()
	defer d.dirMu.Unlock()
	out := make([]protocol.WorkerDirective, len(d.directives))
	copy(out, d.directives)
	return out
}

// directiveRefusal is a clear error when a new task or chat would violate an
// active pause, drain or throttle. An empty string means the start may proceed.
// The tightest throttle cap wins; max 0 refuses even with nothing hosted.
func (d *Daemon) directiveRefusal() string {
	now := time.Now()
	var pause, drain bool
	var max *int
	for _, dir := range d.directiveSnapshot() {
		if dir.Until != nil && !now.Before(*dir.Until) {
			continue
		}
		switch dir.Kind {
		case protocol.DirectiveKindMachinePause:
			pause = true
		case protocol.DirectiveKindMachineDrain:
			drain = true
		case protocol.DirectiveKindMachineThrottle:
			if dir.Params.MaxConcurrency == nil {
				continue
			}
			v := *dir.Params.MaxConcurrency
			if max == nil || v < *max {
				max = &v
			}
		}
	}
	if pause {
		return "paused: this machine is paused by a policy directive and accepts no new work"
	}
	if drain {
		return "draining: this machine is draining and accepts no new work"
	}
	if max != nil && d.hostedSessions() >= *max {
		return fmt.Sprintf("throttled: this machine is throttled by a policy directive (max concurrency %d)", *max)
	}
	return ""
}

// hostedSessions counts work this daemon is already hosting: tasks reserved or
// running, plus chats with an open engine or an active turn.
func (d *Daemon) hostedSessions() int {
	n := 0
	if d.runner != nil {
		n += d.runner.hostedTaskCount()
	}
	if d.chat != nil {
		for _, c := range d.chat.Chats() {
			if c.Open || c.TurnActive {
				n++
			}
		}
	}
	return n
}

// setPolicy installs the machine policy the hub pushed (nil clears it) and hands its
// memory reservation to the guardrails, which then trip above the survival reserve.
func (d *Daemon) setPolicy(p *protocol.MachinePolicy) {
	d.dirMu.Lock()
	d.policy = p
	d.dirMu.Unlock()
	if d.guard != nil {
		d.guard.SetUserReserve(uint64(policy.ExtraReserveMB(p)) << 20)
	}
}

// setPolicyLifts installs the policy rules approved overrides suspend.
func (d *Daemon) setPolicyLifts(lifts []protocol.PolicyLift) {
	d.dirMu.Lock()
	d.policyLifts = lifts
	d.dirMu.Unlock()
}

// liftedRules are the rules suspended at now (an expired lift is ignored even before
// the hub's update arrives).
func (d *Daemon) liftedRules(now time.Time) map[string]bool {
	d.dirMu.Lock()
	defer d.dirMu.Unlock()
	var out map[string]bool
	for _, l := range d.policyLifts {
		if !now.Before(l.Until) {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		for _, r := range l.Rules {
			out[r] = true
		}
	}
	return out
}

func (d *Daemon) policySnapshot() *protocol.MachinePolicy {
	d.dirMu.Lock()
	defer d.dirMu.Unlock()
	return d.policy
}

// admissionRefusal is why new work of the given class may not start here ("" when it
// may): an active directive, or the machine policy. chatID names a chat being opened;
// one already open here is a running session, not new work, and only directives apply.
func (d *Daemon) admissionRefusal(work, chatID string) string {
	if msg := d.directiveRefusal(); msg != "" {
		return msg
	}
	p := d.policySnapshot()
	if p == nil || (chatID != "" && d.chatOpen(chatID)) {
		return ""
	}
	if v := policy.Check(p, d.policyState(work)); v != nil {
		return v.Message
	}
	return ""
}

// policyState is this machine's state for a policy check: hosted sessions, then load
// and memory from the last heartbeat's vitals (unknown before the first).
func (d *Daemon) policyState(work string) policy.State {
	st := policy.State{Work: work, Hosted: d.hostedSessions(), LoadAvg: -1, AvailRAMMB: -1, Now: time.Now()}
	if m := d.lastMetrics.Load(); m != nil {
		st.LoadAvg = m.LoadAvg
		if m.MemTotalMB > 0 {
			st.AvailRAMMB = int(m.MemTotalMB) - int(m.MemUsedMB)
		}
	}
	if d.guard != nil {
		gs := d.guard.Status()
		st.SurvivalReserveMB = int(gs.Reserve >> 20)
	}
	st.CPUs = runtime.NumCPU()
	st.Lifted = d.liftedRules(st.Now)
	return st
}

// chatOpen reports whether chatID already has an open engine here.
func (d *Daemon) chatOpen(chatID string) bool {
	if d.chat == nil {
		return false
	}
	for _, c := range d.chat.Chats() {
		if c.ChatID == chatID && (c.Open || c.TurnActive) {
			return true
		}
	}
	return false
}
