package worker

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// TestVitalsSample reads this machine's vitals (no subprocess on any platform).
func TestVitalsSample(t *testing.T) {
	s := newVitalsSampler()
	var m protocol.WorkerMetrics
	s.sample(&m)
	time.Sleep(50 * time.Millisecond)
	m = protocol.WorkerMetrics{}
	s.sample(&m)
	t.Logf("%s: cpu %.1f%% load %.2f mem %d/%d MB swap %d/%d MB", runtime.GOOS, m.CPUPercent, m.LoadAvg, m.MemUsedMB, m.MemTotalMB, m.SwapUsedMB, m.SwapTotalMB)
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		t.Skip("no vitals reader on " + runtime.GOOS)
	}
	if m.MemTotalMB == 0 || m.MemUsedMB == 0 || m.MemUsedMB > m.MemTotalMB {
		t.Fatalf("memory %d / %d MB", m.MemUsedMB, m.MemTotalMB)
	}
	if m.CPUPercent < 0 || m.CPUPercent > 100 {
		t.Fatalf("cpu %v", m.CPUPercent)
	}
	if m.SwapUsedMB > m.SwapTotalMB {
		t.Fatalf("swap %d / %d MB", m.SwapUsedMB, m.SwapTotalMB)
	}
	if runtime.GOOS != "windows" && m.LoadAvg < 0 {
		t.Fatalf("load %v", m.LoadAvg)
	}
}

func TestMetricsSamplerGPUCadence(t *testing.T) {
	calls := 0
	g := 40.0
	s := &metricsSampler{vit: newVitalsSampler(), gpuFn: func(context.Context) *float64 { calls++; v := g; return &v }}
	t0 := time.Now()
	ctx := context.Background()
	if m := s.collect(ctx, false, t0); m.GPUPercent == nil || *m.GPUPercent != 40 || calls != 1 {
		t.Fatalf("first sample: gpu %v, calls %d", m.GPUPercent, calls)
	}
	// Idle: the last reading rides along until it is gpuIdleEvery old.
	g = 90
	if m := s.collect(ctx, false, t0.Add(time.Minute)); *m.GPUPercent != 40 || calls != 1 {
		t.Fatalf("idle: gpu %v, calls %d", *m.GPUPercent, calls)
	}
	if m := s.collect(ctx, false, t0.Add(gpuIdleEvery)); *m.GPUPercent != 90 || calls != 2 {
		t.Fatalf("idle after 5 min: gpu %v, calls %d", *m.GPUPercent, calls)
	}
	// Active: every sample.
	g = 10
	if m := s.collect(ctx, true, t0.Add(gpuIdleEvery+time.Second)); *m.GPUPercent != 10 || calls != 3 {
		t.Fatalf("active: gpu %v, calls %d", *m.GPUPercent, calls)
	}
}

func TestDedupPorts(t *testing.T) {
	in := []protocol.ListeningPort{{Port: 8080, Process: "a"}, {Port: 3000, Process: "b"}, {Port: 8080, Process: "c"}}
	got := dedupPorts(in)
	if len(got) != 2 || got[0].Port != 3000 || got[1].Port != 8080 || got[1].Process != "a" {
		t.Fatalf("got %+v", got)
	}
	var many []protocol.ListeningPort
	for p := range 40 {
		many = append(many, protocol.ListeningPort{Port: 2000 + p})
	}
	if n := len(dedupPorts(many)); n != maxPorts {
		t.Fatalf("got %d ports, want %d", n, maxPorts)
	}
}

// BenchmarkVitalsSample is the per-heartbeat cost of the vitals read.
func BenchmarkVitalsSample(b *testing.B) {
	s := newVitalsSampler()
	var m protocol.WorkerMetrics
	b.ReportAllocs()
	for b.Loop() {
		m = protocol.WorkerMetrics{}
		s.sample(&m)
	}
	_ = m
}
