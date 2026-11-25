package controller

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/sshutil"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/coder/websocket"
)

func newTestTracker(c *testClock) *restartTracker {
	t := newRestartTracker()
	t.now = c.get
	return t
}

// A daemon that is down is due after three failed attaches in a row, then restarts
// back off 1, 2, 5 and then 10 minutes; attaching ends the outage.
func TestRestartTrackerBackoff(t *testing.T) {
	c := &testClock{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	tr := newTestTracker(c)
	const id = "w1"
	down := func() bool {
		due, _ := tr.failed(id, sshutil.AttachDaemonDown, "konsertd is not running here")
		return due
	}
	if first, second := down(), down(); first || second {
		t.Fatal("due before three failures")
	}
	if tr.view(id) != nil {
		t.Fatal("a brief outage is not shown")
	}
	if !down() {
		t.Fatal("not due after three failures")
	}
	var waits []time.Duration
	for attempt := 1; attempt <= 6; attempt++ {
		n, next := tr.restarted(id)
		if n != attempt {
			t.Fatalf("attempt = %d, want %d", n, attempt)
		}
		waits = append(waits, next.Sub(c.get()))
		// Failing attaches before the next restart is allowed do not restart.
		c.add(next.Sub(c.get()) - time.Second)
		if down() {
			t.Fatalf("restart %d: due before its backoff ended", attempt)
		}
		c.add(time.Second)
		if !down() {
			t.Fatalf("restart %d: not due after its backoff", attempt)
		}
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("backoff = %v, want %v", waits, want)
		}
	}
	v := tr.view(id)
	if v == nil || v.State != "down" || v.Attempt != 6 || v.NextAt == nil || v.Error != "konsertd is not running here" {
		t.Fatalf("view = %+v", v)
	}
	if !tr.attached(id) || tr.view(id) != nil || tr.attached(id) {
		t.Fatal("attaching must end the outage")
	}
	// A new outage starts over: three failures, attempt 1.
	if first, second, third := down(), down(), down(); first || second || !third {
		t.Fatal("a new outage must start over")
	}
	if n, next := tr.restarted(id); n != 1 || next.Sub(c.get()) != time.Minute {
		t.Fatalf("new outage: attempt %d next in %s", n, next.Sub(c.get()))
	}
}

// A daemon failing for over a minute is due even with fewer failures (slow dials).
func TestRestartTrackerWindow(t *testing.T) {
	c := &testClock{now: time.Unix(1_800_000_000, 0)}
	tr := newTestTracker(c)
	if due, _ := tr.failed("w", sshutil.AttachDaemonDown, "down"); due {
		t.Fatal("due at once")
	}
	c.add(time.Minute)
	if due, _ := tr.failed("w", sshutil.AttachDaemonDown, "down"); !due {
		t.Fatal("not due after a minute of failing")
	}
}

// An unreachable host is never restarted (only re-dialed), and failures of another
// kind do not count toward a restart; the startup hold delays restarts.
func TestRestartTrackerUnreachableAndHold(t *testing.T) {
	c := &testClock{now: time.Unix(1_800_000_000, 0)}
	tr := newTestTracker(c)
	for range 10 {
		if due, _ := tr.failed("w", sshutil.AttachUnreachable, "ssh: connect to host box port 22: Connection refused"); due {
			t.Fatal("an unreachable host must not be restarted")
		}
		c.add(time.Minute)
	}
	if v := tr.view("w"); v == nil || v.State != "unreachable" || v.Attempt != 0 || !strings.Contains(v.Error, "refused") {
		t.Fatalf("unreachable view = %+v", v)
	}
	// The host answers again but konsertd is down: a fresh count.
	if due, changed := tr.failed("w", sshutil.AttachDaemonDown, "down"); due || !changed {
		t.Fatalf("first down failure: due %v changed %v", due, changed)
	}
	for range 5 {
		if due, _ := tr.failed("o", sshutil.AttachFailureOther, "handshake timeout"); due {
			t.Fatal("an unexplained failure must not restart")
		}
	}
	if tr.view("o") != nil {
		t.Fatal("unexplained failures are not shown as an outage")
	}

	tr.holdUntil(c.get().Add(15 * time.Second))
	for range 5 {
		if due, _ := tr.failed("h", sshutil.AttachDaemonDown, "down"); due {
			t.Fatal("restarted during the startup hold")
		}
	}
	c.add(15 * time.Second)
	if due, _ := tr.failed("h", sshutil.AttachDaemonDown, "down"); !due {
		t.Fatal("not due after the hold")
	}
}

