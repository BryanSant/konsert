package worker

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/procs"
)

func TestProcWatchReportsDaemonExits(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	old := t0.Add(-time.Hour)
	p := func(pid, ppid int, name string, start time.Time, tty uint64) procs.Proc {
		return procs.Proc{PID: pid, PPID: ppid, Name: name, Start: start, TTY: tty}
	}
	base := []procs.Proc{
		p(1, 0, "systemd", old, 0),
		p(500, 1, "systemd", old, 0),      // a user manager
		p(100, 1, "nginx", old, 0),        // a daemon
		p(101, 100, "nginx", old, 0),      // its worker: not watched itself
		p(200, 500, "postgres", old, 0),   // a user service
		p(300, 1, "cron-job", t0, 0),      // too young
		p(400, 1, "agent-helper", old, 0), // a session's
		p(600, 1, "sshd", old, 0), p(601, 600, "sshd", old, 0), p(602, 601, "zsh", old, 34816),
	}
	var w procWatch
	if ex := w.observe(procs.Sample{At: t0, Procs: base}, map[int]bool{400: true}); len(ex) != 0 {
		t.Fatalf("first sample reported exits: %+v", ex)
	}
	if len(w.known) != 4 || w.known[100].name != "nginx" || w.known[200].name != "postgres" || w.known[600].name != "sshd" {
		t.Fatalf("watched: %+v", w.known)
	}
	// nginx stops; postgres restarts with a new PID (counts as an exit of the old one).
	next := []procs.Proc{base[0], base[1], p(201, 500, "postgres", t0, 0), base[5], base[6], base[7], base[8], base[9]}
	ex := w.observe(procs.Sample{At: t0.Add(5 * time.Second), Procs: next}, map[int]bool{400: true})
	names := map[string]protocol.ProcessExitPayload{}
	for _, e := range ex {
		names[e.Name] = e
	}
	if len(ex) != 2 || names["nginx"].PID != 100 || !names["nginx"].After.Equal(t0) || names["postgres"].PID != 200 {
		t.Fatalf("exits: %+v", ex)
	}
	if sh := names["nginx"].SSHShells; len(sh) != 1 || sh[0].PID != 602 || sh[0].Name != "zsh" {
		t.Fatalf("ssh shells: %+v", sh)
	}
}

type fakeKiller struct {
	terms, kills []int
}

func (k *fakeKiller) Terminate(pid int) error { k.terms = append(k.terms, pid); return nil }
func (k *fakeKiller) Kill(pid int) error      { k.kills = append(k.kills, pid); return nil }

func TestKillProcessGuards(t *testing.T) {
	prev := readProcTable
	t.Cleanup(func() { readProcTable = prev })
	self := os.Getpid()
	start := time.Unix(5000, 0)
	var alive atomic.Bool
	alive.Store(true)
	readProcTable = func() ([]procs.Proc, error) {
		list := []procs.Proc{{PID: self, PPID: 77, Name: "syred"}, {PID: 77, PPID: 1, Name: "launcher"}}
		if alive.Load() {
			list = append(list, procs.Proc{PID: 900, PPID: 1, Name: "nginx", Start: start})
		}
		return list, nil
	}
	d := &Daemon{}
	k := &fakeKiller{}
	for _, c := range []struct {
		p    protocol.ProcessKillPayload
		want string
	}{
		{protocol.ProcessKillPayload{PID: self}, "syred"},
		{protocol.ProcessKillPayload{PID: 77}, "syred"},
		{protocol.ProcessKillPayload{PID: 900, StartedAt: start.Add(time.Minute)}, "another one"},
		{protocol.ProcessKillPayload{PID: 12345}, "already exited"},
	} {
		if res := d.killProcess(c.p, k); res.Killed || !containsStr(res.Error, c.want) {
			t.Fatalf("%+v: %+v", c.p, res)
		}
	}
	if len(k.terms) != 0 {
		t.Fatalf("signalled despite a refusal: %v", k.terms)
	}
	// It stops after the polite signal.
	go func() { time.Sleep(200 * time.Millisecond); alive.Store(false) }()
	if res := d.killProcess(protocol.ProcessKillPayload{PID: 900, StartedAt: start}, k); !res.Killed || res.Forced || res.Name != "nginx" {
		t.Fatalf("kill: %+v", res)
	}
	if len(k.terms) != 1 || len(k.kills) != 0 {
		t.Fatalf("signals: term %v kill %v", k.terms, k.kills)
	}
}

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }
