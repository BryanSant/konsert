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
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/gitutil"
	"github.com/BryanSant/konsert/internal/linkwatch"
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
	// cfg is fixed after NewDaemon except cfg.Credential, which credMu guards (a
	// re-join replaces it while the daemon runs; see reloadCredential).
	cfg      config.WorkerConfig
	credMu   sync.Mutex
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
	// helloWall is helloAt's wall clock (unix ns) for other goroutines (linkedSinceWake).
	helloWall atomic.Int64

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

	// HeartbeatInterval is the vitals cadence while sessions run (10 s); FactsInterval
	// the machine facts refresh (5 min). Tests shorten them.
	HeartbeatInterval time.Duration
	FactsInterval     time.Duration
	// metrics samples heartbeat vitals (metrics.go).
	metrics *metricsSampler

	// Link liveness (linkwatch): LinkQuiet and LinkDead override the 15 s / 45 s
	// defaults, WatchEvery how often wake and network changes are checked (5 s), and
	// WatchAddrs replaces the network fingerprint. Tests set them.
	LinkQuiet  time.Duration
	LinkDead   time.Duration
	WatchEvery time.Duration
	WatchAddrs func() (string, error)
	// link keeps the link state for `konsertd status` and keeps reconnect logs quiet.
	link *linkTracker
	// WakeLockGrace is how long a host wake lock outlives a lost hub connection
	// (DefaultWakeLockGrace when zero).
	WakeLockGrace time.Duration

	// SocketPath overrides LocalSocketPath for the local socket (tests).
	SocketPath string
	// attachMu guards the current attached connection (see daemon_local.go).
	attachMu     sync.Mutex
	attachCancel context.CancelFunc
	attachDone   chan struct{}
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
	if cfg.Credential == "" {
		cfg.Credential, _ = LoadCredential("", cfg.WorkerID)
	}
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
		metrics:           newMetricsSampler(),
	}
	transport := "outbound"
	if strings.TrimSpace(cfg.ControllerURL) == "" {
		transport = "local socket"
	}
	d.link = newLinkTracker(logger, LinkStatusPath(defaultStateDir(), cfg.WorkerID), cfg.WorkerID, cfg.Name, transport)

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

// ErrCredentialRequired is returned by Run when the hub requires a machine credential and
// none is saved: retrying cannot succeed until the machine joins.
var ErrCredentialRequired = errors.New("no machine credential: run `konsertd join <hub-url> <code>`")

// authError reports that the hub refused the WebSocket upgrade (HTTP 401/403).
type authError struct {
	status  int
	credSet bool
	err     error
}

func (e *authError) Error() string {
	if !e.credSet {
		return fmt.Sprintf("hub refused the connection (HTTP %d): %v", e.status, ErrCredentialRequired)
	}
	return fmt.Sprintf("hub rejected this machine's credential (HTTP %d)", e.status)
}

func (e *authError) Unwrap() error {
	if !e.credSet {
		return ErrCredentialRequired
	}
	return e.err
}

// credential is the machine credential presented to the hub ("" if none).
func (d *Daemon) credential() string {
	d.credMu.Lock()
	defer d.credMu.Unlock()
	return d.cfg.Credential
}

// reloadCredential re-reads the saved credential (another process, such as `konsertd
// join`, may have replaced it) and reports whether it changed.
func (d *Daemon) reloadCredential() bool {
	cred, err := LoadCredential("", d.cfg.WorkerID)
	if err != nil {
		return false
	}
	d.credMu.Lock()
	defer d.credMu.Unlock()
	if cred == d.cfg.Credential {
		return false
	}
	d.cfg.Credential = cred
	return true
}

// Reconnect backoff: 1 s doubling to 30 s, plus up to 25 % jitter, forever.
const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
)

// linkConfig is the liveness configuration of every hub link.
func (d *Daemon) linkConfig() linkwatch.Config {
	return linkwatch.Config{QuietAfter: d.LinkQuiet, DeadAfter: d.LinkDead}
}

// linkedSinceWake reports whether the current link was made after the latest wake: the
// watcher reports a wake on its first tick after it (up to WatchEvery + slack later), and
// by then the read loop may already have noticed the dead link and redialed. The wall
// clock counts the time asleep, so a link made before the sleep never looks this fresh.
func (d *Daemon) linkedSinceWake() bool {
	at := d.helloWall.Load()
	if at == 0 {
		return false
	}
	every := d.WatchEvery
	if every <= 0 {
		every = linkwatch.DefaultWatchEvery
	}
	return time.Since(time.Unix(0, at)) < every+linkwatch.DefaultWakeSlack
}

// errLinkDead reports a link on which nothing arrived for the liveness timeout.
var errLinkDead = errors.New("no traffic from the hub")