func TestClassifyAttachTransports(t *testing.T) {
	if f := classifyAttach(TransportLocal, errDaemonSocket{errors.New("connect: connection refused")}); f != sshutil.AttachDaemonDown {
		t.Fatalf("local socket dial = %s", f)
	}
	if f := classifyAttach(TransportLocal, errors.New("failed to WebSocket dial")); f != sshutil.AttachFailureOther {
		t.Fatalf("local handshake = %s", f)
	}
	if f := classifyAttach(TransportSSH, errors.New("ssh: connect to host box port 22: No route to host")); f != sshutil.AttachUnreachable {
		t.Fatalf("ssh no route = %s", f)
	}
}

// down stops accepting attaches for id and drops its links (the daemon died).
func (f *fakeDaemons) down(id string) {
	f.mu.Lock()
	delete(f.infos, id)
	var links []*fakeLink
	for _, l := range f.links {
		if l.dialed == id {
			links = append(links, l)
		}
	}
	f.mu.Unlock()
	for _, l := range links {
		_ = l.ws.Close(websocket.StatusGoingAway, "daemon died")
	}
}

func restartAudits(t *testing.T, st store.Store) []store.AuditEvent {
	t.Helper()
	ev, err := st.ListAudit(context.Background(), store.AuditFilter{Action: AuditWorkerRestart})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// enableRestarts turns automatic restarts on with a settable clock.
func (lt *launchTest) enableRestarts() *testClock {
	c := &testClock{now: time.Now()}
	tr := lt.srv.hub.restarts
	tr.mu.Lock()
	tr.now = c.get
	tr.enableAt = time.Time{}
	tr.mu.Unlock()
	return c
}

// The hub's own konsertd dies: the attach loop restarts it with `konsertd install`
// (once per outage, never a second daemon), audit-logs it and re-adopts the daemon.
func TestRestartLocalDaemonThatDied(t *testing.T) {
	lt := newLaunchTest(t)
	lt.enableRestarts()
	lt.runner.installFn = func([]string) error {
		lt.daemons.up(testWorkerInfo(localID, "hubhost"))
		return nil
	}
	lt.daemons.up(testWorkerInfo(localID, "hubhost"))
	w, err := lt.st.EnsureWorker(context.Background(), store.Worker{ID: localID, Name: "hubhost", Launch: store.LaunchLocal})
	if err != nil {
		t.Fatal(err)
	}
	lt.srv.attach.ensure(w)
	waitFor(t, 3*time.Second, "local daemon attached", func() bool { return lt.srv.hub.IsWorkerConnected(localID) })
	if calls, _ := lt.runner.installCalls(); len(calls) != 0 {
		t.Fatalf("a running daemon must not be restarted: %q", calls)
	}

	lt.daemons.down(localID)
	waitFor(t, 5*time.Second, "restarted daemon re-adopted", func() bool {
		calls, _ := lt.runner.installCalls()
		return len(calls) == 1 && lt.srv.hub.IsWorkerConnected(localID)
	})
	ev := restartAudits(t, lt.st)
	if len(ev) != 1 || ev[0].WorkerID != localID || ev[0].User != "system" || !strings.Contains(ev[0].Message, "attempt 1") ||
		!strings.Contains(ev[0].Message, sshutil.DaemonDownMarker) {
		t.Fatalf("restart audits = %+v", ev)
	}
	waitFor(t, 3*time.Second, "outage cleared", func() bool { return lt.srv.hub.restarts.view(localID) == nil })
	time.Sleep(200 * time.Millisecond)
	if calls, _ := lt.runner.installCalls(); len(calls) != 1 {
		t.Fatalf("one restart per outage: %q", calls)
	}

	// It dies again: a new outage, restarted again from attempt 1.
	lt.daemons.down(localID)
	waitFor(t, 5*time.Second, "second restart", func() bool {
		calls, _ := lt.runner.installCalls()
		return len(calls) == 2 && lt.srv.hub.IsWorkerConnected(localID)
	})
	if ev := restartAudits(t, lt.st); len(ev) != 2 || !strings.Contains(ev[0].Message, "attempt 1") {
		t.Fatalf("restart audits = %+v", ev)
	}
}

// A down SSH daemon whose restart keeps failing is retried on the backoff schedule,
// with one start in flight at a time, and the machine view says so.
func TestRestartSSHDaemonBacksOffAndShowsState(t *testing.T) {
	lt := newLaunchTest(t)
	clock := lt.enableRestarts()
	var inFlight, maxInFlight atomic.Int32
	release := make(chan struct{})
	lt.mu.Lock()
	lt.bootRes = func(o sshutil.BootstrapOptions) sshutil.BootstrapResult {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		<-release
		return sshutil.BootstrapResult{OK: false, Target: o.Target, Stage: "configuring_service", Error: "konsertd install failed: exit status 1"}
	}
	lt.mu.Unlock()
	w := lt.addSSHWorker(t, remoteID, "box", "me@box")
	lt.srv.attach.ensure(w)

	waitFor(t, 5*time.Second, "first restart", func() bool { return len(lt.bootstraps()) == 1 })
	// While it runs, more failed attaches start nothing else.
	time.Sleep(300 * time.Millisecond)
	if n := len(lt.bootstraps()); n != 1 {
		t.Fatalf("starts while one is in flight: %d", n)
	}
	v := lt.srv.hub.workerRestartView(remoteID)
	if v == nil || v.State != "down" || v.Attempt != 1 || v.NextAt == nil || v.NextAt.Sub(clock.get()) != time.Minute {
		t.Fatalf("restart view = %+v", v)
	}
	close(release)
	waitFor(t, 3*time.Second, "failed start recorded", func() bool {
		return strings.Contains(lt.worker(t, remoteID).LaunchError, "konsertd install failed")
	})
	// Not again before the backoff ends...
	time.Sleep(300 * time.Millisecond)
	if n := len(lt.bootstraps()); n != 1 {
		t.Fatalf("restarted before the backoff: %d", n)
	}
	// ...and again once it has.
	clock.add(time.Minute)
	waitFor(t, 5*time.Second, "second restart", func() bool { return len(lt.bootstraps()) == 2 })
	waitFor(t, 3*time.Second, "attempt 2 shown", func() bool {
		v := lt.srv.hub.workerRestartView(remoteID)
		return v != nil && v.Attempt == 2 && v.NextAt.Sub(clock.get()) == 2*time.Minute
	})
	if maxInFlight.Load() != 1 {
		t.Fatalf("starts in flight at once = %d", maxInFlight.Load())
	}
	// The fleet view carries it.
	res, err := lt.srv.service.FleetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Workers) != 1 || res.Workers[0].Restart == nil || res.Workers[0].Restart.Attempt != 2 {
		t.Fatalf("fleet view = %+v", res.Workers)
	}
	if ev := restartAudits(t, lt.st); len(ev) != 2 {
		t.Fatalf("restart audits = %+v", ev)
	}

	// The daemon comes back (started by hand): the outage ends.
	lt.daemons.up(testWorkerInfo(remoteID, "box"))
	waitFor(t, 5*time.Second, "re-adopted", func() bool { return lt.srv.hub.IsWorkerConnected(remoteID) })
	waitFor(t, 3*time.Second, "outage cleared", func() bool { return lt.srv.hub.restarts.view(remoteID) == nil })
}

