package worker

import (
	"context"
	"log/slog"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/guard"
)

// MachineGuardConfig resolves the guardrails configuration for this host from a fast
// platform probe (RAM, virtualization) and the operator's mode (config/--guardrails).
func MachineGuardConfig(ctx context.Context, mode string, logger *slog.Logger) (guard.Config, error) {
	m, err := guard.ParseMode(mode)
	if err != nil {
		return guard.Config{}, err
	}
	var f protocol.MachineFacts
	collectPlatformFacts(ctx, &f)
	virt := classifyVirtualization(&f)
	return guard.Config{
		Baseline: guard.NewBaseline(f.RAMTotalMB<<20, guard.DetectDesktop(), virt),
		Mode:     m,
		Logger:   logger,
	}, nil
}

// SetGuard installs the machine's guardrails: managed sessions started from now on are
// contained and watched, hub directives steer the mode, facts report it.
func (d *Daemon) SetGuard(g *guard.Guard) {
	d.guard = g
	guard.SetDefault(g)
	d.applyGuardDirectives(d.directiveSnapshot())
}

// applyGuardDirectives maps active directives onto the guard: the newest
// machine.guardrails sets the mode; any machine.throttle lowers worker priority and
// the tightest cpu_ceiling_pct becomes a hard CPU ceiling.
func (d *Daemon) applyGuardDirectives(list []protocol.WorkerDirective) {
	if d.guard == nil {
		return
	}
	mode := guard.Mode("")
	throttled, cpu := false, 0
	for _, dir := range list { // newest first (the hub lists them that way)
		switch dir.Kind {
		case protocol.DirectiveKindMachineGuardrails:
			if mode == "" {
				if m, err := guard.ParseMode(dir.Params.Guardrails); err == nil && m != guard.ModeAuto {
					mode = m
				}
			}
		case protocol.DirectiveKindMachineThrottle:
			throttled = true
			if dir.Params.CPUCeilingPct != nil && *dir.Params.CPUCeilingPct > 0 && (cpu == 0 || *dir.Params.CPUCeilingPct < cpu) {
				cpu = *dir.Params.CPUCeilingPct
			}
		}
	}
	d.guard.SetHubMode(mode)
	d.guard.SetThrottle(throttled, cpu)
}

// annotateGuardFacts refreshes the guard's baseline from fresh facts and reports the
// guardrails in them.
func (d *Daemon) annotateGuardFacts(f *protocol.MachineFacts) {
	g := d.guard
	if g == nil {
		return
	}
	desktop := guard.DetectDesktop()
	if f.RAMTotalMB > 0 {
		g.SetBaseline(guard.NewBaseline(f.RAMTotalMB<<20, desktop, f.Virtualization))
	}
	st := g.Status()
	f.IsDesktop = st.Desktop
	f.SurvivalReserveMB = st.Reserve >> 20
	f.GuardrailMode = string(st.Mode)
	f.GuardrailModeSource = st.ModeSource
	f.GuardrailBaseMode = string(st.BaseMode)
	f.GuardrailMechanism = st.Mechanism
}
