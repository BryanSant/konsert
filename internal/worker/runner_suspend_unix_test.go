//go:build !windows

package worker

import (
	"context"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/shells"
)

// A suspended task reports "suspended", its timeout stops counting, and resuming
// reports it running again; the timeout then fires with the time that was left.
func TestRunnerSuspendResumePausesTimeout(t *testing.T) {
	rec := newStatusRecorder()
	runner := NewRunner(scriptRegistry(), false, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
	const id = "t-sus4p"
	if err := runner.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID: id, Agent: "script", Workdir: t.TempDir(), Prompt: "sleep 30",
		Mode: protocol.TaskModeOneshot, TimeoutSeconds: 2,
	}); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, rec, protocol.TaskStatusRunning)
	time.Sleep(500 * time.Millisecond)
	if err := runner.SuspendTask(id); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, rec, protocol.TaskStatusSuspended)
	if got := runner.ActiveTasks(); len(got) != 1 || got[0].Status != protocol.TaskStatusSuspended {
		t.Fatalf("hello would report %+v, want suspended", got)
	}
	// Longer than the whole timeout: a suspended task never times out.
	time.Sleep(2500 * time.Millisecond)
	for _, p := range rec.snapshot() {
		if protocol.IsTerminalTaskStatus(p.Status) {
			t.Fatalf("suspended task ended: %+v", p)
		}
	}
	resumed := time.Now()
	if err := runner.ResumeTask(id); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, rec, protocol.TaskStatusRunning)
	final := waitTerminal(t, rec, 10*time.Second)
	if final.Status != protocol.TaskStatusFailed {
		t.Fatalf("expected the timeout to fail the task, got %+v", final)
	}
	if took := time.Since(resumed); took < time.Second {
		t.Fatalf("timeout fired %s after resume; it should have kept the ~1.5s left", took)
	}
}

// Canceling a suspended task thaws it first so it terminates promptly.
func TestRunnerCancelSuspendedTask(t *testing.T) {
	rec := newStatusRecorder()
	runner := NewRunner(scriptRegistry(), false, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
	const id = "t-sus5q"
	if err := runner.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID: id, Agent: "script", Workdir: t.TempDir(), Prompt: "sleep 30", Mode: protocol.TaskModeOneshot,
	}); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, rec, protocol.TaskStatusRunning)
	// Freeze it once sleep runs. Frozen mid-fork, dash has every signal blocked: the
	// SIGINT after the thaw reaches dash (which catches it and waits for its child) but
	// not the child, which then runs sleep until the grace ends. That is the shell, not
	// the cancel path (SYR-TM8TM).
	runner.mu.RLock()
	root := runner.tasks[id].pty.Pid()
	runner.mu.RUnlock()
	waitForSleep(t, root)
	if err := runner.SuspendTask(id); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, rec, protocol.TaskStatusSuspended)
	start := time.Now()
	if err := runner.CancelTask(id); err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, rec, 10*time.Second)
	if final.Status != protocol.TaskStatusCanceled {
		t.Fatalf("want canceled, got %+v", final)
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("cancel of a suspended task took %s (frozen tree ignored SIGINT until SIGKILL)", took)
	}
}

// waitForSleep waits until root is sleep or has a sleep child.
func waitForSleep(t *testing.T, root int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, _ := shells.OSSource().Snapshot()
		for _, p := range list {
			if p.Name == "sleep" && (p.PID == root || p.PPID == root) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("sleep never started under %d", root)
}

func expectStatus(t *testing.T, rec *statusRecorder, status string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case p := <-rec.ch:
			if p.Status == status {
				return
			}
		case <-deadline:
			t.Fatalf("no %s status; got %+v", status, rec.snapshot())
		}
	}
}
