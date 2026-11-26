//go:build !windows

package worker

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func waitForPtyOutput(t *testing.T, p PtyProcess, needle string, timeout time.Duration) {
	t.Helper()
	ready := make(chan struct{})
	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 256)
		for {
			n, err := p.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
				if bytes.Contains(buf.Bytes(), []byte(needle)) {
					close(ready)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %q in PTY output", needle)
	}
}

func TestUnixPtyStopGraceHonouredThenKill(t *testing.T) {
	// Child ignores SIGINT so only SIGKILL after the grace period will terminate it.
	p, err := StartPty(context.Background(), PtyStartOptions{
		Binary: "sh",
		Args:   []string{"-c", `trap '' INT; echo READY; while :; do sleep 1; done`},
		Dir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("StartPty: %v", err)
	}
	defer p.Close()

	waitForPtyOutput(t, p, "READY", 5*time.Second)

	grace := 400 * time.Millisecond
	start := time.Now()
	if err := p.TerminateWithGrace(grace); err != nil {
		t.Fatalf("TerminateWithGrace: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < grace-50*time.Millisecond {
		t.Fatalf("expected grace period (%v) to be honoured before SIGKILL, elapsed=%v", grace, elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("process took too long to die after grace (%v): elapsed=%v", grace, elapsed)
	}
}

func TestUnixPtyStopImmediateOnSIGINT(t *testing.T) {
	// Child exits immediately on SIGINT (background sleep + wait so POSIX sh
	// delivers the INT trap immediately instead of deferring it).
	p, err := StartPty(context.Background(), PtyStartOptions{
		Binary: "sh",
		Args:   []string{"-c", `trap 'exit 0' INT; echo READY; sleep 30 & wait`},
		Dir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("StartPty: %v", err)
	}
	defer p.Close()

	waitForPtyOutput(t, p, "READY", 5*time.Second)

	start := time.Now()
	if err := p.TerminateWithGrace(5 * time.Second); err != nil {
		t.Fatalf("TerminateWithGrace: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("expected immediate stop when process exits on SIGINT, elapsed=%v", elapsed)
	}
}
