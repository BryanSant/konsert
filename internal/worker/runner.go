package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BryanSant/konsert/internal/gitutil"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/chat"
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

	mu       sync.RWMutex
	tasks    map[string]*runningTask
	starting map[string]*pendingStart
	accounts chat.AccountResolver // agent accounts for TaskStartPayload.AccountID
}

// SetAccounts sets the agent account resolver used for tasks with an AccountID.
func (r *Runner) SetAccounts(a chat.AccountResolver) {
	r.mu.Lock()
	r.accounts = a
	r.mu.Unlock()
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
		starting:     make(map[string]*pendingStart),
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

// IsManagedPID reports whether pid is the main process of a task this runner started.
func (r *Runner) IsManagedPID(pid int) bool {
	if pid <= 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rt := range r.tasks {
		if rt.pty != nil && rt.pty.Pid() == pid {
			return true
		}
	}
	return false
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

// ErrDuplicateTask is returned when a task.start arrives for an ID that is already starting
// or running on this worker. The existing task is left untouched.
var ErrDuplicateTask = errors.New("task is already starting or running on this worker")

// GitSetupTimeout bounds repository inspection and worktree creation for one task.start.
var GitSetupTimeout = 2 * time.Minute

// createWorktreeFn is gitutil.CreateWorktreeInfo; tests replace it with a slow stub.
var createWorktreeFn = gitutil.CreateWorktreeInfo

// pendingStart tracks a task between task.start and its PTY being registered, so that a
// duplicate start is refused and a cancel that arrives during setup is honoured.
type pendingStart struct {
	ctx       context.Context // bounded by GitSetupTimeout
	cancel    context.CancelFunc
	canceled  bool // guarded by Runner.mu
	subscribe bool // a pty.subscribe arrived during setup; guarded by Runner.mu
}

// beginStart validates the task ID and claims it for a new start. It is cheap and must run
// in message order (on the read loop) so a following cancel or subscribe finds the task.
func (r *Runner) beginStart(parentCtx context.Context, req protocol.TaskStartPayload) (*pendingStart, error) {
	if err := protocol.ValidateTaskID(req.TaskID); err != nil {
		r.failTask(req.TaskID, "", fmt.Sprintf("invalid task id: %v", err))
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, running := r.tasks[req.TaskID]
	_, starting := r.starting[req.TaskID]
	if running || starting {
		// Never emit a status here: it would be attributed to the task that is already running.
		return nil, fmt.Errorf("task %s: %w", req.TaskID, ErrDuplicateTask)
	}
	ctx, cancel := context.WithTimeout(parentCtx, GitSetupTimeout)
	ps := &pendingStart{ctx: ctx, cancel: cancel}
	r.starting[req.TaskID] = ps
	return ps, nil
}

// StartTask validates inputs, sets up optional git worktree, spawns the agent PTY, and runs until completion.
// It blocks for the duration of git setup; network read loops should use StartTaskAsync.
func (r *Runner) StartTask(parentCtx context.Context, req protocol.TaskStartPayload) error {
	ps, err := r.beginStart(parentCtx, req)
	if err != nil {
		return err
	}
	return r.finishStart(parentCtx, req, ps)
}

// StartTaskAsync validates and reserves the task synchronously, then performs the slow part
// (git worktree creation, PTY spawn) in a goroutine. Setup failures are reported through
// task.status as usual and additionally passed to onErr.
func (r *Runner) StartTaskAsync(parentCtx context.Context, req protocol.TaskStartPayload, onErr func(error)) error {
	ps, err := r.beginStart(parentCtx, req)
	if err != nil {
		return err
	}
	go func() {
		if err := r.finishStart(parentCtx, req, ps); err != nil && onErr != nil {
			onErr(err)
		}
	}()
	return nil
}

func (r *Runner) finishStart(parentCtx context.Context, req protocol.TaskStartPayload, ps *pendingStart) error {
	setupCtx := ps.ctx
	defer ps.cancel()
	registered := false
	defer func() {
		r.mu.Lock()
		if r.starting[req.TaskID] == ps {
			delete(r.starting, req.TaskID)
		}
		r.mu.Unlock()
	}()

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

	if r.sandbox && drivers.ApprovalModeFullAccess(req.Agent, req.ApprovalMode) {
		r.failTask(req.TaskID, "", fmt.Sprintf("worker is in sandboxed mode and refuses the full-access approval mode %q", req.ApprovalMode))
		return fmt.Errorf("sandboxed worker refuses approval mode %q for task %s", req.ApprovalMode, req.TaskID)
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

	var (
		baseSHA string
		wt      gitutil.WorktreeInfo
	)
	if repoRoot, isGit := gitutil.IsGitRepo(setupCtx, effectiveDir); isGit {
		if req.Branch != "" && req.UseWorktree {
			info, wtErr := createWorktreeFn(setupCtx, repoRoot, req.TaskID, req.Branch)
			if wtErr != nil {
				r.failTask(req.TaskID, effectiveDir, fmt.Sprintf("git worktree setup failed: %v", wtErr))
				return wtErr
			}
			wt = info
			effectiveDir = info.Path
			baseSHA = info.BaseSHA
		} else {
			baseSHA, _ = gitutil.CurrentSHA(setupCtx, effectiveDir)
		}
	}
	// undoWorktree removes a worktree created above when the task never gets to run.
	undoWorktree := func() {
		if wt.Path == "" || registered {
			return
		}
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		if err := gitutil.RemoveTaskWorktree(cctx, wt); err != nil {
			r.logger.Warn("failed to remove worktree after launch failure", "task_id", req.TaskID, "path", wt.Path, "err", err)
		}
	}

	r.mu.Lock()
	canceled := ps.canceled
	r.mu.Unlock()
	if canceled || setupCtx.Err() != nil {
		undoWorktree()
		status := protocol.TaskStatusCanceled
		reason := "task canceled during setup"
		if !canceled {
			status = protocol.TaskStatusFailed
			reason = fmt.Sprintf("task setup aborted: %v", setupCtx.Err())
		}
		code := -1
		r.emitStatus(protocol.TaskStatusPayload{TaskID: req.TaskID, Status: status, ExitCode: &code, Summary: reason})
		return fmt.Errorf("task %s: %s", req.TaskID, reason)
	}

	// The selected account's env (config dir, token) is merged into the PTY process env;
	// drivers also see it, so they read that account's session logs.
	var acctEnv []string
	if req.AccountID != "" {
		r.mu.RLock()
		accounts := r.accounts
		r.mu.RUnlock()
		if accounts == nil {
			undoWorktree()
			r.failTask(req.TaskID, effectiveDir, "agent accounts are not available on this worker")
			return fmt.Errorf("task %s: no account registry", req.TaskID)
		}
		acct, aErr := accounts.Resolve(req.Agent, req.AccountID)
		if aErr != nil {
			undoWorktree()
			r.failTask(req.TaskID, effectiveDir, fmt.Sprintf("agent account %q: %v", req.AccountID, aErr))
			return aErr
		}
		acctEnv = chat.EnvList(acct)
	}

	spec := drv.BuildCommand(drivers.LaunchOptions{
		TaskID:         req.TaskID,
		Prompt:         req.Prompt,
		Workdir:        effectiveDir,
		Model:          req.Model,
		Mode:           req.Mode,
		PermissionMode: permMode,
		Env:            acctEnv,
		AccountID:      req.AccountID,
		ApprovalMode:   req.ApprovalMode,
		Effort:         req.Effort,
	})

	// Exactly one task context: with a deadline when the task has a timeout.
	var (
		taskCtx context.Context
		cancel  context.CancelFunc
	)
	if req.TimeoutSeconds > 0 {
		taskCtx, cancel = context.WithTimeout(parentCtx, time.Duration(req.TimeoutSeconds)*time.Second)
	} else {
		taskCtx, cancel = context.WithCancel(parentCtx)
	}

	env := append(append([]string(nil), spec.Env...), ManagedTaskEnv+"="+req.TaskID)
	proc, err := StartPty(taskCtx, PtyStartOptions{
		Binary: spec.Binary,
		Args:   spec.Args,
		Dir:    effectiveDir,
		Env:    env,
		Cols:   req.Cols,
		Rows:   req.Rows,
	})
	if err != nil {
		cancel()
		undoWorktree()
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
	delete(r.starting, req.TaskID)
	registered = true
	wantSubscribe := ps.subscribe
	canceledLate := ps.canceled
	r.mu.Unlock()

	r.emitStatus(protocol.TaskStatusPayload{
		TaskID:       req.TaskID,
		Status:       protocol.TaskStatusRunning,
		EffectiveDir: effectiveDir,
	})
	if wantSubscribe {
		r.SubscribePty(req.TaskID)
	}
	if canceledLate {
		// A cancel raced with the final registration step.
		_ = r.CancelTask(req.TaskID)
	}

	go r.superviseTask(taskCtx, rt, drv)
	return nil
}

func (r *Runner) superviseTask(ctx context.Context, rt *runningTask, drv drivers.AgentDriver) {
	defer func() {
		rt.cancelFunc()
		_ = rt.pty.Close()
		r.mu.Lock()
		// Only remove our own entry: never a newer task that reuses the ID.
		if r.tasks[rt.id] == rt {
			delete(r.tasks, rt.id)
		}
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

	// Wait for the main process in its own goroutine. Completion is driven by process exit,
	// not PTY EOF: a background child (e.g. `nohup devserver &`) can hold the TTY open
	// forever, and ConPTY output often never reaches EOF until ClosePseudoConsole.
	var (
		exitCode int
		waitErr  error
	)
	waitDone := make(chan struct{})
	go func() {
		exitCode, waitErr = rt.pty.Wait()
		close(waitDone)
	}()

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer close(lineCh)
		r.pumpPtyOutput(rt, lineCh)
	}()

	select {
	case <-readDone:
		<-waitDone
	case <-waitDone:
		// Drain whatever output the process left behind, then close the PTY so a
		// lingering reader (or an un-EOF'd ConPTY) cannot keep the task alive.
		select {
		case <-readDone:
		case <-time.After(ptyDrainGrace):
			_ = rt.pty.Close()
			select {
			case <-readDone:
			case <-time.After(2 * time.Second):
				r.logger.Warn("pty reader did not stop after close; abandoning it", "task_id", rt.id)
			}
		}
	}
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

// ptyDrainGrace is how long the supervisor keeps reading PTY output after the main process
// exits before it closes the PTY to unblock the reader.
const ptyDrainGrace = 500 * time.Millisecond

// maxTelemetryLine caps the bytes buffered for a single unterminated output line.
const maxTelemetryLine = 1024 * 1024

// pumpPtyOutput reads PTY bytes until EOF/close, teeing them into the ring buffer, the live
// stream, and (line-split, non-blocking) the telemetry channel. It never blocks on telemetry:
// an over-long line is truncated to maxTelemetryLine and the remainder discarded.
func (r *Runner) pumpPtyOutput(rt *runningTask, lineCh chan<- string) {
	ls := lineSplitter{max: maxTelemetryLine}
	emit := func(line string) {
		select {
		case lineCh <- line:
		default:
			// Drop noisy line rather than blocking PTY reads.
		}
	}
	buf := make([]byte, 16*1024)
	for {
		n, err := rt.pty.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			_, _ = rt.ring.Write(chunk)
			ls.feed(chunk, emit)
			if rt.streamActive.Load() && r.callbacks.OnPtyFrame != nil {
				r.callbacks.OnPtyFrame(protocol.FramePtyData, rt.id, chunk)
			}
		}
		if err != nil {
			ls.flush(emit)
			return
		}
	}
}

// lineSplitter turns a byte stream into lines (without the trailing "\n" or "\r\n"),
// bounding the memory held for a line that has no newline.
type lineSplitter struct {
	max       int
	pending   []byte
	truncated bool
}

func (l *lineSplitter) feed(chunk []byte, emit func(string)) {
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			l.appendBounded(chunk, emit)
			return
		}
		l.appendBounded(chunk[:i], emit)
		l.endLine(emit)
		chunk = chunk[i+1:]
	}
}

func (l *lineSplitter) appendBounded(b []byte, emit func(string)) {
	if l.truncated {
		return // discarding the rest of an over-long line
	}
	if room := l.max - len(l.pending); len(b) > room {
		l.pending = append(l.pending, b[:room]...)
		emit(string(l.pending))
		l.pending = l.pending[:0]
		l.truncated = true
		return
	}
	l.pending = append(l.pending, b...)
}

func (l *lineSplitter) endLine(emit func(string)) {
	if !l.truncated {
		emit(string(bytes.TrimSuffix(l.pending, []byte{'\r'})))
	}
	l.pending = l.pending[:0]
	l.truncated = false
}

func (l *lineSplitter) flush(emit func(string)) {
	if len(l.pending) > 0 && !l.truncated {
		emit(string(bytes.TrimSuffix(l.pending, []byte{'\r'})))
	}
	l.pending = nil
	l.truncated = false
}

// SubscribePty turns on live streaming for taskID and immediately emits a 0x01 snapshot frame.
func (r *Runner) SubscribePty(taskID string) bool {
	r.mu.Lock()
	rt, ok := r.tasks[taskID]
	if !ok {
		// Remember a subscribe for a task still in setup; it is applied once the PTY exists.
		if ps, starting := r.starting[taskID]; starting {
			ps.subscribe = true
			r.mu.Unlock()
			return true
		}
	}
	r.mu.Unlock()
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if rt, ok := r.tasks[taskID]; ok {
		rt.streamActive.Store(false)
	} else if ps, starting := r.starting[taskID]; starting {
		ps.subscribe = false
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
	return r.WriteStdin(taskID, []byte(steerPayload(inputText, appendNewline, runtime.GOOS)))
}

// steerPayload appends the line terminator that submits input on the given OS.
// ConPTY-hosted programs submit on CR; a bare LF only moves the cursor.
func steerPayload(inputText string, appendNewline bool, goos string) string {
	if !appendNewline || strings.HasSuffix(inputText, "\n") || strings.HasSuffix(inputText, "\r") {
		return inputText
	}
	if goos == "windows" {
		return inputText + "\r"
	}
	return inputText + "\n"
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

// CancelTask cancels a running task with process-group signal escalation. A task that is
// still in setup (git worktree, PTY spawn) is aborted before it launches.
func (r *Runner) CancelTask(taskID string) error {
	r.mu.Lock()
	rt, ok := r.tasks[taskID]
	if !ok {
		if ps, starting := r.starting[taskID]; starting {
			ps.canceled = true
			ps.cancel()
			r.mu.Unlock()
			return nil
		}
	}
	r.mu.Unlock()
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
	r.mu.Lock()
	for _, ps := range r.starting {
		ps.canceled = true
		ps.cancel()
	}
	r.mu.Unlock()
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
