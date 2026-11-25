package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/install"
	"github.com/BryanSant/syre/internal/sshutil"
	"github.com/BryanSant/syre/internal/store"
)

// The hub starts syred itself on the machines it was told to manage: its own host
// ("local", via `syred install`) and SSH hosts ("ssh", via
// sshutil.BootstrapRemoteWorker, which runs the remote `syred install` and allows the
// hub key). Both run syred without a controller URL, serving the hub on its local
// socket, and the hub attaches to them (transport.go). A machine row records how
// (workers.launch), so the hub restarts a configured daemon that is down on a host it
// can reach (restart.go). It never starts a second daemon for a machine that is
// connected.

// setupCompletedSetting marks first-run setup as done (a host was added, or the user
// dismissed the Add Machine dialog), so the web app stops opening it automatically.
const setupCompletedSetting = "setup_completed"

// InstallDryRunEnv, when set to a true value, keeps the hub from registering any service:
// starting the local syred runs `syred install --dry-run` (which validates the
// arguments and changes nothing) and then runs the daemon as a child process for the
// hub's lifetime. The e2e suite sets it so tests never install a real service.
const InstallDryRunEnv = install.DryRunEnv

// launcher holds the hub's process-starting dependencies; tests replace them.
type launcher struct {
	// runner runs local commands (`syred id`, `syred install`).
	runner install.Runner
	// findSyred locates this machine's syred binary ("" when there is none).
	findSyred func() string
	// bootstrap runs the SSH install/start path.
	bootstrap func(context.Context, sshutil.BootstrapOptions) sshutil.BootstrapResult
	// uninstallRemote runs `syred uninstall --worker <id> --hub <hub id>` on a removed
	// ssh machine over the user's SSH (target, hub id, worker id) and returns its output.
	uninstallRemote func(ctx context.Context, target, hubID, workerID string) (string, error)
	// startChild runs a local daemon as a hub child process (dry-run mode only), with
	// stdin as its standard input.
	startChild func(ctx context.Context, exe string, args []string, stdin string) error
	hostname   func() string
	// connectTimeout bounds the wait for a started daemon's worker.hello.
	connectTimeout time.Duration
	dryRun         bool

	mu       sync.Mutex
	inFlight map[string]bool // worker id -> a start is running
}

func newLauncher() *launcher {
	return &launcher{
		runner:          install.ExecRunner{},
		findSyred:       func() string { return sshutil.FindSyred(runtime.GOOS, runtime.GOARCH) },
		bootstrap:       sshutil.BootstrapRemoteWorker,
		uninstallRemote: sshutil.UninstallRemote,
		startChild:      startChildDaemon,
		hostname:        func() string { h, _ := os.Hostname(); return h },
		connectTimeout:  45 * time.Second,
		dryRun:          envTrue(os.Getenv(InstallDryRunEnv)),
		inFlight:        map[string]bool{},
	}
}

func envTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// begin claims the right to start workerID; false if a start is already running.
func (l *launcher) begin(workerID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight[workerID] {
		return false
	}
	l.inFlight[workerID] = true
	return true
}

func (l *launcher) end(workerID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.inFlight, workerID)
}

// starting reports whether a start of workerID is running.
func (l *launcher) starting(workerID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inFlight[workerID]
}

