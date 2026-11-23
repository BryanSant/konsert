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

func TestSameHostnameMachinesCoexistByMachineID(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()

	// A legacy row from before machine IDs existed.
	legacy := th.connectWorker(t, testWorkerInfo("w-ubuntu", "ubuntu"), nil, "w-ubuntu")
	legacy.close()
	waitFor(t, 3*time.Second, "legacy worker to disconnect", func() bool { return !th.h.IsWorkerConnected("w-ubuntu") })

	// Two cloned VMs: same hostname, same deterministic hello ID, distinct machine IDs.
	infoA := testWorkerInfo("w-ubuntu", "ubuntu")
	infoA.MachineID = "11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	infoB := infoA
	infoB.MachineID = "22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

	a := th.connectWorker(t, infoA, nil, "w-ubuntu") // adopts the legacy row
	b := th.dialWorker(t, infoB, nil)
	// B's proposed ID is taken by A, so the hub mints B its own machine ID.
	var bID string
	waitFor(t, 5*time.Second, "clone B to get its own id", func() bool {
		mids, _ := th.st.ListWorkerMachineIDs(ctx)
		for id, mid := range mids {
			if mid == infoB.MachineID {
				bID = id
			}
		}
		return bID != "" && th.h.IsWorkerConnected(bID)
	})
	if !ids.Valid(ids.Machine, bID) {
		t.Fatalf("clone got id %q, want a minted m- id", bID)
	}
	if !th.h.IsWorkerConnected("w-ubuntu") {
		t.Fatal("both same-hostname machines should stay connected")
	}
	select {
	case <-a.done:
		t.Fatal("first machine was kicked off by its clone")
	case <-time.After(300 * time.Millisecond):
	}
	workers, err := th.st.ListWorkers(ctx)
	if err != nil || len(workers) != 2 {
		t.Fatalf("expected 2 worker rows, got %d (err=%v)", len(workers), err)
	}

	// Reconnects keep each machine on its own row, whatever hello ID it sends.
	b.close()
	waitFor(t, 3*time.Second, "B to disconnect", func() bool { return !th.h.IsWorkerConnected(bID) })
	infoB.ID = "m-zz9yx"
	th.connectWorker(t, infoB, nil, bID)
	if workers, _ := th.st.ListWorkers(ctx); len(workers) != 2 {
		t.Fatalf("reconnect created a duplicate row: %d workers", len(workers))
	}
	mids, _ := th.st.ListWorkerMachineIDs(ctx)
	if mids["w-ubuntu"] != infoA.MachineID || mids[bID] != infoB.MachineID {
		t.Fatalf("unexpected machine IDs: %v", mids)
	}
}

func TestDuplicateLiveConnectionIsTurnedAwayReconnectably(t *testing.T) {
	th := newTestHub(t)
	info := testWorkerInfo("w-dup", "dup")
	info.MachineID = "33333333-cccc-4ccc-8ccc-cccccccccccc"
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
	info.MachineID = "44444444-dddd-4ddd-8ddd-dddddddddddd"

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
