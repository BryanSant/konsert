package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/hubkey"
	"github.com/BryanSant/konsert/internal/install"
	"gopkg.in/yaml.v3"
)

// Removing a machine uninstalls konsertd from it (KON-AV0YT). Both the hub (running
// `konsertd uninstall --worker <id>` over the user's SSH, or locally for its own
// machine) and the daemon itself (on close code 4001, or at start when a removal was
// interrupted) end up in Uninstaller.RemoveIdentity, which removes everything installed
// for one konsertd identity and nothing of any other identity on the same account.
//
// Several hosts can share one home directory (a container, its siblings and its host),
// so ~/.konsert, the konsertd config directory and ~/.claude/settings.json are shared.
// What belongs to an identity is found by worker id, never by "the default instance":
// each installed config (konsertd*.yaml in the config directory, and the nohup configs
// in the state directory) names the worker it runs, whose id is in
// worker-id-<name>-<isolation>. The agent hooks are shared by every identity on the
// account, so they go only with the last installed one.

// Uninstaller removes what konsertd installed for one identity.
type Uninstaller struct {
	Installer *install.Installer
	// StateDir is konsertd's state directory ("" = ~/.konsert or $KONSERT_STATE_DIR).
	StateDir string
	// Self is set when the daemon being removed runs the removal: services are removed
	// with the self-safe sequence, and its own nohup process is not signalled.
	Self bool
	// KeepHooks keeps the agent hooks even when no other identity needs them.
	KeepHooks bool
	// LockWait bounds the wait for another removal on this account (default 30 s).
	LockWait time.Duration

	// Test seams; nil uses the real implementation.
	RemoveHooks  func() ([]string, error)
	DropHubGrant func(hubID, workerID string) (path string, changed bool, err error)
	ProcArgs     func(pid int) (string, error)
	Kill         func(pid int) error
	// ServiceConfigs lists this account's native Windows services (--service);
	// RemoveService removes one (self: from inside the daemon it runs).
	ServiceConfigs func() ([]install.InstalledConfig, error)
	RemoveService  func(ctx context.Context, instance string, self bool) error
}

// UninstallReport says what a removal did.
type UninstallReport struct {
	WorkerID string
	// Found is set when something was installed for the identity.
	Found bool
	// Services counts the service registrations removed (not nohup daemons).
	Services int
	Removed  []string
	Kept     []string
}

// Lines renders the report for a terminal or the hub's audit log.
func (r UninstallReport) Lines() []string {
	var out []string
	for _, s := range r.Removed {
		out = append(out, "Removed "+s)
	}
	for _, s := range r.Kept {
		out = append(out, "Kept "+s)
	}
	if !r.Found {
		out = append(out, "Nothing installed for konsertd identity "+r.WorkerID+" (already removed)")
	}
	return out
}

func (u *Uninstaller) stateDir() string {
	if u.StateDir != "" {
		return u.StateDir
	}
	return defaultStateDir()
}

// installedIdentity is one installed daemon found on this account.
type installedIdentity struct {
	workerID string // "" when its identity file is missing
	name     string
	// instance and nohup locate it: a service instance, or a nohup config path.
	instance string
	config   string
	nohup    bool
	// service is a native Windows service (`install --service`), whose config is in
	// %ProgramData%\konsert. Install granted the account stop and delete on it, so
	// removing it needs no elevation (KON-28HHD).
	service bool
}

func (i installedIdentity) label() string {
	if i.name != "" {
		return i.name
	}
	return filepath.Base(i.config)
}

// installed lists every daemon installed on this account (all hosts sharing it).
func (u *Uninstaller) installed() ([]installedIdentity, error) {
	var out []installedIdentity
	dir := u.stateDir()
	configs, err := u.Installer.InstalledConfigs()
	if err != nil {
		return nil, err
	}
	for _, c := range configs {
		cfg, _ := readWorkerConfig(c.Path)
		id, _ := lookupWorkerIDIn(dir, cfg)
		out = append(out, installedIdentity{workerID: id, name: cfg.Name, instance: c.Instance, config: c.Path})
	}
	listServices := u.ServiceConfigs
	if listServices == nil {
		listServices = u.Installer.ServiceConfigs
	}
	services, err := listServices()
	if err != nil {
		return nil, err
	}
	for _, c := range services {
		cfg, _ := readWorkerConfig(c.Path)
		id, _ := lookupWorkerIDIn(dir, cfg)
		out = append(out, installedIdentity{workerID: id, name: cfg.Name, instance: c.Instance, config: c.Path, service: true})
	}
	if dir != "" {
		matches, _ := filepath.Glob(filepath.Join(dir, "konsertd-nohup-*.yaml"))
		for _, p := range matches {
			cfg, _ := readWorkerConfig(p)
			id, _ := lookupWorkerIDIn(dir, cfg)
			out = append(out, installedIdentity{workerID: id, name: cfg.Name, config: p, nohup: true})
		}
	}
	return out, nil
}

