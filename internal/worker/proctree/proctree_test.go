package proctree

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// readCounter reads the counter the ticking child writes (0 when unreadable).
func readCounter(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(string(b), "\xef\xbb\xbf")))
	return n
}

func waitAdvance(t *testing.T, path string, from int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n := readCounter(path); n > from {
			return n
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("counter did not advance past %d within %s", from, timeout)
	return 0
}

// A suspended tree (a shell and the child it runs) makes no progress; resumed, it
// continues where it stopped.
func TestSuspendResumeFreezesTree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counter")
	pid, stop := startTicker(t, path)
	defer stop()

	// Slow CI runners (Windows starting PowerShell) can stall for seconds: these waits
	// only check that the tree runs, so they are generous; the frozen check is strict.
	waitAdvance(t, path, waitAdvance(t, path, 0, 30*time.Second), 30*time.Second)
	if err := Suspend(pid); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // a write in flight may still land
	frozen := readCounter(path)
	time.Sleep(1500 * time.Millisecond)
	if got := readCounter(path); got != frozen {
		t.Fatalf("suspended tree kept running: counter %d -> %d", frozen, got)
	}
	if err := Resume(pid); err != nil {
		t.Fatalf("resume: %v", err)
	}
	waitAdvance(t, path, frozen, 30*time.Second)
}

func TestSuspendMissingProcess(t *testing.T) {
	if err := Suspend(0); err == nil {
		t.Fatal("suspend of pid 0 succeeded")
	}
}
