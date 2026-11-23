package worker

import (
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

func TestTelemetryThrottleAdaptiveAndDeltaSuppression(t *testing.T) {
	th := NewTelemetryThrottle(10 * time.Second)
	if got := th.NextInterval(2); got != 10*time.Second {
		t.Fatalf("expected 10s active interval, got %v", got)
	}
	if got := th.NextInterval(0); got != 30*time.Second {
		t.Fatalf("expected 30s idle interval, got %v", got)
	}

	t0 := time.Now().UTC()
	m1 := &protocol.WorkerMetrics{CPUPercent: 12.0, MemUsedMB: 4096, DiskFreeGB: 200}

	// First heartbeat always sends
	if !th.ShouldSend(t0, nil, m1) {
		t.Fatal("first heartbeat must be sent")
	}

	// Negligible drift (+1% CPU, +8MB RAM) within MaxSilent must be throttled
	mMinor := &protocol.WorkerMetrics{CPUPercent: 13.0, MemUsedMB: 4104, DiskFreeGB: 200}
	if th.ShouldSend(t0.Add(15*time.Second), nil, mMinor) {
		t.Fatal("minor idle drift should be suppressed to save bandwidth/CPU")
	}

	// Significant CPU spike (+10%) triggers immediate delta send
	mSpike := &protocol.WorkerMetrics{CPUPercent: 23.0, MemUsedMB: 4104, DiskFreeGB: 200}
	if !th.ShouldSend(t0.Add(20*time.Second), nil, mSpike) {
		t.Fatal("significant CPU delta should trigger send")
	}

	// Task list change triggers immediate send
	if !th.ShouldSend(t0.Add(25*time.Second), []string{"task-1"}, mSpike) {
		t.Fatal("active task change should trigger send")
	}

	// Exceeding MaxSilent forces keepalive
	if !th.ShouldSend(t0.Add(90*time.Second), []string{"task-1"}, mSpike) {
		t.Fatal("keepalive ceiling should force send")
	}
}

func BenchmarkTelemetryThrottleShouldSend(b *testing.B) {
	th := NewTelemetryThrottle(10 * time.Second)
	now := time.Now().UTC()
	m := &protocol.WorkerMetrics{CPUPercent: 10, MemUsedMB: 2048, DiskFreeGB: 100}
	th.ShouldSend(now, nil, m)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		th.ShouldSend(now.Add(5*time.Second), nil, m)
	}
}
