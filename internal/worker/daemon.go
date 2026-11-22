package worker

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/gitutil"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/drivers"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

type outboundMsg struct {
	msgType websocket.MessageType
	data    []byte
}

// Daemon connects outbound to konsert, handles reconnects with exponential backoff,
// sends heartbeats, and bridges controller commands to Runner.
type Daemon struct {
	cfg      config.WorkerConfig
	registry *drivers.Registry
	runner   *Runner
	logger   *slog.Logger

	sendMu sync.RWMutex
	sendCh chan outboundMsg

	// Test hook: optional custom heartbeat interval (defaults to 10s).
	HeartbeatInterval time.Duration
}

// NewDaemon constructs a worker daemon from WorkerConfig.
func NewDaemon(cfg config.WorkerConfig, registry *drivers.Registry, logger *slog.Logger) *Daemon {
	if registry == nil {
		registry = drivers.DefaultRegistry
	}
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = uuid.NewString()
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
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 2
	}

	d := &Daemon{
		cfg:               cfg,
		registry:          registry,
		logger:            logger,
		HeartbeatInterval: 10 * time.Second,
	}

	d.runner = NewRunner(registry, cfg.Sandbox, cfg.AllowedRoots, RunnerCallbacks{
		OnTaskStatus: func(p protocol.TaskStatusPayload) {
			d.sendJSON(protocol.MsgTaskStatus, p)
		},
		OnTaskEvent: func(p protocol.TaskEventPayload) {
			d.sendJSON(protocol.MsgTaskEvent, p)
		},
		OnPtyFrame: func(frameType byte, taskID string, data []byte) {
			frame, err := protocol.EncodeFrame(frameType, taskID, data)
			if err != nil {
				return
			}
			d.enqueue(outboundMsg{msgType: websocket.MessageBinary, data: frame})
		},
	}, logger)

	return d
}

// WorkerID returns the stable ID for this worker daemon.
func (d *Daemon) WorkerID() string {
	return d.cfg.WorkerID
}

// Runner returns the underlying task runner (useful for testing).
func (d *Daemon) Runner() *Runner {
	return d.runner
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

		connectedAt := time.Now()
		err := d.connectAndServe(ctx)
		if ctx.Err() != nil {
			d.runner.ShutdownAll()
			return ctx.Err()
		}

		// Reset backoff if we stayed connected for > 10 seconds.
		if time.Since(connectedAt) > 10*time.Second {
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
	conn, _, err := websocket.Dial(dialCtx, d.cfg.ControllerURL, &websocket.DialOptions{
		HTTPHeader: headers,
	})
	dialCancel()
	if err != nil {
		return fmt.Errorf("dial %s: %w", d.cfg.ControllerURL, err)
	}
	conn.SetReadLimit(4 * 1024 * 1024)
	defer conn.Close(websocket.StatusNormalClosure, "shutdown")

	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	sendCh := make(chan outboundMsg, 512)
	d.sendMu.Lock()
	d.sendCh = sendCh
	d.sendMu.Unlock()

	defer func() {
		d.sendMu.Lock()
		if d.sendCh == sendCh {
			d.sendCh = nil
		}
		d.sendMu.Unlock()
	}()

	// Send worker.hello immediately with current capabilities and any active in-flight tasks.
	permMode := protocol.PermissionFullAccess
	if d.cfg.Sandbox {
		permMode = protocol.PermissionSandboxed
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
			Agents:         d.registry.DetectInstalled(d.cfg.EnableMockDriver),
			Tags:           d.cfg.Tags,
			MaxConcurrent:  d.cfg.MaxConcurrent,
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
	d.logger.Info("connected to controller",
		"url", d.cfg.ControllerURL,
		"worker_id", d.cfg.WorkerID,
		"agents", hello.Worker.Agents,
		"permission_mode", permMode,
		"active_tasks", len(hello.ActiveTasks),
	)

	// Writer goroutine
	writeErrCh := make(chan error, 1)
	go func() {
		for {
			select {
			case <-connCtx.Done():
				writeErrCh <- connCtx.Err()
				return
			case msg := <-sendCh:
				wCtx, wCancel := context.WithTimeout(connCtx, 10*time.Second)
				err := conn.Write(wCtx, msg.msgType, msg.data)
				wCancel()
				if err != nil {
					writeErrCh <- err
					connCancel()
					return
				}
			}
		}
	}()

	// Heartbeat goroutine (send initial heartbeat immediately for live CPU/RAM/ports, then on interval)
	go func() {
		sendHB := func() {
			metrics := CollectMetrics(connCtx)
			d.sendJSON(protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{
				ActiveTaskIDs: d.runner.ActiveTaskIDs(),
				LoadAvg:       metrics.LoadAvg,
				Metrics:       metrics,
			})
		}
		sendHB()
		interval := d.HeartbeatInterval
		if interval <= 0 {
			interval = 10 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-ticker.C:
				sendHB()
			}
		}
	}()

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
		if err := d.runner.StartTask(daemonCtx, p); err != nil {
			d.logger.Error("start task failed", "task_id", p.TaskID, "err", err)
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

func (d *Daemon) enqueue(msg outboundMsg) {
	d.sendMu.RLock()
	ch := d.sendCh
	d.sendMu.RUnlock()
	if ch == nil {
		return
	}
	select {
	case ch <- msg:
	default:
		// If outbound queue is full, drop live PTY frame rather than stalling worker.
		if msg.msgType == websocket.MessageText {
			// Best-effort block briefly for control messages.
			select {
			case ch <- msg:
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}
