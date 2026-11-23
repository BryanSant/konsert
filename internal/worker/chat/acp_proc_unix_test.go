//go:build !windows

package chat

import (
	"bufio"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Kill must stop the whole process group (grandchildren included) and reap the leader.
func TestProcKillGroupNoZombies(t *testing.T) {
	acpIsolateHome(t)
	p, err := StartProc(ProcSpec{Binary: "/bin/sh", Args: []string{"-c", "sleep 60 & echo $!; echo started >&2; wait"}, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(p.Stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	grandchild, _ := strconv.Atoi(strings.TrimSpace(line))
	if grandchild <= 0 {
		t.Fatalf("bad pid %q", line)
	}
	start := time.Now()
	p.Kill(2 * time.Second)
	if !p.Exited() {
		t.Fatal("leader not reaped")
	}
	if time.Since(start) > 2500*time.Millisecond {
		t.Errorf("Kill took %s (SIGTERM ignored?)", time.Since(start))
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(grandchild, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(grandchild, 0) == nil {
		t.Errorf("grandchild %d survived", grandchild)
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
	}
	// The leader itself is reaped: signalling its pid fails (no zombie).
	if err := syscall.Kill(p.Cmd.Process.Pid, 0); err == nil {
		t.Errorf("leader pid %d still exists (zombie?)", p.Cmd.Process.Pid)
	}
	if !strings.Contains(p.StderrTail(100), "started") {
		t.Errorf("stderr tail = %q", p.StderrTail(100))
	}
	p.Kill(0) // idempotent
}
