package worker

import (
	"runtime"
	"sync"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/shells"
)

// Terminal state (SYR-4GBB5). A shell waiting at its prompt is idle, not working. A
// user's terminal is working only while a command runs:
//   - with shell integration, from its command start and end marks (OSC 133), which
//     also name the command;
//   - without (an unsupported shell, integration that didn't start), from the process
//     table: on Unix the terminal's foreground process group is not the shell's own
//     while a command runs in the foreground; on Windows the shell has a child.
// Agents' shells are not judged this way: an agent works while its turn or tool call is
// open, which its own activity says.

// terminalStates remembers what was last published per terminal, so only changes go out.
type terminalStates struct {
	mu    sync.Mutex
	state map[string]string // task id -> working / done
}

// terminalCommand publishes a terminal's state from a command event: working with the
// command while it runs, idle (done) once it ends.
func (d *Daemon) terminalCommand(p protocol.OperatorCommandPayload) {
	if p.EndedAt == nil && p.ExitCode == nil {
		d.publishTerminal(p.TaskID, protocol.ActivityWorking, p.Command)
		return
	}
	d.publishTerminal(p.TaskID, protocol.ActivityDone, "")
}

func (d *Daemon) publishTerminal(taskID, state, label string) {
	d.termStates.mu.Lock()
	if d.termStates.state == nil {
		d.termStates.state = map[string]string{}
	}
	prev := d.termStates.state[taskID]
	d.termStates.state[taskID] = state
	d.termStates.mu.Unlock()
	if prev == state && state == protocol.ActivityDone {
		return
	}
	a := protocol.SessionActivity{TaskID: taskID, State: state, Source: protocol.ActivitySourceLog}
	if state == protocol.ActivityWorking {
		a.Tool, a.Label = "shell", label
	}
	d.activity.Update(a)
}

// pollTerminals settles every terminal's state each shell poll: integrated shells get
// their first idle state here (their command events do the rest); the others are judged
// from the process table.
func (d *Daemon) pollTerminals(snapshot func() ([]shells.Proc, error)) {
	if d.runner == nil || d.activity == nil {
		return
	}
	terms := d.runner.TerminalShells()
	var procs []shells.Proc
	for _, t := range terms {
		if !t.Integrated {
			procs, _ = snapshot()
			break
		}
	}
	d.termStates.mu.Lock()
	for id := range d.termStates.state {
		if _, ok := terms[id]; !ok {
			delete(d.termStates.state, id)
		}
	}
	known := make(map[string]bool, len(d.termStates.state))
	for id := range d.termStates.state {
		known[id] = true
	}
	d.termStates.mu.Unlock()
	var byPID map[int]shells.Proc
	var kids map[int]int
	for id, t := range terms {
		if t.Integrated {
			if !known[id] {
				d.publishTerminal(id, protocol.ActivityDone, "")
			}
			continue
		}
		if procs == nil {
			continue
		}
		if byPID == nil {
			byPID, kids = make(map[int]shells.Proc, len(procs)), map[int]int{}
			for _, p := range procs {
				byPID[p.PID] = p
				kids[p.PPID]++
			}
		}
		busy, label := shellBusy(byPID, kids, t.PID)
		if busy {
			d.publishTerminal(id, protocol.ActivityWorking, label)
		} else {
			d.publishTerminal(id, protocol.ActivityDone, "")
		}
	}
}

// shellBusy judges a shell without integration from the process table.
func shellBusy(byPID map[int]shells.Proc, kids map[int]int, pid int) (bool, string) {
	sh, ok := byPID[pid]
	if !ok {
		return false, ""
	}
	if runtime.GOOS == "windows" || sh.TPGID == 0 {
		if kids[pid] == 0 {
			return false, ""
		}
		for _, p := range byPID {
			if p.PPID == pid && p.Name != "conhost.exe" {
				return true, p.Name
			}
		}
		return false, ""
	}
	if sh.TPGID == sh.PGID {
		return false, "" // the shell itself is in the foreground: at its prompt
	}
	for _, p := range byPID {
		if p.PGID == sh.TPGID && p.PID == sh.TPGID {
			return true, p.Name
		}
	}
	return true, ""
}
