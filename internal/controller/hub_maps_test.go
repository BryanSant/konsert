package controller

import (
	"context"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

func (th *testHub) taskWorkerLen() int {
	th.h.mu.RLock()
	defer th.h.mu.RUnlock()
	return len(th.h.taskWorker)
}

func TestHubTaskMapsShrinkAfterTasksFinish(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	fw := th.connectWorker(t, testWorkerInfo("w-1", "box"), nil, "w-1")

	var ids []string
	for i := 0; i < 3; i++ {
		res, err := th.svc.DispatchTask(ctx, DispatchRequest{Agent: "mock", RepoPath: "/repo", Prompt: "p"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.TaskID)
		fw.expect(protocol.MsgTaskStart, 3*time.Second)
	}
	if n := th.taskWorkerLen(); n != 3 {
		t.Fatalf("taskWorker len = %d, want 3", n)
	}

	// Terminal statuses prune their entries.
	code := 0
	fw.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: ids[0], Status: protocol.TaskStatusCompleted, ExitCode: &code})
	fw.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: ids[1], Status: protocol.TaskStatusFailed, ExitCode: &code})
	waitFor(t, 3*time.Second, "terminal statuses to prune taskWorker", func() bool { return th.taskWorkerLen() == 1 })

	// Cancelling a task whose worker is unreachable prunes it too.
	fw.close()
	waitFor(t, 3*time.Second, "worker to disconnect", func() bool { return !th.h.IsWorkerConnected("w-1") })
	if _, err := th.svc.CancelTask(ctx, ids[2]); err != nil {
		t.Fatal(err)
	}
	if n := th.taskWorkerLen(); n != 0 {
		t.Fatalf("taskWorker len after cancel = %d, want 0", n)
	}
}

func TestHubTaskMapsPrunedOnOrphanAndQueuedCancel(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	fw := th.connectWorker(t, testWorkerInfo("w-1", "box"), nil, "w-1")
	res, err := th.svc.DispatchTask(ctx, DispatchRequest{Agent: "mock", RepoPath: "/repo", Prompt: "p", OrphanPolicy: protocol.OrphanPolicyRequeue})
	if err != nil {
		t.Fatal(err)
	}
	fw.expect(protocol.MsgTaskStart, 3*time.Second)
	fw.close()
	waitFor(t, 3*time.Second, "worker to disconnect", func() bool { return !th.h.IsWorkerConnected("w-1") })
	th.h.handleOrphanTasks(ctx, "w-1")
	if n := th.taskWorkerLen(); n != 0 {
		t.Fatalf("taskWorker len after orphan requeue = %d, want 0", n)
	}
	if got := mustTask(t, th.st, res.TaskID); got.Status != protocol.TaskStatusQueued {
		t.Fatalf("expected requeued task, got %s", got.Status)
	}

	// Cancelling the now-queued task leaves no waiters or routing behind.
	wait, cancelWait := th.h.WaitForTask(res.TaskID)
	defer cancelWait()
	if _, err := th.svc.CancelTask(ctx, res.TaskID); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-wait:
		if got.Status != protocol.TaskStatusCanceled {
			t.Fatalf("waiter got status %s", got.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not notified")
	}
	th.h.mu.RLock()
	defer th.h.mu.RUnlock()
	if len(th.h.taskWorker) != 0 || len(th.h.waiters) != 0 {
		t.Fatalf("maps not pruned: taskWorker=%v waiters=%d", th.h.taskWorker, len(th.h.waiters))
	}
}
