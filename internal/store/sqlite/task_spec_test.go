package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

func TestTaskStartSpecClaimAndRequeue(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "spec.db")
	st, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"w1", "w2"} {
		if err := st.UpsertWorker(ctx, store.Worker{ID: id, Name: id, HostGroup: id, Status: "online", LastSeenAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateTask(ctx, store.Task{ID: "t1", Agent: "mock", Workdir: "/r", Status: protocol.TaskStatusQueued}); err != nil {
		t.Fatal(err)
	}

	// Legacy rows (no spec yet) read back empty.
	if spec, err := st.GetTaskStartSpec(ctx, "t1"); err != nil || spec != (store.TaskStartSpec{}) {
		t.Fatalf("expected empty spec, got %+v err=%v", spec, err)
	}
	want := store.TaskStartSpec{SelectorJSON: `{"os":"darwin"}`, PayloadJSON: `{"task_id":"t1","model":"m"}`, WorkerPinned: true}
	if err := st.SetTaskStartSpec(ctx, "t1", want); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTaskStartSpec(ctx, "missing", want); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	_ = st.Close()

	// The spec survives a restart.
	st, err = Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got, err := st.GetTaskStartSpec(ctx, "t1"); err != nil || got != want {
		t.Fatalf("spec after reopen = %+v err=%v, want %+v", got, err, want)
	}

	// Exactly one of many concurrent claims wins.
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wid := "w1"
			if i%2 == 1 {
				wid = "w2"
			}
			if ok, err := st.ClaimQueuedTask(ctx, "t1", wid, store.Cause{Actor: store.ActorScheduler}); err == nil && ok {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected exactly one successful claim, got %d", wins.Load())
	}
	task, _ := st.GetTask(ctx, "t1")
	if task.Status != protocol.TaskStatusAssigned || task.WorkerID == "" {
		t.Fatalf("claimed task should be assigned with a worker: %+v", task)
	}

	// Requeue keeping the worker, then clearing it.
	if ok, err := st.RequeueTask(ctx, "t1", "", true, store.Cause{}); err != nil || !ok {
		t.Fatalf("requeue keep: ok=%v err=%v", ok, err)
	}
	if task, _ = st.GetTask(ctx, "t1"); task.Status != protocol.TaskStatusQueued || task.WorkerID == "" {
		t.Fatalf("expected queued with worker kept: %+v", task)
	}
	if ok, _ := st.ClaimQueuedTask(ctx, "t1", "w1", store.Cause{Actor: store.ActorScheduler}); !ok {
		t.Fatal("re-claim after requeue failed")
	}
	if ok, err := st.RequeueTask(ctx, "t1", "requeued", false, store.Cause{}); err != nil || !ok {
		t.Fatalf("requeue clear: ok=%v err=%v", ok, err)
	}
	if task, _ = st.GetTask(ctx, "t1"); task.Status != protocol.TaskStatusQueued || task.WorkerID != "" || task.Summary != "requeued" {
		t.Fatalf("expected queued with worker cleared: %+v", task)
	}

	// Terminal tasks are never requeued.
	_, _ = st.UpdateTaskStatus(ctx, "t1", protocol.TaskStatusCompleted, "", "", nil, store.Cause{})
	if ok, _ := st.RequeueTask(ctx, "t1", "", false, store.Cause{}); ok {
		t.Fatal("terminal task must not be requeued")
	}
}
