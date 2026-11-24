package worker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/gitutil"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/activity"
	"github.com/BryanSant/konsert/internal/worker/chat"
	"github.com/BryanSant/konsert/internal/worker/drivers"
	"github.com/BryanSant/konsert/internal/worker/shells"
	"github.com/coder/websocket"
)

// Daemon connects outbound to konsert, handles reconnects with exponential backoff,
// sends heartbeats, and bridges controller commands to Runner.
type Daemon struct {
	cfg      config.WorkerConfig
	registry *drivers.Registry
	runner   *Runner
	logger   *slog.Logger

	sendMu sync.RWMutex
	queues *connQueues // nil while disconnected

	// lastFacts caches the most recent machine facts so a reconnect can send
	// worker.hello immediately instead of re-running every probe first (slow on
	// Windows, where timed-out probe processes are not killed promptly).
	factsMu   sync.Mutex
	lastFacts *protocol.MachineFacts

	// version is konsertd's build version, reported as WorkerInfo.Version.
	version string

	// outbox keeps final task statuses until written to a hub connection (persisted).
	outbox *statusOutbox

	// helloAt is when worker.hello was last written on a connection (zero if the last
	// attempt never got that far). Only touched by the Run goroutine.
	helloAt time.Time

	// discovery outlives connections so hook events written while disconnected are
	// delivered after reconnect.
	discovery *SessionDiscovery

	// wake holds the host Keep Awake inhibitor requested by the hub (host.wakelock).
	wake *WakeLockManager

	// ghRun runs gh for PR reviews (git.pr.review); nil = gh from PATH. Tests replace it.
	ghRun gitutil.GHRunner

	// activity publishes session activity (chats and PTY tasks) at a bounded rate;
	// taskActivity derives PTY tasks' activity from their telemetry (guarded by taskActMu).
	activity     *activity.Publisher
	taskActMu    sync.Mutex
	taskActivity *activity.Tracker
	// shells counts the shell processes under every hosted session (see daemon_shells.go).
	shells *shells.Tracker
	// ShellInterval overrides DefaultShellInterval (tests).
	ShellInterval time.Duration

	// Agent chat (see daemon_chat.go). They outlive connections.
	accounts   *chat.AccountRegistry
	chat       *chat.Manager
	caps       *chat.Capabilities
	capRefresh chan struct{} // re-probe and send capabilities
	capPush    chan struct{} // send cached capabilities (chat usage changed)
	// chatPending buffers chat events while disconnected (guarded by sendMu).
	chatPending    []outboundMsg
	chatDropWarned bool

	// CapabilitiesInterval overrides DefaultCapabilitiesInterval (tests).
	CapabilitiesInterval time.Duration

	// Test hooks: optional custom heartbeat & facts refresh intervals.
	HeartbeatInterval time.Duration
	FactsInterval     time.Duration
	// WakeLockGrace is how long a host wake lock outlives a lost hub connection
	// (DefaultWakeLockGrace when zero).
	WakeLockGrace time.Duration
}

// activityKeep is how long a finished session's last activity is kept for re-sending
// after a reconnect.
const activityKeep = time.Hour