// readWorkerConfig reads an installed config the way `konsertd run` does: over the
// defaults, so an unset isolation is the detected one (a container's identity file is
// worker-id-<name>-container).
func readWorkerConfig(path string) (config.WorkerConfig, error) {
	cfg := config.DefaultWorkerConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	err = yaml.Unmarshal(data, &cfg)
	return cfg, err
}

// RemoveIdentity removes everything installed for workerID: its service or nohup
// daemon and config, its hub key grant (hubID given), its credential and per-identity
// state, and the agent hooks when no other identity on the account remains.
func (u *Uninstaller) RemoveIdentity(ctx context.Context, workerID, hubID string) (UninstallReport, error) {
	rep := UninstallReport{WorkerID: workerID}
	if !hubkey.ValidWorkerID(workerID) {
		return rep, fmt.Errorf("invalid worker id %q", workerID)
	}
	unlock, err := u.lock(ctx)
	if err != nil {
		return rep, err
	}
	defer unlock()

	var errs []error
	if hubID != "" && hubkey.ValidHubID(hubID) {
		drop := u.DropHubGrant
		if drop == nil {
			drop = hubkey.Uninstall
		}
		path, changed, err := drop(hubID, workerID)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("remove the hub key: %w", err))
		case changed:
			rep.Found = true
			rep.Removed = append(rep.Removed, "the hub key grant from "+path)
		}
	}

	all, err := u.installed()
	if err != nil {
		return rep, err
	}
	// The hooks are only ever removed along with an installed daemon: state files or a
	// key grant alone never touch the account's ~/.claude/settings.json.
	daemonFound := false
	for _, it := range all {
		if it.workerID != workerID {
			continue
		}
		rep.Found, daemonFound = true, true
		if it.nohup {
			if err := u.removeNohup(it); err != nil {
				errs = append(errs, err)
				continue
			}
			rep.Removed = append(rep.Removed, "the nohup daemon "+it.label()+" ("+it.config+")")
			continue
		}
		var err error
		kind := serviceKind()
		switch {
		case it.service:
			kind = "Windows service"
			err = u.removeService(ctx, it.instance)
		case u.Self:
			err = u.Installer.UninstallSelf(ctx, it.instance)
		default:
			err = u.Installer.Uninstall(ctx, it.instance)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("uninstall %s: %w", it.label(), err))
			continue
		}
		rep.Services++
		rep.Removed = append(rep.Removed, "the "+kind+" for "+it.label())
	}

	for _, p := range u.identityState(workerID) {
		if removed, err := removeIfExists(p); err != nil {
			errs = append(errs, err)
		} else if removed {
			rep.Found = true
			rep.Removed = append(rep.Removed, p)
		}
	}

	if daemonFound && len(errs) == 0 {
		u.removeHooksIfLast(&rep, workerID, &errs)
	}
	if len(errs) == 0 {
		if p := RemovedMarkerPath(u.stateDir(), workerID); p != "" {
			_ = os.Remove(p)
			_ = os.Remove(filepath.Dir(p)) // only when empty
		}
	}
	return rep, errors.Join(errs...)
}

func (u *Uninstaller) removeService(ctx context.Context, instance string) error {
	if u.RemoveService != nil {
		return u.RemoveService(ctx, instance, u.Self)
	}
	if u.Self {
		return u.Installer.UninstallServiceSelf(ctx, instance)
	}
	return u.Installer.UninstallService(ctx, instance)
}

// RemoveHooksIfLast removes the agent hooks unless another identity on the account is
// still installed (`konsertd uninstall --name`, which removes one instance by name).
func (u *Uninstaller) RemoveHooksIfLast(rep *UninstallReport) error {
	var errs []error
	u.removeHooksIfLast(rep, "", &errs)
	return errors.Join(errs...)
}

