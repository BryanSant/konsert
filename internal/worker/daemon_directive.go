package worker

import (
	"fmt"
	"time"

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
