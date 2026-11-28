package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/guard"
)

// The daemon refuses work the machine policy rules out even when the hub sends it:
// the second of the two layers.
func TestDaemonRefusesWorkThatBreaksThePolicy(t *testing.T) {
	hub := newFakeHub(t)
	startTestDaemon(t, hub, nil)
	c := hub.accept(t)
	c.waitFor(t, 10*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))

	c.send(t, protocol.MsgWorkerDirectives, "", protocol.WorkerDirectivesPayload{
		Directives: []protocol.WorkerDirective{},
		Policy:     &protocol.MachinePolicy{AllowedWork: []string{protocol.WorkChat}},
	})
	c.send(t, protocol.MsgTaskStart, "", protocol.TaskStartPayload{
		TaskID: "t-pol1", Agent: "mock", Workdir: t.TempDir(), Prompt: "EXIT=0", Mode: protocol.TaskModeOneshot,
	})
	assertRefused(t, c, "t-pol1", "machine policy: this machine runs only chat work")

	c.send(t, protocol.MsgWorkerDirectives, "", protocol.WorkerDirectivesPayload{
		Directives: []protocol.WorkerDirective{},
		Policy:     &protocol.MachinePolicy{Role: protocol.MachineRoleControlOnly},
	})
	c.send(t, protocol.MsgTaskStart, "", protocol.TaskStartPayload{
		TaskID: "t-pol2", Agent: "mock", Workdir: t.TempDir(), Prompt: "EXIT=0", Mode: protocol.TaskModeOneshot,
	})
	assertRefused(t, c, "t-pol2", "control-only")

	// Cleared: work runs again.
	sendDirectives(t, c)
	c.send(t, protocol.MsgTaskStart, "", protocol.TaskStartPayload{
		TaskID: "t-pol3", Agent: "mock", Workdir: t.TempDir(), Prompt: "EXIT=0", Mode: protocol.TaskModeOneshot,
	})
	if st := waitTaskTerminal(t, c, "t-pol3"); strings.Contains(st.Summary, "policy") {
		t.Fatalf("a cleared policy still refused: %s", st.Summary)
	}
}

func TestPolicyRAMAndGuardFloor(t *testing.T) {
	prev := guard.Default()
	t.Cleanup(func() { guard.SetDefault(prev) })
	d := &Daemon{}
	g := guard.New(guard.Config{Baseline: guard.NewBaseline(16<<30, true, "bare_metal"), Mode: guard.ModeAuto})
	d.SetGuard(g)
	reserveMB := int(g.Status().Reserve >> 20)

	d.setPolicy(&protocol.MachinePolicy{ReserveRAMMB: 4096, MinFreeRAMMB: 512})
	if st := g.Status(); st.UserReserve != 4608<<20 {
		t.Fatalf("guard user reserve = %d MiB, want 4608", st.UserReserve>>20)
	}
	var f protocol.MachineFacts
	f.RAMTotalMB = 16384
	d.annotateGuardFacts(&f)
	if f.PolicyReserveMB != 4608 || f.SurvivalReserveMB == 0 {
		t.Fatalf("facts = survival %d, policy %d", f.SurvivalReserveMB, f.PolicyReserveMB)
	}
	// Admission: 4 GiB free is below survival reserve + 4.5 GiB.
	d.lastMetrics.Store(&protocol.WorkerMetrics{MemTotalMB: 16384, MemUsedMB: 12288})
	if msg := d.admissionRefusal(protocol.WorkTask, ""); !strings.Contains(msg, "kept free") {
		t.Fatalf("refusal = %q (reserve %d MB)", msg, reserveMB)
	}
	d.lastMetrics.Store(&protocol.WorkerMetrics{MemTotalMB: 16384, MemUsedMB: 4096})
	if msg := d.admissionRefusal(protocol.WorkTask, ""); msg != "" {
		t.Fatalf("refused with 12 GiB free: %s", msg)
	}
	d.setPolicy(nil)
	if g.Status().UserReserve != 0 {
		t.Fatal("clearing the policy kept the user reserve")
	}
}