// NewDaemon constructs a worker daemon from WorkerConfig.
func NewDaemon(cfg config.WorkerConfig, registry *drivers.Registry, logger *slog.Logger) *Daemon {
	if registry == nil {
		registry = drivers.DefaultRegistry
	}
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Name == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "worker"
		}
		cfg.Name = host
	}
	if cfg.HostGroup == "" {
		cfg.HostGroup = cfg.Name
	}
	cfg.WorkerID = WorkerIDFor(cfg)
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 2
	}

	d := &Daemon{
		cfg:               cfg,
		registry:          registry,
		logger:            logger,
		outbox:            newStatusOutbox(outboxPath(defaultStateDir(), cfg.WorkerID), logger),
		HeartbeatInterval: 10 * time.Second,
		FactsInterval:     5 * time.Minute,
	}

	d.activity = activity.NewPublisher(activity.PublisherOptions{}, func(a protocol.SessionActivity) {
		d.sendJSON(protocol.MsgSessionActivity, a)
	})
	d.taskActivity = activity.NewTracker(d.activity, nil)
	d.shells = shells.New(shells.Options{})

	d.runner = NewRunner(registry, cfg.Sandbox, cfg.AllowedRoots, RunnerCallbacks{
		OnTaskStatus: func(p protocol.TaskStatusPayload) {
			d.taskActMu.Lock()
			d.taskActivity.Status(p)
			d.taskActMu.Unlock()
			if protocol.IsTerminalTaskStatus(p.Status) {
				// Final statuses go through the persistent outbox so they survive a
				// disconnect and are re-sent after the next worker.hello.
				d.outbox.add(p)
				return
			}
			d.sendJSON(protocol.MsgTaskStatus, p)
		},
		OnTaskEvent: func(p protocol.TaskEventPayload) {
			d.sendJSON(protocol.MsgTaskEvent, p)
			d.taskActMu.Lock()
			d.taskActivity.Event(p)
			d.taskActMu.Unlock()
		},
		OnOperatorCommand: func(p protocol.OperatorCommandPayload) {
			d.sendJSON(protocol.MsgOperatorCommand, p)
		},
		OnPtyFrame: func(frameType byte, taskID string, data []byte) {
			frame, err := protocol.EncodeFrame(frameType, taskID, data)
			if err != nil {
				logger.Warn("dropping PTY frame: encode failed", "task_id", taskID, "frame_type", frameType, "err", err)
				return
			}
			// Snapshots are control traffic (never dropped); live data may be dropped.
			d.enqueue(outboundMsg{msgType: websocket.MessageBinary, data: frame, droppable: frameType == protocol.FramePtyData})
		},
	}, logger)

	// Anything descending from konsertd itself (PTY tasks, chat engines and their helpers)
	// is managed, not an external session.
	self := os.Getpid()
	isManaged := func(pid int) bool { return pid == self || d.runner.IsManagedPID(pid) }
	d.discovery = NewSessionDiscovery("", isManaged, func(p protocol.ExternalSessionPayload) {
		d.sendJSON(protocol.MsgExternalSession, p)
	})

	d.wake = NewWakeLockManager(nil, logger, func(st protocol.HostWakeLockState) {
		d.sendJSON(protocol.MsgHostWakeLockState, st)
	})

	d.initChat()

	return d
}

// WakeLock returns the host wake lock manager (useful for testing).
func (d *Daemon) WakeLock() *WakeLockManager {
	return d.wake
}

// shutdown stops every task and chat engine and drops the host wake lock; Run calls it on
// every exit path.
func (d *Daemon) shutdown() {
	d.shutdownChat()
	d.runner.ShutdownAll()
	d.wake.Shutdown()
}

// SetAllowSandboxShell opts a --sandbox worker in to the unrestricted shell driver.
func (d *Daemon) SetAllowSandboxShell(allow bool) {
	d.runner.SetAllowSandboxShell(allow)
}

// SetVersion records konsertd's build version for worker.hello. Call before Run.
func (d *Daemon) SetVersion(v string) {
	d.version = v
}

// advertisedAgents lists installed drivers that this worker will actually accept tasks for.
func (d *Daemon) advertisedAgents() []string {
	installed := d.registry.DetectInstalled(d.cfg.EnableMockDriver)
	out := make([]string, 0, len(installed))
	for _, a := range installed {
		if d.runner.AgentAllowed(a) {
			out = append(out, a)
		}
	}
	return out
}

// WorkerID returns this daemon's stable identity (the hub's workers.id).
func (d *Daemon) WorkerID() string {
	return d.cfg.WorkerID
}

// Runner returns the underlying task runner (useful for testing).
func (d *Daemon) Runner() *Runner {
	return d.runner
}

// ErrFleetTokenRequired is returned by Run when the hub requires a fleet token and none is
// configured: retrying cannot succeed until the configuration changes.
var ErrFleetTokenRequired = errors.New("hub requires a fleet token: set --token or KONSERT_FLEET_TOKEN (or fleet_token in the worker config); only a hub started with --no-auth accepts workers without one")

// authError reports that the hub refused the WebSocket upgrade (HTTP 401/403).
type authError struct {
	status   int
	tokenSet bool
	err      error
}

func (e *authError) Error() string {
	if !e.tokenSet {
		return fmt.Sprintf("hub rejected the connection (HTTP %d): %v", e.status, ErrFleetTokenRequired)
	}
	return fmt.Sprintf("hub rejected the fleet token (HTTP %d): check --token / KONSERT_FLEET_TOKEN against the hub's fleet token", e.status)
}

func (e *authError) Unwrap() error {
	if !e.tokenSet {
		return ErrFleetTokenRequired
	}
	return e.err
}

