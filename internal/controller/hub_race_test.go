package controller

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// TestWorkerFactsDuringDispatchRace sends worker.facts frames while tasks are
// dispatched to the same worker. Run under -race: the facts handler reads the
// active-task count that DispatchToWorker writes (SYR-FVR3K).
func TestWorkerFactsDuringDispatchRace(t *testing.T) {
	th := newTestHub(t)
	info := testWorkerInfo("w-race", "race-box")
	info.MaxConcurrent = 500
	fw := th.connectWorker(t, info, nil, "w-race")

	const n = 100
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			fw.send(protocol.MsgWorkerFacts, protocol.WorkerFactsPayload{Facts: protocol.MachineFacts{CPUCores: i + 1}})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			_ = th.h.DispatchToWorker("w-race", protocol.TaskStartPayload{TaskID: fmt.Sprintf("race-%d", i), Agent: "mock"})
		}
	}()
	wg.Wait()
	waitFor(t, 5*time.Second, "all dispatches tracked", func() bool {
		return th.h.ActiveTaskCounts()["w-race"] == n
	})
	fw.drain(protocol.MsgTaskStart, 100*time.Millisecond)
}