// Run runs the reconnect loop until ctx is canceled.
// Active tasks stay alive across transient controller disconnects and are re-adopted on reconnect.
func (d *Daemon) Run(ctx context.Context) error {
	defer d.link.stopped()
	if strings.TrimSpace(d.cfg.ControllerURL) == "" {
		return d.runLocal(ctx)
	}
	// A wake from sleep or a network change drops the link (or the wait for the next
	// attempt) and redials at once with the backoff reset.
	kick := make(chan string, 1)
	w := &linkwatch.Watcher{Every: d.WatchEvery, Addrs: d.WatchAddrs}
	go w.Watch(ctx, func(reason string) {
		select {
		case kick <- reason:
		default:
		}
	})

	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			d.shutdown()
			return ctx.Err()
		}

		d.helloAt = time.Time{}
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		var redial atomic.Pointer[string]
		attemptDone := make(chan struct{})
		go func() {
			for {
				select {
				case r := <-kick:
					if r == linkwatch.ReasonWake && d.linkedSinceWake() {
						continue // this link was made after the wake (it noticed first)
					}
					redial.Store(&r)
					cancelAttempt()
					return
				case <-attemptDone:
					return
				}
			}
		}()
		err := d.connectAndServe(ctx, attemptCtx)
		close(attemptDone)
		cancelAttempt()
		if ctx.Err() != nil {
			d.shutdown()
			return ctx.Err()
		}
		wasUp := !d.helloAt.IsZero()
		if r := redial.Load(); r != nil {
			err = fmt.Errorf("%s", *r)
			backoff = minBackoff
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
			err = errors.New("the hub asked to retry later (another live connection uses this identity)")
		case websocket.StatusCode(protocol.WSCloseCredentialRevoked):
			// Retry slowly: `konsertd join` (or the hub, over SSH) saves a new credential,
			// which the next attempt picks up.
			err = errors.New("credential revoked by the hub; re-join: konsertd join")
			if !d.reloadCredential() {
				backoff = maxBackoff
			}
		}

		var aerr *authError
		if errors.As(err, &aerr) {
			if !aerr.credSet && !d.reloadCredential() {
				// No credential: retrying cannot succeed. Exit with a clear error
				// instead of looping.
				d.logger.Error(aerr.Error())
				d.shutdown()
				return ErrCredentialRequired
			}
			// A revoked or replaced credential: keep retrying at the slowest backoff,
			// re-reading the saved credential (a re-join or re-provision replaces it).
			if aerr.credSet && !d.reloadCredential() {
				err = errors.New(aerr.Error() + "; re-join: konsertd join")
				backoff = maxBackoff
			}
		}

		// Reset backoff only after a session that got past worker.hello and lasted > 10 s.
		// The timestamp is taken after hello, so a slow or failing 15 s dial never counts
		// as a long session.
		if wasUp && time.Since(d.helloAt) > 10*time.Second {
			backoff = minBackoff
		}

		// Keep a host wake lock through short outages; drop it if the hub stays away.
		d.wake.Disconnected(d.WakeLockGrace)

		sleepDur := backoff + time.Duration(rand.Int64N(int64(backoff/4+1)))
		if redial.Load() != nil {
			sleepDur = 0
		}
		d.link.failed(err, sleepDur)

		timer := time.NewTimer(sleepDur)
		select {
		case <-ctx.Done():
			timer.Stop()
			d.shutdown()
			return ctx.Err()
		case <-timer.C:
			backoff = min(backoff*2, maxBackoff)
		case r := <-kick:
			timer.Stop()
			d.logger.Info(r + ": redialing now")
			backoff = minBackoff
		}
	}
}

// connectAndServe dials the hub and serves the connection until it ends. daemonCtx
// bounds what outlives the connection; connCtx ends this attempt.
func (d *Daemon) connectAndServe(daemonCtx, connCtx context.Context) error {
	headers := http.Header{}
	cred := d.credential()
	if cred != "" {
		headers.Set("Authorization", "Bearer "+cred)
	}

	lv := linkwatch.New(d.linkConfig(), nil)
	dialCtx, dialCancel := context.WithTimeout(connCtx, 15*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, d.cfg.ControllerURL, &websocket.DialOptions{
		HTTPHeader:     headers,
		OnPingReceived: func(context.Context, []byte) bool { lv.Received(); return true },
		OnPongReceived: func(context.Context, []byte) { lv.Received() },
	})
	dialCancel()
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return &authError{status: resp.StatusCode, credSet: cred != "", err: err}
		}
		return fmt.Errorf("dial %s: %w", d.cfg.ControllerURL, err)
	}
	return d.serveConn(daemonCtx, connCtx, conn, linkPeer{transport: "outbound", addr: d.cfg.ControllerURL}, lv)
}

// linkPeer describes the other end of a hub link (for logs and `konsertd status`).
type linkPeer struct {
	transport string // outbound, ssh or local
	addr      string
}

