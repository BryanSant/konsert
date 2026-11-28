// Package procs samples the machine's process table: every process's parent, name,
// start time, CPU and memory, read the cheapest way each OS offers and without
// spawning a process (docs/plan.md, Phase 6):
//
//   - Linux: /proc/<pid>/stat, one read per process.
//   - macOS: sysctl kern.proc.all for the table, then proc_pid_rusage per process for
//     CPU and memory. macOS refuses that for other users' processes unless syred runs
//     as root: those come back Partial (no CPU or memory).
//   - Windows: one NtQuerySystemInformation(SystemProcessInformation) call returns
//     every process with its times and memory, whoever owns it.
//
// A Sampler turns the cumulative CPU times of two samples into a per-process CPU
// percentage and samples every BusyInterval while work runs, IdleInterval otherwise.
package procs

import (
	"context"
	"sync"
	"time"
)

// Proc is one process of a sample.
type Proc struct {
	PID, PPID int
	// Name is the executable's base name (macOS truncates it to 16 bytes).
	Name string
	// Start is when the process started (zero when unknown). With PID it identifies the
	// process across samples: PIDs are reused.
	Start time.Time
	// CPU is the cumulative user + system time.
	CPU time.Duration
	// RSS is the memory the process holds, in bytes: resident set on Linux, physical
	// footprint on macOS (resident plus compressed and swapped, what Activity Monitor
	// shows), working set on Windows.
	RSS uint64
	// TTY is the controlling terminal's device number (0: none; Windows: always 0).
	TTY uint64
	// Partial marks a process the OS would not report CPU and memory for (macOS: another
	// user's process, syred not root). CPU and RSS are zero then.
	Partial bool
	// CPUPercent is the CPU used since the previous sample of the same process, 100 per
	// fully used core; 0 on first sight.
	CPUPercent float64
}

// Sample is the process table at one moment.
type Sample struct {
	At    time.Time
	Procs []Proc
	// Cost is how long taking the sample took (wall time).
	Cost time.Duration
}

// Sampling intervals.
const (
	BusyInterval = 5 * time.Second
	IdleInterval = 30 * time.Second
)

// table reads the process table (per OS); tests replace it.
var table = osTable

// Sampler samples the process table and keeps the latest sample.
type Sampler struct {
	read func() ([]Proc, error)
	now  func() time.Time
	// busyEvery and idleEvery are BusyInterval and IdleInterval (tests shorten them).
	busyEvery, idleEvery time.Duration

	mu     sync.Mutex
	prev   map[procKey]time.Duration // CPU at the previous sample
	prevAt time.Time
	last   Sample
}

type procKey struct {
	pid   int
	start int64 // unix ns; the name stands in when the start is unknown
	name  string
}

func keyOf(p Proc) procKey {
	k := procKey{pid: p.PID}
	if p.Start.IsZero() {
		k.name = p.Name
	} else {
		k.start = p.Start.UnixNano()
	}
	return k
}

// NewSampler returns a sampler of this machine's process table.
func NewSampler() *Sampler {
	return &Sampler{read: table, now: time.Now, busyEvery: BusyInterval, idleEvery: IdleInterval}
}

// Sample reads the process table now.
func (s *Sampler) Sample() (Sample, error) {
	start := time.Now()
	list, err := s.read()
	if err != nil {
		return Sample{}, err
	}
	at := s.now()
	cost := time.Since(start)
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[procKey]time.Duration, len(list))
	dt := at.Sub(s.prevAt)
	for i := range list {
		p := &list[i]
		if p.Partial {
			continue
		}
		k := keyOf(*p)
		next[k] = p.CPU
		if before, ok := s.prev[k]; ok && dt > 0 && p.CPU >= before {
			p.CPUPercent = 100 * float64(p.CPU-before) / float64(dt)
		}
	}
	s.prev, s.prevAt = next, at
	s.last = Sample{At: at, Procs: list, Cost: cost}
	return s.last, nil
}

// Latest returns the last sample taken (zero before the first).
func (s *Sampler) Latest() Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// Run samples until ctx ends: every BusyInterval while busy reports true (a session
// runs), every IdleInterval otherwise. busy is asked every BusyInterval, so a session
// that starts is sampled within one. onSample (optional) receives each sample.
func (s *Sampler) Run(ctx context.Context, busy func() bool, onSample func(Sample)) {
	tick := time.NewTicker(s.busyEvery)
	defer tick.Stop()
	var lastAt time.Time
	for {
		// A little slack: the ticks don't land exactly IdleInterval apart.
		if lastAt.IsZero() || (busy != nil && busy()) || time.Since(lastAt) >= s.idleEvery-s.busyEvery/5 {
			if smp, err := s.Sample(); err == nil {
				lastAt = time.Now()
				if onSample != nil {
					onSample(smp)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
