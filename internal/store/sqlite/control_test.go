package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func openControlStore(t *testing.T) *SQLiteStore {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestFleetEventsHoldsRequestsAndDrain(t *testing.T) {
	ctx := context.Background()
	st := openControlStore(t)
	if err := st.UpsertWorker(ctx, store.Worker{ID: "w1", Name: "box", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWorkerDrained(ctx, "w1", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if w, _ := st.GetWorker(ctx, "w1"); !w.Drained || w.DrainedAt == nil {
		t.Fatalf("drain not stored: %+v", w)
	}
	// worker.hello (UpsertWorker) keeps the drain flag.
	_ = st.UpsertWorker(ctx, store.Worker{ID: "w1", Name: "box", Status: "online"})
	if w, _ := st.GetWorker(ctx, "w1"); !w.Drained {
		t.Fatal("hello cleared the drain flag")
	}

	user := store.Cause{Actor: store.ActorUser, Source: store.SourceTerminal, Confidence: store.ConfidenceHigh, CommandID: "cmd-1"}
	for i, e := range []store.FleetEvent{
		{Kind: store.EventKindStatus, Action: "task", Cause: store.Cause{Actor: store.ActorAgent, Confidence: store.ConfidenceHigh}, WorkerID: "w1", FromStatus: "running", ToStatus: "completed", Summary: "done"},
		{Kind: store.EventKindAction, Action: "command", Cause: user, WorkerID: "w1", Summary: "stopped nginx"},
		{Kind: store.EventKindStatus, Action: "task", Cause: store.Cause{Actor: store.ActorSystem}, WorkerID: "w1", ToStatus: "failed", Summary: "lost"},
	} {
		if id, err := st.AppendFleetEvent(ctx, e); err != nil || id != int64(i+1) {
			t.Fatalf("append %d: %d %v", i, id, err)
		}
	}
	all, _ := st.ListFleetEvents(ctx, store.FleetEventFilter{})
	notable, _ := st.ListFleetEvents(ctx, store.FleetEventFilter{Notable: true})
	users, _ := st.ListFleetEvents(ctx, store.FleetEventFilter{Actor: store.ActorUser})
	if len(all) != 3 || len(notable) != 2 || len(users) != 1 || users[0].Cause != user {
		t.Fatalf("events: all %d notable %d users %+v", len(all), len(notable), users)
	}
	if max, _ := st.MaxFleetEventID(ctx); max != 3 {
		t.Fatalf("max id %d", max)
	}

	hd, err := st.CreateHold(ctx, store.Hold{ScopeKind: store.HoldScopeResource, WorkerID: "w1", Resource: &store.Resource{Kind: "service", Name: "nginx"}, Summary: "stopped", Cause: user})
	if err != nil || !ids.Valid(ids.Hold, hd.SID) || !ids.IsUUIDv7(hd.ID) {
		t.Fatalf("hold: %+v %v", hd, err)
	}
	if id, err := st.ResolveID(ctx, ids.Hold, hd.SID); err != nil || id != hd.ID {
		t.Fatalf("resolve hold sid: %q %v", id, err)
	}
	active, _ := st.ListHolds(ctx, store.HoldFilter{ActiveOnly: true, WorkerID: "w1"})
	if len(active) != 1 || active[0].Resource.Name != "nginx" || active[0].Cause != user || !active[0].Active() {
		t.Fatalf("active holds: %+v", active)
	}
	if ok, _ := st.ReleaseHold(ctx, hd.ID, "tester", "go", time.Now()); !ok {
		t.Fatal("release failed")
	}
	if ok, _ := st.ReleaseHold(ctx, hd.ID, "tester", "again", time.Now()); ok {
		t.Fatal("released twice")
	}
	if active, _ := st.ListHolds(ctx, store.HoldFilter{ActiveOnly: true}); len(active) != 0 {
		t.Fatalf("still active: %+v", active)
	}

	r, err := st.CreateUserRequest(ctx, store.UserRequest{ChatID: "c1", WorkerID: "w1", Resource: &store.Resource{Name: "nginx"}, Description: "restart nginx"})
	if err != nil || r.Status != store.RequestOpen || !ids.Valid(ids.Request, r.SID) {
		t.Fatalf("request: %+v %v", r, err)
	}
	if ok, _ := st.ResolveUserRequest(ctx, r.ID, store.RequestDone, "tester", "ran it", "cmd-1", time.Now()); !ok {
		t.Fatal("resolve failed")
	}
	got, _ := st.GetUserRequest(ctx, r.ID)
	if got.Status != store.RequestDone || got.CommandID != "cmd-1" || got.ResolvedAt == nil || got.Resource == nil || got.Resource.Name != "nginx" {
		t.Fatalf("resolved: %+v", got)
	}
	if ok, _ := st.ResolveUserRequest(ctx, r.ID, store.RequestDeclined, "x", "", "", time.Now()); ok {
		t.Fatal("resolved twice")
	}
}

func TestTaskCauseSuspendAndParentChat(t *testing.T) {
	ctx := context.Background()
	st := openControlStore(t)
	task := store.Task{ID: ids.UUID(), Agent: "mock", Workdir: "/tmp", Prompt: "x", Status: protocol.TaskStatusRunning,
		Cause: &store.Cause{Actor: store.ActorCommander, Confidence: store.ConfidenceCertain}, ParentChatID: "c1"}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTaskSuspendedBy(ctx, task.ID, store.SuspendedByPause); err != nil {
		t.Fatal(err)
	}
	user := store.Cause{Actor: store.ActorUser, Source: store.SourceUI, Confidence: store.ConfidenceCertain}
	prev, err := st.UpdateTaskStatus(ctx, task.ID, protocol.TaskStatusSuspended, "", "", nil, user)
	if err != nil || prev != protocol.TaskStatusRunning {
		t.Fatalf("prev %q %v", prev, err)
	}
	got, _ := st.GetTask(ctx, task.ID)
	if got.SuspendedBy != store.SuspendedByPause || got.Cause == nil || *got.Cause != user || got.ParentChatID != "c1" {
		t.Fatalf("suspended task: %+v", got)
	}
	// Leaving "suspended" clears who suspended it.
	if _, err := st.UpdateTaskStatus(ctx, task.ID, protocol.TaskStatusRunning, "", "", nil, user); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetTask(ctx, task.ID); got.SuspendedBy != "" {
		t.Fatalf("suspended_by kept: %+v", got)
	}
	if _, err := st.UpdateTaskStatus(ctx, "missing", "running", "", "", nil, user); err != store.ErrNotFound {
		t.Fatalf("missing task: %v", err)
	}
}