// serveConn speaks the worker protocol on one hub connection, outbound or attached,
// until it ends. daemonCtx bounds what outlives the connection (tasks it starts);
// connParent ends the connection (a newer attach supersedes an older one). lv tracks the
// link's liveness: the caller wires the connection's ping and pong callbacks to it.
func (d *Daemon) serveConn(daemonCtx, connParent context.Context, conn *websocket.Conn, peer linkPeer, lv *linkwatch.Liveness) error {
	ctx := daemonCtx
	conn.SetReadLimit(20 * 1024 * 1024)
	defer conn.Close(websocket.StatusNormalClosure, "shutdown")
	go PruneScratchAttachments("", AttachmentTTL)

	connCtx, connCancel := context.WithCancel(connParent)
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
	lv.Sent()
	d.helloAt = time.Now()
	d.helloWall.Store(d.helloAt.UnixNano())
	// Report the host wake lock state right after hello so the hub knows it after every
	// reconnect (the writer below sends queued control messages after hello).
	d.wake.Connected()
	d.sendJSON(protocol.MsgHostWakeLockState, d.wake.State())
	// The hub keeps session activity in memory only: re-send what it may have missed.
	d.activity.Prune(activityKeep)
	for _, a := range d.activity.Snapshot() {
		d.sendJSON(protocol.MsgSessionActivity, a)
	}
	d.link.up(peer.transport, peer.addr)
	d.logger.Debug("worker.hello sent",
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
			if err := conn.Write(wCtx, msg.msgType, msg.data); err != nil {
				return err
			}
			lv.Sent()
			return nil
		}); err != nil && connCtx.Err() == nil {
			d.logger.Debug("write to the hub failed", "err", err)
		}
		connCancel()
	}()

	// Liveness: when this side has been quiet, the heartbeat goroutine sends a vitals
	// heartbeat (the hub pings when it is quiet, so a live hub is always heard from);
	// when nothing arrived for LinkDead, the link is dead.
	hbKick := make(chan struct{}, 1)
	var dead atomic.Bool
	go lv.Monitor(connCtx, func() {
		select {
		case hbKick <- struct{}{}:
		default:
		}
	}, func() {
		dead.Store(true)
		connCancel()
	})

	// hello carried cached facts; send fresh ones as soon as they are collected.
	if cached {
		go func() {
			facts := d.refreshFacts(connCtx)
			if connCtx.Err() == nil {
				d.sendJSON(protocol.MsgWorkerFacts, protocol.WorkerFactsPayload{Facts: facts})
			}
		}()
	}

	go d.heartbeatLoop(connCtx, hbKick, lv.Config().QuietAfter*4)

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

	// Listening ports every portsEvery (and on request: worker.ports.request).
	go func() {
		ticker := time.NewTicker(portsEvery)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-ticker.C:
				d.sendPorts(connCtx, "")
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
			if dead.Load() {
				return fmt.Errorf("%w for %s", errLinkDead, lv.Config().DeadAfter)
			}
			return err
		}
		lv.Received()

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

// heartbeatLoop sends vitals heartbeats: every HeartbeatInterval while sessions run or
// when the set of active tasks changed, when the liveness monitor reports a quiet link
// (kick), and at least every maxIdle.
func (d *Daemon) heartbeatLoop(ctx context.Context, kick <-chan struct{}, maxIdle time.Duration) {
	every := d.HeartbeatInterval
	if every <= 0 {
		every = 10 * time.Second
	}
	var lastIDs []string
	var lastAt time.Time
	send := func(ids []string, active bool) {
		now := time.Now()
		m := d.metrics.collect(ctx, active, now)
		if ctx.Err() != nil {
			return
		}
		d.sendJSON(protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{
			ActiveTaskIDs: ids,
			LoadAvg:       m.LoadAvg,
			Metrics:       m,
		})
		lastIDs, lastAt = ids, now
	}
	ids := d.runner.ActiveTaskIDs()
	send(ids, d.sessionsActive(ids))
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ids := d.runner.ActiveTaskIDs()
			active := d.sessionsActive(ids)
			if active || !slices.Equal(ids, lastIDs) || time.Since(lastAt) >= maxIdle {
				send(ids, active)
			}
		case <-kick:
			ids := d.runner.ActiveTaskIDs()
			send(ids, d.sessionsActive(ids))
		}
	}
}

// sessionsActive reports whether anything is running: a PTY task or an agent chat turn.
func (d *Daemon) sessionsActive(activeTaskIDs []string) bool {
	if len(activeTaskIDs) > 0 {
		return true
	}
	if d.chat == nil {
		return false
	}
	for _, c := range d.chat.Chats() {
		if c.TurnActive {
			return true
		}
	}
	return false
}

// sendPorts collects the listening ports and sends them (as the reply to request reqID,
// or unsolicited when reqID is "").
func (d *Daemon) sendPorts(ctx context.Context, reqID string) {
	ports := collectPorts(ctx)
	if ctx.Err() != nil {
		return
	}
	if ports == nil {
		ports = []protocol.ListeningPort{}
	}
	d.sendJSONWithID(protocol.MsgWorkerPorts, reqID, protocol.WorkerPortsPayload{Ports: ports, CollectedAt: time.Now().UTC()})
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

	case protocol.MsgWorkerPortsRequest:
		go d.sendPorts(daemonCtx, env.ID)

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
