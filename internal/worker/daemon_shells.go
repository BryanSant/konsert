package worker

import (
	"context"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/shells"
)

// DefaultShellInterval is how often the shells under hosted sessions are counted (one
// process-table snapshot per tick for every session; none while nothing is hosted).
const DefaultShellInterval = time.Second

// shellLoop counts the shells of every hosted session (chat engines and PTY agent tasks)
// and of sessions that ended with shells still running, and publishes changes with the
// session's activity.
func (d *Daemon) shellLoop(ctx context.Context) {
	iv := d.ShellInterval
	if iv <= 0 {
		iv = DefaultShellInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.pollShells()
		}
	}
}

// shellRoots maps every hosted session to its agent process id.
func (d *Daemon) shellRoots() map[string]int {
	roots := map[string]int{}
	if d.chat != nil {
		for id, pid := range d.chat.ProcessIDs() {
			roots["chat:"+id] = pid
		}
	}
	if d.runner != nil {
		for id, pid := range d.runner.SessionPIDs() {
			roots["task:"+id] = pid
		}
	}
	return roots
}

func (d *Daemon) pollShells() {
	for key, s := range d.shells.Tick(d.shellRoots()) {
		d.activity.SetShells(key, s)
	}
	d.pollTerminals(terminalSnapshot)
}

// terminalSnapshot reads the process table for terminals without shell integration
// (tests replace it).
var terminalSnapshot = func() ([]shells.Proc, error) { return shells.OSSource().Snapshot() }

// killShells handles session.shell.kill: the tracker only touches processes it still
// attributes to that session, then the next poll reports the new count.
func (d *Daemon) killShells(p protocol.SessionShellKill) {
	if p.ChatID == "" && p.TaskID == "" {
		return
	}
	pids, err := d.shells.Kill(p.Key(), p.Shells)
	if err != nil {
		d.logger.Warn("shell kill refused", "session", p.Key(), "err", err)
		return
	}
	d.logger.Info("killed shells", "session", p.Key(), "pids", pids)
	d.pollShells()
}
