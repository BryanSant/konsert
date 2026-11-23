package worker

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

// statusRecorder collects task.status payloads emitted by a Runner.
type statusRecorder struct {
	mu  sync.Mutex
	got []protocol.TaskStatusPayload
	ch  chan protocol.TaskStatusPayload
}

func newStatusRecorder() *statusRecorder {
	return &statusRecorder{ch: make(chan protocol.TaskStatusPayload, 64)}
}

func (s *statusRecorder) record(p protocol.TaskStatusPayload) {
	s.mu.Lock()
	s.got = append(s.got, p)
	s.mu.Unlock()
	select {
	case s.ch <- p:
	default:
	}
}

func (s *statusRecorder) snapshot() []protocol.TaskStatusPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.TaskStatusPayload(nil), s.got...)
}

func TestRunnerSandboxedRefusesShellUnlessOptedIn(t *testing.T) {
	workdir := t.TempDir()
	rec := newStatusRecorder()
	runner := NewRunner(drivers.DefaultRegistry, true, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
	if runner.AgentAllowed(drivers.ShellDriverName) {
		t.Fatal("sandboxed runner must not allow the shell driver by default")
	}
	err := runner.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID:         "TASK-E4RKJ",
		Agent:          drivers.ShellDriverName,
		Workdir:        workdir,
		Prompt:         "id",
		Mode:           protocol.TaskModeOneshot,
		PermissionMode: protocol.PermissionSandboxed,
	})
	if err == nil {
		t.Fatal("expected sandboxed worker to refuse the shell driver")
	}
	got := rec.snapshot()
	if len(got) != 1 || got[0].Status != protocol.TaskStatusFailed || !strings.Contains(got[0].Summary, "shell") {
		t.Fatalf("expected one failed status mentioning shell, got %+v", got)
	}

	runner.SetAllowSandboxShell(true)
	if !runner.AgentAllowed(drivers.ShellDriverName) {
		t.Fatal("opt-in must allow the shell driver")
	}
	if !NewRunner(drivers.DefaultRegistry, false, nil, RunnerCallbacks{}, nil).AgentAllowed(drivers.ShellDriverName) {
		t.Fatal("full-access runner must allow the shell driver")
	}
}
