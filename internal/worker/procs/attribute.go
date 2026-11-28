package procs

import (
	"sort"
	"sync"
	"time"
)

// Root is one session whose processes are attributed: its agent process and, for a
// managed session, the members its container holds (cgroup scope, job object), which
// catch processes a double fork took out of the root's descent between samples.
type Root struct {
	Key     string // "task:<id>", "chat:<id>", "observed:<external id>"
	PID     int
	Members []int
}

// SessionUsage is what a session's processes use in one sample.
type SessionUsage struct {
	Key        string
	CPUPercent float64
	RSS        uint64
	PeakRSS    uint64 // highest RSS sum seen for the key
	Procs      int
	// Orphans counts processes kept by lineage: their session's process tree no longer
	// reaches them (a Gradle daemon, a dev server left running), or the session ended.
	Orphans int
	PIDs    []int
}

// Attributor assigns every process of a sample to at most one session and keeps that
// assignment for the process's lifetime: a descendant that outlives its parent, or its
// whole session, stays attributed until it exits.
type Attributor struct {
	mu    sync.Mutex
	owner map[procKey]string // processes attributed so far
	peak  map[string]uint64
	seen  map[string]time.Time // when a key last had processes
}

// peakKeep is how long a key's peak survives without processes.
const peakKeep = time.Hour

// NewAttributor returns an empty attributor.
func NewAttributor() *Attributor {
	return &Attributor{owner: map[procKey]string{}, peak: map[string]uint64{}, seen: map[string]time.Time{}}
}

// Attribute splits smp among roots. Roots earlier in the list win a process claimed by
// two (list managed sessions first). Processes no session owns are left out.
func (a *Attributor) Attribute(smp Sample, roots []Root) []SessionUsage {
	byPID := make(map[int]*Proc, len(smp.Procs))
	kids := map[int][]int{}
	for i := range smp.Procs {
		p := &smp.Procs[i]
		byPID[p.PID] = p
		if p.PPID != p.PID {
			kids[p.PPID] = append(kids[p.PPID], p.PID)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	owner := map[int]string{} // this sample, by PID
	orphan := map[int]bool{}
	var claim func(pid int, key string, kept bool)
	claim = func(pid int, key string, kept bool) {
		if _, done := owner[pid]; done {
			return
		}
		if _, ok := byPID[pid]; !ok {
			return
		}
		owner[pid] = key
		if kept {
			orphan[pid] = true
		}
		for _, c := range kids[pid] {
			claim(c, key, kept && a.owner[keyOf(*byPID[c])] == key)
		}
	}
	live := map[string]bool{}
	for _, r := range roots {
		live[r.Key] = true
		if r.PID > 0 {
			claim(r.PID, r.Key, false)
		}
		for _, m := range r.Members {
			claim(m, r.Key, false)
		}
	}
	// Lineage: processes attributed before and not reached through a root now.
	for pid, p := range byPID {
		if _, done := owner[pid]; done {
			continue
		}
		if key, ok := a.owner[keyOf(*p)]; ok {
			claim(pid, key, true)
		}
	}
	next := make(map[procKey]string, len(owner))
	usage := map[string]*SessionUsage{}
	for pid, key := range owner {
		p := byPID[pid]
		next[keyOf(*p)] = key
		u := usage[key]
		if u == nil {
			u = &SessionUsage{Key: key}
			usage[key] = u
		}
		u.CPUPercent += p.CPUPercent
		u.RSS += p.RSS
		u.Procs++
		u.PIDs = append(u.PIDs, pid)
		if orphan[pid] {
			u.Orphans++
		}
	}
	a.owner = next
	out := make([]SessionUsage, 0, len(usage))
	for key, u := range usage {
		a.peak[key] = max(a.peak[key], u.RSS)
		a.seen[key] = smp.At
		u.PeakRSS = a.peak[key]
		sort.Ints(u.PIDs)
		out = append(out, *u)
	}
	for key, at := range a.seen {
		if usage[key] == nil && !live[key] && smp.At.Sub(at) > peakKeep {
			delete(a.seen, key)
			delete(a.peak, key)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
