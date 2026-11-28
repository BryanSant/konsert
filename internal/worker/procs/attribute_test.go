package procs

import (
	"testing"
	"time"
)

func sample(at time.Time, ps ...Proc) Sample { return Sample{At: at, Procs: ps} }

func proc(pid, ppid int, cpu float64, rssMB uint64) Proc {
	return Proc{PID: pid, PPID: ppid, Name: "p", Start: time.Unix(int64(pid), 0), CPUPercent: cpu, RSS: rssMB << 20}
}

func byKey(us []SessionUsage) map[string]SessionUsage {
	m := map[string]SessionUsage{}
	for _, u := range us {
		m[u.Key] = u
	}
	return m
}

func TestAttributeTreeAndLineage(t *testing.T) {
	a := NewAttributor()
	t0 := time.Unix(1000, 0)
	roots := []Root{{Key: "task:t1", PID: 100}, {Key: "observed:s1", PID: 200}}
	// t1's agent (100) runs a shell (101) running gradle (102), which forks its daemon
	// (103). The observed session has one helper. 300 is the user's own build.
	got := byKey(a.Attribute(sample(t0,
		proc(1, 0, 0, 10), proc(100, 1, 10, 300), proc(101, 100, 0, 5), proc(102, 101, 50, 200), proc(103, 102, 80, 900),
		proc(200, 1, 5, 400), proc(201, 200, 1, 20),
		proc(300, 1, 90, 1000),
	), roots))
	if u := got["task:t1"]; u.Procs != 4 || u.CPUPercent != 140 || u.RSS != 1405<<20 || u.Orphans != 0 {
		t.Fatalf("task: %+v", u)
	}
	if u := got["observed:s1"]; u.Procs != 2 || u.RSS != 420<<20 {
		t.Fatalf("observed: %+v", u)
	}
	if len(got) != 2 {
		t.Fatalf("the user's build or init was attributed: %+v", got)
	}

	// The build ends: gradle (102) exits and its daemon is reparented to init. It stays
	// t1's, as an orphan; its own new child is t1's too.
	got = byKey(a.Attribute(sample(t0.Add(5*time.Second),
		proc(1, 0, 0, 10), proc(100, 1, 1, 300), proc(101, 100, 0, 5), proc(103, 1, 5, 950), proc(104, 103, 1, 10),
		proc(200, 1, 5, 400),
		proc(300, 1, 90, 1000),
	), roots))
	if u := got["task:t1"]; u.Procs != 4 || u.Orphans != 1 || u.PeakRSS != 1405<<20 {
		t.Fatalf("after reparenting: %+v", u)
	}

	// The task ends: its daemon is still running and still counts as t1's until it exits.
	got = byKey(a.Attribute(sample(t0.Add(10*time.Second),
		proc(1, 0, 0, 10), proc(103, 1, 5, 950), proc(300, 1, 90, 1000),
	), roots[1:]))
	if u := got["task:t1"]; u.Procs != 1 || u.Orphans != 1 {
		t.Fatalf("after the session ended: %+v", u)
	}
	// A new process reusing PID 103 (another start time) is not t1's.
	reused := proc(103, 1, 5, 10)
	reused.Start = t0.Add(time.Minute)
	got = byKey(a.Attribute(sample(t0.Add(15*time.Second), proc(1, 0, 0, 10), reused), nil))
	if len(got) != 0 {
		t.Fatalf("reused pid attributed: %+v", got)
	}
}

func TestAttributeContainerMembersAndPriority(t *testing.T) {
	a := NewAttributor()
	t0 := time.Unix(1000, 0)
	// 501 was double-forked out of task:t2's tree before any sample saw the link; the
	// session's cgroup still holds it. 600 is claimed by both roots: the first wins.
	got := byKey(a.Attribute(sample(t0,
		proc(500, 1, 1, 100), proc(501, 1, 30, 200), proc(600, 500, 1, 50),
	), []Root{{Key: "task:t2", PID: 500, Members: []int{500, 501, 999}}, {Key: "observed:x", PID: 600}}))
	if u := got["task:t2"]; u.Procs != 3 || u.CPUPercent != 32 {
		t.Fatalf("container members: %+v", u)
	}
	if _, ok := got["observed:x"]; ok {
		t.Fatal("a process went to two sessions")
	}
}

func TestAttributePeakForgotten(t *testing.T) {
	a := NewAttributor()
	t0 := time.Unix(1000, 0)
	a.Attribute(sample(t0, proc(10, 1, 0, 500)), []Root{{Key: "k", PID: 10}})
	a.Attribute(sample(t0.Add(2*peakKeep), proc(1, 0, 0, 1)), nil)
	if len(a.peak) != 0 || len(a.seen) != 0 {
		t.Fatalf("peak kept for a gone session: %v", a.peak)
	}
}
