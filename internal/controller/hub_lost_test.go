package controller

import (
	"context"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func seedRunningTask(t *testing.T, th *testHub, id, workerID, orphanPolicy string) {
	t.Helper()
	if err := th.st.CreateTask(context.Background(), store.Task{
		ID: id, WorkerID: workerID, Agent: "mock", Workdir: "/r",
		Status: protocol.TaskStatusRunning, OrphanPolicy: orphanPolicy,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTasksMissingFromHelloAreReconciledAfterGrace(t *testing.T) {
	th := newTestHub(t)
	th.h.LostTaskGrace = 300 * time.Millisecond

	// The worker row must exist before tasks can reference it.
	first := th.connectWorker(t, testWorkerInfo("w-1", "box"), nil, "w-1")
	first.close()
	waitFor(t, 3*time.Second, "disconnect", func() bool { return !th.h.IsWorkerConnected("w-1") })

	seedRunningTask(t, th, "t-lost", "w-1", protocol.OrphanPolicyFail)
	seedRunningTask(t, th, "t-outbox", "w-1", protocol.OrphanPolicyFail)
	seedRunningTask(t, th, "t-requeue", "w-1", protocol.OrphanPolicyRequeue)
	seedRunningTask(t, th, "t-listed", "w-1", protocol.OrphanPolicyFail)

	fw := th.connectWorker(t, testWorkerInfo("w-1", "box"),
		[]protocol.ActiveTaskInfo{{TaskID: "t-listed", Status: protocol.TaskStatusRunning}}, "w-1")

	// Within the grace window nothing is touched yet...
	time.Sleep(100 * time.Millisecond)
	if got := mustTask(t, th.st, "t-lost"); got.Status != protocol.TaskStatusRunning {
		t.Fatalf("task reconciled before the grace period: %s", got.Status)
	}
	// ...and the worker's outbox delivers the final status of one task.
	zero := 0
	fw.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: "t-outbox", Status: protocol.TaskStatusCompleted, ExitCode: &zero})

	waitFor(t, 3*time.Second, "lost task to fail", func() bool {
		return mustTask(t, th.st, "t-lost").Status == protocol.TaskStatusFailed
	})
	if got := mustTask(t, th.st, "t-lost"); got.ExitCode == nil || *got.ExitCode != -1 {
		t.Fatalf("lost task should fail with exit -1: %+v", got)
	}
	if got := mustTask(t, th.st, "t-outbox"); got.Status != protocol.TaskStatusCompleted {
		t.Fatalf("outbox-delivered status was overwritten: %s", got.Status)
	}
	if got := mustTask(t, th.st, "t-listed"); got.Status != protocol.TaskStatusRunning {
		t.Fatalf("task listed in hello must stay running: %s", got.Status)
	}
	// The requeue-policy task returns to the queue and is re-dispatched to the
	// connected worker.
	env := fw.expect(protocol.MsgTaskStart, 3*time.Second)
	if p, _ := protocol.DecodePayload[protocol.TaskStartPayload](env); p.TaskID != "t-requeue" {
		t.Fatalf("expected t-requeue to be re-dispatched, got %s", p.TaskID)
	}
}

func TestTasksVanishingFromHeartbeatAreReconciledAfterGrace(t *testing.T) {
	th := newTestHub(t)
	th.h.LostTaskGrace = 300 * time.Millisecond

	first := th.connectWorker(t, testWorkerInfo("w-1", "box"), nil, "w-1")
	first.close()
	waitFor(t, 3*time.Second, "disconnect", func() bool { return !th.h.IsWorkerConnected("w-1") })
	seedRunningTask(t, th, "t-gone", "w-1", protocol.OrphanPolicyFail)
	seedRunningTask(t, th, "t-blip", "w-1", protocol.OrphanPolicyFail)

	fw := th.connectWorker(t, testWorkerInfo("w-1", "box"), []protocol.ActiveTaskInfo{
		{TaskID: "t-gone", Status: protocol.TaskStatusRunning},
		{TaskID: "t-blip", Status: protocol.TaskStatusRunning},
	}, "w-1")

	// Both drop off, then t-blip reappears within the grace window.
	fw.send(protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{ActiveTaskIDs: []string{}})
	time.Sleep(50 * time.Millisecond)
	fw.send(protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{ActiveTaskIDs: []string{"t-blip"}})

	waitFor(t, 3*time.Second, "vanished task to fail", func() bool {
		return mustTask(t, th.st, "t-gone").Status == protocol.TaskStatusFailed
	})
	time.Sleep(100 * time.Millisecond)
	if got := mustTask(t, th.st, "t-blip"); got.Status != protocol.TaskStatusRunning {
		t.Fatalf("task that reappeared within the grace period was reconciled: %s", got.Status)
	}
}
