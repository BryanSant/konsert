//go:build windows

package worker

import (
	"context"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/drivers"
)

// ConPTY output usually does not reach EOF until ClosePseudoConsole is called, so a task
// whose process exited normally used to hang in "running". It must now complete promptly.
func TestConPtyTaskCompletes(t *testing.T) {
	rec := newStatusRecorder()
	runner := NewRunner(drivers.DefaultRegistry, false, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
	err := runner.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID:  "t-2b0mk",
		Agent:   "mock",
		Workdir: t.TempDir(),
		Prompt:  "EXIT=0",
		Mode:    protocol.TaskModeOneshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(15 * time.Second)
	for {
		select {
		case p := <-rec.ch:
			if protocol.IsTerminalTaskStatus(p.Status) {
				if p.Status != protocol.TaskStatusCompleted {
					t.Fatalf("expected completed, got %+v", p)
				}
				return
			}
		case <-deadline:
			t.Fatalf("ConPTY task did not complete; statuses: %+v", rec.snapshot())
		}
	}
}