// startChildDaemon runs syred as a child of the hub until ctx ends (dry-run mode).
func startChildDaemon(ctx context.Context, exe string, args []string, stdin string) error {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// isLoopbackAddr reports whether a host:port (or bare host) peer address is loopback.
func isLoopbackAddr(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// sameHost compares hostnames, ignoring case and any domain suffix.
func sameHost(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if strings.EqualFold(a, b) {
		return true
	}
	sa, _, _ := strings.Cut(a, ".")
	sb, _, _ := strings.Cut(b, ".")
	return strings.EqualFold(sa, sb)
}

// localConnectedWorker returns the id of a connected daemon on the hub's own machine:
// wantID if it is connected, else one that connected over loopback and reports this
// hostname (as its OS hostname or its name), which covers a daemon run with --name.
func (h *Hub) localConnectedWorker(wantID, hostname string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if wantID != "" {
		if _, ok := h.workers[wantID]; ok {
			return wantID
		}
	}
	for id, wc := range h.workers {
		if wc.loopback && (sameHost(wc.hostname, hostname) || sameHost(wc.name, hostname) || sameHost(wc.hostGroup, hostname)) {
			return id
		}
	}
	return ""
}

// listenAddr is the address the hub's HTTP server is bound to (SetListenAddr), else
// the configured one.
func (s *Server) listenAddr() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if s.boundAddr != "" {
		return s.boundAddr
	}
	return s.cfg.ListenAddr
}

// SetListenAddr records the address the HTTP server listens on (join links use its
// port).
func (s *Server) SetListenAddr(addr string) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	s.boundAddr = addr
}

// syredNotFoundFix tells the user how to make syred available to the hub.
func syredNotFoundFix() string {
	exe := "syred"
	if runtime.GOOS == "windows" {
		exe = "syred.exe"
	}
	return fmt.Sprintf("Put %s next to the syre binary or on the hub's PATH (make build puts both in bin/), then reload.", exe)
}

var uuidRe = regexp.MustCompile(`(?m)^\s*([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\s*$`)

// LocalHost describes the hub's own machine for first-run setup.
type LocalHost struct {
	Hostname   string `json:"hostname"`
	SyredPath  string `json:"syred_path,omitempty"`
	SyredFound bool   `json:"syred_found"`
	// Fix explains how to make syred available when it was not found.
	Fix string `json:"fix,omitempty"`
	// WorkerID is the identity a local daemon has (or will have): `syred id`, or the
	// id of a connected daemon on this machine.
	WorkerID  string `json:"worker_id,omitempty"`
	WorkerSID string `json:"worker_sid,omitempty"`
	// Connected: a daemon on this machine is connected, so adding it only attaches.
	Connected bool `json:"connected"`
	// Added: the hub already starts this machine's daemon (launch = local).
	Added       bool   `json:"added"`
	LaunchState string `json:"launch_state,omitempty"`
	LaunchError string `json:"launch_error,omitempty"`
	// Error reports a failing `syred id`.
	Error string `json:"error,omitempty"`
}

// detectLocal finds this machine's syred and its identity, and whether it is running.
func (s *Server) detectLocal(ctx context.Context) LocalHost {
	l := s.launch
	v := LocalHost{Hostname: l.hostname()}
	v.SyredPath = l.findSyred()
	v.SyredFound = v.SyredPath != ""
	if !v.SyredFound {
		v.Fix = syredNotFoundFix()
	} else {
		idCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, err := l.runner.Run(idCtx, "", v.SyredPath, "id", "--name", v.Hostname)
		cancel()
		if m := uuidRe.FindStringSubmatch(out); err == nil && m != nil {
			v.WorkerID = strings.ToLower(m[1])
		} else if err != nil {
			v.Error = fmt.Sprintf("syred id failed (is %s older than this hub?): %v", v.SyredPath, err)
		} else {
			v.Error = fmt.Sprintf("syred id printed no worker id: %q", strings.TrimSpace(out))
		}
	}
	if id := s.hub.localConnectedWorker(v.WorkerID, v.Hostname); id != "" {
		v.WorkerID, v.Connected = id, true
	}
	if v.WorkerID != "" {
		if w, err := s.store.GetWorker(ctx, v.WorkerID); err == nil {
			v.WorkerSID = w.SID
			v.Added = w.Launch == store.LaunchLocal
			v.LaunchState, v.LaunchError = w.LaunchState, w.LaunchError
		}
	}
	return v
}

// SetupStatus is GET /api/setup.
type SetupStatus struct {
	// NeedsSetup: first run (setup neither completed nor dismissed, and no machines).
	NeedsSetup bool      `json:"needs_setup"`
	Local      LocalHost `json:"local"`
}

