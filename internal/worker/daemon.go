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
	"github.com/BryanSant/konsert/internal/worker/drivers"
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

	// machineID is the persisted per-installation UUID sent as WorkerInfo.MachineID.
	machineID string

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

	// Test hooks: optional custom heartbeat & facts refresh intervals.
	HeartbeatInterval time.Duration
	FactsInterval     time.Duration
}

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
	if cfg.WorkerID == "" {
		cfg.WorkerID = ResolveStableWorkerID("", cfg.Name, cfg.HostGroup, cfg.Isolation)
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 2
	}

	d := &Daemon{
		cfg:               cfg,
		registry:          registry,
		logger:            logger,
		machineID:         ResolveMachineID("", cfg.Name, cfg.Isolation),
		outbox:            newStatusOutbox(outboxPath(defaultStateDir(), cfg.WorkerID), logger),
		HeartbeatInterval: 10 * time.Second,
		FactsInterval:     5 * time.Minute,
	}

	d.runner = NewRunner(registry, cfg.Sandbox, cfg.AllowedRoots, RunnerCallbacks{
		OnTaskStatus: func(p protocol.TaskStatusPayload) {
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

	d.discovery = NewSessionDiscovery("", d.runner.IsManagedPID, func(p protocol.ExternalSessionPayload) {
		d.sendJSON(protocol.MsgExternalSession, p)
	})

	return d
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

// MachineID returns the persisted per-installation UUID sent in worker.hello.
func (d *Daemon) MachineID() string {
	return d.machineID
}

// WorkerID returns the stable ID for this worker daemon.
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
			d.runner.ShutdownAll()
			return ctx.Err()
		}

		d.helloAt = time.Time{}
		err := d.connectAndServe(ctx)
		if ctx.Err() != nil {
			d.runner.ShutdownAll()
			return ctx.Err()
		}
		// Only an operator removal (4001) stops the daemon. Every other close, including a
		// normal 1000 (superseded connection) and 1013 (try again later: another live
		// connection holds this identity), reconnects with backoff.
		switch websocket.CloseStatus(err) {
		case websocket.StatusCode(protocol.WSCloseWorkerRemoved):
			d.logger.Info("machine was removed from fleet by controller; stopping daemon", "worker_id", d.cfg.WorkerID)
			d.runner.ShutdownAll()
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
				d.runner.ShutdownAll()
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

		jitter := time.Duration(rand.Int64N(int64(backoff/4 + 1)))
		sleepDur := backoff + jitter
		d.logger.Warn("disconnected from controller; reconnecting", "err", err, "backoff", sleepDur)

		select {
		case <-ctx.Done():
			d.runner.ShutdownAll()
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
	d.sendMu.Lock()
	d.queues = queues
	d.sendMu.Unlock()

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
	initialFacts := CollectMachineFacts(connCtx)
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
			MachineID:      d.machineID,
			Version:        d.version,
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

	// Periodic Machine Facts + Subscription Quota refresher (keeps cached facts & quotas fresh without bloating heartbeats).
	go func() {
		// Push initial subscription quotas right away if present.
		if qs := CollectSubscriptionQuotas(connCtx, "", d.cfg.WorkerID); len(qs) > 0 {
			d.sendJSON(protocol.MsgWorkerQuotas, protocol.WorkerQuotasPayload{Quotas: qs})
		}
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
				facts := CollectMachineFacts(connCtx)
				d.sendJSON(protocol.MsgWorkerFacts, protocol.WorkerFactsPayload{Facts: facts})
				if qs := CollectSubscriptionQuotas(connCtx, "", d.cfg.WorkerID); len(qs) > 0 {
					d.sendJSON(protocol.MsgWorkerQuotas, protocol.WorkerQuotasPayload{Quotas: qs})
				}
			}
		}
	}()

	// Lightweight Hook + Passive Session Discovery (zero blind spots for externally SSH-launched agents).
	go d.discovery.Run(connCtx)

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
