package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker"
)

func TestWriteLinkStatus(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.Local)
	var b bytes.Buffer
	writeLinkStatus(&b, worker.LinkStatus{
		State: worker.LinkDisconnected, Transport: "outbound", Peer: "wss://hub/ws/worker",
		Since: now.Add(-3 * time.Minute), LastError: "dial: connection refused",
		NextAttempt: now.Add(12 * time.Second), Attempts: 7, PID: 42,
	}, now, true)
	out := b.String()
	for _, want := range []string{
		"Link       : disconnected", "Transport  : outbound (wss://hub/ws/worker)", "(3m0s ago)",
		"Last error : dial: connection refused", "Next try   : in 12s (7 failed so far)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	b.Reset()
	writeLinkStatus(&b, worker.LinkStatus{State: worker.LinkConnected, PID: 42, Since: now}, now, false)
	if !strings.Contains(b.String(), "not running (pid 42 is gone; last state connected") {
		t.Errorf("stopped daemon:\n%s", b.String())
	}
}

func TestWriteLinkStatusSpool(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.Local)
	var b bytes.Buffer
	writeLinkStatus(&b, worker.LinkStatus{
		State: worker.LinkConnected, Transport: "local", PID: 42, Since: now,
		Spool: &protocol.SpoolStatus{
			Bytes: 3 << 20, BudgetBytes: 256 << 20, Records: 1200, OldestUnsentAt: now.Add(-10 * time.Minute),
			Replaying: true, ReplaySent: 300, ReplayTotal: 1200, Full: true, FullReason: "over 256 MB", Dropped: 9,
		},
	}, now, true)
	out := b.String()
	for _, want := range []string{
		"Spool      : 3.0 MB of 256.0 MB, 1200 records not yet acknowledged, oldest unsent 10m0s ago",
		"Replay     : 300 of 1200 records sent",
		"SPOOL FULL : over 256 MB: vitals and activity are no longer kept (9 dropped)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