func (s *Server) handleGetSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	saved, err := s.store.GetAllSettings(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, SetupStatus{
		NeedsSetup: saved[setupCompletedSetting] != "true" && len(workers) == 0,
		Local:      s.detectLocal(ctx),
	})
}

func (s *Server) markSetupCompleted(ctx context.Context) {
	if err := s.store.SetSettings(ctx, map[string]string{setupCompletedSetting: "true"}); err != nil {
		s.logger.Warn("could not record first-run setup as completed", "err", err)
	}
}

func (s *Server) handleDismissSetup(w http.ResponseWriter, r *http.Request) {
	s.markSetupCompleted(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAddLocalWorker adds the hub's own machine: it attaches a daemon that is already
// connected, or starts one with `syred install`. The start runs in the background;
// fleet updates report its launch_state until the machine is online.
func (s *Server) handleAddLocalWorker(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := s.detectLocal(ctx)
	if v.Connected {
		row, err := s.store.EnsureWorker(ctx, store.Worker{ID: v.WorkerID, Launch: store.LaunchLocal})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.forgetRemoval(ctx, row.ID)
		s.markSetupCompleted(ctx)
		s.hub.broadcastWorkerView(ctx, row.ID)
		writeJSON(w, http.StatusOK, map[string]any{"worker_id": row.ID, "worker_sid": row.SID, "state": "connected"})
		return
	}
	if !v.SyredFound {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "syred not found on this machine. " + v.Fix})
		return
	}
	if v.WorkerID == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": v.Error})
		return
	}
	row, err := s.store.EnsureWorker(ctx, store.Worker{
		ID: v.WorkerID, Name: v.Hostname, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Launch: store.LaunchLocal,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.forgetRemoval(ctx, row.ID)
	s.markSetupCompleted(ctx)
	if err := s.startWorker(ctx, row); err != nil && !errors.Is(err, errAlreadyStarting) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"worker_id": row.ID, "worker_sid": row.SID, "state": store.LaunchStateStarting})
}

// handleStartWorker retries starting a configured machine's daemon.
func (s *Server) handleStartWorker(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	row, err := s.store.GetWorker(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "worker not found"})
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if row.Launch == store.LaunchManual {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the hub does not start this machine's syred (it was started by hand); start it on that machine"})
		return
	}
	if s.hub.IsWorkerConnected(row.ID) {
		writeJSON(w, http.StatusOK, map[string]any{"worker_id": row.ID, "state": "connected"})
		return
	}
	switch err := s.startWorker(ctx, row); {
	case errors.Is(err, errAlreadyStarting):
		writeJSON(w, http.StatusAccepted, map[string]any{"worker_id": row.ID, "state": store.LaunchStateStarting})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"worker_id": row.ID, "state": store.LaunchStateStarting})
	}
}

var errAlreadyStarting = errors.New("a start is already in progress for this machine")

// startWorker starts w's daemon in the background according to w.Launch, unless it is
// connected or already being started. Progress is recorded in launch_state and pushed
// to browsers.
func (s *Server) startWorker(ctx context.Context, w store.Worker) error {
	if s.hub.IsWorkerConnected(w.ID) {
		return nil
	}
	if !s.launch.begin(w.ID) {
		return errAlreadyStarting
	}
	s.setLaunchState(ctx, w.ID, store.LaunchStateStarting, "")
	started := s.hub.goBackground(func(hctx context.Context) {
		defer s.launch.end(w.ID)
		waitID, deferred, err := w.ID, "", error(nil)
		switch w.Launch {
		case store.LaunchLocal:
			err = s.startLocal(hctx, w)
		case store.LaunchSSH:
			waitID, deferred, err = s.startSSH(hctx, w)
		default:
			err = fmt.Errorf("unknown launch mode %q", w.Launch)
		}
		if err != nil {
			s.logger.Warn("starting syred failed", "worker_id", w.ID, "launch", w.Launch, "err", err)
			s.setLaunchState(hctx, waitID, store.LaunchStateFailed, err.Error())
			return
		}
		// The daemon is coming up: attach now rather than after the loop's backoff.
		s.attach.poke(waitID)
		s.awaitConnect(hctx, waitID, deferred)
	})
	if !started {
		s.launch.end(w.ID)
		return errors.New("hub is shutting down")
	}
	return nil
}

