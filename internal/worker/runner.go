package worker

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BryanSant/syre/internal/gitutil"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/chat"
	"github.com/BryanSant/syre/internal/worker/digest"
	"github.com/BryanSant/syre/internal/worker/drivers"
	"github.com/BryanSant/syre/internal/worker/guard"
	"github.com/BryanSant/syre/internal/worker/proctree"
	"github.com/BryanSant/syre/internal/worker/shellint"
)

// RunnerCallbacks delivers worker task events, status updates, and PTY frames back to the daemon transport.
type RunnerCallbacks struct {
	OnTaskStatus func(payload protocol.TaskStatusPayload)
	OnTaskEvent  func(payload protocol.TaskEventPayload)
	OnPtyFrame   func(frameType byte, taskID string, data []byte)
	// OnOperatorCommand reports a command the user ran in a Syre terminal (shell
	// integration): once when it starts and once when it ends.
	OnOperatorCommand func(payload protocol.OperatorCommandPayload)
}

type runningTask struct {
	id               string
	ticketSID        string
	agent            string
	model            string
	effectiveDir     string
	worktrees        []protocol.TaskWorktree
	worktreeBaseSHAs map[string]string
	baseSHA          string
	startedAt        time.Time
	pty              PtyProcess
	ring             *RingBuffer
	// scroll keeps the terminal output on disk for a redraw after the task ended or
	// syred restarted (nil without a scrollback directory).
	scroll       *scrollback
	streamActive atomic.Bool
	cancelFunc   context.CancelFunc
	// deadline is the task timeout; it does not run while the task is suspended.
	deadline *taskDeadline

	// Shell integration (Syre terminals): marks are parsed out of the PTY stream.
	shellParser  *shellint.Parser
	shellTracker *shellint.Tracker

	mu        sync.Mutex
	status    string
	canceled  bool
	stopGrace time.Duration
	// hangup: the user closed this terminal (TaskCancelPayload.Hangup).
	hangup bool
	events []drivers.TelemetryEvent
	// suspended is set while the process tree is frozen; prevStatus is restored on resume.
	suspended  bool
	prevStatus string
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

	// commands keeps full terminal command lines in memory for shellint.HistoryTTL.
	commands *shellint.History

	// scrollDir holds each PTY task's on-disk scrollback ("": none; scrollback.go).
	scrollDir string
	// machineName is the human-readable name of this worker machine (used in RunDigest).
	machineName string
}

// SetMachineName sets the worker machine name included in post-run digests.
func (r *Runner) SetMachineName(name string) {
	r.mu.Lock()
	r.machineName = strings.TrimSpace(name)
	r.mu.Unlock()
}

func (r *Runner) getMachineName() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.machineName
}

// SetScrollbackDir keeps PTY tasks' terminal output in dir (call before tasks start).
func (r *Runner) SetScrollbackDir(dir string) {
	r.mu.Lock()
	r.scrollDir = dir
	r.mu.Unlock()
	go pruneScrollback(dir, scrollbackKeep)
}

// CommandHistory returns the in-memory history of commands run in Syre terminals
// (full, unredacted text; expires after shellint.HistoryTTL; never written to disk).
func (r *Runner) CommandHistory() *shellint.History { return r.commands }

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
		commands:     shellint.NewHistory(),
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

// hostedTaskCount is tasks reserved to start plus non-terminal running tasks.
// A task that is in both maps counts once.
func (r *Runner) hostedTaskCount() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make(map[string]struct{}, len(r.starting)+len(r.tasks))
	for id := range r.starting {
		ids[id] = struct{}{}
	}
	for id, rt := range r.tasks {
		rt.mu.Lock()
		st := rt.status
		rt.mu.Unlock()
		if !protocol.IsTerminalTaskStatus(st) {
			ids[id] = struct{}{}
		}
	}
	return len(ids)
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

