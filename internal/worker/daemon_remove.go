package worker

import (
	"context"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/install"
)

// selfRemoveStopWait bounds the wait for the service manager to stop this daemon after
// it removed its own registration (launchd bootout, systemd stop --no-block).
var selfRemoveStopWait = 15 * time.Second

// defaultSelfRemove is the removal a daemon without selfRemove runs; the package's
// tests replace it so no test daemon touches the developer's real registrations.
var defaultSelfRemove = func(d *Daemon) func(context.Context, string) (bool, error) {
	return d.uninstallSelf
}

// removedByHub runs when the hub removed this machine (close code 4001), or at start
// when an earlier removal did not finish: it records the removal, uninstalls
// everything installed for this identity (its service, so nothing restarts it, the
// hub's key grant, and the agent hooks if no other identity on the account needs
// them), then waits briefly for the service manager to stop it. The caller exits.
func (d *Daemon) removedByHub(ctx context.Context, hubID string) {
	remove := d.selfRemove
	if remove == nil {
		remove = defaultSelfRemove(d)
	}
	d.link.forget()
	if err := WriteRemovedMarker(d.stateDir(), d.cfg.WorkerID, hubID); err != nil {
		d.logger.Warn("could not record the removal", "err", err)
	}
	stopPending, err := remove(ctx, hubID)
	if err != nil {
		d.logger.Warn("removed from the hub, but uninstalling konsertd failed; it retries at the next start", "err", err)
		return
	}
	if stopPending {
		select {
		case <-ctx.Done():
		case <-time.After(selfRemoveStopWait):
		}
	}
}

// uninstallSelf is removedByHub's default removal. It reports whether a service
// manager will stop this process.
func (d *Daemon) uninstallSelf(ctx context.Context, hubID string) (bool, error) {
	if dryRunEnv() {
		// The e2e suite runs daemons as hub children with nothing registered.
		d.logger.Info("uninstall skipped (" + install.DryRunEnv + " is set)")
		return false, nil
	}
	env, err := install.DefaultEnv()
	if err != nil {
		return false, err
	}
	u := &Uninstaller{
		Installer: &install.Installer{Runner: install.ExecRunner{}, Env: env},
		StateDir:  d.stateDir(),
		Self:      true,
		LockWait:  20 * time.Second,
	}
	// The daemon's own context ends when the service manager stops it; the removal
	// must still finish.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	rep, err := u.RemoveIdentity(rctx, d.cfg.WorkerID, hubID)
	for _, l := range rep.Lines() {
		d.logger.Info("uninstall: " + l)
	}
	if err != nil {
		return false, err
	}
	// launchd and systemd stop a removed service (this process) shortly; a deleted
	// Windows task, a nohup or a foreground daemon just exits.
	return rep.Services > 0 && runtime.GOOS != "windows", nil
}

// finishPendingRemoval completes a removal interrupted earlier (the marker is still
// there). It reports whether the daemon must stop instead of serving.
func (d *Daemon) finishPendingRemoval(ctx context.Context) bool {
	hubID, removed := ReadRemovedMarker(d.stateDir(), d.cfg.WorkerID)
	if !removed {
		return false
	}
	d.logger.Info("this machine was removed from the hub; finishing the uninstall", "worker_id", d.cfg.WorkerID)
	d.removedByHub(ctx, hubID)
	return true
}

func dryRunEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(install.DryRunEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
