package worker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BryanSant/konsert/internal/gitutil"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

// RunnerCallbacks delivers worker task events, status updates, and PTY frames back to the daemon transport.
type RunnerCallbacks struct {
	OnTaskStatus func(payload protocol.TaskStatusPayload)
	OnTaskEvent  func(payload protocol.TaskEventPayload)
	OnPtyFrame   func(frameType byte, taskID string, data []byte)
}

type runningTask struct {
	id           string
	agent        string
	effectiveDir string
	baseSHA      string
	startedAt    time.Time
	pty          PtyProcess
	ring         *RingBuffer
	streamActive atomic.Bool
	cancelFunc   context.CancelFunc

	mu       sync.Mutex
	status   string
	canceled bool
	events   []drivers.TelemetryEvent
}

// Runner manages the lifecycle of all executing tasks on a worker node.
type Runner struct {
	registry     *drivers.Registry
	sandbox      bool
	allowedRoots []string
	callbacks    RunnerCallbacks
	logger       *slog.Logger

	// allowSandboxShell opts a sandboxed worker in to the unrestricted shell driver.
	allowSandboxShell atomic.Bool

	mu    sync.RWMutex
	tasks map[string]*runningTask
}

// NewRunner constructs a task runner.
func NewRunner(registry *drivers.Registry, sandbox bool, allowedRoots []string, cb RunnerCallbacks, logger *slog.Logger) *Runner {
	if registry == nil {
		registry = drivers.DefaultRegistry
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		registry:     registry,
		sandbox:      sandbox,
		allowedRoots: allowedRoots,
		callbacks:    cb,
		logger:       logger,
		tasks:        make(map[string]*runningTask),
	}
}

// SetAllowSandboxShell opts a sandboxed worker in to running the shell driver. A shell
// ignores permission mode entirely, so sandboxed workers refuse it by default.
func (r *Runner) SetAllowSandboxShell(allow bool) {
	r.allowSandboxShell.Store(allow)
}

// AgentAllowed reports whether this runner will accept tasks for the named driver.
func (r *Runner) AgentAllowed(agent string) bool {
	if r.sandbox && agent == drivers.ShellDriverName && !r.allowSandboxShell.Load() {
		return false
	}
	return true
}

// ActiveTasks returns snapshots of currently running tasks for worker.hello re-adoption.
func (r *Runner) ActiveTasks() []protocol.ActiveTaskInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]protocol.ActiveTaskInfo, 0, len(r.tasks))
	for _, rt := range r.tasks {
		rt.mu.Lock()
		st := rt.status
		rt.mu.Unlock()
		out = append(out, protocol.ActiveTaskInfo{
			TaskID:       rt.id,
			Status:       st,
			EffectiveDir: rt.effectiveDir,
			StartedAt:    rt.startedAt,
		})
	}
	return out
}

// ActiveTaskIDs returns IDs of currently running tasks for heartbeats.
func (r *Runner) ActiveTaskIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.tasks))
	for id := range r.tasks {
		ids = append(ids, id)
	}
	return ids
}

// StartTask validates inputs, sets up optional git worktree, spawns the agent PTY, and runs until completion.
func (r *Runner) StartTask(parentCtx context.Context, req protocol.TaskStartPayload) error {
	// Enforce permission downgrade: sandboxed worker refuses full_access escalation.
	permMode := req.PermissionMode
	if permMode == "" {
		if r.sandbox {
			permMode = protocol.PermissionSandboxed
		} else {
			permMode = protocol.PermissionFullAccess
		}
	}
	if r.sandbox && permMode == protocol.PermissionFullAccess {
		r.failTask(req.TaskID, "", "worker is in sandboxed mode and refuses full_access tasks")
		return fmt.Errorf("sandboxed worker refuses full_access task %s", req.TaskID)
	}

	if !r.AgentAllowed(req.Agent) {
		r.failTask(req.TaskID, "", "worker is in sandboxed mode and refuses the unrestricted shell driver (start konsertd with --sandbox-allow-shell to opt in)")
		return fmt.Errorf("sandboxed worker refuses shell task %s", req.TaskID)
	}

	drv, ok := r.registry.Get(req.Agent)
	if !ok {
		r.failTask(req.TaskID, "", fmt.Sprintf("unknown agent driver %q", req.Agent))
		return fmt.Errorf("unknown agent driver %q", req.Agent)
	}

	effectiveDir, err := gitutil.ValidateWorkdir(req.Workdir, r.allowedRoots)
	if err != nil {
		r.failTask(req.TaskID, "", fmt.Sprintf("workdir validation failed: %v", err))
		return err
	}

	var baseSHA string
	if repoRoot, isGit := gitutil.IsGitRepo(parentCtx, effectiveDir); isGit {
		if req.Branch != "" && req.UseWorktree {
			wtPath, sha, wtErr := gitutil.CreateWorktree(parentCtx, repoRoot, req.TaskID, req.Branch)
			if wtErr != nil {
				r.failTask(req.TaskID, effectiveDir, fmt.Sprintf("git worktree setup failed: %v", wtErr))
				return wtErr
			}
			effectiveDir = wtPath
			baseSHA = sha
		} else {
			baseSHA, _ = gitutil.CurrentSHA(parentCtx, effectiveDir)
		}
	}

	spec := drv.BuildCommand(drivers.LaunchOptions{
		TaskID:         req.TaskID,
		Prompt:         req.Prompt,
		Workdir:        effectiveDir,
		Model:          req.Model,
		Mode:           req.Mode,
		PermissionMode: permMode,
	})

	taskCtx, cancel := context.WithCancel(parentCtx)
	if req.TimeoutSeconds > 0 {
		taskCtx, cancel = context.WithTimeout(parentCtx, time.Duration(req.TimeoutSeconds)*time.Second)
	}

	proc, err := StartPty(taskCtx, PtyStartOptions{
		Binary: spec.Binary,
		Args:   spec.Args,
		Dir:    effectiveDir,
		Env:    spec.Env,
		Cols:   req.Cols,
		Rows:   req.Rows,
	})
	if err != nil {
		cancel()
		r.failTask(req.TaskID, effectiveDir, fmt.Sprintf("failed to launch %s: %v", spec.Binary, err))
		return err
	}

	rt := &runningTask{
		id:           req.TaskID,
		agent:        req.Agent,
		effectiveDir: effectiveDir,
		baseSHA:      baseSHA,
		startedAt:    time.Now().UTC(),
		pty:          proc,
		ring:         NewRingBuffer(DefaultRingBufferSize),
		cancelFunc:   cancel,
		status:       protocol.TaskStatusRunning,
	}

	r.mu.Lock()
	r.tasks[req.TaskID] = rt
	r.mu.Unlock()

	r.emitStatus(protocol.TaskStatusPayload{
		TaskID:       req.TaskID,
		Status:       protocol.TaskStatusRunning,
		EffectiveDir: effectiveDir,
	})

	go r.superviseTask(taskCtx, rt, drv)
	return nil
}

