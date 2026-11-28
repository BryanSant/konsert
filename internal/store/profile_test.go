package store

import (
	"testing"
	"time"
)

func TestComputeWorkProfile(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	at := func(m int) *time.Time { v := t0.Add(time.Duration(m) * time.Minute); return &v }
	d := func(s int64) *int64 { return &s }
	attempts := []Attempt{
		{Status: AttemptCompleted, StartedAt: at(0), EndedAt: at(10), DurationS: d(600), PeakRSSMB: 2000, CPUSeconds: 900},
		{Status: AttemptReview, StartedAt: at(20), EndedAt: at(40), PeakRSSMB: 3000, CPUSeconds: 1500}, // duration from times
		{Status: AttemptCompleted, StartedAt: at(50), EndedAt: at(55), DurationS: d(300)},              // no metric history
		{Status: AttemptFailed, FailureCategory: "oom", EndedAt: at(60), PeakRSSMB: 9000},
		{Status: AttemptFailed, FailureCategory: "oom", EndedAt: at(70)},
		{Status: AttemptFailed, FailureCategory: "timeout", EndedAt: at(80)},
		{Status: AttemptCanceled, EndedAt: at(90)},
		{Status: AttemptCompleted}, // still open: ignored
	}
	p := ComputeWorkProfile("p/e/r", "linux/amd64/32g", attempts, t0)
	if p.Runs != 6 || p.Successes != 3 || p.Failures != 3 || p.Canceled != 1 || p.SuccessRate != 0.5 {
		t.Fatalf("counts: %+v", p)
	}
	if p.DurationAvgS != 700 || p.DurationP50S != 600 || p.DurationP90S != 1200 {
		t.Fatalf("durations: avg %v p50 %v p90 %v", p.DurationAvgS, p.DurationP50S, p.DurationP90S)
	}
	if p.PeakRSSMaxMB != 9000 || p.PeakRSSAvgMB != 14000.0/3 || p.CPUSecondsAvg != 1200 {
		t.Fatalf("resources: %+v", p)
	}
	if len(p.CommonFailures) != 2 || p.CommonFailures[0] != (FailureCount{"oom", 2}) || p.CommonFailures[1].Category != "timeout" {
		t.Fatalf("failures: %+v", p.CommonFailures)
	}
	if p.LastAttemptAt == nil || !p.LastAttemptAt.Equal(*at(90)) {
		t.Fatalf("last attempt %v", p.LastAttemptAt)
	}
	if empty := ComputeWorkProfile("k", "c", nil, t0); empty.Runs != 0 || empty.SuccessRate != 0 {
		t.Fatalf("empty: %+v", empty)
	}
}
