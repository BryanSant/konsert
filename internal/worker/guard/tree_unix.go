//go:build darwin || linux

package guard

import (
	"sync"
	"syscall"
	"time"
)

// procTree tracks a session's processes where no cgroup holds them (macOS, and Linux
// without a systemd user manager). A process group alone is not enough: Claude Code
// starts every shell command in a new process group (seen live on macOS), so a memory
// hog run from an agent's shell sat outside the session's group and a group kill
// missed it. The tree follows descendants by parent PID (start times guard against PID
// reuse) and remembers each descendant's process group, so grandchildren reparented to
// init/launchd when their parent dies are still caught through their group.
type procTree struct {
	mu     sync.Mutex
	root   int
	known  map[int]uint64 // pid -> start time of every descendant seen
	groups map[int]bool   // process groups started by descendants
	last   time.Time
}

const (
	treeScanEvery = time.Second // background rate; freeze, kill and rss force a scan
	treeKnownCap  = 4096
)

// procInfo is one process from listProcs. start is in platform units, comparable only
// with other starts from the same host.
type procInfo struct {
	pid, ppid, pgid int
	start           uint64
}

func newProcTree(root int) *procTree {
	return &procTree{root: root, known: map[int]uint64{}, groups: map[int]bool{}}
}

// members scans (unforced: at most once per treeScanEvery, else it returns nil) and
// returns the session's live processes: tracked descendants, the session's own
// process group and every group a descendant started.
func (t *procTree) members(force bool) []int {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !force && time.Since(t.last) < treeScanEvery {
		return nil
	}
	t.last = time.Now()
	procs, err := listProcs()
	if err != nil {
		return nil
	}
	byPID := make(map[int]procInfo, len(procs))
	kids := map[int][]procInfo{}
	for _, p := range procs {
		byPID[p.pid] = p
		kids[p.ppid] = append(kids[p.ppid], p)
	}
	if r, ok := byPID[t.root]; ok && len(t.known) == 0 {
		t.known[t.root] = r.start
	}
	// Walk from every tracked process that is still the same process.
	queue := make([]int, 0, len(t.known))
	for pid, start := range t.known {
		if p, ok := byPID[pid]; ok && p.start == start {
			queue = append(queue, pid)
		}
	}
	for i := 0; i < len(queue); i++ {
		parent := byPID[queue[i]]
		for _, c := range kids[parent.pid] {
			if _, ok := t.known[c.pid]; ok || c.start < parent.start || len(t.known) >= treeKnownCap {
				continue
			}
			t.known[c.pid] = c.start
			if c.pgid != t.root && c.pgid == c.pid {
				t.groups[c.pgid] = true
			}
			queue = append(queue, c.pid)
		}
	}
	live := map[int]bool{}
	var out []int
	for _, p := range procs {
		if start, ok := t.known[p.pid]; (ok && start == p.start) || p.pgid == t.root || t.groups[p.pgid] {
			out = append(out, p.pid)
			live[p.pgid] = true
		}
	}
	for g := range t.groups {
		if !live[g] {
			delete(t.groups, g) // emptied: never signal a reused group id
		}
	}
	return out
}

// signal sends sig to every process of the session; ok is whether any was signalled.
func (t *procTree) signal(sig syscall.Signal) bool {
	ok := false
	for _, pid := range t.members(true) {
		if syscall.Kill(pid, sig) == nil {
			ok = true
		}
	}
	return ok
}