// A host ssh cannot reach is re-dialed, never restarted, and shown as unreachable.
func TestUnreachableSSHHostIsNotRestarted(t *testing.T) {
	lt := newLaunchTest(t)
	clock := lt.enableRestarts()
	var dials atomic.Int32
	lt.srv.attach.dialSSH = func(context.Context, string, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("ssh: connect to host box port 22: Connection refused")
	}
	w := lt.addSSHWorker(t, remoteID, "box", "me@box")
	lt.srv.attach.ensure(w)
	waitFor(t, 5*time.Second, "several dials", func() bool { return dials.Load() >= 5 })
	clock.add(time.Hour)
	waitFor(t, 5*time.Second, "more dials", func() bool { return dials.Load() >= 7 })
	if n := len(lt.bootstraps()); n != 0 {
		t.Fatalf("an unreachable host was restarted %d times", n)
	}
	if v := lt.srv.hub.workerRestartView(remoteID); v == nil || v.State != "unreachable" || v.Attempt != 0 {
		t.Fatalf("view = %+v", v)
	}
	if ev := restartAudits(t, lt.st); len(ev) != 0 {
		t.Fatalf("audits = %+v", ev)
	}
}

// Restarts never run for a machine the hub stopped dialing (removed).
func TestRemovedMachineForgetsOutage(t *testing.T) {
	lt := newLaunchTest(t)
	lt.enableRestarts()
	lt.srv.attach.dialSSH = func(context.Context, string, string, string) (net.Conn, error) {
		return nil, errors.New("ssh: connect to host box port 22: Connection refused")
	}
	w := lt.addSSHWorker(t, remoteID, "box", "me@box")
	lt.srv.attach.ensure(w)
	waitFor(t, 5*time.Second, "outage shown", func() bool { return lt.srv.hub.restarts.view(remoteID) != nil })
	lt.srv.attach.stop(remoteID)
	waitFor(t, 3*time.Second, "loop ended", func() bool { return !lt.srv.attach.running(remoteID) })
	if lt.srv.hub.restarts.view(remoteID) != nil {
		t.Fatal("a stopped loop must forget its outage")
	}
}