// awaitConnect waits (bounded) for workerID's hello and records the outcome. deferred,
// when set, is the reason a missing connection is expected (a Windows logon task that
// starts at the next login).
func (s *Server) awaitConnect(ctx context.Context, workerID, deferred string) {
	deadline := time.NewTimer(s.launch.connectTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if s.hub.IsWorkerConnected(workerID) {
			// The hello already cleared launch_state; make sure a race cannot leave it.
			if w, err := s.store.GetWorker(ctx, workerID); err == nil && w.LaunchState != store.LaunchStateNone {
				s.setLaunchState(ctx, workerID, store.LaunchStateNone, "")
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			msg := deferred
			if msg == "" {
				msg = fmt.Sprintf("syred was started but has not connected within %s; check `syred status` and its logs on that machine", s.launch.connectTimeout)
			}
			s.setLaunchState(ctx, workerID, store.LaunchStateFailed, msg)
			return
		case <-tick.C:
		}
	}
}

func (s *Server) setLaunchState(ctx context.Context, workerID, state, msg string) {
	if err := s.store.SetWorkerLaunchState(ctx, workerID, state, msg, time.Now().UTC()); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.logger.Warn("record launch state failed", "worker_id", workerID, "err", err)
		}
		return
	}
	s.hub.broadcastWorkerView(ctx, workerID)
}

// startLocal runs `syred install` for the hub's own machine (idempotent: a reinstall
// updates and restarts the existing registration). The daemon serves the hub on its
// local socket, which the hub dials directly: no credential, no SSH.
func (s *Server) startLocal(ctx context.Context, w store.Worker) error {
	l := s.launch
	exe := l.findSyred()
	if exe == "" {
		return errors.New("syred not found on this machine. " + syredNotFoundFix())
	}
	name := w.Name
	if name == "" {
		name = l.hostname()
	}
	args := []string{"install", "--name", name}
	if l.dryRun {
		args = append(args, "--dry-run")
	}
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := l.runner.Run(runCtx, "", exe, args...); err != nil {
		return fmt.Errorf("syred install: %w", err)
	}
	if l.dryRun {
		// Dry run: nothing was registered; run the daemon as a hub child instead.
		if err := l.startChild(ctx, exe, []string{"run", "--name", name}, ""); err != nil {
			return err
		}
	}
	s.attach.ensure(w)
	return nil
}

// startSSH re-runs the SSH install/start path for w (the binary is copied when the host
// lacks it or has another build) and re-allows the hub key. It returns the id to wait for
// (the remote daemon's, normally w.ID) and, for a Windows logon task that starts at the
// next login, why it may not connect.
func (s *Server) startSSH(ctx context.Context, w store.Worker) (string, string, error) {
	if w.SSHTarget == "" {
		return w.ID, "", errors.New("no SSH target recorded for this machine; add it again from Add machine")
	}
	grant, err := s.hubKeyGrant(ctx)
	if err != nil {
		return w.ID, "", err
	}
	res := s.launch.bootstrap(ctx, sshutil.BootstrapOptions{Target: w.SSHTarget, HubKey: grant})
	if !res.OK {
		msg := res.Error
		if res.Stage != "" {
			msg = "[" + res.Stage + "] " + msg
		}
		return w.ID, "", errors.New(msg)
	}
	waitID := w.ID
	if res.WorkerID != "" && res.WorkerID != w.ID {
		// The remote identity changed (its state dir was reset): track the new machine.
		if _, err := s.recordSSHWorker(ctx, res); err != nil {
			return w.ID, "", err
		}
		s.setLaunchState(ctx, w.ID, store.LaunchStateFailed,
			fmt.Sprintf("syred on %s now runs with a different identity (%s); remove this machine", w.SSHTarget, res.WorkerID))
		// The old record is no longer started by the hub (launch, target and URL cleared).
		if _, err := s.store.EnsureWorker(ctx, store.Worker{ID: w.ID, Launch: store.LaunchManual}); err != nil {
			return w.ID, "", err
		}
		waitID = res.WorkerID
		s.setLaunchState(ctx, waitID, store.LaunchStateStarting, "")
	}
	if nw, err := s.store.GetWorker(ctx, waitID); err == nil {
		s.attach.ensure(nw)
	}
	return waitID, deferredMessage(res), nil
}

