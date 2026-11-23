package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

func createQueuedTask(t *testing.T, st store.Store, id string, sel WorkerSelector, pinned bool) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateTask(ctx, store.Task{
		ID: id, Agent: "mock", Workdir: "/repo", Prompt: "do it",
		Status: protocol.TaskStatusQueued, OrphanPolicy: protocol.OrphanPolicyRequeue,
	}); err != nil {
		t.Fatal(err)
	}
	selJSON, _ := json.Marshal(sel)
	payJSON, _ := json.Marshal(protocol.TaskStartPayload{
		TaskID: id, Agent: "mock", Workdir: "/repo", Prompt: "do it",
		Mode: protocol.TaskModeOneshot, TimeoutSeconds: 60,
	})
	if err := st.SetTaskStartSpec(ctx, id, store.TaskStartSpec{
		SelectorJSON: string(selJSON), PayloadJSON: string(payJSON), WorkerPinned: pinned,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulePendingConcurrentRunsNeverDoubleDispatch(t *testing.T) {
	th := newTestHub(t)
	info := testWorkerInfo("w-big", "big")
	info.MaxConcurrent = 100
	fw := th.connectWorker(t, info, nil, "w-big")

	const n = 25
	for i := 0; i < n; i++ {
		createQueuedTask(t, th.st, fmt.Sprintf("task-%02d", i), WorkerSelector{}, false)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			th.h.SchedulePending(context.Background())
		}()
		go func() {
			defer wg.Done()
			th.h.kickScheduler()
		}()
	}
	wg.Wait()

	starts := startedTaskIDs(t, fw.drain(protocol.MsgTaskStart, 750*time.Millisecond))
	seen := map[string]int{}
	for _, id := range starts {
		seen[id]++
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("task-%02d", i)
		if seen[id] != 1 {
			t.Errorf("task %s dispatched %d times, want exactly 1", id, seen[id])
		}
		if got := mustTask(t, th.st, id); got.Status != protocol.TaskStatusAssigned || got.WorkerID != "w-big" {
			t.Errorf("task %s: status=%s worker=%s, want assigned on w-big", id, got.Status, got.WorkerID)
		}
	}
}

func TestQueuedTaskKeepsSelectorConstraints(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()

	linux := th.connectWorker(t, testWorkerInfo("w-linux", "linux-box"), nil, "w-linux")

	res, err := th.svc.DispatchTask(ctx, DispatchRequest{
		WorkerSelector: WorkerSelector{OS: "darwin", RequiredTools: map[string]string{"postgres": "18"}},
		Agent:          "mock",
		RepoPath:       "/repo",
		Prompt:         "needs postgres on a mac",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.TaskStatusQueued {
		t.Fatalf("expected queued (no matching worker), got %s", res.Status)
	}

	th.h.SchedulePending(ctx)
	if got := linux.drain(protocol.MsgTaskStart, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("linux worker must not receive a darwin/postgres task, got %d task.start", len(got))
	}

	macInfo := testWorkerInfo("w-mac", "mac-box")
	macInfo.OS = "darwin"
	macInfo.Facts = &protocol.MachineFacts{Tools: map[string]string{"postgres": "18.1"}}
	mac := th.connectWorker(t, macInfo, nil, "w-mac")

	env := mac.expect(protocol.MsgTaskStart, 3*time.Second)
	p, _ := protocol.DecodePayload[protocol.TaskStartPayload](env)
	if p.TaskID != res.TaskID {
		t.Fatalf("mac got task %s, want %s", p.TaskID, res.TaskID)
	}
	if got := mustTask(t, th.st, res.TaskID); got.WorkerID != "w-mac" || got.Status != protocol.TaskStatusAssigned {
		t.Fatalf("task not assigned to w-mac: %+v", got)
	}
}

func TestQueuedPayloadSurvivesRestart(t *testing.T) {
	th1 := newTestHub(t)
	ctx := context.Background()
	noWorktree := false
	timeout := 123
	res, err := th1.svc.DispatchTask(ctx, DispatchRequest{
		Agent:          "mock",
		Model:          "mock-large",
		SessionKind:    "chat",
		RepoPath:       "/repo",
		Branch:         "feat/x",
		UseWorktree:    &noWorktree,
		Prompt:         "persist me",
		Mode:           protocol.TaskModeOneshot,
		TimeoutSeconds: &timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.TaskStatusQueued {
		t.Fatalf("expected queued, got %s", res.Status)
	}
	th1.stop()

	// A fresh hub on the same database has no in-memory state.
	th2 := newTestHubAt(t, th1.dbPath)
	fw := th2.connectWorker(t, testWorkerInfo("w-1", "box"), nil, "w-1")
	env := fw.expect(protocol.MsgTaskStart, 3*time.Second)
	p, _ := protocol.DecodePayload[protocol.TaskStartPayload](env)
	if p.TaskID != res.TaskID || p.Model != "mock-large" || p.SessionKind != "chat" ||
		p.UseWorktree || p.TimeoutSeconds != 123 || p.Branch != "feat/x" || p.Prompt != "persist me" {
		t.Fatalf("payload not preserved across restart: %+v", p)
	}
}

// installStuckWorker registers a worker whose send queue is always full, so
// every task.start enqueue fails.
func installStuckWorker(t *testing.T, th *testHub, id string) {
	t.Helper()
	if err := th.st.UpsertWorker(context.Background(), store.Worker{
		ID: id, Name: id, HostGroup: id, OS: "linux", Arch: "amd64", Agents: []string{"mock"},
		MaxConcurrent: 4, Status: "online", LastSeenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	th.h.mu.Lock()
	th.h.workers[id] = &workerConn{id: id, sendCh: make(chan wsFrame), cancel: func() {}, activeTasks: map[string]bool{}}
	th.h.mu.Unlock()
}

func TestFailedDispatchRevertsToQueued(t *testing.T) {
	th := newTestHub(t)
	installStuckWorker(t, th, "w-stuck")

	createQueuedTask(t, th.st, "t-free", WorkerSelector{}, false)
	createQueuedTask(t, th.st, "t-pinned", WorkerSelector{WorkerID: "w-stuck"}, true)
	th.h.SchedulePending(context.Background())

	free := mustTask(t, th.st, "t-free")
	if free.Status != protocol.TaskStatusQueued || free.WorkerID != "" {
		t.Fatalf("unpinned task should be queued with no worker after failed send, got status=%s worker=%q", free.Status, free.WorkerID)
	}
	pinned := mustTask(t, th.st, "t-pinned")
	if pinned.Status != protocol.TaskStatusQueued || pinned.WorkerID != "w-stuck" {
		t.Fatalf("pinned task should be queued and keep its worker, got status=%s worker=%q", pinned.Status, pinned.WorkerID)
	}
	if n := len(th.h.ActiveTaskCounts()); n != 1 || th.h.ActiveTaskCounts()["w-stuck"] != 0 {
		t.Fatalf("failed sends must not count as active: %v", th.h.ActiveTaskCounts())
	}
	th.h.mu.RLock()
	defer th.h.mu.RUnlock()
	if len(th.h.taskWorker) != 0 {
		t.Fatalf("failed sends must not leave taskWorker entries: %v", th.h.taskWorker)
	}
}

func TestDirectDispatchSendFailureQueuesTask(t *testing.T) {
	th := newTestHub(t)
	installStuckWorker(t, th, "w-stuck")
	res, err := th.svc.DispatchTask(context.Background(), DispatchRequest{Agent: "mock", RepoPath: "/repo", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.TaskStatusQueued || res.WorkerID != "" {
		t.Fatalf("expected queued with no worker, got %+v", res)
	}
	if got := mustTask(t, th.st, res.TaskID); got.Status != protocol.TaskStatusQueued || got.WorkerID != "" {
		t.Fatalf("store: expected queued with no worker, got status=%s worker=%q", got.Status, got.WorkerID)
	}
}

func TestRequeuedTaskMovesToDifferentWorker(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()

	a := th.connectWorker(t, testWorkerInfo("w-a", "box-a"), nil, "w-a")
	res, err := th.svc.DispatchTask(ctx, DispatchRequest{
		Agent: "mock", RepoPath: "/repo", Prompt: "survive", OrphanPolicy: protocol.OrphanPolicyRequeue,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.WorkerID != "w-a" {
		t.Fatalf("expected initial placement on w-a, got %q", res.WorkerID)
	}
	a.expect(protocol.MsgTaskStart, 3*time.Second)

	b := th.connectWorker(t, testWorkerInfo("w-b", "box-b"), nil, "w-b")
	a.close()
	waitFor(t, 3*time.Second, "w-a to disconnect", func() bool { return !th.h.IsWorkerConnected("w-a") })
	th.h.handleOrphanTasks(ctx, "w-a")

	env := b.expect(protocol.MsgTaskStart, 3*time.Second)
	p, _ := protocol.DecodePayload[protocol.TaskStartPayload](env)
	if p.TaskID != res.TaskID || p.Prompt != "survive" {
		t.Fatalf("w-b got unexpected task.start %+v", p)
	}
	if got := mustTask(t, th.st, res.TaskID); got.WorkerID != "w-b" || got.Status != protocol.TaskStatusAssigned {
		t.Fatalf("requeued task should be assigned to w-b, got status=%s worker=%q", got.Status, got.WorkerID)
	}
}

func TestPinnedRequeueWaitsForItsWorker(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	a := th.connectWorker(t, testWorkerInfo("w-a", "box-a"), nil, "w-a")
	res, err := th.svc.DispatchTask(ctx, DispatchRequest{
		WorkerSelector: WorkerSelector{WorkerID: "w-a"},
		Agent:          "mock", RepoPath: "/repo", Prompt: "only on a", OrphanPolicy: protocol.OrphanPolicyRequeue,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.expect(protocol.MsgTaskStart, 3*time.Second)
	b := th.connectWorker(t, testWorkerInfo("w-b", "box-b"), nil, "w-b")
	a.close()
	waitFor(t, 3*time.Second, "w-a to disconnect", func() bool { return !th.h.IsWorkerConnected("w-a") })
	th.h.handleOrphanTasks(ctx, "w-a")
	th.h.SchedulePending(ctx)
	if got := b.drain(protocol.MsgTaskStart, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("pinned task must not move to w-b")
	}
	if got := mustTask(t, th.st, res.TaskID); got.Status != protocol.TaskStatusQueued || got.WorkerID != "w-a" {
		t.Fatalf("pinned task should be queued on w-a, got status=%s worker=%q", got.Status, got.WorkerID)
	}
}
