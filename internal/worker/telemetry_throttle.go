package worker

import (
	"math"
	"slices"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// TelemetryThrottle enforces adaptive sampling intervals and delta-only heartbeat transmission
// so idle fleet daemons stay gently below <1% CPU and avoid chatty redundant network frames.
type TelemetryThrottle struct {
	ActiveInterval time.Duration // faster cadence when tasks are running (default 10s)
	IdleInterval   time.Duration // gentler cadence when idle (default 30s)
	MaxSilent      time.Duration // forced keepalive ceiling (default 60s)

	lastSentAt   time.Time
	lastTaskIDs  []string
	lastMetrics  *protocol.WorkerMetrics
	sentCount    int
	skippedCount int
}

func NewTelemetryThrottle(baseActive time.Duration) *TelemetryThrottle {
	if baseActive <= 0 {
		baseActive = 10 * time.Second
	}
	idle := baseActive * 3
	if idle < 30*time.Second && baseActive >= 10*time.Second {
		idle = 30 * time.Second
	}
	maxSilent := idle * 2
	if maxSilent < 2*baseActive {
		maxSilent = 2 * baseActive
	}
	return &TelemetryThrottle{
		ActiveInterval: baseActive,
		IdleInterval:   idle,
		MaxSilent:      maxSilent,
	}
}

// NextInterval returns the adaptive sampling interval based on whether sessions are active.
func (t *TelemetryThrottle) NextInterval(activeTaskCount int) time.Duration {
	if activeTaskCount > 0 {
		return t.ActiveInterval
	}
	return t.IdleInterval
}

// ShouldSend returns true only when metrics/tasks changed meaningfully or MaxSilent elapsed.
func (t *TelemetryThrottle) ShouldSend(now time.Time, activeTaskIDs []string, metrics *protocol.WorkerMetrics) bool {
	if t.lastSentAt.IsZero() || now.Sub(t.lastSentAt) >= t.MaxSilent {
		t.record(now, activeTaskIDs, metrics)
		return true
	}
	if !slices.Equal(t.lastTaskIDs, activeTaskIDs) {
		t.record(now, activeTaskIDs, metrics)
		return true
	}
	if metricsChangedSignificantly(t.lastMetrics, metrics) {
		t.record(now, activeTaskIDs, metrics)
		return true
	}
	t.skippedCount++
	return false
}

func (t *TelemetryThrottle) record(now time.Time, activeTaskIDs []string, metrics *protocol.WorkerMetrics) {
	t.lastSentAt = now
	t.lastTaskIDs = slices.Clone(activeTaskIDs)
	if metrics != nil {
		cp := *metrics
		cp.ListeningPorts = slices.Clone(metrics.ListeningPorts)
		t.lastMetrics = &cp
	} else {
		t.lastMetrics = nil
	}
	t.sentCount++
}

func metricsChangedSignificantly(prev, cur *protocol.WorkerMetrics) bool {
	if prev == nil || cur == nil {
		return prev != cur
	}
	if math.Abs(prev.CPUPercent-cur.CPUPercent) >= 5.0 {
		return true
	}
	if absDiffUint64(prev.MemUsedMB, cur.MemUsedMB) >= 64 {
		return true
	}
	if absDiffUint64(prev.SwapUsedMB, cur.SwapUsedMB) >= 64 {
		return true
	}
	if prev.DiskFreeGB != cur.DiskFreeGB {
		return true
	}
	if len(prev.ListeningPorts) != len(cur.ListeningPorts) {
		return true
	}
	for i := range prev.ListeningPorts {
		if prev.ListeningPorts[i] != cur.ListeningPorts[i] {
			return true
		}
	}
	return false
}

func absDiffUint64(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}