// Run runs the reconnect loop until ctx is canceled.
// Active tasks stay alive across transient controller disconnects and are re-adopted on reconnect.
func (d *Daemon) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			d.shutdown()
			return ctx.Err()
		}

		d.helloAt = time.Time{}
		err := d.connectAndServe(ctx)
		if ctx.Err() != nil {
			d.shutdown()
			return ctx.Err()
		}
		// Only an operator removal (4001) stops the daemon. Every other close, including a
		// normal 1000 (superseded connection) and 1013 (try again later: another live
		// connection holds this identity), reconnects with backoff.
		switch websocket.CloseStatus(err) {
		case websocket.StatusCode(protocol.WSCloseWorkerRemoved):
			d.logger.Info("machine was removed from fleet by controller; stopping daemon", "worker_id", d.cfg.WorkerID)
			d.shutdown()
			return nil
		case websocket.StatusCode(protocol.WSCloseTryAgainLater):
			d.logger.Warn("controller asked this worker to retry later (another live connection uses this identity)", "worker_id", d.cfg.WorkerID)
		}

		var aerr *authError
		if errors.As(err, &aerr) {
			if !aerr.tokenSet {
				// No token configured: retrying cannot succeed. Exit with a clear error
				// instead of looping.
				d.logger.Error(aerr.Error())
				d.shutdown()
				return ErrFleetTokenRequired
			}
			// A wrong token may be fixed on the hub side (rotation): keep retrying, but
			// only at the slowest backoff and with an explicit error.
			d.logger.Error(aerr.Error(), "retry_in", maxBackoff)
			backoff = maxBackoff
		}

		// Reset backoff only after a session that got past worker.hello and lasted > 10 s.
		// The timestamp is taken after hello, so a slow or failing 15 s dial never counts
		// as a long session.
		if !d.helloAt.IsZero() && time.Since(d.helloAt) > 10*time.Second {
			backoff = time.Second
		}

		// Keep a host wake lock through short outages; drop it if the hub stays away.
		d.wake.Disconnected(d.WakeLockGrace)

		jitter := time.Duration(rand.Int64N(int64(backoff/4 + 1)))
		sleepDur := backoff + jitter
		d.logger.Warn("disconnected from controller; reconnecting", "err", err, "backoff", sleepDur)

		select {
		case <-ctx.Done():
			d.shutdown()
			return ctx.Err()
		case <-time.After(sleepDur):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (d *Daemon) connectAndServe(ctx context.Context) error {
	headers := http.Header{}
	if d.cfg.FleetToken != "" {
		headers.Set("Authorization", "Bearer "+d.cfg.FleetToken)
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, d.cfg.ControllerURL, &websocket.DialOptions{
		HTTPHeader: headers,
	})
	dialCancel()
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return &authError{status: resp.StatusCode, tokenSet: d.cfg.FleetToken != "", err: err}
		}
		return fmt.Errorf("dial %s: %w", d.cfg.ControllerURL, err)
	}
	conn.SetReadLimit(20 * 1024 * 1024)
	defer conn.Close(websocket.StatusNormalClosure, "shutdown")
	go PruneScratchAttachments("", AttachmentTTL)

	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	queues := newConnQueues()
	// Chat events buffered while disconnected are queued now and written after hello.
	d.attachQueues(queues)

	defer func() {
		d.sendMu.Lock()
		if d.queues == queues {
			d.queues = nil
		}
		d.sendMu.Unlock()
	}()

	// Send worker.hello immediately with current capabilities, machine facts, and any active in-flight tasks.
	permMode := protocol.PermissionFullAccess
	if d.cfg.Sandbox {
		permMode = protocol.PermissionSandboxed
	}
	initialFacts, cached := d.cachedFacts()
	if !cached {
		initialFacts = d.refreshFacts(connCtx)
	}
	hello := protocol.WorkerHelloPayload{
		Worker: protocol.WorkerInfo{
			ID:             d.cfg.WorkerID,
			Name:           d.cfg.Name,
			HostGroup:      d.cfg.HostGroup,
			OS:             runtime.GOOS,
			Arch:           runtime.GOARCH,
			Isolation:      d.cfg.Isolation,
			PermissionMode: permMode,
			Agents:         d.advertisedAgents(),
			Tags:           d.cfg.Tags,
			MaxConcurrent:  d.cfg.MaxConcurrent,
			Facts:          &initialFacts,
			Version:        d.version,
			Hostname:       osHostname(),
		},
		ActiveTasks: d.runner.ActiveTasks(),
	}
	helloBytes, err := protocol.NewEnvelope(protocol.MsgWorkerHello, "", hello)
	if err != nil {
		return err
	}
	if err := conn.Write(connCtx, websocket.MessageText, helloBytes); err != nil {
		return fmt.Errorf("write worker.hello: %w", err)
	}
	d.helloAt = time.Now()
	// Report the host wake lock state right after hello so the hub knows it after every
	// reconnect (the writer below sends queued control messages after hello).
	d.wake.Connected()
	d.sendJSON(protocol.MsgHostWakeLockState, d.wake.State())
	// The hub keeps session activity in memory only: re-send what it may have missed.
	d.activity.Prune(activityKeep)
	for _, a := range d.activity.Snapshot() {
		d.sendJSON(protocol.MsgSessionActivity, a)
	}
	d.logger.Info("connected to controller",
		"url", d.cfg.ControllerURL,
		"worker_id", d.cfg.WorkerID,
		"agents", hello.Worker.Agents,
		"permission_mode", permMode,
		"active_tasks", len(hello.ActiveTasks),
	)

	// Writer goroutine. It starts after worker.hello has been written, so pending final
	// statuses from the outbox are always delivered after hello.
	go func() {
		if err := d.writeLoop(connCtx, queues, func(msg outboundMsg) error {
			wCtx, wCancel := context.WithTimeout(connCtx, 10*time.Second)
			defer wCancel()
			return conn.Write(wCtx, msg.msgType, msg.data)
		}); err != nil && connCtx.Err() == nil {
			d.logger.Warn("write to controller failed", "err", err)
		}
		connCancel()
	}()

	// hello carried cached facts; send fresh ones as soon as they are collected.
	if cached {
		go func() {
			facts := d.refreshFacts(connCtx)
			if connCtx.Err() == nil {
				d.sendJSON(protocol.MsgWorkerFacts, protocol.WorkerFactsPayload{Facts: facts})
			}
		}()
	}

	// Adaptive delta-throttled heartbeat goroutine (faster when active, gentle when idle).
	go func() {
		base := d.HeartbeatInterval
		if base <= 0 {
			base = 10 * time.Second
		}
		throttle := NewTelemetryThrottle(base)
		sendIfNeeded := func(force bool) {
			metrics := CollectMetrics(connCtx)
			activeIDs := d.runner.ActiveTaskIDs()
			if force || throttle.ShouldSend(time.Now().UTC(), activeIDs, metrics) {
				d.sendJSON(protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{
					ActiveTaskIDs: activeIDs,
					LoadAvg:       metrics.LoadAvg,
					Metrics:       metrics,
				})
			}
		}
		sendIfNeeded(true)
		for {
			waitDur := throttle.NextInterval(len(d.runner.ActiveTaskIDs()))
			timer := time.NewTimer(waitDur)
			select {
			case <-connCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
				sendIfNeeded(false)
			}
		}
	}()

	// Agent capabilities (and the legacy worker.quotas derived from them).
	go d.capabilitiesLoop(connCtx)

	// Periodic Machine Facts refresher (keeps cached facts fresh without bloating heartbeats).
	go func() {
		fInt := d.FactsInterval
		if fInt <= 0 {
			fInt = 5 * time.Minute
		}
		ticker := time.NewTicker(fInt)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-ticker.C:
				facts := d.refreshFacts(connCtx)
				d.sendJSON(protocol.MsgWorkerFacts, protocol.WorkerFactsPayload{Facts: facts})
			}
		}
	}()

	// Lightweight Hook + Passive Session Discovery (zero blind spots for externally SSH-launched agents).
	go d.discovery.Run(connCtx)

	// Shells under hosted sessions, from the process tree (~1 s).
	go d.shellLoop(connCtx)

	// Read loop
	for {
		msgType, data, err := conn.Read(connCtx)
		if err != nil {
			connCancel()
			return err
		}

		switch msgType {
		case websocket.MessageBinary:
			ft, taskID, payload, decErr := protocol.DecodeFrame(data)
			if decErr != nil {
				d.logger.Warn("invalid binary frame from controller", "err", decErr)
				continue
			}
			if ft == protocol.FramePtyStdin {
				if err := d.runner.WriteStdin(taskID, payload); err != nil {
					d.logger.Debug("write stdin failed", "task_id", taskID, "err", err)
				}
			}

		case websocket.MessageText:
			env, decErr := protocol.DecodeEnvelope(data)
			if decErr != nil {
				d.logger.Warn("invalid json frame from controller", "err", decErr)
				continue
			}
			d.handleControlMessage(ctx, env)
		}
	}
}

