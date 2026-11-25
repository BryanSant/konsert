// Package shells counts the shell processes under each agent session syred hosts
// (chat engines and PTY tasks), from the process tree rather than the agent's event
// stream, so background shells (claude's run_in_background, codex background terminals)
// count for as long as they run, even after the tool call returned and after the session
// ended. One process-table snapshot per tick serves every session. See
// docs/design/activity.md.
package shells

import (
	"errors"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/activity"
)

// Proc is one process of a snapshot.
type Proc struct {
	PID, PPID int
	// PGID is the process group (0 where the OS has none, Windows).
	PGID int
	// Name is the executable's base name ("zsh", "bash.exe").
	Name string
	// Start is the process start time; zero where the snapshot does not carry it (the
	// source's Details fills it in).
	Start time.Time
	// CPU is the cumulative user + system time, when CPUKnown.
	CPU      time.Duration
	CPUKnown bool
	Zombie   bool
}

// Source reads the process table.
type Source interface {
	// Snapshot lists every process.
	Snapshot() ([]Proc, error)
	// Details fills in what Snapshot leaves out because it is costly for every process
	// (start time and CPU on Windows). Called only for processes of a session's tree.
	Details(p *Proc)
	// Argv is pid's command line ("" entries where unknown).
	Argv(pid int) []string
}

// Killer signals one process.
type Killer interface {
	// Terminate asks pid to stop (SIGTERM; Windows has no gentle signal and terminates).
	Terminate(pid int) error
	// Kill stops pid at once (SIGKILL / TerminateProcess).
	Kill(pid int) error
}

// shellNames are the executables counted as shells.
var shellNames = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true,
	"mksh": true, "ash": true, "csh": true, "tcsh": true, "nu": true,
	"pwsh": true, "powershell": true, "cmd": true,
}

// IsShell reports whether an executable name is a shell.
func IsShell(name string) bool {
	n := strings.ToLower(filepath.Base(strings.TrimPrefix(strings.TrimSpace(name), "-")))
	n = strings.TrimSuffix(n, ".exe")
	return shellNames[n]
}

// busyCPU is the share of the tick a shell tree must spend on the CPU to count busy
// without a child process (a loop in the shell itself).
const busyCPU = 0.05

// KillGrace is how long killed processes get to exit after Terminate before Kill.
const KillGrace = 2 * time.Second

// Options configures a Tracker. Zero values take the defaults.
type Options struct {
	Source Source
	Killer Killer
	Now    func() time.Time
	// AfterFunc schedules the forced kill after KillGrace (tests replace it).
	AfterFunc func(d time.Duration, f func())
}

// Tracker keeps the shells of every session. Safe for concurrent use.
type Tracker struct {
	opts Options

	mu       sync.Mutex
	sessions map[string]*session
	argv     map[procID]string // cleaned command lines by process
	lastTick time.Time
}

type procID struct {
	pid   int
	start int64
}

type session struct {
	root      int
	rootStart time.Time
	shells    map[procID]*shell
	reported  activity.Shells
	hasReport bool
}

type shell struct {
	id       procID
	start    time.Time
	cmd      string
	cmdFinal bool // cmd is settled (see command); until then it is re-read each tick
	busy     bool
	since    time.Time
	dangling bool
	cpu      time.Duration // the tree's CPU at the last tick
	cpuKnown bool
}

