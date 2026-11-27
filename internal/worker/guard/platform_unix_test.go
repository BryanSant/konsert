//go:build linux || darwin

package guard

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRealShedKillsTree sheds a real process tree with the native platform: the
// grandchild dies with the root and the owner hears about it before the kill.
func TestRealShedKillsTree(t *testing.T) {
	g := newGuard(nativePlatform(), Config{Baseline: NewBaseline(8<<30, true, "bare_metal"), Mode: ModeAdaptive})
	g.running = true // no watchdog loop: shed by hand
	l := g.Prepare(LaunchSpec{Binary: "sh", Args: []string{"-c", "sleep 60 & echo $!; wait"}, Owner: "tree", Kind: "task"})
	cmd := exec.Command(l.Binary, l.Args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := l.Attach(cmd.Process.Pid)
	buf := make([]byte, 64)
	n, _ := out.Read(buf)
	child, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatalf("child pid %q: %v", buf[:n], err)
	}

	var heard []Evidence
	defer OnShed(func(ev Evidence) {
		// Frozen, not yet killed: the child still exists.
		if syscall.Kill(child, 0) != nil {
			t.Error("child gone before subscribers ran")
		}
		heard = append(heard, ev)
	})()
	g.shed(s, ReasonBelowReserve, Signal{Avail: 1, AvailOK: true}, g.baseline.Reserve, g.baseline.TotalRAM, 1)

	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("root survived the shed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(child, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("grandchild survived the shed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(heard) != 1 || heard[0].Owner != "tree" || heard[0].RSS == 0 {
		t.Fatalf("evidence = %+v", heard)
	}
	if u := s.Release(cmd.ProcessState); u.Shed == nil {
		t.Fatal("Release lost the shed")
	}
}

// A root killed by SIGKILL while the kernel's OOM count rose is reported OOM-killed;
// the same death without an OOM kill, or a shed, is not.
func TestReleaseOOMKilled(t *testing.T) {
	run := func(t *testing.T, ooms bool, shed bool) Usage {
		g, p, clk := testGuard(ModeAdaptive)
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		s := attach(g, clk, cmd.Process.Pid, PriorityTicket, "job")
		if ooms {
			p.mu.Lock()
			p.ooms++
			p.mu.Unlock()
		}
		if shed {
			s.mu.Lock()
			s.shed = &Evidence{Reason: ReasonBelowReserve}
			s.mu.Unlock()
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return s.Release(cmd.ProcessState)
	}
	if u := run(t, true, false); !u.OOMKilled {
		t.Fatal("SIGKILL with an OOM kill not reported")
	}
	if u := run(t, false, false); u.OOMKilled {
		t.Fatal("SIGKILL without an OOM kill reported as OOM")
	}
	if u := run(t, true, true); u.OOMKilled {
		t.Fatal("a shed reported as OOM")
	}
}
