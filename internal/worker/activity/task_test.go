package activity

import (
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

func TestTaskTelemetryStates(t *testing.T) {
	tk := NewTask("t1")
	now := time.Now()
	check := func(changed bool, state, tool string) {
		t.Helper()
		a := tk.Activity(now)
		if !changed || a.State != state || a.Tool != tool || !a.Coarse || a.Source != protocol.ActivitySourceLog {
			t.Fatalf("got changed=%v %s/%s coarse=%v, want %s/%s coarse", changed, a.State, a.Tool, a.Coarse, state, tool)
		}
	}
	check(tk.ObserveStatus(protocol.TaskStatusPayload{TaskID: "t1", Status: protocol.TaskStatusRunning}, now), protocol.ActivityThinking, "")
	check(tk.ObserveEvent(protocol.TaskEventPayload{Kind: "tool_call", Message: "Bash: go test", Meta: map[string]any{"tool": "Bash"}}, now), protocol.ActivityWorking, "Bash")
	check(tk.ObserveEvent(protocol.TaskEventPayload{Kind: "file_edit", Message: "Edit: main.go"}, now), protocol.ActivityWorking, "Edit")
	check(tk.ObserveEvent(protocol.TaskEventPayload{Kind: "usage"}, now), protocol.ActivityThinking, "")
	check(tk.ObserveEvent(protocol.TaskEventPayload{Kind: "waiting_input", Message: "Do you want to proceed?"}, now), protocol.ActivityAsking, "")
	if tk.ObserveStatus(protocol.TaskStatusPayload{Status: protocol.TaskStatusWaitingInput}, now) {
		t.Fatal("the waiting_input status after the event should change nothing")
	}
	check(tk.ObserveStatus(protocol.TaskStatusPayload{Status: protocol.TaskStatusSuspended}, now), protocol.ActivityBlocked, "")
	check(tk.ObserveStatus(protocol.TaskStatusPayload{Status: protocol.TaskStatusRunning}, now), protocol.ActivityThinking, "")
	check(tk.ObserveEvent(protocol.TaskEventPayload{Kind: "waiting_input", Message: "grok is waiting for your next prompt"}, now), protocol.ActivityDone, "")
	code := 1
	check(tk.ObserveStatus(protocol.TaskStatusPayload{Status: protocol.TaskStatusFailed, ExitCode: &code}, now), protocol.ActivityError, "")
}

func TestTrackerPublishesFinalStateAtOnce(t *testing.T) {
	p, clk, out := newTestPublisher()
	tr := NewTracker(p, clk.Now)
	tr.Status(protocol.TaskStatusPayload{TaskID: "t1", Status: protocol.TaskStatusRunning})
	tr.Event(protocol.TaskEventPayload{TaskID: "t1", Kind: "tool_call", Message: "Bash", Meta: map[string]any{"tool": "Bash"}})
	tr.Event(protocol.TaskEventPayload{TaskID: "t1", Kind: "exit"})
	tr.Status(protocol.TaskStatusPayload{TaskID: "t1", Status: protocol.TaskStatusCompleted})
	got := states(*out)
	if len(got) == 0 || got[len(got)-1] != "done:" {
		t.Fatalf("published %v, want the final done without waiting", got)
	}
	if len(tr.tasks) != 0 {
		t.Fatal("a finished task should be forgotten")
	}
}