// New returns a tracker reading the OS process table (or opts.Source).
func New(opts Options) *Tracker {
	if opts.Source == nil {
		opts.Source = OSSource()
	}
	if opts.Killer == nil {
		opts.Killer = OSKiller()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.AfterFunc == nil {
		opts.AfterFunc = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	return &Tracker{opts: opts, sessions: map[string]*session{}, argv: map[procID]string{}}
}

// table is one snapshot indexed by pid and parent.
type table struct {
	src    Source
	procs  map[int]*Proc
	kids   map[int][]int
	group  map[int][]int // pgid → members
	filled map[int]bool
}

func newTable(src Source, procs []Proc) *table {
	t := &table{src: src, procs: make(map[int]*Proc, len(procs)), kids: map[int][]int{}, group: map[int][]int{}, filled: map[int]bool{}}
	for i := range procs {
		p := &procs[i]
		if p.PID <= 0 {
			continue
		}
		t.procs[p.PID] = p
	}
	for pid, p := range t.procs {
		if p.PPID != pid && p.PPID > 0 {
			t.kids[p.PPID] = append(t.kids[p.PPID], pid)
		}
		if p.PGID > 0 {
			t.group[p.PGID] = append(t.group[p.PGID], pid)
		}
	}
	for _, k := range t.kids {
		slices.Sort(k)
	}
	return t
}

// get returns pid with its details filled in.
func (t *table) get(pid int) *Proc {
	p := t.procs[pid]
	if p == nil {
		return nil
	}
	if !t.filled[pid] {
		t.filled[pid] = true
		if p.Start.IsZero() || !p.CPUKnown {
			t.src.Details(p)
		}
	}
	return p
}

// children returns pid's live children. A child that started before its parent is a
// stale parent link (the parent's pid was reused, Windows) and is skipped.
func (t *table) children(pid int) []*Proc {
	parent := t.get(pid)
	var out []*Proc
	for _, k := range t.kids[pid] {
		c := t.get(k)
		if c == nil {
			continue
		}
		if parent != nil && !parent.Start.IsZero() && !c.Start.IsZero() && c.Start.Before(parent.Start) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// alive reports whether id still runs (same pid and start time).
func (t *table) alive(id procID) (*Proc, bool) {
	p := t.get(id.pid)
	if p == nil || p.Zombie || startKey(p.Start) != id.start {
		return nil, false
	}
	return p, true
}

func startKey(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// subtree returns root and its descendants (root first).
func (t *table) subtree(root *Proc) []*Proc {
	out := []*Proc{root}
	seen := map[int]bool{root.PID: true}
	for i := 0; i < len(out); i++ {
		for _, c := range t.children(out[i].PID) {
			if !seen[c.PID] {
				seen[c.PID] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// topShells finds the shells directly under root: walking down from the agent, the first
// shell on each path counts (its nested shells belong to it). Members of root's process
// group that lost their parent (a background job whose shell exited: `sleep 300 &`)
// count too, shell or not, since nothing else would show them.
func (t *table) topShells(root *Proc) []*Proc {
	var out []*Proc
	seen := map[int]bool{root.PID: true}
	queue := []*Proc{root}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range t.children(p.PID) {
			if seen[c.PID] || c.Zombie {
				continue
			}
			seen[c.PID] = true
			if IsShell(c.Name) {
				out = append(out, c)
				for _, d := range t.subtree(c) {
					seen[d.PID] = true
				}
				continue
			}
			queue = append(queue, c)
		}
	}
	if root.PGID == root.PID && root.PGID > 0 {
		for _, pid := range t.group[root.PGID] {
			if seen[pid] {
				continue
			}
			p := t.get(pid)
			if p == nil || p.Zombie {
				continue
			}
			if par := t.procs[p.PPID]; par != nil && par.PGID == root.PGID {
				continue // reached from its orphaned parent
			}
			out = append(out, p)
			for _, d := range t.subtree(p) {
				seen[d.PID] = true
			}
		}
	}
	return out
}

// busy reports whether the shell runs a command: a live non-shell process under it, or
// (where CPU time is known) the tree used the CPU since the last tick. A process counted
// directly (an orphaned background job) is its own command.
func (t *table) busy(top *Proc, sh *shell, dt time.Duration) (bool, time.Duration, bool) {
	tree := t.subtree(top)
	running := !IsShell(top.Name)
	var cpu time.Duration
	known := true
	for _, p := range tree {
		if p.Zombie {
			continue
		}
		if p != top && !IsShell(p.Name) {
			running = true
		}
		if p.CPUKnown {
			cpu += p.CPU
		} else {
			known = false
		}
	}
	if !running && known && sh.cpuKnown && dt > 0 {
		if used := cpu - sh.cpu; used > 0 && float64(used) >= busyCPU*float64(dt) {
			running = true
		}
	}
	return running, cpu, known
}

// Tick takes one process snapshot, updates every session's shells and returns the
// sessions whose report changed (key → shells). roots maps each hosted session key
// ("chat:<id>", "task:<id>") to its agent process id; a session missing from roots has
// ended, and its shells are tracked (dangling) until they exit.
func (tr *Tracker) Tick(roots map[string]int) map[string]activity.Shells {
	tr.mu.Lock()
	idle := len(roots) == 0 && len(tr.sessions) == 0
	tr.mu.Unlock()
	if idle {
		return nil // nothing hosted, nothing dangling: no snapshot
	}
	procs, err := tr.opts.Source.Snapshot()
	if err != nil {
		return nil // keep what we know; the next tick retries
	}
	now := tr.opts.Now()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	dt := time.Duration(0)
	if !tr.lastTick.IsZero() {
		dt = now.Sub(tr.lastTick)
	}
	tr.lastTick = now
	t := newTable(tr.opts.Source, procs)

	for key, pid := range roots {
		s := tr.sessions[key]
		if s == nil {
			s = &session{shells: map[procID]*shell{}}
			tr.sessions[key] = s
		}
		if s.root != pid {
			s.root, s.rootStart = pid, time.Time{}
			if p := t.get(pid); p != nil {
				s.rootStart = p.Start
			}
		}
	}
	changed := map[string]activity.Shells{}
	usedArgv := map[procID]bool{}
	for key, s := range tr.sessions {
		if _, hosted := roots[key]; !hosted {
			s.root = 0
		}
		found := map[procID]bool{}
		if s.root > 0 {
			if root := t.get(s.root); root != nil && !root.Zombie && (s.rootStart.IsZero() || root.Start.Equal(s.rootStart)) {
				for _, p := range t.topShells(root) {
					id := procID{p.PID, startKey(p.Start)}
					found[id] = true
					sh := s.shells[id]
					if sh == nil {
						sh = &shell{id: id, start: p.Start, since: now}
						s.shells[id] = sh
					}
					if !sh.cmdFinal {
						sh.cmd, sh.cmdFinal = tr.command(t, p, now)
					}
					sh.dangling = false
					tr.update(t, p, sh, now, dt)
				}
			}
		}
		for id, sh := range s.shells {
			if found[id] {
				continue
			}
			p, ok := t.alive(id)
			if !ok {
				delete(s.shells, id)
				continue
			}
			sh.dangling = true // still runs, no longer under the agent
			tr.update(t, p, sh, now, dt)
		}
		for id := range s.shells {
			usedArgv[id] = true
		}
		rep := report(s)
		if !s.hasReport || !sameReport(rep, s.reported) {
			if s.hasReport || rep.Count > 0 {
				changed[key] = rep
			}
			s.reported, s.hasReport = rep, true
		}
		if s.root == 0 && len(s.shells) == 0 {
			delete(tr.sessions, key)
		}
	}
	for id := range tr.argv {
		if !usedArgv[id] {
			delete(tr.argv, id)
		}
	}
	return changed
}

func (tr *Tracker) update(t *table, p *Proc, sh *shell, now time.Time, dt time.Duration) {
	busy, cpu, known := t.busy(p, sh, dt)
	if busy != sh.busy {
		sh.busy, sh.since = busy, now
	}
	sh.cpu, sh.cpuKnown = cpu, known
}

// argvSettle is how old a process must be before an argument-less command line is
// trusted: a snapshot taken between fork and exec sees the parent's image (a bare "sh").
const argvSettle = 2 * time.Second

// command returns p's cleaned command line and whether it is settled. A command line with
// arguments, or one read from a process older than argvSettle, is settled and cached per
// process; anything else is re-read on the next tick.
func (tr *Tracker) command(t *table, p *Proc, now time.Time) (string, bool) {
	id := procID{p.PID, startKey(p.Start)}
	if c, ok := tr.argv[id]; ok {
		return c, true
	}
	argv := t.src.Argv(p.PID)
	c := CleanCommand(argv)
	if c == "" {
		c = p.Name
	}
	if len(argv) < 2 && (p.Start.IsZero() || now.Sub(p.Start) < argvSettle) {
		return c, false
	}
	tr.argv[id] = c
	return c, true
}

func report(s *session) activity.Shells {
	list := make([]*shell, 0, len(s.shells))
	for _, sh := range s.shells {
		list = append(list, sh)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].start.Equal(list[j].start) {
			return list[i].start.Before(list[j].start)
		}
		return list[i].id.pid < list[j].id.pid
	})
	var r activity.Shells
	r.Count = len(list)
	for _, sh := range list {
		if sh.busy {
			r.Busy++
		}
		if len(r.List) < protocol.MaxShellList {
			r.List = append(r.List, protocol.ShellInfo{
				PID: sh.id.pid, Command: sh.cmd, StartedAt: sh.start.UTC(), Busy: sh.busy,
				Since: sh.since.UTC(), Dangling: sh.dangling,
			})
		}
	}
	return r
}

func sameReport(a, b activity.Shells) bool {
	x := protocol.SessionActivity{Shells: a.Count, ShellsBusy: a.Busy, ShellList: a.List}
	y := protocol.SessionActivity{Shells: b.Count, ShellsBusy: b.Busy, ShellList: b.List}
	return x.SameDetail(y)
}

// ErrNotFound means none of the named shells belongs to the session (any more).
var ErrNotFound = errors.New("no such shell in this session")

// Kill terminates shells of session key (all of them when idents is empty), each with
// its whole process tree: Terminate now, Kill whatever still runs after KillGrace. Only
// processes the tracker attributes to that session are touched, and only while their
// pid still has the same start time. It returns the pids it signalled.
func (tr *Tracker) Kill(key string, idents []protocol.ShellIdent) ([]int, error) {
	tr.mu.Lock()
	s := tr.sessions[key]
	var targets []procID
	if s != nil {
		for id := range s.shells {
			if len(idents) == 0 {
				targets = append(targets, id)
				continue
			}
			for _, w := range idents {
				if w.PID == id.pid && startKey(w.StartedAt) == id.start {
					targets = append(targets, id)
				}
			}
		}
	}
	tr.mu.Unlock()
	if len(targets) == 0 {
		return nil, ErrNotFound
	}
	procs, err := tr.opts.Source.Snapshot()
	if err != nil {
		return nil, err
	}
	t := newTable(tr.opts.Source, procs)
	var victims []procID
	for _, id := range targets {
		p, ok := t.alive(id)
		if !ok {
			continue
		}
		for _, d := range t.subtree(p) {
			victims = append(victims, procID{d.PID, startKey(d.Start)})
		}
	}
	if len(victims) == 0 {
		return nil, ErrNotFound
	}
	var pids []int
	for _, v := range victims {
		if tr.opts.Killer.Terminate(v.pid) == nil {
			pids = append(pids, v.pid)
		}
	}
	tr.opts.AfterFunc(KillGrace, func() {
		procs, err := tr.opts.Source.Snapshot()
		if err != nil {
			return
		}
		t := newTable(tr.opts.Source, procs)
		for _, v := range victims {
			if _, ok := t.alive(v); ok {
				_ = tr.opts.Killer.Kill(v.pid)
			}
		}
	})
	return pids, nil
}

// Forget drops a session's record (its chat was discarded).
func (tr *Tracker) Forget(key string) {
	tr.mu.Lock()
	delete(tr.sessions, key)
	tr.mu.Unlock()
}
