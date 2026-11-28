package observe

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/worker/shells"
)

// agentProc is a running agent CLI: the top process of its tree.
type agentProc struct {
	pid       int
	cwd       string
	open      []string // files it holds open, where the OS lists them cheaply
	ancestors []int
	// server marks a process serving another program (Syre's own chats: codex
	// app-server, grok agent stdio): never matched to a session by cwd.
	server bool
}

// procFinder finds an agent's running processes, for agents that publish no live
// status of their own: the process holding a session's files tells that the session
// is alive and which process it is.
type procFinder struct {
	procs     CodexProcs
	cwd       func(pid int) string
	openFiles func(pid int) []string
	ttl       time.Duration // 0: procTableTTL; negative: list on every call
	isAgent   func(name string) bool
	isServer  func(argv []string) bool

	mu   sync.Mutex
	list []agentProc
	at   time.Time
}

// running lists the agent's processes: the cached list while fresh.
func (f *procFinder) running() []agentProc {
	if f.procs == nil {
		return nil
	}
	ttl := f.ttl
	if ttl == 0 {
		ttl = procTableTTL
	}
	f.mu.Lock()
	if ttl > 0 && f.at.After(time.Now().Add(-ttl)) {
		list := f.list
		f.mu.Unlock()
		return list
	}
	f.mu.Unlock()
	snap, err := f.procs.Snapshot()
	if err != nil {
		return nil
	}
	byPID := make(map[int]shells.Proc, len(snap))
	for _, p := range snap {
		byPID[p.PID] = p
	}
	var out []agentProc
	for _, p := range snap {
		if p.Zombie || !f.isAgent(p.Name) {
			continue
		}
		if parent, ok := byPID[p.PPID]; ok && f.isAgent(parent.Name) {
			continue // a helper of the agent (its sandbox): the top process holds the session
		}
		ap := agentProc{pid: p.PID, ancestors: ancestors(byPID, p.PID)}
		if f.cwd != nil {
			ap.cwd = f.cwd(p.PID)
		}
		if f.openFiles != nil {
			ap.open = f.openFiles(p.PID)
		}
		if f.isServer != nil {
			ap.server = f.isServer(f.procs.Argv(p.PID))
		}
		out = append(out, ap)
	}
	f.mu.Lock()
	f.list, f.at = out, time.Now()
	f.mu.Unlock()
	return out
}

// holder is the process holding a file of the session open (holds reports one), else,
// where open files aren't listed, the only interactive agent process in cwd; nil when
// none or ambiguous.
func (f *procFinder) holder(list []agentProc, holds func(file string) bool, cwd string) *agentProc {
	if f.openFiles != nil {
		for i := range list {
			for _, file := range list[i].open {
				if holds(file) {
					return &list[i]
				}
			}
		}
		return nil
	}
	if cwd == "" {
		return nil
	}
	var found *agentProc
	for i := range list {
		p := &list[i]
		if p.server || p.cwd == "" || filepath.Clean(p.cwd) != filepath.Clean(cwd) {
			continue
		}
		if found != nil {
			return nil // two in one dir: which session is whose is unknown
		}
		found = p
	}
	return found
}
