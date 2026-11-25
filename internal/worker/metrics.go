package worker

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// Heartbeat metrics (KON-G8WA3). Every heartbeat carries vitals read without a
// subprocess (vitals_<os>.go): CPU %, load average, memory and swap. GPU utilization
// needs one (ioreg / nvidia-smi), so it is read for every heartbeat only while sessions
// run and otherwise every gpuIdleEvery, the last reading riding along in between.
// Listening ports (lsof / ss) are not in heartbeats: konsertd sends worker.ports every
// portsEvery and when the hub asks (a machine's Details is open). Disk free is a machine
// fact (refreshed every FactsInterval).

const (
	gpuIdleEvery = 5 * time.Minute
	portsEvery   = 5 * time.Minute
	maxPorts     = 32
)

// metricsSampler builds heartbeat metrics. Only the heartbeat goroutine calls collect.
type metricsSampler struct {
	mu    sync.Mutex
	vit   *vitalsSampler
	gpuFn func(context.Context) *float64 // collectGPUPercent; tests replace it
	gpu   *float64
	gpuAt time.Time
}

func newMetricsSampler() *metricsSampler {
	s := &metricsSampler{vit: newVitalsSampler(), gpuFn: readGPU}
	// Take the CPU baseline now, so the first heartbeat already has a delta.
	s.vit.sample(&protocol.WorkerMetrics{})
	return s
}

// collect samples vitals, and the GPU when active or when the last reading is older than
// gpuIdleEvery.
func (s *metricsSampler) collect(ctx context.Context, active bool, now time.Time) *protocol.WorkerMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := &protocol.WorkerMetrics{}
	s.vit.sample(m)
	if s.gpuFn != nil && (active || s.gpuAt.IsZero() || now.Sub(s.gpuAt) >= gpuIdleEvery) {
		s.gpu, s.gpuAt = s.gpuFn(ctx), now
	}
	if s.gpu != nil {
		v := *s.gpu
		m.GPUPercent = &v
	}
	return m
}

// readGPU and collectPorts fork tools; tests replace them.
var readGPU = collectGPUPercent

// collectPorts is CollectListeningPorts (tests replace it).
var collectPorts = CollectListeningPorts

// CollectListeningPorts lists the machine's listening TCP ports (>= 1024), one per port,
// sorted, at most maxPorts. It runs lsof / ss: on demand only.
func CollectListeningPorts(parent context.Context) []protocol.ListeningPort {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	return dedupPorts(collectPlatformPorts(ctx))
}

func dedupPorts(ports []protocol.ListeningPort) []protocol.ListeningPort {
	seen := make(map[int]bool, len(ports))
	out := make([]protocol.ListeningPort, 0, len(ports))
	for _, p := range ports {
		if !seen[p.Port] {
			seen[p.Port] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	if len(out) > maxPorts {
		out = out[:maxPorts]
	}
	return out
}
