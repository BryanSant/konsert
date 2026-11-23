package controller

import (
	"context"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/coder/websocket"
)

// The hub keys a worker by the UUID it sends in hello, mints its m- sid the first time it
// sees it, and keeps that sid across reconnects (even if the name changes).
func TestHubMintsMachineSIDOnFirstHelloAndKeepsIt(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	id := ids.UUID()

	c := th.connectWorker(t, testWorkerInfo(id, "box"), nil, id)
	w, err := th.st.GetWorker(ctx, id)
	if err != nil || !ids.Valid(ids.Machine, w.SID) {
		t.Fatalf("worker after hello = %+v, %v; want an m- sid", w, err)
	}
	c.close()
	waitFor(t, 3*time.Second, "worker to disconnect", func() bool { return !th.h.IsWorkerConnected(id) })

	info := testWorkerInfo(id, "box-renamed")
	th.connectWorker(t, info, nil, id)
	again, err := th.st.GetWorker(ctx, id)
	if err != nil || again.SID != w.SID || again.Name != "box-renamed" {
		t.Fatalf("after reconnect = %+v, %v; want sid %s kept", again, err, w.SID)
	}
	if workers, _ := th.st.ListWorkers(ctx); len(workers) != 1 {
		t.Fatalf("reconnect created a duplicate row: %d workers", len(workers))
	}

	// A different machine with the same hostname is a separate worker with its own sid.
	other := ids.UUID()
	th.connectWorker(t, testWorkerInfo(other, "box-renamed"), nil, other)
	ow, err := th.st.GetWorker(ctx, other)
	if err != nil || ow.SID == w.SID || !ids.Valid(ids.Machine, ow.SID) {
		t.Fatalf("second machine = %+v, %v", ow, err)
	}
}

func TestDuplicateLiveConnectionIsTurnedAwayReconnectably(t *testing.T) {
	th := newTestHub(t)
	info := testWorkerInfo("w-dup", "dup")
	first := th.connectWorker(t, info, nil, "w-dup")

	second := th.dialWorker(t, info, nil)
	code := second.waitClosed(5 * time.Second)
	if code != websocket.StatusTryAgainLater {
		t.Fatalf("duplicate got close code %d, want %d (reconnectable)", code, websocket.StatusTryAgainLater)
	}
	if code == websocket.StatusCode(protocol.WSCloseWorkerRemoved) {
		t.Fatal("duplicate must never receive the removal code")
	}
	select {
	case <-first.done:
		t.Fatal("live connection must keep its place")
	default:
	}
}

func TestDeadConnectionIsSupersededWithReconnectableCode(t *testing.T) {
	th := newTestHub(t)
	info := testWorkerInfo("w-stale", "stale")

	// The old connection never reads, so it cannot answer the liveness ping.
	old := th.dialWorkerOpts(t, info, nil, true)
	waitFor(t, 3*time.Second, "old connection to register", func() bool { return th.h.IsWorkerConnected("w-stale") })
	th.h.mu.RLock()
	oldConn := th.h.workers["w-stale"]
	th.h.mu.RUnlock()

	start := time.Now()
	th.dialWorker(t, info, nil)
	waitFor(t, 2*supersedePingTimeout+2*time.Second, "new connection to replace the dead one", func() bool {
		th.h.mu.RLock()
		defer th.h.mu.RUnlock()
		cur := th.h.workers["w-stale"]
		return cur != nil && cur != oldConn
	})
	if time.Since(start) < supersedePingTimeout/2 {
		t.Fatal("hub replaced the old connection without checking its liveness")
	}

	old.resumeReading()
	code := old.waitClosed(10 * time.Second)
	if code == websocket.StatusCode(protocol.WSCloseWorkerRemoved) || code == -1 {
		t.Fatalf("superseded connection got close code %d, want a normal reconnectable close", code)
	}
}

func TestWorkerCannotClaimOrUpdateAnotherWorkersTask(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()

	a := th.connectWorker(t, testWorkerInfo("w-a", "box-a"), nil, "w-a")
	if err := th.st.CreateTask(ctx, store.Task{ID: "t-owned", WorkerID: "w-a", Agent: "mock", Workdir: "/r", Status: protocol.TaskStatusRunning}); err != nil {
		t.Fatal(err)
	}

	// w-b claims the task in its hello and then reports a status for it.
	b := th.connectWorker(t, testWorkerInfo("w-b", "box-b"),
		[]protocol.ActiveTaskInfo{{TaskID: "t-owned", Status: protocol.TaskStatusWaitingInput}}, "w-b")
	code := 1
	b.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: "t-owned", Status: protocol.TaskStatusFailed, ExitCode: &code})

	// A later status from the real owner is applied, which also orders the check after w-b's frames.
	a.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: "t-owned", Status: protocol.TaskStatusRunning, Summary: "owner update"})
	waitFor(t, 3*time.Second, "owner status to apply", func() bool { return mustTask(t, th.st, "t-owned").Summary == "owner update" })

	// Give w-b's frames (sent before the owner's on a separate connection) time to land.
	time.Sleep(200 * time.Millisecond)
	got := mustTask(t, th.st, "t-owned")
	if got.WorkerID != "w-a" || got.Status != protocol.TaskStatusRunning {
		t.Fatalf("foreign worker changed the task: worker=%s status=%s", got.WorkerID, got.Status)
	}
	th.h.mu.RLock()
	owner := th.h.taskWorker["t-owned"]
	th.h.mu.RUnlock()
	if owner == "w-b" {
		t.Fatal("hello.active_tasks re-routed another worker's task")
	}
}
