//go:build !windows

package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/drivers"
)

// scriptDriver runs its prompt as a /bin/sh script; used to exercise the supervisor.
type scriptDriver struct{ name string }

func (d *scriptDriver) Name() string           { return d.name }
func (d *scriptDriver) Detect() (string, bool) { return "/bin/sh", true }
func (d *scriptDriver) ExtractSummary(context.Context, drivers.SummaryOptions) string {
	return "script done"
}
func (d *scriptDriver) BuildCommand(opts drivers.LaunchOptions) drivers.LaunchSpec {
	return drivers.LaunchSpec{Binary: "/bin/sh", Args: []string{"-c", "--", opts.Prompt}, Env: opts.Env}
}
func (d *scriptDriver) WatchTelemetry(ctx context.Context, opts drivers.WatchOptions, _ func(drivers.TelemetryEvent)) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-opts.OutputLines:
			if !ok {
				return
			}
		}
	}
}

func scriptRegistry() *drivers.Registry {
	reg := drivers.NewRegistry()
	reg.Register(&scriptDriver{name: "script"})
	return reg
}

func waitTerminal(t *testing.T, rec *statusRecorder, timeout time.Duration) protocol.TaskStatusPayload {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case p := <-rec.ch:
			if protocol.IsTerminalTaskStatus(p.Status) {
				return p
			}
		case <-deadline:
			t.Fatalf("timed out after %s waiting for terminal status; got %+v", timeout, rec.snapshot())
		}
	}
}

// A single 2 MiB line with no newline used to wedge the io.Pipe + bufio.Scanner tee.
func TestRunnerTwoMiBLineCompletes(t *testing.T) {
	rec := newStatusRecorder()
	runner := NewRunner(scriptRegistry(), false, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
	err := runner.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID:  "t-21vyd",
		Agent:   "script",
		Workdir: t.TempDir(),
		Prompt:  "head -c 2097152 /dev/zero | tr '\\000' a; exit 3",
		Mode:    protocol.TaskModeOneshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, rec, 20*time.Second)
	if final.Status != protocol.TaskStatusFailed || final.ExitCode == nil || *final.ExitCode != 3 {
		t.Fatalf("expected failed with exit code 3, got %+v", final)
	}
}

// The main process exits while a background child keeps the PTY slave open.
func TestRunnerCompletesWhenBackgroundChildHoldsPty(t *testing.T) {
	rec := newStatusRecorder()
	runner := NewRunner(scriptRegistry(), false, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
	start := time.Now()
	err := runner.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID:  "t-k18cb",
		Agent:   "script",
		Workdir: t.TempDir(),
		Prompt:  "trap '' HUP; sleep 30 & echo main-done",
		Mode:    protocol.TaskModeOneshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, rec, 10*time.Second)
	if final.Status != protocol.TaskStatusCompleted || final.ExitCode == nil || *final.ExitCode != 0 {
		t.Fatalf("expected completed/0, got %+v", final)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("task completion took %s; supervisor waited on PTY EOF", took)
	}
	if len(runner.ActiveTaskIDs()) != 0 {
		// The task entry is removed in a deferred cleanup right after the final status.
		time.Sleep(200 * time.Millisecond)
		if ids := runner.ActiveTaskIDs(); len(ids) != 0 {
			t.Fatalf("task still registered after completion: %v", ids)
		}
	}
}

func TestLineSplitterBoundsLongLines(t *testing.T) {
	var got []string
	ls := lineSplitter{max: 8}
	emit := func(s string) { got = append(got, s) }
	ls.feed([]byte("ab\r\ncd"), emit)
	ls.feed([]byte("ef\n"), emit)
	ls.feed([]byte(strings.Repeat("x", 20)), emit)
	ls.feed([]byte("yyy\nshort"), emit)
	ls.flush(emit)
	want := []string{"ab", "cdef", "xxxxxxxx", "short"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Managed tasks expose their PID (so discovery skips them) and carry SYRE_TASK_ID.
func TestRunnerManagedPIDAndTaskEnv(t *testing.T) {
	rec := newStatusRecorder()
	runner := NewRunner(scriptRegistry(), false, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
	workdir := t.TempDir()
	const id = "t-bzwsq"
	err := runner.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID:  id,
		Agent:   "script",
		Workdir: workdir,
		Prompt:  `printf %s "$SYRE_TASK_ID" > tid.txt; sleep 30`,
		Mode:    protocol.TaskModeOneshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.ShutdownAll)

	runner.mu.RLock()
	pid := runner.tasks[id].pty.Pid()
	runner.mu.RUnlock()
	if pid <= 0 || !runner.IsManagedPID(pid) {
		t.Fatalf("expected task pid %d to be managed", pid)
	}
	if runner.IsManagedPID(os.Getpid()) {
		t.Fatal("unrelated pid reported as managed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, _ := os.ReadFile(filepath.Join(workdir, "tid.txt"))
		if string(raw) == id {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SYRE_TASK_ID not set in task env (got %q)", raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