func (d *Daemon) handleControlMessage(daemonCtx context.Context, env protocol.Envelope) {
	if d.handleChatMessage(env) {
		return
	}
	switch env.Type {
	case protocol.MsgTaskStart:
		p, err := protocol.DecodePayload[protocol.TaskStartPayload](env)
		if err != nil {
			d.logger.Warn("decode task.start failed", "err", err)
			return
		}
		// Validation and reservation happen here, in message order; git worktree creation and
		// the PTY spawn run in a goroutine so the read loop keeps serving stdin, cancel,
		// resize and WebSocket pings during a slow checkout.
		taskID := p.TaskID
		if err := d.runner.StartTaskAsync(daemonCtx, p, func(err error) {
			d.logger.Error("start task failed", "task_id", taskID, "err", err)
		}); err != nil {
			d.logger.Error("start task rejected", "task_id", taskID, "err", err)
		}

	case protocol.MsgTaskCancel:
		p, err := protocol.DecodePayload[protocol.TaskCancelPayload](env)
		if err != nil {
			return
		}
		_ = d.runner.CancelTask(p.TaskID)

	case protocol.MsgSessionShellKill:
		if p, err := protocol.DecodePayload[protocol.SessionShellKill](env); err == nil {
			go d.killShells(p)
		}

	case protocol.MsgTaskSuspend, protocol.MsgTaskResume:
		p, err := protocol.DecodePayload[protocol.TaskSuspendPayload](env)
		if err != nil {
			return
		}
		op, verb := d.runner.SuspendTask, "suspend"
		if env.Type == protocol.MsgTaskResume {
			op, verb = d.runner.ResumeTask, "resume"
		}
		if err := op(p.TaskID); err != nil {
			d.logger.Warn(verb+" task failed", "task_id", p.TaskID, "err", err)
			d.sendJSON(protocol.MsgTaskEvent, protocol.TaskEventPayload{TaskID: p.TaskID, Kind: "error", Message: "Could not " + verb + " the task: " + err.Error()})
		}

	case protocol.MsgTaskSteer:
		p, err := protocol.DecodePayload[protocol.TaskSteerPayload](env)
		if err != nil {
			return
		}
		if err := d.runner.SteerTask(p.TaskID, p.InputText, p.AppendNewline); err != nil {
			d.logger.Warn("steer task failed", "task_id", p.TaskID, "err", err)
		}

	case protocol.MsgPtySubscribe:
		p, err := protocol.DecodePayload[protocol.PtySubscribePayload](env)
		if err != nil {
			return
		}
		d.runner.SubscribePty(p.TaskID)

	case protocol.MsgPtyUnsubscribe:
		p, err := protocol.DecodePayload[protocol.PtyUnsubscribePayload](env)
		if err != nil {
			return
		}
		d.runner.UnsubscribePty(p.TaskID)

	case protocol.MsgPtyResize:
		p, err := protocol.DecodePayload[protocol.PtyResizePayload](env)
		if err != nil {
			return
		}
		_ = d.runner.ResizePty(p.TaskID, p.Cols, p.Rows)

	case protocol.MsgGitInspect:
		p, err := protocol.DecodePayload[protocol.GitInspectRequest](env)
		if err != nil {
			return
		}
		go func(reqID string, req protocol.GitInspectRequest) {
			ctx, cancel := context.WithTimeout(daemonCtx, 10*time.Second)
			defer cancel()
			dir, vErr := gitutil.ValidateWorkdir(req.Workdir, d.cfg.AllowedRoots)
			if vErr != nil {
				d.sendJSONWithID(protocol.MsgGitInspectResult, reqID, protocol.GitInspectResult{Error: vErr.Error(), Files: []protocol.GitChangedFile{}})
				return
			}
			res, iErr := gitutil.InspectWorkingTree(ctx, dir)
			if iErr != nil {
				d.sendJSONWithID(protocol.MsgGitInspectResult, reqID, protocol.GitInspectResult{Error: iErr.Error(), Files: []protocol.GitChangedFile{}})
				return
			}
			d.sendJSONWithID(protocol.MsgGitInspectResult, reqID, res)
		}(env.ID, p)

	case protocol.MsgGitCommit:
		p, err := protocol.DecodePayload[protocol.GitCommitRequest](env)
		if err != nil {
			return
		}
		go func(reqID string, req protocol.GitCommitRequest) {
			ctx, cancel := context.WithTimeout(daemonCtx, 15*time.Second)
			defer cancel()
			dir, vErr := gitutil.ValidateWorkdir(req.Workdir, d.cfg.AllowedRoots)
			if vErr != nil {
				d.sendJSONWithID(protocol.MsgGitCommitResult, reqID, protocol.GitCommitResult{OK: false, Error: vErr.Error()})
				return
			}
			res, cErr := gitutil.CommitChanges(ctx, dir, req.Message, req.StageAll)
			if cErr != nil {
				d.sendJSONWithID(protocol.MsgGitCommitResult, reqID, protocol.GitCommitResult{OK: false, Error: cErr.Error()})
				return
			}
			d.sendJSONWithID(protocol.MsgGitCommitResult, reqID, res)
		}(env.ID, p)

	case protocol.MsgGitPRReview:
		p, err := protocol.DecodePayload[protocol.GitPRReviewRequest](env)
		if err != nil {
			return
		}
		go func(reqID string, req protocol.GitPRReviewRequest) {
			ctx, cancel := context.WithTimeout(daemonCtx, 45*time.Second)
			defer cancel()
			dir, vErr := gitutil.ValidateWorkdir(req.Workdir, d.cfg.AllowedRoots)
			if vErr != nil {
				d.sendJSONWithID(protocol.MsgGitPRReviewResult, reqID, protocol.GitPRReviewResult{Status: protocol.PRReviewFailed, Message: vErr.Error()})
				return
			}
			d.sendJSONWithID(protocol.MsgGitPRReviewResult, reqID, gitutil.ReviewPR(ctx, dir, req, d.ghRun))
		}(env.ID, p)

	case protocol.MsgGitWorktreeCleanup:
		p, err := protocol.DecodePayload[protocol.GitWorktreeCleanupRequest](env)
		if err != nil {
			return
		}
		go func(reqID string, req protocol.GitWorktreeCleanupRequest) {
			ctx, cancel := context.WithTimeout(daemonCtx, 15*time.Second)
			defer cancel()
			dir, vErr := gitutil.ValidateWorkdir(req.Workdir, d.cfg.AllowedRoots)
			if vErr != nil {
				d.sendJSONWithID(protocol.MsgGitWorktreeCleanupResult, reqID, protocol.GitWorktreeCleanupResult{OK: false, Error: vErr.Error()})
				return
			}
			if err := gitutil.CleanupKonsertWorktree(ctx, dir); err != nil {
				d.sendJSONWithID(protocol.MsgGitWorktreeCleanupResult, reqID, protocol.GitWorktreeCleanupResult{OK: false, Error: err.Error()})
				return
			}
			d.sendJSONWithID(protocol.MsgGitWorktreeCleanupResult, reqID, protocol.GitWorktreeCleanupResult{OK: true})
		}(env.ID, p)

	case protocol.MsgFSList:
		p, err := protocol.DecodePayload[protocol.FSListRequest](env)
		if err != nil {
			return
		}
		go func(reqID string, req protocol.FSListRequest) {
			d.sendJSONWithID(protocol.MsgFSListResult, reqID, d.handleFSList(req))
		}(env.ID, p)

	case protocol.MsgWorktreeScan:
		p, err := protocol.DecodePayload[protocol.WorktreeScanRequest](env)
		if err != nil {
			return
		}
		go func(reqID string, req protocol.WorktreeScanRequest) {
			d.sendJSONWithID(protocol.MsgWorktreeScanResult, reqID, d.handleWorktreeScan(req))
		}(env.ID, p)

	case protocol.MsgHostWakeLock:
		p, err := protocol.DecodePayload[protocol.HostWakeLockPayload](env)
		if err != nil {
			d.logger.Warn("decode host.wakelock failed", "err", err)
			return
		}
		// Set reports changes through onChange. Answer explicitly as well so the hub
		// always gets a reply, even when nothing changed (idempotent request).
		before := d.wake.State()
		st := d.wake.Set(p.Enabled, p.Reason)
		if wakeStateEqual(before, st) {
			d.sendJSON(protocol.MsgHostWakeLockState, st)
		}
	case protocol.MsgFileUpload:
		p, err := protocol.DecodePayload[protocol.FileUploadRequest](env)
		if err != nil {
			return
		}
		go func(reqID string, req protocol.FileUploadRequest) {
			d.sendJSONWithID(protocol.MsgFileUploadResult, reqID, d.handleFileUpload(req))
		}(env.ID, p)
	}
}

