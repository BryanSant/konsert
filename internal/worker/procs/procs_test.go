package procs

import (
	"context"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func findSelf(t *testing.T, list []Proc) Proc {
	t.Helper()
	for _, p := range list {
		if p.PID == os.Getpid() {
			return p
		}
	}
	t.Fatalf("own process %d not in the table (%d processes)", os.Getpid(), len(list))
	return Proc{}
}

// burn keeps one goroutine on the CPU for d.
func burn(d time.Duration) {
	end := time.Now().Add(d)
	x := 0
	for time.Now().Before(end) {
		for i := range 10000 {
			x += i
		}
	}
	_ = x
}

func TestTableHasSelf(t *testing.T) {
	burn(50 * time.Millisecond)
	list, err := osTable()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 3 {
		t.Fatalf("only %d processes", len(list))
	}
	self := findSelf(t, list)
	if self.PPID != os.Getppid() || self.Name == "" || self.Partial || self.RSS < 1<<20 || self.CPU <= 0 {
		t.Fatalf("self = %+v (ppid %d)", self, os.Getppid())
	}
	if self.Start.IsZero() || time.Since(self.Start) < 0 || time.Since(self.Start) > time.Hour {
		t.Fatalf("self start = %v", self.Start)
	}
	parent := Proc{}
	for _, p := range list {
		if p.PID == self.PPID {
			parent = p
		}
	}
	if parent.PID == 0 || !parent.Start.Before(self.Start.Add(time.Second)) {
		t.Fatalf("parent = %+v", parent)
	}
}

// TestSelfCPUMatchesOS checks the table's CPU time against the OS's own accounting of
// this process (on macOS that checks the mach time conversion).
func TestSelfCPUMatchesOS(t *testing.T) {
	burn(300 * time.Millisecond)
	before := processCPU()
	list, err := osTable()
	if err != nil {
		t.Fatal(err)
	}
	after := processCPU()
	got := findSelf(t, list).CPU
	if got < before*8/10 || got > after*12/10+20*time.Millisecond {
		t.Fatalf("table CPU %s, OS says %s..%s", got, before, after)
	}
}

func TestSamplerSelfPercent(t *testing.T) {
	s := NewSampler()
	if _, err := s.Sample(); err != nil {
		t.Fatal(err)
	}
	burn(400 * time.Millisecond)
	smp, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	pct := findSelf(t, smp.Procs).CPUPercent
	// One goroutine busy the whole time is ~100% (plus the runtime's own threads).
	if pct < 50 || pct > 100*float64(runtime.NumCPU()) {
		t.Fatalf("self CPU = %.0f%%", pct)
	}
	// Cost can read 0 where a sample is quicker than the clock's resolution (Windows).
	if smp.Cost < 0 || s.Latest().At != smp.At {
		t.Fatalf("sample = cost %s at %v, latest %v", smp.Cost, smp.At, s.Latest().At)
	}
}

func TestSamplerPercentAndPIDReuse(t *testing.T) {
	t0 := time.Unix(1000, 0)
	now := t0
	start := t0.Add(-time.Hour)
	rows := []Proc{{PID: 10, Name: "a", Start: start, CPU: time.Second}, {PID: 20, Name: "b", CPU: 0}, {PID: 30, Partial: true}}
	s := &Sampler{read: func() ([]Proc, error) { return append([]Proc(nil), rows...), nil }, now: func() time.Time { return now }}
	first, _ := s.Sample()
	for _, p := range first.Procs {
		if p.CPUPercent != 0 {
			t.Fatalf("first sight has a percentage: %+v", p)
		}
	}
	now = t0.Add(2 * time.Second)
	rows[0].CPU = 2 * time.Second      // 1 s over 2 s: 50%
	rows[1].CPU = 4 * time.Second      // no start time: matched by name, 200%
	rows = append(rows, Proc{PID: 40}) // new
	got, _ := s.Sample()
	want := map[int]float64{10: 50, 20: 200, 30: 0, 40: 0}
	for _, p := range got.Procs {
		if p.CPUPercent != want[p.PID] {
			t.Fatalf("pid %d: %.1f%%, want %.1f%%", p.PID, p.CPUPercent, want[p.PID])
		}
	}
	// PID 10 is reused by a new process (another start time): not compared.
	now = t0.Add(4 * time.Second)
	rows[0] = Proc{PID: 10, Name: "a", Start: t0.Add(3 * time.Second), CPU: 10 * time.Second}
	got, _ = s.Sample()
	if got.Procs[0].CPUPercent != 0 {
		t.Fatalf("reused pid compared with its predecessor: %+v", got.Procs[0])
	}
}

func TestRunAdaptiveInterval(t *testing.T) {
	var calls atomic.Int32
	s := &Sampler{read: func() ([]Proc, error) { calls.Add(1); return nil, nil }, now: time.Now,
		busyEvery: 20 * time.Millisecond, idleEvery: 200 * time.Millisecond}
	var busy atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, busy.Load, nil); close(done) }()

	time.Sleep(450 * time.Millisecond) // idle: the first sample, then one per 200 ms
	idle := calls.Load()
	busy.Store(true)
	time.Sleep(400 * time.Millisecond) // busy: one per 20 ms
	busyCalls := calls.Load() - idle
	cancel()
	<-done
	if idle < 2 || idle > 4 {
		t.Fatalf("idle: %d samples in 450 ms at 200 ms", idle)
	}
	if busyCalls < 6 { // ~20 expected; a loaded machine coalesces ticks
		t.Fatalf("busy: %d samples in 400 ms at 20 ms", busyCalls)
	}
}
