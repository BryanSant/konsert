package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/gitutil"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

// missingBinaryDriver always fails to launch.
type missingBinaryDriver struct{ scriptlessDriver }

func (missingBinaryDriver) Name() string { return "missing" }
func (missingBinaryDriver) BuildCommand(drivers.LaunchOptions) drivers.LaunchSpec {
	return drivers.LaunchSpec{Binary: filepath.Join(os.TempDir(), "konsert-definitely-missing-binary")}
}

// scriptlessDriver supplies no-op telemetry/summary methods.
type scriptlessDriver struct{}

func (scriptlessDriver) Detect() (string, bool) { return "", true }
func (scriptlessDriver) WatchTelemetry(ctx context.Context, opts drivers.WatchOptions, _ func(drivers.TelemetryEvent)) {
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
func (scriptlessDriver) ExtractSummary(context.Context, drivers.SummaryOptions) string { return "" }

func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo
}

func TestRunnerRejectsInvalidTaskIDs(t *testing.T) {
	workdir := t.TempDir()
	for _, id := range []string{"", "../../x", "a/b", `a\b`, "has space", strings.Repeat("a", 37), "ok;rm"} {
		rec := newStatusRecorder()
		r := NewRunner(drivers.DefaultRegistry, false, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
		err := r.StartTask(context.Background(), protocol.TaskStartPayload{TaskID: id, Agent: "mock", Workdir: workdir, Prompt: "EXIT=0"})
		if !errors.Is(err, protocol.ErrInvalidTaskID) {
			t.Fatalf("id %q: expected ErrInvalidTaskID, got %v", id, err)
		}
		if got := rec.snapshot(); len(got) != 1 || got[0].Status != protocol.TaskStatusFailed {
			t.Fatalf("id %q: expected one failed status, got %+v", id, got)
		}
		if ids := r.ActiveTaskIDs(); len(ids) != 0 {
			t.Fatalf("id %q: invalid task registered: %v", id, ids)
		}
	}
}

func TestRunnerRejectsDuplicateTaskID(t *testing.T) {
	workdir := t.TempDir()
	rec := newStatusRecorder()
	r := NewRunner(drivers.DefaultRegistry, false, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
	const id = "aaaaaaaa-0000-0000-0000-000000000001"
	req := protocol.TaskStartPayload{TaskID: id, Agent: "mock", Workdir: workdir, Prompt: "SLEEP=30", Mode: protocol.TaskModeOneshot}
	if err := r.StartTask(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.ShutdownAll)
	before := len(rec.snapshot())

	err := r.StartTask(context.Background(), req)
	if !errors.Is(err, ErrDuplicateTask) {
		t.Fatalf("expected ErrDuplicateTask, got %v", err)
	}
	if err := r.StartTaskAsync(context.Background(), req, nil); !errors.Is(err, ErrDuplicateTask) {
		t.Fatalf("async: expected ErrDuplicateTask, got %v", err)
	}
	// The duplicate must not emit anything (a failed status would kill the running task's row).
	if after := rec.snapshot(); len(after) != before {
		t.Fatalf("duplicate start emitted statuses: %+v", after[before:])
	}
	if ids := r.ActiveTaskIDs(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("running task disturbed: %v", ids)
	}
}

func TestRunnerRemovesWorktreeOnLaunchFailure(t *testing.T) {
	repo := initGitRepo(t)
	reg := drivers.NewRegistry()
	reg.Register(missingBinaryDriver{})
	rec := newStatusRecorder()
	r := NewRunner(reg, false, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
	const id = "bbbbbbbb-0000-0000-0000-000000000001"
	err := r.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID: id, Agent: "missing", Workdir: repo, Branch: "konsert/launch-fail", UseWorktree: true,
	})
	if err == nil {
		t.Fatal("expected launch failure")
	}
	if got := rec.snapshot(); len(got) != 1 || got[0].Status != protocol.TaskStatusFailed {
		t.Fatalf("expected one failed status, got %+v", got)
	}
	root, _ := gitutil.IsGitRepo(context.Background(), repo)
	if _, statErr := os.Stat(filepath.Join(root, ".konsert-worktrees", id)); !os.IsNotExist(statErr) {
		t.Fatalf("worktree left behind after launch failure (stat err=%v)", statErr)
	}
	out, _ := exec.Command("git", "-C", repo, "branch", "--list", "konsert/launch-fail").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("branch created for the task was left behind: %q", out)
	}
}

// stubSlowWorktree replaces worktree creation with a stub that blocks until released (or ctx ends).
func stubSlowWorktree(t *testing.T) (entered <-chan struct{}, release func()) {
	t.Helper()
	enteredCh := make(chan struct{}, 1)
	releaseCh := make(chan struct{})
	orig := createWorktreeFn
	createWorktreeFn = func(ctx context.Context, repoRoot, taskID, branch string) (gitutil.WorktreeInfo, error) {
		enteredCh <- struct{}{}
		select {
		case <-releaseCh:
		case <-ctx.Done():
			return gitutil.WorktreeInfo{}, ctx.Err()
		}
		return gitutil.WorktreeInfo{Path: filepath.Join(repoRoot), RepoRoot: repoRoot}, nil
	}
	var released bool
	t.Cleanup(func() {
		if !released {
			close(releaseCh)
		}
		createWorktreeFn = orig
	})
	return enteredCh, func() { released = true; close(releaseCh) }
}

func TestRunnerCancelDuringSlowSetup(t *testing.T) {
	repo := initGitRepo(t)
	entered, _ := stubSlowWorktree(t)
	rec := newStatusRecorder()
	r := NewRunner(drivers.DefaultRegistry, false, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
	const id = "cccccccc-0000-0000-0000-000000000001"
	errCh := make(chan error, 1)
	if err := r.StartTaskAsync(context.Background(), protocol.TaskStartPayload{
		TaskID: id, Agent: "mock", Workdir: repo, Branch: "x", UseWorktree: true, Prompt: "SLEEP=30",
	}, func(err error) { errCh <- err }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worktree stub never entered")
	}
	// The task is reserved: a duplicate is refused and a subscribe is accepted for later.
	if err := r.StartTaskAsync(context.Background(), protocol.TaskStartPayload{TaskID: id, Agent: "mock", Workdir: repo}, nil); !errors.Is(err, ErrDuplicateTask) {
		t.Fatalf("expected duplicate during setup, got %v", err)
	}
	if !r.SubscribePty(id) {
		t.Fatal("subscribe during setup should be accepted")
	}
	if err := r.CancelTask(id); err != nil {
		t.Fatalf("cancel during setup: %v", err)
	}
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("start did not abort after cancel")
	}
	got := rec.snapshot()
	if len(got) == 0 || !protocol.IsTerminalTaskStatus(got[len(got)-1].Status) {
		t.Fatalf("expected a terminal status after cancel during setup, got %+v", got)
	}
	if ids := r.ActiveTaskIDs(); len(ids) != 0 {
		t.Fatalf("canceled task registered: %v", ids)
	}
}