func (d *Daemon) handleFileUpload(req protocol.FileUploadRequest) protocol.FileUploadResult {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(req.DataBase64))
	if err != nil {
		return protocol.FileUploadResult{OK: false, Error: "invalid base64 payload"}
	}
	savedPath, err := SaveScratchAttachment("", req.SessionID, req.Filename, raw)
	if err != nil {
		return protocol.FileUploadResult{OK: false, Error: err.Error()}
	}
	return protocol.FileUploadResult{
		OK:         true,
		RemotePath: savedPath,
		Filename:   SanitizeAttachmentFilename(req.Filename),
		Size:       int64(len(raw)),
	}
}

func (d *Daemon) handleWorktreeScan(req protocol.WorktreeScanRequest) protocol.WorktreeScanResult {
	root := strings.TrimSpace(req.RootPath)
	if root == "" {
		if len(d.cfg.AllowedRoots) > 0 {
			root = d.cfg.AllowedRoots[0]
		} else {
			root = "~"
		}
	}
	validRoot, err := gitutil.ValidateWorkdir(root, d.cfg.AllowedRoots)
	if err != nil {
		return protocol.WorktreeScanResult{RootPath: root, Matches: []protocol.WorktreeScanMatch{}, Error: err.Error()}
	}
	query := strings.ToLower(strings.TrimSpace(req.Query))
	res := protocol.WorktreeScanResult{
		RootPath: validRoot,
		Matches:  []protocol.WorktreeScanMatch{},
	}

	// Bounded depth-3 walk (skips heavy vendor dirs) to find .konsert-worktrees/* or query-matching temp worktrees.
	maxDepth := 3
	baseDepth := strings.Count(filepath.Clean(validRoot), string(filepath.Separator))
	_ = filepath.WalkDir(validRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if path != validRoot {
			if name == "node_modules" || name == ".git" || name == "vendor" || name == "target" || name == ".cache" {
				return filepath.SkipDir
			}
		}
		depth := strings.Count(filepath.Clean(path), string(filepath.Separator)) - baseDepth
		if name == ".konsert-worktrees" {
			children, _ := os.ReadDir(path)
			repoRoot := filepath.Dir(path)
			for _, ch := range children {
				if !ch.IsDir() {
					continue
				}
				wtPath := filepath.Join(path, ch.Name())
				if query != "" && !strings.Contains(strings.ToLower(wtPath), query) {
					continue
				}
				res.Matches = append(res.Matches, protocol.WorktreeScanMatch{
					Path:      wtPath,
					RepoRoot:  repoRoot,
					Branch:    ch.Name(),
					Kind:      "konsert_worktree",
					Removable: true,
				})
			}
			return filepath.SkipDir
		}
		if query != "" && path != validRoot && strings.Contains(strings.ToLower(name), query) {
			// Match temp worktree or folder matching query (e.g. "epic-123")
			if _, gitFileErr := os.Stat(filepath.Join(path, ".git")); gitFileErr == nil {
				res.Matches = append(res.Matches, protocol.WorktreeScanMatch{
					Path:      path,
					Kind:      "git_worktree",
					Removable: false,
				})
				return filepath.SkipDir
			}
		}
		if depth >= maxDepth {
			return filepath.SkipDir
		}
		return nil
	})

	sort.Slice(res.Matches, func(i, j int) bool { return res.Matches[i].Path < res.Matches[j].Path })
	if len(res.Matches) > 50 {
		res.Matches = res.Matches[:50]
	}
	return res
}

