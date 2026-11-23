package worker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

func TestRunnerMockLifecycleSubscribeAndCancel(t *testing.T) {
	ctx := context.Background()
	workdir := t.TempDir()

	var (
		mu       sync.Mutex
		statuses []protocol.TaskStatusPayload
		events   []protocol.TaskEventPayload
		frames   [][]byte
		doneCh   = make(chan protocol.TaskStatusPayload, 1)
	)

	runner := NewRunner(drivers.DefaultRegistry, false, nil, RunnerCallbacks{
		OnTaskStatus: func(p protocol.TaskStatusPayload) {
			mu.Lock()
			statuses = append(statuses, p)
			mu.Unlock()
			if protocol.IsTerminalTaskStatus(p.Status) {
				select {
				case doneCh <- p:
				default:
				}
			}
		},
		OnTaskEvent: func(p protocol.TaskEventPayload) {
			mu.Lock()
			events = append(events, p)
			mu.Unlock()
		},
		OnPtyFrame: func(frameType byte, taskID string, data []byte) {
			mu.Lock()
			frames = append(frames, append([]byte{frameType}, data...))
			mu.Unlock()
		},
	}, nil)

	// 1. Run a oneshot task with STREAM_LINES=5 and verify zero PTY frames while unsubscribed.
	taskID := "TASK-TX8W4"
	err := runner.StartTask(ctx, protocol.TaskStartPayload{
		TaskID:         taskID,
		Agent:          "mock",
		Workdir:        workdir,
		Prompt:         "STREAM_LINES=5",
		Mode:           protocol.TaskModeOneshot,
		PermissionMode: protocol.PermissionFullAccess,
	})
	if err != nil {
		t.Fatalf("StartTask failed: %v", err)
	}

	select {
	case final := <-doneCh:
		if final.Status != protocol.TaskStatusCompleted {
			t.Fatalf("expected completed, got %+v", final)
		}
		if final.ExitCode == nil || *final.ExitCode != 0 {
			t.Fatalf("expected exit code 0, got %+v", final.ExitCode)
		}
		if !strings.Contains(final.Summary, "Mock agent finished cleanly") {
			t.Fatalf("unexpected summary: %q", final.Summary)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for mock task completion")
	}

	mu.Lock()
	if len(frames) != 0 {
		t.Fatalf("expected zero PTY frames while unsubscribed, got %d", len(frames))
	}
	if len(events) < 3 {
		t.Fatalf("expected >= 3 telemetry events, got %d: %+v", len(events), events)
	}
	mu.Unlock()

	// 2. Interactive task: test subscribe replay, ResizePty, SteerTask, and clean completion.
	doneCh2 := make(chan protocol.TaskStatusPayload, 1)
	waitingCh := make(chan struct{}, 1)
	snapCh := make(chan []byte, 4)

	runner2 := NewRunner(drivers.DefaultRegistry, false, nil, RunnerCallbacks{
		OnTaskStatus: func(p protocol.TaskStatusPayload) {
			if p.Status == protocol.TaskStatusWaitingInput {
				select {
				case waitingCh <- struct{}{}:
				default:
				}
			}
			if protocol.IsTerminalTaskStatus(p.Status) {
				select {
				case doneCh2 <- p:
				default:
				}
			}
		},
		OnPtyFrame: func(frameType byte, _ string, data []byte) {
			if frameType == protocol.FramePtySnapshot {
				select {
				case snapCh <- data:
				default:
				}
			}
		},
	}, nil)

	taskID2 := "TASK-5HH0K"
	if err := runner2.StartTask(ctx, protocol.TaskStartPayload{
		TaskID:         taskID2,
		Agent:          "mock",
		Workdir:        workdir,
		Prompt:         "WAIT_INPUT",
		Mode:           protocol.TaskModeInteractive,
		PermissionMode: protocol.PermissionFullAccess,
	}); err != nil {
		t.Fatalf("StartTask interactive failed: %v", err)
	}

	select {
	case <-waitingCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for waiting_input")
	}

	if !runner2.SubscribePty(taskID2) {
		t.Fatal("expected SubscribePty to succeed")
	}
	select {
	case snap := <-snapCh:
		if !strings.Contains(string(snap), "konsert-mock") && !strings.Contains(string(snap), "[MOCK:") {
			t.Fatalf("unexpected snapshot content: %q", string(snap))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for PTY snapshot")
	}

	if err := runner2.ResizePty(taskID2, 100, 40); err != nil {
		t.Fatalf("ResizePty failed: %v", err)
	}
	if err := runner2.SteerTask(taskID2, "exit", true); err != nil {
		t.Fatalf("SteerTask failed: %v", err)
	}

	select {
	case final := <-doneCh2:
		if final.Status != protocol.TaskStatusCompleted {
			t.Fatalf("expected completed after exit steer, got %+v", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for interactive task exit")
	}

	// 3. Cancellation test: SLEEP=30 canceled quickly via PGID teardown.
	doneCh3 := make(chan protocol.TaskStatusPayload, 1)
	runningCh3 := make(chan struct{}, 1)
	runner3 := NewRunner(drivers.DefaultRegistry, false, nil, RunnerCallbacks{
		OnTaskStatus: func(p protocol.TaskStatusPayload) {
			if p.Status == protocol.TaskStatusRunning {
				select {
				case runningCh3 <- struct{}{}:
				default:
				}
			}
			if protocol.IsTerminalTaskStatus(p.Status) {
				select {
				case doneCh3 <- p:
				default:
				}
			}
		},
	}, nil)

	taskID3 := "TASK-PY1D2"
	if err := runner3.StartTask(ctx, protocol.TaskStartPayload{
		TaskID:         taskID3,
		Agent:          "mock",
		Workdir:        workdir,
		Prompt:         "SLEEP=30",
		Mode:           protocol.TaskModeOneshot,
		PermissionMode: protocol.PermissionFullAccess,
	}); err != nil {
		t.Fatalf("StartTask cancel test failed: %v", err)
	}
	<-runningCh3
	time.Sleep(150 * time.Millisecond)
	if err := runner3.CancelTask(taskID3); err != nil {
		t.Fatalf("CancelTask failed: %v", err)
	}
	select {
	case final := <-doneCh3:
		if final.Status != protocol.TaskStatusCanceled {
			t.Fatalf("expected canceled, got %+v", final)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for task cancellation")
	}
}

func TestRunnerSandboxedRefusesFullAccess(t *testing.T) {
	workdir := t.TempDir()
	var got protocol.TaskStatusPayload
	runner := NewRunner(drivers.DefaultRegistry, true, []string{workdir}, RunnerCallbacks{
		OnTaskStatus: func(p protocol.TaskStatusPayload) {
			got = p
		},
	}, nil)

	err := runner.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID:         "TASK-WVHWR",
		Agent:          "mock",
		Workdir:        workdir,
		Prompt:         "hello",
		PermissionMode: protocol.PermissionFullAccess,
	})
	if err == nil {
		t.Fatal("expected error when sandboxed worker receives full_access task")
	}
	if got.Status != protocol.TaskStatusFailed {
		t.Fatalf("expected failed status emission, got %+v", got)
	}
}
