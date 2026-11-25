package controller

import (
	"context"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestWakeGuardDetectsJumpsAndTracksGrace(t *testing.T) {
	var g wakeGuard
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	linked := func() []string { return []string{"a", "b"} }
	if g.observe(t0, 30*time.Second, 2*time.Minute, linked) {
		t.Fatal("first observation is a baseline")
	}
	if g.observe(t0.Add(5*time.Second), 30*time.Second, 2*time.Minute, linked) {
		t.Fatal("a normal tick is not a wake")
	}
	wake := t0.Add(15 * time.Minute)
	if !g.observe(wake, 30*time.Second, 2*time.Minute, linked) {
		t.Fatal("a 15-minute jump is a wake")
	}
	if !g.active(wake.Add(time.Minute)) || g.active(wake.Add(3*time.Minute)) {
		t.Fatal("grace lasts two minutes")
	}
	if !g.machineInGrace("a", wake) || !g.machineInGrace("b", wake) || g.machineInGrace("c", wake) {
		t.Fatal("only machines linked at the wake wait")
	}
	g.attach("a")
	if g.machineInGrace("a", wake) || !g.machineInGrace("b", wake) {
		t.Fatal("re-attaching ends a machine's grace")
	}
	if g.machineInGrace("b", wake.Add(2*time.Minute+time.Second)) {
		t.Fatal("grace ends after two minutes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if g.wait(ctx, "b") {
		t.Fatal("wait outside a grace returns at once") // wall clock is far past the fake wake
	}
}

// simulateWake makes the hub believe it just woke from a sleep of d.
func simulateWake(t *testing.T, h *Hub, d time.Duration) {
	t.Helper()
	h.sleepWake.mu.Lock()
	h.sleepWake.last = time.Now().Add(-d).Round(0)
	h.sleepWake.mu.Unlock()
	if !h.checkWake(time.Now()) {
		t.Fatal("wake not detected")
	}
}

// After a hub wake, no task or machine is treated as lost during the grace (the links
// have not had a chance to re-attach), but real-time deadlines still fire; once the
// grace is over, a machine that did not come back is handled as before.
func TestWakeGraceDefersLostActionsButNotDeadlines(t *testing.T) {
	th := newTestHub(t)
	h := th.h
	h.ReapInterval = 30 * time.Millisecond
	h.StaleWorkerTimeout = 150 * time.Millisecond
	h.LostTaskGrace = 50 * time.Millisecond
	h.WakeGrace = 1500 * time.Millisecond
	ctx := context.Background()

	const wid = "w-wake"
	th.connectWorker(t, testWorkerInfo(wid, "box"), nil, wid).close()
	waitFor(t, 3*time.Second, "disconnect", func() bool { return !h.IsWorkerConnected(wid) })
	seedRunningTask(t, th, "t-wake", wid, protocol.OrphanPolicyFail)
	past := time.Now().Add(-time.Minute).UTC()
	ov, err := th.st.CreateOverride(ctx, store.Override{ScopeKind: "worker", WorkerID: wid, DurationSeconds: 60, Reason: "r", Status: store.OverridePending})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := th.st.DecideOverride(ctx, ov.ID, store.OverrideApproved, "tester", "", past, &past); err != nil || !ok {
		t.Fatalf("approve override: %v %v", ok, err)
	}

	// The machine is linked (it did not report the task: a lost-task check is pending)
	// when the hub goes to sleep, and it wakes up with every link stale.
	fw := th.connectWorker(t, testWorkerInfo(wid, "box"), nil, wid)
	simulateWake(t, h, 10*time.Minute)
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go h.RunReaper(rctx)

	time.Sleep(600 * time.Millisecond)
	if got := mustTask(t, th.st, "t-wake"); got.Status != protocol.TaskStatusRunning {
		t.Fatalf("task treated as lost during the wake grace: %s", got.Status)
	}
	if w, _ := th.st.GetWorker(ctx, wid); w.Status != "online" {
		t.Fatalf("machine marked %s during the wake grace", w.Status)
	}
	if o := mustOverride(t, th.st, ov.ID); o.Status != store.OverrideExpired {
		t.Fatalf("override expiry (a real deadline) did not fire during the grace: %s", o.Status)
	}
	res, err := th.svc.FleetStatus(ctx)
	if err != nil || len(res.Workers) != 1 || !res.Workers[0].ReconnectingAfterSleep {
		t.Fatalf("fleet does not show reconnecting after sleep: %+v %v", res.Workers, err)
	}

	// The machine never comes back: after the grace the usual handling applies.
	waitFor(t, 5*time.Second, "lost task after the grace", func() bool {
		return mustTask(t, th.st, "t-wake").Status == protocol.TaskStatusFailed
	})
	res, _ = th.svc.FleetStatus(ctx)
	if res.Workers[0].ReconnectingAfterSleep {
		t.Fatal("still reconnecting after the grace")
	}
	_ = fw
}

// A machine that re-attaches ends its own wake grace at once.
func TestWakeGraceEndsWhenMachineReattaches(t *testing.T) {
	th := newTestHub(t)
	h := th.h
	h.WakeGrace = time.Minute
	const wid = "w-back"
	fw := th.connectWorker(t, testWorkerInfo(wid, "box"), nil, wid)
	simulateWake(t, h, 10*time.Minute)
	if !h.sleepWake.machineInGrace(wid, time.Now()) {
		t.Fatal("linked machine not in grace after the wake")
	}
	fw.close()
	waitFor(t, 3*time.Second, "disconnect", func() bool { return !h.IsWorkerConnected(wid) })
	th.connectWorker(t, testWorkerInfo(wid, "box"), nil, wid)
	if h.sleepWake.machineInGrace(wid, time.Now()) {
		t.Fatal("re-attached machine still in grace")
	}
	if !h.sleepWake.active(time.Now()) {
		t.Fatal("the hub-wide grace (stale reaping) still runs for machines not back yet")
	}
}