func (d *Daemon) handleFSList(req protocol.FSListRequest) protocol.FSListResult {
	rawPath := strings.TrimSpace(req.Path)
	if rawPath == "" {
		rawPath = "~"
	}
	// Expand leading ~
	if rawPath == "~" || strings.HasPrefix(rawPath, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if rawPath == "~" {
				rawPath = home
			} else {
				rawPath = filepath.Join(home, rawPath[2:])
			}
		}
	}

	// Support type-ahead partial paths (e.g. "/Users/bsant/src/kon" -> dir="/Users/bsant/src", prefix="kon")
	prefix := strings.ToLower(strings.TrimSpace(req.Prefix))
	dirToRead := rawPath
	if info, err := os.Stat(rawPath); err != nil || !info.IsDir() {
		dirToRead = filepath.Dir(rawPath)
		if prefix == "" {
			prefix = strings.ToLower(filepath.Base(rawPath))
		}
	}

	validatedDir, vErr := gitutil.ValidateWorkdir(dirToRead, d.cfg.AllowedRoots)
	if vErr != nil {
		return protocol.FSListResult{Error: vErr.Error(), Entries: []protocol.FSEntry{}}
	}

	dirEntries, err := os.ReadDir(validatedDir)
	if err != nil {
		return protocol.FSListResult{CurrentDir: validatedDir, Error: err.Error(), Entries: []protocol.FSEntry{}}
	}

	showHidden := strings.HasPrefix(prefix, ".")
	var entries []protocol.FSEntry
	for _, de := range dirEntries {
		name := de.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		if prefix != "" && !strings.HasPrefix(strings.ToLower(name), prefix) {
			continue
		}
		isDir := de.IsDir()
		fullPath := filepath.Join(validatedDir, name)
		if !isDir && de.Type()&os.ModeSymlink != 0 {
			if st, sErr := os.Stat(fullPath); sErr == nil && st.IsDir() {
				isDir = true
			}
		}
		if req.DirsOnly && !isDir {
			continue
		}
		isGit := false
		if isDir {
			if _, gErr := os.Stat(filepath.Join(fullPath, ".git")); gErr == nil {
				isGit = true
			}
		}
		entries = append(entries, protocol.FSEntry{
			Name:      name,
			Path:      fullPath,
			IsDir:     isDir,
			IsGitRepo: isGit,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		if entries[i].IsGitRepo != entries[j].IsGitRepo {
			return entries[i].IsGitRepo
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	if len(entries) > 80 {
		entries = entries[:80]
	}

	parent := filepath.Dir(validatedDir)
	if parent == validatedDir {
		parent = ""
	}
	return protocol.FSListResult{
		CurrentDir: validatedDir,
		ParentDir:  parent,
		Entries:    entries,
	}
}

func (d *Daemon) sendJSON(msgType string, payload any) {
	d.sendJSONWithID(msgType, "", payload)
}

func (d *Daemon) sendJSONWithID(msgType, id string, payload any) {
	raw, err := protocol.NewEnvelope(msgType, id, payload)
	if err != nil {
		return
	}
	d.enqueue(outboundMsg{msgType: websocket.MessageText, data: raw})
}

// enqueue hands msg to the current connection's queues. It never blocks. While
// disconnected, messages are discarded: final task statuses do not pass through here (they
// live in the outbox), live state is re-sent in worker.hello, and request results are
// meaningless on a new connection.
func (d *Daemon) enqueue(msg outboundMsg) {
	d.sendMu.RLock()
	q := d.queues
	d.sendMu.RUnlock()
	if q == nil {
		return
	}
	q.push(msg)
}

// writeLoop drains queues in priority order until ctx ends or write fails:
// control messages first, then pending outbox statuses, then live PTY data.
func (d *Daemon) writeLoop(ctx context.Context, q *connQueues, write func(outboundMsg) error) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if msg, ok := q.popControl(); ok {
			if err := write(msg); err != nil {
				return err
			}
			continue
		}
		if entries := d.outbox.pending(); len(entries) > 0 {
			for _, e := range entries {
				raw, err := protocol.NewEnvelope(protocol.MsgTaskStatus, "", e.Status)
				if err != nil {
					d.outbox.remove(e.Seq)
					continue
				}
				if err := write(outboundMsg{msgType: websocket.MessageText, data: raw}); err != nil {
					return err
				}
				d.outbox.remove(e.Seq)
			}
			continue
		}
		select {
		case msg := <-q.pty:
			if err := write(msg); err != nil {
				return err
			}
			continue
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-q.signal:
		case <-d.outbox.signal:
		case msg := <-q.pty:
			if err := write(msg); err != nil {
				return err
			}
		}
	}
}

// cachedFacts returns the last collected machine facts, if any.
func (d *Daemon) cachedFacts() (protocol.MachineFacts, bool) {
	d.factsMu.Lock()
	defer d.factsMu.Unlock()
	if d.lastFacts == nil {
		return protocol.MachineFacts{}, false
	}
	return *d.lastFacts, true
}

// collectFacts gathers machine facts for worker.hello and worker.facts. Tests replace
// it: the real probes spawn tools (e.g. `podman info` initializes container storage
// under $HOME) that tests running with a temporary HOME must not trigger.
var collectFacts = CollectMachineFacts

// refreshFacts collects machine facts and caches them for the next reconnect.
func (d *Daemon) refreshFacts(ctx context.Context) protocol.MachineFacts {
	f := collectFacts(ctx)
	d.factsMu.Lock()
	d.lastFacts = &f
	d.factsMu.Unlock()
	return f
}