// SessionPIDs returns the agent process id of every running agent task (task id → pid);
// scratch terminals (the shell driver) are not agent sessions.
func (r *Runner) SessionPIDs() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int, len(r.tasks))
	for id, rt := range r.tasks {
		if rt.pty == nil || rt.agent == drivers.ShellDriverName {
			continue
		}
		if pid := rt.pty.Pid(); pid > 0 {
			out[id] = pid
		}
	}
	return out
}

// TerminalShell is a user terminal's shell for its idle/working state.
type TerminalShell struct {
	PID int
	// Integrated: shell integration marks arrive, so command events say when it is
	// busy; otherwise the process table does.
	Integrated bool
}

// TerminalShells maps each running user terminal (the shell driver) to its shell.
func (r *Runner) TerminalShells() map[string]TerminalShell {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]TerminalShell{}
	for id, rt := range r.tasks {
		if rt.pty == nil || rt.agent != drivers.ShellDriverName {
			continue
		}
		if pid := rt.pty.Pid(); pid > 0 {
			out[id] = TerminalShell{PID: pid, Integrated: rt.shellTracker != nil && rt.shellTracker.Integrated()}
		}
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
	if req.SID != "" {
		if err := protocol.ValidateTaskID(req.SID); err != nil {
			r.failTask(req.TaskID, "", fmt.Sprintf("invalid task sid: %v", err))
			return nil, err
		}
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
		r.failTask(req.TaskID, "", "worker is in sandboxed mode and refuses the unrestricted shell driver (start syred with --sandbox-allow-shell to opt in)")
		return fmt.Errorf("sandboxed worker refuses shell task %s", req.TaskID)
	}

	drv, ok := r.registry.Get(req.Agent)
	if !ok {
		r.failTask(req.TaskID, "", fmt.Sprintf("unknown agent driver %q", req.Agent))
		return fmt.Errorf("unknown agent driver %q", req.Agent)
	}

	var (
		effectiveDir     string
		baseSHA          string
		createdWTs       []gitutil.WorktreeInfo
		taskWorktrees    []protocol.TaskWorktree
		worktreeBaseSHAs map[string]string
		provisioned      *provisionedBindings
		err              error
	)

	// undoWorktrees removes any worktrees created during setup if the task never gets to run.
	undoWorktree := func() {
		if registered {
			return
		}
		if provisioned != nil {
			provisioned.undo(r.logger, req.TaskID)
			return
		}
		if len(createdWTs) == 0 {
			return
		}
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		for _, info := range createdWTs {
			if rmErr := gitutil.RemoveTaskWorktree(cctx, info); rmErr != nil {
				r.logger.Warn("failed to remove worktree after launch failure", "task_id", req.TaskID, "path", info.Path, "err", rmErr)
			}
		}
	}

	if len(req.Bindings) > 0 {
		ticketDirName := cmp.Or(strings.TrimSpace(req.TicketSID), strings.TrimSpace(req.SID), req.TaskID)
		provisioned, err = provisionBindings(setupCtx, r.allowedRoots, req.Workdir, ticketDirName, req.Bindings, false, r.logger)
		if err != nil {
			r.failTask(req.TaskID, "", err.Error())
			return err
		}
		effectiveDir = provisioned.workdir
		taskWorktrees = provisioned.worktrees
		worktreeBaseSHAs = provisioned.worktreeBaseSHAs
		baseSHA = provisioned.baseSHA
	} else {
		if req.CreateWorkdir {
			if err := gitutil.EnsureHomeWorkdir(req.Workdir, r.allowedRoots); err != nil {
				r.failTask(req.TaskID, "", fmt.Sprintf("could not create %s: %v", req.Workdir, err))
				return err
			}
		}
		effectiveDir, err = gitutil.ValidateWorkdir(req.Workdir, r.allowedRoots)
		if err != nil {
			r.failTask(req.TaskID, "", fmt.Sprintf("workdir validation failed: %v", err))
			return err
		}
		if repoRoot, isGit := gitutil.IsGitRepo(setupCtx, effectiveDir); isGit {
			if req.Branch != "" && req.UseWorktree {
				// The directory is user-visible, so it is named after the short sid when known.
				info, wtErr := createWorktreeFn(setupCtx, repoRoot, cmp.Or(req.SID, req.TaskID), req.Branch)
				if wtErr != nil {
					r.failTask(req.TaskID, effectiveDir, fmt.Sprintf("git worktree setup failed: %v", wtErr))
					return wtErr
				}
				createdWTs = append(createdWTs, info)
				effectiveDir = info.Path
				baseSHA = info.BaseSHA
			} else {
				baseSHA, _ = gitutil.CurrentSHA(setupCtx, effectiveDir)
			}
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
		// A shell task may borrow any agent account's profile: that is how the UI's
		// "Open terminal and sign in" runs e.g. `codex login` with the account's env.
		agent := req.Agent
		if agent == "shell" {
			agent = ""
		}
		acct, aErr := accounts.Resolve(agent, req.AccountID)
		if aErr != nil {
			undoWorktree()
			r.failTask(req.TaskID, effectiveDir, fmt.Sprintf("agent account %q: %v", req.AccountID, aErr))
			return aErr
		}
		acctEnv = chat.EnvList(acct)
	}

	// Syre terminals (interactive shell tasks) get shell integration so the daemon
	// learns each command the user runs. Failing to prepare it only loses the events.
	var integ *shellint.Integration
	if req.Agent == drivers.ShellDriverName && req.Mode != protocol.TaskModeOneshot {
		base := cmp.Or(defaultStateDir(), os.TempDir())
		if dir, err := shellint.Prepare(filepath.Join(base, "shell-integration")); err != nil {
			r.logger.Warn("shell integration unavailable", "task_id", req.TaskID, "err", err)
		} else {
			integ = &shellint.Integration{Dir: dir, Nonce: shellint.NewNonce()}
		}
	}

	spec := drv.BuildCommand(drivers.LaunchOptions{
		TaskID:           req.TaskID,
		Prompt:           req.Prompt,
		Workdir:          effectiveDir,
		Model:            req.Model,
		Mode:             req.Mode,
		PermissionMode:   permMode,
		Env:              acctEnv,
		AccountID:        req.AccountID,
		ApprovalMode:     req.ApprovalMode,
		Effort:           req.Effort,
		Shell:            req.Shell,
		UseGitBash:       req.UseGitBash,
		ShellIntegration: integ,
	})

	// Exactly one task context. The timeout is a pausable deadline (it stops while the
	// user has the task suspended) that cancels the context with errTaskTimeout.
	taskCtx, cancelCause := context.WithCancelCause(parentCtx)
	cancel := func() { cancelCause(context.Canceled) }
	var deadline *taskDeadline
	if req.TimeoutSeconds > 0 {
		deadline = newTaskDeadline(time.Duration(req.TimeoutSeconds)*time.Second, func() { cancelCause(errTaskTimeout) })
	}

	env := append(append([]string(nil), spec.Env...), ManagedTaskEnv+"="+req.TaskID)
	gspec := &guard.LaunchSpec{Owner: req.TaskID, Kind: "task", Priority: guard.PriorityTask}
	if req.TicketSID != "" {
		gspec.Priority, gspec.Headless = guard.PriorityTicket, true
	}
	proc, err := StartPty(taskCtx, PtyStartOptions{
		Binary: spec.Binary,
		Args:   spec.Args,
		Dir:    effectiveDir,
		Env:    env,
		Cols:   req.Cols,
		Rows:   req.Rows,
		Guard:  gspec,
	})
	if err != nil {
		cancel()
		deadline.stop()
		undoWorktree()
		r.failTask(req.TaskID, effectiveDir, fmt.Sprintf("failed to launch %s: %v", spec.Binary, err))
		return err
	}

	rt := &runningTask{
		id:               req.TaskID,
		ticketSID:        req.TicketSID,
		agent:            req.Agent,
		model:            req.Model,
		effectiveDir:     effectiveDir,
		worktrees:        taskWorktrees,
		worktreeBaseSHAs: worktreeBaseSHAs,
		baseSHA:          baseSHA,
		startedAt:        time.Now().UTC(),
		pty:              proc,
		ring:             NewRingBuffer(DefaultRingBufferSize),
		scroll:           openScrollback(r.scrollbackDir(), req.TaskID),
		cancelFunc:       cancel,
		deadline:         deadline,
		status:           protocol.TaskStatusRunning,
	}
	if spec.ShellIntegration && integ != nil {
		rt.shellParser = &shellint.Parser{}
		rt.shellTracker = shellint.NewTracker(req.TaskID, integ.Nonce, r.commands, r.callbacks.OnOperatorCommand)
		if runtime.GOOS == "windows" && shellint.Kind(spec.Binary) == "bash" {
			rt.shellTracker.SetTranslateMSYS(true)
		}
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
		Worktrees:    taskWorktrees,
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
		rt.deadline.stop()
		_ = rt.pty.Close()
		rt.scroll.Close()
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
			rt.mu.Lock()
			grace, hangup := rt.stopGrace, rt.hangup
			rt.mu.Unlock()
			if hangup {
				_ = rt.pty.Hangup(terminalHangupGrace)
			} else if grace > 0 {
				_ = rt.pty.TerminateWithGrace(grace)
			} else {
				_ = rt.pty.Terminate()
			}
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
				needDigest := digest.Build(digest.Input{
					TicketSID:  rt.ticketSID,
					TaskStatus: protocol.TaskStatusWaitingInput,
					Machine:    r.getMachineName(),
					Agent:      rt.agent,
					Model:      rt.model,
					Duration:   time.Since(rt.startedAt),
					Question:   ev.Message,
					Output:     rt.ring.Snapshot(),
				})
				r.emitStatus(protocol.TaskStatusPayload{
					TaskID:       rt.id,
					Status:       protocol.TaskStatusWaitingInput,
					EffectiveDir: rt.effectiveDir,
					Digest:       &needDigest,
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

	usage := rt.pty.Guard().Release(nil)
	timedOut := errors.Is(context.Cause(ctx), errTaskTimeout)
	finalStatus := protocol.TaskStatusCompleted
	if wasCanceled {
		finalStatus = protocol.TaskStatusCanceled
	} else if timedOut || usage.Shed != nil {
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
	if timedOut {
		summary = "task timed out; " + summary
	}
	failureCat := ""
	if usage.Shed != nil && !wasCanceled {
		summary = chat.ShedMessage(*usage.Shed)
		failureCat = protocol.ErrKindMemoryPressureShed
	} else if usage.OOMKilled && !wasCanceled && finalStatus == protocol.TaskStatusFailed {
		failureCat = protocol.ErrKindOOM
	}

	var (
		gitSummary *protocol.GitSummary
		gitByRepo  map[string]*protocol.GitSummary
	)
	if len(rt.worktrees) > 0 {
		gitByRepo = make(map[string]*protocol.GitSummary, len(rt.worktrees))
		for i, wt := range rt.worktrees {
			bSHA := rt.worktreeBaseSHAs[wt.RepoName]
			if bSHA == "" && i == 0 {
				bSHA = rt.baseSHA
			}
			gs, _ := gitutil.CaptureSummary(context.Background(), wt.WorktreePath, bSHA)
			if i == 0 {
				gitSummary = gs
			}
			if gs != nil {
				gitByRepo[wt.RepoName] = gs
			}
		}
	} else {
		gitSummary, _ = gitutil.CaptureSummary(context.Background(), rt.effectiveDir, rt.baseSHA)
		if gitSummary != nil {
			repoKey := filepath.Base(rt.effectiveDir)
			if repoKey == "" || repoKey == "." || repoKey == string(filepath.Separator) {
				repoKey = "repo"
			}
			gitByRepo = map[string]*protocol.GitSummary{repoKey: gitSummary}
		}
	}

	runDigest := digest.Build(digest.Input{
		TicketSID:  rt.ticketSID,
		Attempt:    0,
		TaskStatus: finalStatus,
		Machine:    r.getMachineName(),
		Agent:      rt.agent,
		Model:      rt.model,
		Duration:   time.Since(rt.startedAt),
		ExitCode:   exitCode,
		TimedOut:   timedOut,
		Canceled:   wasCanceled,
		WaitErr:    waitErr,
		Summary:    summary,
		Output:     rt.ring.Snapshot(),
		Git:        gitByRepo,

		FailureCategory: failureCat,
	})
	runDigest.PeakRSSMB = usage.PeakRSS >> 20
	switch {
	case failureCat == protocol.ErrKindMemoryPressureShed:
		runDigest.TerminationReason = protocol.TerminationMemoryPressureShed
		runDigest.HostAvailRAMAtExitMB = usage.Shed.HostAvail >> 20
		runDigest.PeakRSSMB = max(runDigest.PeakRSSMB, usage.Shed.RSS>>20)
	case wasCanceled:
		runDigest.TerminationReason = protocol.TerminationUserStop
	case timedOut:
		runDigest.TerminationReason = protocol.TerminationTimeout
	case runDigest.Failure != nil && runDigest.Failure.Category == protocol.ErrKindOOM:
		runDigest.TerminationReason = protocol.TerminationOOM
	}

	if rt.shellTracker != nil {
		rt.shellTracker.Close(exitCode)
	}

	if r.callbacks.OnTaskEvent != nil {
		r.callbacks.OnTaskEvent(protocol.TaskEventPayload{
			TaskID:  rt.id,
			Kind:    "exit",
			Message: fmt.Sprintf("Process exited with code %d (%s)", exitCode, finalStatus),
			Meta:    map[string]any{"exit_code": exitCode, "status": finalStatus},
		})
	}

	// hostedTaskCount treats a non-terminal status as a taken slot. Publish the
	// terminal status before the completion message so a directive cap sees the slot free.
	rt.mu.Lock()
	rt.status = finalStatus
	rt.mu.Unlock()

	ec := exitCode
	r.emitStatus(protocol.TaskStatusPayload{
		TaskID:       rt.id,
		Status:       finalStatus,
		EffectiveDir: rt.effectiveDir,
		Worktrees:    rt.worktrees,
		ExitCode:     &ec,
		Summary:      summary,
		GitSummary:   gitSummary,
		Digest:       &runDigest,
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
			var chunk []byte
			if rt.shellParser != nil {
				// Strips Syre-private marks; the result never aliases buf.
				var marks []shellint.Mark
				chunk, marks = rt.shellParser.Feed(buf[:n])
				rt.shellTracker.Handle(marks)
			} else {
				chunk = make([]byte, n)
				copy(chunk, buf[:n])
			}
			r.forwardPtyOutput(rt, chunk, &ls, emit)
		}
		if err != nil {
			if rt.shellParser != nil {
				r.forwardPtyOutput(rt, rt.shellParser.Flush(), &ls, emit)
			}
			ls.flush(emit)
			return
		}
	}
}

// forwardPtyOutput tees one chunk of terminal output into the ring buffer, the telemetry
// line splitter and (when subscribed) the live stream.
func (r *Runner) forwardPtyOutput(rt *runningTask, chunk []byte, ls *lineSplitter, emit func(string)) {
	if len(chunk) == 0 {
		return
	}
	_, _ = rt.ring.Write(chunk)
	rt.scroll.Write(chunk)
	ls.feed(chunk, emit)
	if rt.streamActive.Load() && r.callbacks.OnPtyFrame != nil {
		r.callbacks.OnPtyFrame(protocol.FramePtyData, rt.id, chunk)
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
	dir := r.scrollDir
	r.mu.Unlock()
	if !ok {
		// An ended task (or one from before a syred restart): redraw its terminal
		// from the on-disk scrollback.
		snap := readScrollback(dir, taskID, DefaultRingBufferSize)
		if len(snap) == 0 {
			return false
		}
		if r.callbacks.OnPtyFrame != nil {
			r.callbacks.OnPtyFrame(protocol.FramePtySnapshot, taskID, snap)
		}
		return true
	}

	snap := rt.ring.Snapshot()
	if r.callbacks.OnPtyFrame != nil {
		r.callbacks.OnPtyFrame(protocol.FramePtySnapshot, taskID, snap)
	}
	rt.streamActive.Store(true)
	return true
}

// TerminalLog returns the retained terminal output for taskID: the live ring
// when the task is still in memory, otherwise the on-disk scrollback. ok is
// false when this daemon has nothing for the task. The caller pages the text.
func (r *Runner) TerminalLog(taskID string) (string, bool) {
	if r == nil {
		return "", false
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return "", false
	}
	r.mu.RLock()
	rt := r.tasks[taskID]
	dir := r.scrollDir
	r.mu.RUnlock()
	if rt != nil && rt.ring != nil {
		if snap := rt.ring.Snapshot(); len(snap) > 0 {
			return string(snap), true
		}
	}
	if b := readScrollback(dir, taskID, DefaultRingBufferSize); len(b) > 0 {
		return string(b), true
	}
	return "", false
}

func (r *Runner) scrollbackDir() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scrollDir
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

// CancelTask cancels a running task with the default grace period (10s). A task that is
// still in setup (git worktree, PTY spawn) is aborted before it launches.
func (r *Runner) CancelTask(taskID string) error {
	return r.CancelTaskWithGrace(taskID, time.Duration(protocol.DefaultStopGraceS)*time.Second)
}

// CancelTaskWithGrace sends a graceful interrupt to taskID, waits up to grace (clamped to
// MaxStopGraceS; default DefaultStopGraceS when <= 0), and then kills its process group /
// job object.
func (r *Runner) CancelTaskWithGrace(taskID string, grace time.Duration) error {
	if grace <= 0 {
		grace = time.Duration(protocol.DefaultStopGraceS) * time.Second
	}
	maxGrace := time.Duration(protocol.MaxStopGraceS) * time.Second
	if grace > maxGrace {
		grace = maxGrace
	}
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
	rt.stopGrace = grace
	rt.mu.Unlock()
	// A frozen process tree can't handle SIGINT/SIGTERM: thaw it first.
	_ = r.ResumeTask(taskID)
	rt.cancelFunc()
	return nil
}

// terminalHangupGrace is how long a closed terminal's shell and job get after SIGHUP.
const terminalHangupGrace = 2 * time.Second

// CloseTerminal ends a user's terminal as closing its window would (Hangup). Only the
// shell driver's terminals: an agent's session is canceled, never hung up.
func (r *Runner) CloseTerminal(taskID string) error {
	r.mu.Lock()
	rt, ok := r.tasks[taskID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("task %s not running", taskID)
	}
	if rt.agent != drivers.ShellDriverName {
		return r.CancelTask(taskID)
	}
	rt.mu.Lock()
	rt.canceled, rt.hangup = true, true
	rt.mu.Unlock()
	_ = r.ResumeTask(taskID)
	rt.cancelFunc()
	return nil
}

// errTaskTimeout is the cancellation cause of a task that ran out of time.
var errTaskTimeout = errors.New("task timed out")

// suspendTree and resumeTree freeze and thaw a process tree (replaced in tests).
var (
	suspendTree = proctree.Suspend
	resumeTree  = proctree.Resume
)

// SuspendTask freezes a running task's process tree (SIGSTOP of its process group on
// Unix; every thread of its processes on Windows). Its timeout stops counting until it
// is resumed. The task reports status "suspended".
func (r *Runner) SuspendTask(taskID string) error {
	r.mu.RLock()
	rt, ok := r.tasks[taskID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("task %s not running", taskID)
	}
	rt.mu.Lock()
	if rt.suspended || rt.canceled {
		rt.mu.Unlock()
		return nil
	}
	if err := suspendTree(rt.pty.Pid()); err != nil {
		rt.mu.Unlock()
		return fmt.Errorf("suspend task %s: %w", taskID, err)
	}
	rt.suspended = true
	rt.prevStatus = rt.status
	rt.status = protocol.TaskStatusSuspended
	rt.mu.Unlock()
	rt.deadline.pause()
	r.emitStatus(protocol.TaskStatusPayload{TaskID: rt.id, Status: protocol.TaskStatusSuspended, EffectiveDir: rt.effectiveDir})
	return nil
}

// ResumeTask thaws a suspended task and reports its status again.
func (r *Runner) ResumeTask(taskID string) error {
	r.mu.RLock()
	rt, ok := r.tasks[taskID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("task %s not running", taskID)
	}
	rt.mu.Lock()
	if !rt.suspended {
		rt.mu.Unlock()
		return nil
	}
	if err := resumeTree(rt.pty.Pid()); err != nil && !errors.Is(err, proctree.ErrNoProcess) {
		rt.mu.Unlock()
		return fmt.Errorf("resume task %s: %w", taskID, err)
	}
	rt.suspended = false
	rt.status = cmp.Or(rt.prevStatus, protocol.TaskStatusRunning)
	st := rt.status
	canceled := rt.canceled
	rt.mu.Unlock()
	rt.deadline.resume()
	if !canceled {
		r.emitStatus(protocol.TaskStatusPayload{TaskID: rt.id, Status: st, EffectiveDir: rt.effectiveDir})
	}
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
		_ = r.ResumeTask(rt.id) // a frozen tree can't handle the termination signals
		rt.cancelFunc()
	}
	r.mu.Lock()
	for _, ps := range r.starting {
		ps.canceled = true
		ps.cancel()
	}
	r.mu.Unlock()
}

// resolveWorkdirRoot picks the base directory under which per-ticket workdirs
// (<root>/.syre/tickets/<ticket sid>/<repo name>) and clones
// (<root>/.syre/clones/<repo name>) live.
func (r *Runner) resolveWorkdirRoot(requested string) (string, error) {
	return resolveWorkdirRoot(requested, r.allowedRoots)
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

// taskDeadline is a task timeout that can be paused (while the task is suspended).
// All methods are safe on a nil receiver (no timeout).
type taskDeadline struct {
	mu        sync.Mutex
	remaining time.Duration
	armedAt   time.Time
	timer     *time.Timer
	fire      func()
	stopped   bool
}

func newTaskDeadline(d time.Duration, fire func()) *taskDeadline {
	t := &taskDeadline{remaining: d, fire: fire, armedAt: time.Now()}
	t.timer = time.AfterFunc(d, fire)
	return t
}

// pause stops the clock, keeping the remaining time.
func (t *taskDeadline) pause() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.timer == nil {
		return
	}
	if t.timer.Stop() {
		t.remaining -= time.Since(t.armedAt)
		if t.remaining < 0 {
			t.remaining = 0
		}
	}
	t.timer = nil
}

// resume restarts the clock with the remaining time.
func (t *taskDeadline) resume() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.timer != nil {
		return
	}
	t.armedAt = time.Now()
	t.timer = time.AfterFunc(t.remaining, t.fire)
}

// stop disarms the deadline for good.
func (t *taskDeadline) stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}
