package worker

import (
	"testing"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/guard"
)

func TestDirectivesSteerGuardrails(t *testing.T) {
	prev := guard.Default()
	t.Cleanup(func() { guard.SetDefault(prev) })

	d := &Daemon{}
	g := guard.New(guard.Config{Baseline: guard.NewBaseline(8<<30, true, "container"), Mode: guard.ModeAuto})
	d.SetGuard(g)
	if st := g.Status(); st.Mode != guard.ModeAdaptive || st.ModeSource != "auto" {
		t.Fatalf("container default = %s/%s", st.Mode, st.ModeSource)
	}
	pct := 40
	d.setDirectives([]protocol.WorkerDirective{
		{Kind: protocol.DirectiveKindMachineGuardrails, Params: protocol.WorkerDirectiveParams{Guardrails: "adaptive_strict"}}, // newest
		{Kind: protocol.DirectiveKindMachineGuardrails, Params: protocol.WorkerDirectiveParams{Guardrails: "passthrough"}},
		{Kind: protocol.DirectiveKindMachineThrottle, Params: protocol.WorkerDirectiveParams{CPUCeilingPct: &pct}},
	})
	st := g.Status()
	if st.Mode != guard.ModeAdaptiveStrict || st.ModeSource != "hub" || st.BaseMode != guard.ModeAdaptive || !st.Throttled {
		t.Fatalf("after directives: %+v", st)
	}
	d.setDirectives(nil)
	if st := g.Status(); st.Mode != guard.ModeAdaptive || st.Throttled {
		t.Fatalf("after clearing: %+v", st)
	}

	var f protocol.MachineFacts
	f.RAMTotalMB, f.Virtualization = 16384, "bare_metal"
	d.annotateGuardFacts(&f)
	if f.GuardrailMode != "adaptive_strict" || f.GuardrailBaseMode != "adaptive_strict" || f.SurvivalReserveMB == 0 || f.GuardrailMechanism == "" {
		t.Fatalf("facts = %+v", f)
	}
}
