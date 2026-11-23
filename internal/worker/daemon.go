package worker

import (
	"context"
	"encoding/base64"
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
		HeartbeatInterval: 10 * time.Second,
		FactsInterval:     5 * time.Minute,
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
		if websocket.CloseStatus(err) == websocket.StatusCode(protocol.WSCloseWorkerRemoved) {
			d.logger.Info("machine was removed from fleet by controller; stopping daemon", "worker_id", d.cfg.WorkerID)
			d.runner.ShutdownAll()
			return nil
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
	conn.SetReadLimit(20 * 1024 * 1024)
	defer conn.Close(websocket.StatusNormalClosure, "shutdown")
	go PruneScratchAttachments("", AttachmentTTL)

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
			Agents:         d.registry.DetectInstalled(d.cfg.EnableMockDriver),
			Tags:           d.cfg.Tags,
			MaxConcurrent:  d.cfg.MaxConcurrent,
			Facts:          &initialFacts,
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
	go func() {
		disc := NewSessionDiscovery("", nil, func(p protocol.ExternalSessionPayload) {
			d.sendJSON(protocol.MsgExternalSession, p)
		})
		disc.Run(connCtx)
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