func (r *Runner) superviseTask(ctx context.Context, rt *runningTask, drv drivers.AgentDriver) {
	defer func() {
		rt.cancelFunc()
		_ = rt.pty.Close()
		r.mu.Lock()
		delete(r.tasks, rt.id)
		r.mu.Unlock()
	}()

	// Watch context cancellation / timeout -> terminate process group.
	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = rt.pty.Terminate()
		case <-stopWatch:
		}
	}()

	// Tee PTY output into ring buffer, optional live stream, and line channel for telemetry scraper.
	lineCh := make(chan string, 128)
	telemDone := make(chan struct{})
	telemCtx, telemCancel := context.WithCancel(context.Background())

	go func() {
		defer close(telemDone)
		drv.WatchTelemetry(telemCtx, drivers.WatchOptions{
			TaskID:       rt.id,
			Workdir:      rt.effectiveDir,
			OutputLines:  lineCh,
			RingSnapshot: rt.ring.Snapshot,
		}, func(ev drivers.TelemetryEvent) {
			rt.mu.Lock()
			rt.events = append(rt.events, ev)
			if ev.Kind == "waiting_input" && rt.status == protocol.TaskStatusRunning {
				rt.status = protocol.TaskStatusWaitingInput
			}
			rt.mu.Unlock()

			if ev.Kind == "waiting_input" {
				r.emitStatus(protocol.TaskStatusPayload{
					TaskID:       rt.id,
					Status:       protocol.TaskStatusWaitingInput,
					EffectiveDir: rt.effectiveDir,
				})
			}

			if r.callbacks.OnTaskEvent != nil {
				r.callbacks.OnTaskEvent(protocol.TaskEventPayload{
					TaskID:  rt.id,
					Kind:    ev.Kind,
					Message: ev.Message,
					Meta:    ev.Meta,
				})
			}
		})
	}()

	// Read PTY bytes until EOF.
	pr, pw := io.Pipe()
	go func() {
		defer close(lineCh)
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			select {
			case lineCh <- scanner.Text():
			default:
				// Drop noisy line rather than blocking PTY reads.
			}
		}
	}()

	buf := make([]byte, 16*1024)
	for {
		n, err := rt.pty.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			_, _ = rt.ring.Write(chunk)
			_, _ = pw.Write(chunk)
			if rt.streamActive.Load() && r.callbacks.OnPtyFrame != nil {
				r.callbacks.OnPtyFrame(protocol.FramePtyData, rt.id, chunk)
			}
		}
		if err != nil {
			break
		}
	}
	_ = pw.Close()

	exitCode, waitErr := rt.pty.Wait()
	close(stopWatch)

	// Allow telemetry watcher a brief window to flush remaining lines.
	select {
	case <-telemDone:
	case <-time.After(300 * time.Millisecond):
	}
	telemCancel()
	<-telemDone

	rt.mu.Lock()
	wasCanceled := rt.canceled
	eventsCopy := make([]drivers.TelemetryEvent, len(rt.events))
	copy(eventsCopy, rt.events)
	rt.mu.Unlock()

	finalStatus := protocol.TaskStatusCompleted
	if wasCanceled {
		finalStatus = protocol.TaskStatusCanceled
	} else if ctx.Err() == context.DeadlineExceeded {
		finalStatus = protocol.TaskStatusFailed
	} else if waitErr != nil || exitCode != 0 {
		finalStatus = protocol.TaskStatusFailed
	}

	summary := drv.ExtractSummary(context.Background(), drivers.SummaryOptions{
		TaskID:       rt.id,
		Workdir:      rt.effectiveDir,
		ExitCode:     exitCode,
		RingSnapshot: rt.ring.Snapshot,
		Events:       eventsCopy,
	})
	if ctx.Err() == context.DeadlineExceeded {
		summary = "task timed out; " + summary
	}

	gitSummary, _ := gitutil.CaptureSummary(context.Background(), rt.effectiveDir, rt.baseSHA)

	if r.callbacks.OnTaskEvent != nil {
		r.callbacks.OnTaskEvent(protocol.TaskEventPayload{
			TaskID:  rt.id,
			Kind:    "exit",
			Message: fmt.Sprintf("Process exited with code %d (%s)", exitCode, finalStatus),
			Meta:    map[string]any{"exit_code": exitCode, "status": finalStatus},
		})
	}

	ec := exitCode
	r.emitStatus(protocol.TaskStatusPayload{
		TaskID:       rt.id,
		Status:       finalStatus,
		EffectiveDir: rt.effectiveDir,
		ExitCode:     &ec,
		Summary:      summary,
		GitSummary:   gitSummary,
	})
}

