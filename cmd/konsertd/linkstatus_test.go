package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/worker"
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
