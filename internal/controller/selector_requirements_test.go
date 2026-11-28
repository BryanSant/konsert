package controller

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Work whose requirements no machine meets is never placed (SYR-FSQP8).
func TestRequirementMismatchIsNeverPlaced(t *testing.T) {
	facts := func(f protocol.MachineFacts) string { raw, _ := json.Marshal(f); return string(raw) }
	workers := []store.Worker{
		{ID: "mac", OS: "darwin", Arch: "arm64", Status: "online", MaxConcurrent: 4, Agents: []string{"mock"},
			FactsJSON: facts(protocol.MachineFacts{RAMTotalMB: 16384, GPUs: []protocol.GPU{{Vendor: "apple", Model: "Apple M4", VRAMMB: 16384, Unified: true}},
				Tools: map[string]string{"xcode": "26.1"}})},
		{ID: "amd", OS: "linux", Arch: "amd64", Status: "online", MaxConcurrent: 4, Agents: []string{"mock"},
			FactsJSON: facts(protocol.MachineFacts{RAMTotalMB: 32768, GPUs: []protocol.GPU{
				{Vendor: "amd", Model: "Radeon RX 9060 XT", VRAMMB: 8144}, {Vendor: "amd", Model: "Radeon 610M", VRAMMB: 512, Integrated: true}}})},
		{ID: "igpu", OS: "linux", Arch: "amd64", Status: "online", MaxConcurrent: 4, Agents: []string{"mock"},
			FactsJSON: facts(protocol.MachineFacts{RAMTotalMB: 16384, GPUs: []protocol.GPU{{Vendor: "intel", VRAMMB: 256, Integrated: true}}})},
	}
	pick := func(r WorkRequirements) string {
		var sel WorkerSelector
		r.apply(&sel)
		w, ok := SelectWorker(workers, nil, sel, "mock", "")
		if !ok {
			return "-"
		}
		return w.ID
	}
	for _, c := range []struct {
		name string
		r    WorkRequirements
		want string // "-": never placed; "*": any machine
	}{
		{"arm64", WorkRequirements{Arch: "arm64"}, "mac"},
		{"riscv", WorkRequirements{Arch: "riscv64"}, "-"},
		{"windows", WorkRequirements{OS: "windows"}, "-"},
		{"a gpu (an iGPU doesn't count)", WorkRequirements{GPU: true}, "*gpu"},
		{"nvidia", WorkRequirements{GPU: true, GPUVendor: "nvidia"}, "-"},
		{"amd", WorkRequirements{GPUVendor: "AMD"}, "amd"},
		{"12 GB of VRAM", WorkRequirements{MinVRAMGB: 12}, "mac"},
		{"32 GB of VRAM", WorkRequirements{MinVRAMGB: 32}, "-"},
		{"xcode 26", WorkRequirements{Tools: map[string]string{"xcode": "26"}}, "mac"},
		{"cuda", WorkRequirements{Tools: map[string]string{"cuda": ""}}, "-"},
		{"48 GB of RAM", WorkRequirements{MinRAMGB: 48}, "-"},
	} {
		got := pick(c.r)
		switch {
		case c.want == "*gpu":
			if got != "mac" && got != "amd" {
				t.Errorf("%s: placed on %s", c.name, got)
			}
		case got != c.want:
			t.Errorf("%s: placed on %s, want %s", c.name, got, c.want)
		}
	}
}

// A ticket's gpu hint is a requirement.
func TestTicketGPUHintIsRequired(t *testing.T) {
	th := newTestHub(t)
	sel := th.h.ticketWorkerSelector(t.Context(), store.Ticket{Hints: store.TicketHints{GPU: true}})
	if !sel.GPU {
		t.Fatalf("selector: %+v", sel)
	}
}

// An interactive machine in use holds the scheduler's and the Commander's work, not the user's.
func TestInteractiveRoleGatesAutomaticWorkOnly(t *testing.T) {
	th := newTestHub(t)
	ctx := t.Context()
	upsertBox(t, th.st, "w1", "desk")
	pol, _ := json.Marshal(protocol.MachinePolicy{Role: protocol.MachineRoleInteractive})
	if err := th.st.SetWorkerPolicy(ctx, "w1", string(pol)); err != nil {
		t.Fatal(err)
	}
	w, _ := th.st.GetWorker(ctx, "w1")
	idle := int64(20)
	th.h.mu.Lock()
	th.h.workerMetrics["w1"] = &protocol.WorkerMetrics{MemTotalMB: 16384, MemUsedMB: 4000, Load: &protocol.MachineLoad{UserIdleS: &idle}}
	th.h.mu.Unlock()
	if v, _ := th.h.policyViolation(ctx, w, protocol.WorkTicket, 0, "t"); v == nil {
		t.Fatal("a ticket run was admitted while the user types")
	}
	if v, _ := th.h.policyViolation(commanderCtx("orch"), w, protocol.WorkTask, 0, "t"); v == nil {
		t.Fatal("the commander's task was admitted while the user types")
	}
	if v, _ := th.h.policyViolation(userCtx(), w, protocol.WorkTask, 0, "t"); v != nil {
		t.Fatalf("the user's own task was held: %v", v)
	}
}

// Dispatch ranks machines with the placement scorer: work known to peak at 8 GB goes
// to the machine with room for it, though the other has more free slots (SYR-YCQQ1).
func TestDispatchPlacesKnownWorkWhereItFits(t *testing.T) {
	th := newTestHub(t)
	ctx := t.Context()
	small := testWorkerInfo("w-a-small", "small")
	small.MaxConcurrent = 8
	big := testWorkerInfo("w-z-big", "big")
	big.MaxConcurrent = 2
	// big connects first: every tie-break (seen last, id) favours small.
	th.connectWorker(t, big, nil, "w-z-big")
	time.Sleep(10 * time.Millisecond)
	th.connectWorker(t, small, nil, "w-a-small")
	th.h.mu.Lock()
	th.h.workerMetrics["w-a-small"] = &protocol.WorkerMetrics{MemTotalMB: 8192, MemUsedMB: 4096}
	th.h.workerMetrics["w-z-big"] = &protocol.WorkerMetrics{MemTotalMB: 65536, MemUsedMB: 8192}
	th.h.mu.Unlock()
	key := "default/adhoc/train/ml-train"
	if err := th.st.UpsertWorkProfile(ctx, store.WorkProfile{WorkKey: key, MachineClass: "linux/amd64", Runs: 4, PeakRSSMaxMB: 8000}); err != nil {
		t.Fatal(err)
	}
	res, err := th.svc.DispatchTask(userCtx(), DispatchRequest{Agent: "mock", RepoPath: "/src/train", Prompt: "train", WorkKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if res.WorkerID != "w-z-big" {
		t.Fatalf("placed on %q, want w-z-big", res.WorkerID)
	}
}