// SubscribePty turns on live streaming for taskID and immediately emits a 0x01 snapshot frame.
func (r *Runner) SubscribePty(taskID string) bool {
	r.mu.RLock()
	rt, ok := r.tasks[taskID]
	r.mu.RUnlock()
	if !ok {
		return false
	}

	snap := rt.ring.Snapshot()
	if r.callbacks.OnPtyFrame != nil {
		r.callbacks.OnPtyFrame(protocol.FramePtySnapshot, taskID, snap)
	}
	rt.streamActive.Store(true)
	return true
}

// UnsubscribePty stops live PTY byte streaming while keeping the local ring buffer updated.
func (r *Runner) UnsubscribePty(taskID string) {
	r.mu.RLock()
	rt, ok := r.tasks[taskID]
	r.mu.RUnlock()
	if ok {
		rt.streamActive.Store(false)
	}
}

// WriteStdin writes interactive bytes to a running task's PTY.
func (r *Runner) WriteStdin(taskID string, data []byte) error {
	r.mu.RLock()
	rt, ok := r.tasks[taskID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("task %s not running", taskID)
	}
	if _, err := rt.pty.Write(data); err != nil {
		return err
	}
	rt.mu.Lock()
	wasWaiting := rt.status == protocol.TaskStatusWaitingInput
	if wasWaiting {
		rt.status = protocol.TaskStatusRunning
	}
	rt.mu.Unlock()
	if wasWaiting {
		r.emitStatus(protocol.TaskStatusPayload{
			TaskID:       rt.id,
			Status:       protocol.TaskStatusRunning,
			EffectiveDir: rt.effectiveDir,
		})
	}
	return nil
}

// SteerTask sends steering input (with optional trailing newline) into a running task's PTY.
func (r *Runner) SteerTask(taskID, inputText string, appendNewline bool) error {
	payload := inputText
	if appendNewline && !strings.HasSuffix(payload, "\n") && !strings.HasSuffix(payload, "\r") {
		payload += "\n"
	}
	return r.WriteStdin(taskID, []byte(payload))
}

// ResizePty updates the PTY window dimensions for taskID.
func (r *Runner) ResizePty(taskID string, cols, rows uint16) error {
	r.mu.RLock()
	rt, ok := r.tasks[taskID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("task %s not running", taskID)
	}
	return rt.pty.Resize(cols, rows)
}

// CancelTask cancels a running task with process-group signal escalation.
func (r *Runner) CancelTask(taskID string) error {
	r.mu.RLock()
	rt, ok := r.tasks[taskID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("task %s not running", taskID)
	}
	rt.mu.Lock()
	rt.canceled = true
	rt.mu.Unlock()
	rt.cancelFunc()
	return nil
}

// ShutdownAll cancels all active tasks cleanly on daemon shutdown.
func (r *Runner) ShutdownAll() {
	r.mu.RLock()
	list := make([]*runningTask, 0, len(r.tasks))
	for _, rt := range r.tasks {
		list = append(list, rt)
	}
	r.mu.RUnlock()
	for _, rt := range list {
		rt.mu.Lock()
		rt.canceled = true
		rt.mu.Unlock()
		rt.cancelFunc()
	}
}

func (r *Runner) failTask(taskID, effectiveDir, reason string) {
	code := -1
	r.emitStatus(protocol.TaskStatusPayload{
		TaskID:       taskID,
		Status:       protocol.TaskStatusFailed,
		EffectiveDir: effectiveDir,
		ExitCode:     &code,
		Summary:      reason,
	})
}

func (r *Runner) emitStatus(p protocol.TaskStatusPayload) {
	if r.callbacks.OnTaskStatus != nil {
		r.callbacks.OnTaskStatus(p)
	}
}
