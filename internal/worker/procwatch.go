package worker

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/procs"
	"github.com/BryanSant/syre/internal/worker/shells"
)

// Process-level attribution (SYR-AT7T1). Between two process samples the daemon notices
// that a watched process exited, a long-lived daemon that belongs to no session
// (nginx, postgres, a dev server started with nohup). It tells the hub, with the
// interactive SSH shells open outside Syre at the time and when their terminals last
// had input, so the hub can attribute the exit: a Syre terminal command, an agent's
// tool call, the user over SSH ("likely"), or nobody known.

const (
	// watchMinAge keeps short-lived processes (cron jobs, helpers) out of the watch.
	watchMinAge = 60 * time.Second
	watchMax    = 4096
)

type watchedProc struct {
	name  string
	pid   int
	start time.Time
}

type procWatch struct {
	mu     sync.Mutex
	known  map[int]watchedProc // by PID
	lastAt time.Time
}

// daemonParents are the parents whose children are services, not someone's tools.
var daemonParents = map[string]bool{"systemd": true, "launchd": true, "init": true}

// observe compares smp with the previous sample and returns the exits of watched
// processes. agent holds the PIDs attributed to sessions (and syred's own tree).
func (w *procWatch) observe(smp procs.Sample, agent map[int]bool) []protocol.ProcessExitPayload {
	byPID := make(map[int]procs.Proc, len(smp.Procs))
	for _, p := range smp.Procs {
		byPID[p.PID] = p
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var exits []protocol.ProcessExitPayload
	var shellsNow []protocol.SSHShell
	for pid, k := range w.known {
		if p, ok := byPID[pid]; ok && p.Start.Equal(k.start) {
			continue
		}
		if shellsNow == nil {
			shellsNow = sshShells(smp, byPID, agent)
		}
		exits = append(exits, protocol.ProcessExitPayload{Name: k.name, PID: pid, StartedAt: k.start,
			After: w.lastAt, Before: smp.At, SSHShells: shellsNow})
		delete(w.known, pid)
	}
	if w.known == nil {
		w.known = map[int]watchedProc{}
	}
	for _, p := range smp.Procs {
		if len(w.known) >= watchMax {
			break
		}
		if _, ok := w.known[p.PID]; ok || agent[p.PID] || p.Start.IsZero() || smp.At.Sub(p.Start) < watchMinAge {
			continue
		}
		parent, ok := byPID[p.PPID]
		if p.PPID != 1 && (!ok || !daemonParents[parent.Name]) {
			continue
		}
		w.known[p.PID] = watchedProc{name: p.Name, pid: p.PID, start: p.Start}
	}
	w.lastAt = smp.At
	return exits
}

// sshShells lists the interactive shells (with a terminal) under sshd that are not
// part of Syre's own sessions.
func sshShells(smp procs.Sample, byPID map[int]procs.Proc, agent map[int]bool) []protocol.SSHShell {
	var out []protocol.SSHShell
	for _, p := range smp.Procs {
		if p.TTY == 0 || agent[p.PID] || !shells.IsShell(p.Name) || !underSSHD(p, byPID) {
			continue
		}
		s := protocol.SSHShell{PID: p.PID, Name: p.Name}
		if path, at, ok := ttyLastInput(p.TTY); ok {
			s.TTY, s.LastInput = path, at.UTC()
		}
		out = append(out, s)
	}
	return out
}

// underSSHD reports whether p descends from an sshd session process.
func underSSHD(p procs.Proc, byPID map[int]procs.Proc) bool {
	for range 32 {
		parent, ok := byPID[p.PPID]
		if !ok || parent.PID == p.PID {
			return false
		}
		if strings.HasPrefix(parent.Name, "sshd") {
			return true
		}
		p = parent
	}
	return false
}

// killGrace is how long a process gets to stop after the polite signal.
const killGrace = 5 * time.Second

// readProcTable is the process table a kill is checked against (tests replace it).
var readProcTable = func() ([]procs.Proc, error) {
	smp, err := procs.NewSampler().Sample()
	return smp.Procs, err
}

// killProcess stops one process for the user: never syred, its ancestors or PID 1, and
// only the process that started at StartedAt (a reused PID is left alone).
func (d *Daemon) killProcess(p protocol.ProcessKillPayload, killer shells.Killer) protocol.ProcessKillResult {
	list, err := readProcTable()
	if err != nil {
		return protocol.ProcessKillResult{Error: "cannot read the process table: " + err.Error()}
	}
	byPID := make(map[int]procs.Proc, len(list))
	for _, pr := range list {
		byPID[pr.PID] = pr
	}
	target, ok := byPID[p.PID]
	if !ok {
		return protocol.ProcessKillResult{Error: "no such process (it already exited)"}
	}
	res := protocol.ProcessKillResult{Name: target.Name}
	if !p.StartedAt.IsZero() && !target.Start.IsZero() {
		if d := target.Start.Sub(p.StartedAt); d > time.Second || d < -time.Second {
			res.Error = "that process exited; its PID now belongs to another one"
			return res
		}
	}
	self := os.Getpid()
	for pid, i := self, 0; pid > 1 && i < 64; i++ {
		if pid == p.PID {
			res.Error = "refusing to kill syred or a process it runs under"
			return res
		}
		pid = byPID[pid].PPID
	}
	if p.PID <= 1 {
		res.Error = "refusing to kill PID 1"
		return res
	}
	if err := killer.Terminate(p.PID); err != nil {
		res.Error = err.Error()
		return res
	}
	deadline := time.Now().Add(killGrace)
	for time.Now().Before(deadline) {
		if !procAlive(p.PID, target.Start) {
			res.Killed = true
			return res
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := killer.Kill(p.PID); err != nil {
		res.Error = err.Error()
		return res
	}
	res.Killed, res.Forced = true, true
	return res
}

// procAlive reports whether pid still runs as the process started at start.
func procAlive(pid int, start time.Time) bool {
	list, err := readProcTable()
	if err != nil {
		return true
	}
	for _, p := range list {
		if p.PID == pid {
			return start.IsZero() || p.Start.Equal(start) || p.Start.Sub(start).Abs() < time.Second
		}
	}
	return false
}