func (u *Uninstaller) removeHooksIfLast(rep *UninstallReport, workerID string, errs *[]error) {
	if u.KeepHooks {
		rep.Kept = append(rep.Kept, "the agent hooks (--keep-hooks)")
		return
	}
	all, err := u.installed()
	if err != nil {
		*errs = append(*errs, err)
		return
	}
	var others []string
	for _, it := range all {
		if workerID != "" && it.workerID == workerID {
			continue
		}
		others = append(others, it.label())
	}
	if len(others) > 0 {
		slices.Sort(others)
		others = slices.Compact(others)
		rep.Kept = append(rep.Kept, "the agent hooks: still used by "+strings.Join(others, ", ")+" on this account")
		return
	}
	remove := u.RemoveHooks
	if remove == nil {
		remove = func() ([]string, error) {
			r, err := RemoveAgentHooks("")
			return r.Notes, err
		}
	}
	notes, err := remove()
	if err != nil {
		*errs = append(*errs, fmt.Errorf("remove the agent hooks: %w", err))
		return
	}
	if len(notes) > 0 {
		rep.Removed = append(rep.Removed, "the agent hooks, no other konsertd on this account ("+strings.Join(notes, "; ")+")")
	}
}

// identityState is the per-identity state removal deletes: the credential, link status,
// outbound spool and terminal scrollback. The worker-id file stays, so the machine keeps its identity if it
// is added again.
func (u *Uninstaller) identityState(workerID string) []string {
	dir := u.stateDir()
	var out []string
	for _, p := range []string{CredentialPath(dir, workerID), LinkStatusPath(dir, workerID), spoolDir(dir, workerID), scrollbackDir(dir, workerID)} {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// removeNohup stops a nohup daemon (unless it is this process) and removes its files.
func (u *Uninstaller) removeNohup(it installedIdentity) error {
	pidPath := strings.TrimSuffix(it.config, ".yaml") + ".pid"
	if !u.Self {
		if raw, err := os.ReadFile(pidPath); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				procArgs := u.ProcArgs
				if procArgs == nil {
					procArgs = psArgs
				}
				// Only a konsertd run from this very config: the pid may have been reused.
				if args, err := procArgs(pid); err == nil && strings.Contains(args, "konsertd") && strings.Contains(args, it.config) {
					kill := u.Kill
					if kill == nil {
						kill = terminate
					}
					if err := kill(pid); err != nil {
						return fmt.Errorf("stop the nohup daemon %s (pid %d): %w", it.label(), pid, err)
					}
				}
			}
		}
	}
	for _, p := range []string{it.config, pidPath} {
		if _, err := removeIfExists(p); err != nil {
			return err
		}
	}
	return nil
}

func psArgs(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	return strings.TrimSpace(string(out)), err
}

func terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGTERM)
}

func serviceKind() string {
	switch runtime.GOOS {
	case "darwin":
		return "LaunchAgent"
	case "windows":
		return "scheduled task"
	}
	return "systemd user unit"
}

func removeIfExists(p string) (bool, error) {
	if st, err := os.Lstat(p); err == nil && st.IsDir() {
		return true, os.RemoveAll(p)
	}
	err := os.Remove(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// lockStale is when a leftover lock (a removal that crashed) is ignored.
const lockStale = 2 * time.Minute

// lock serializes removals on this account: the hub's `konsertd uninstall` and the
// daemon's own removal on 4001 can run at the same moment.
func (u *Uninstaller) lock(ctx context.Context) (func(), error) {
	dir := u.stateDir()
	if dir == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "uninstall.lock")
	wait := u.LockWait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) > lockStale {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another konsertd removal is running (%s)", path)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// RemovedMarkerPath is the marker a daemon writes when the hub removes it, before it
// uninstalls itself: if that removal is interrupted, the daemon finishes it when it
// next starts. `konsertd install` and `konsertd hub-key add` (adding the machine
// again) clear it.
func RemovedMarkerPath(stateDir, workerID string) string {
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	if stateDir == "" || !hubkey.ValidWorkerID(workerID) {
		return ""
	}
	return filepath.Join(stateDir, "removed", workerID)
}

// WriteRemovedMarker records that the hub removed workerID (hubID, if attached over the
// local socket, so the restarted daemon can also drop that hub's key grant).
func WriteRemovedMarker(stateDir, workerID, hubID string) error {
	p := RemovedMarkerPath(stateDir, workerID)
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(hubID+"\n"), 0o600)
}

// ReadRemovedMarker reports whether workerID was removed, and by which hub.
func ReadRemovedMarker(stateDir, workerID string) (hubID string, removed bool) {
	p := RemovedMarkerPath(stateDir, workerID)
	if p == "" {
		return "", false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(raw)), true
}

// ClearRemovedMarker forgets a removal (the machine is being added again).
func ClearRemovedMarker(stateDir, workerID string) {
	if p := RemovedMarkerPath(stateDir, workerID); p != "" {
		_ = os.Remove(p)
	}
}