func deferredMessage(res sshutil.BootstrapResult) string {
	if !res.StartDeferred {
		return ""
	}
	return "installed; syred will start at the next Windows login of the SSH user (a logon task only runs while that user is signed in). " + install.HeadlessHint
}

// recordSSHWorker stores the machine an SSH bootstrap installed, so the hub knows it
// (offline) before it attaches and restarts it later.
func (s *Server) recordSSHWorker(ctx context.Context, res sshutil.BootstrapResult) (store.Worker, error) {
	s.forgetRemoval(ctx, res.WorkerID)
	return s.store.EnsureWorker(ctx, store.Worker{
		ID: res.WorkerID, Name: res.Name, OS: res.RemoteOS, Arch: res.RemoteArch,
		Launch: store.LaunchSSH, SSHTarget: res.Target,
	})
}

// afterSSHBootstrap records a successful bootstrap, starts attaching to the daemon and
// tracks it until it connects (or the wait times out), so the machine list shows its
// state.
func (s *Server) afterSSHBootstrap(ctx context.Context, res sshutil.BootstrapResult) {
	if !res.OK || res.WorkerID == "" {
		return
	}
	row, err := s.recordSSHWorker(ctx, res)
	if err != nil {
		s.logger.Warn("record SSH-bootstrapped worker failed", "target", res.Target, "err", err)
		return
	}
	s.markSetupCompleted(ctx)
	s.attach.ensure(row)
	if s.hub.IsWorkerConnected(row.ID) {
		s.hub.broadcastWorkerView(ctx, row.ID)
		return
	}
	if !s.launch.begin(row.ID) {
		return
	}
	s.setLaunchState(ctx, row.ID, store.LaunchStateStarting, "")
	deferred := deferredMessage(res)
	if !s.hub.goBackground(func(hctx context.Context) {
		defer s.launch.end(row.ID)
		s.awaitConnect(hctx, row.ID, deferred)
	}) {
		s.launch.end(row.ID)
	}
}

// StartLaunchReconciler attaches to every ssh and local machine right away. Daemons
// that are down are restarted by the attach loops (restart.go), but not before grace
// has passed: it lets running daemons attach (or reconnect) first. After grace it also
// clears launch states a previous hub run left behind.
func (s *Server) StartLaunchReconciler(grace time.Duration) {
	s.hub.restarts.holdUntil(time.Now().Add(grace))
	s.hub.goBackground(s.removalLoop)
	s.hub.goBackground(func(ctx context.Context) {
		s.startAttachers(ctx)
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.reconcileLaunches(ctx)
	})
}

// reconcileLaunches clears stale launch states: a connected machine has none, and a
// "starting" with no start running is left over from an earlier hub run.
func (s *Server) reconcileLaunches(ctx context.Context) {
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		s.logger.Warn("launch reconciler: list workers failed", "err", err)
		return
	}
	for _, w := range workers {
		if w.Launch == store.LaunchManual || w.LaunchState == store.LaunchStateNone {
			continue
		}
		if s.hub.IsWorkerConnected(w.ID) || (w.LaunchState == store.LaunchStateStarting && !s.launch.starting(w.ID)) {
			s.setLaunchState(ctx, w.ID, store.LaunchStateNone, "")
		}
	}
}
