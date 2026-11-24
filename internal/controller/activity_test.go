package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

// nextActivity returns the next fleet.update activity frame for key, skipping others.
func nextActivity(t *testing.T, b *fakeWorker, key string, timeout time.Duration) protocol.SessionActivity {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		env := b.expect(protocol.MsgFleetUpdate, time.Until(deadline))
		p, err := protocol.DecodePayload[struct {
			Kind     string                   `json:"kind"`
			Activity protocol.SessionActivity `json:"activity"`
		}](env)
		if err == nil && p.Kind == "activity" && p.Activity.Key() == key {
			return p.Activity
		}
	}
	t.Fatalf("no activity for %s", key)
	return protocol.SessionActivity{}
}

func TestSessionActivityFlowsToBrowsersFromItsHostOnly(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	w1 := th.connectWorker(t, testWorkerInfo("w-1", "one"), nil, "w-1")
	w2 := th.connectWorker(t, testWorkerInfo("w-2", "two"), nil, "w-2")
	browser := th.dialBrowser(t)

	res, err := th.svc.DispatchTask(ctx, DispatchRequest{WorkerSelector: WorkerSelector{WorkerID: "w-1"}, Agent: "mock", RepoPath: "/repo", Prompt: "do KON-2Q0CH"})
	if err != nil {
		t.Fatal(err)
	}
	key := "task:" + res.TaskID
	// The hub reports what only it knows: the task is starting on its machine.
	if a := nextActivity(t, browser, key, 3*time.Second); a.State != protocol.ActivityQueued {
		t.Fatalf("dispatched task activity %+v, want queued", a)
	}

	// Another worker cannot report activity for a task it doesn't host.
	w2.send(protocol.MsgSessionActivity, protocol.SessionActivity{TaskID: res.TaskID, State: protocol.ActivitySpeaking, At: time.Now()})
	w1.send(protocol.MsgSessionActivity, protocol.SessionActivity{TaskID: res.TaskID, State: protocol.ActivityWorking, Tool: "Bash", Label: "Run: cd /repo && go test\n./...", At: time.Now()})
	a := nextActivity(t, browser, key, 3*time.Second)
	// The hub adds the readable now line and keeps the raw label.
	if a.State != protocol.ActivityWorking || a.Tool != "Bash" || a.WorkerID != "w-1" || strings.Contains(a.Label, "\n") ||
		a.Summary != "running tests" || !strings.HasPrefix(a.Label, "Run: cd /repo") {
		t.Fatalf("activity %+v, want working Bash from w-1 on one line", a)
	}
	if got, _ := th.h.sessionActivity(key); got.State != protocol.ActivityWorking {
		t.Fatalf("hub keeps %+v", got)
	}

	// The worker's running status doesn't override its better report.
	if _, err := th.h.setTaskStatus(ctx, res.TaskID, protocol.TaskStatusRunning, "", "", nil, store.Cause{Actor: store.ActorAgent}); err != nil {
		t.Fatal(err)
	}
	if got, _ := th.h.sessionActivity(key); got.State != protocol.ActivityWorking {
		t.Fatalf("running status replaced the worker's report: %+v", got)
	}
	// A final status is final, and a late report doesn't revive it.
	code := 0
	if _, err := th.h.setTaskStatus(ctx, res.TaskID, protocol.TaskStatusCompleted, "all good", "", &code, store.Cause{Actor: store.ActorAgent}); err != nil {
		t.Fatal(err)
	}
	if a := nextActivity(t, browser, key, 3*time.Second); a.State != protocol.ActivityDone {
		t.Fatalf("completed task activity %+v", a)
	}

	// The snapshot carries the current set.
	found := false
	for _, x := range th.h.activityList() {
		if x.Key() == key && x.State == protocol.ActivityDone {
			found = true
		}
	}
	if !found {
		t.Fatalf("activityList misses %s: %+v", key, th.h.activityList())
	}
}

func TestTaskStatusActivityQueuedBlockedFailed(t *testing.T) {
	h := &Hub{}
	h.taskStatusActivity(store.Task{ID: "t1", Status: protocol.TaskStatusQueued})
	if a, _ := h.sessionActivity("task:t1"); a.State != protocol.ActivityQueued || a.Coarse {
		t.Fatalf("queued: %+v", a)
	}
	h.taskStatusActivity(store.Task{ID: "t1", Status: protocol.TaskStatusSuspended, SuspendedBy: "pause"})
	if a, _ := h.sessionActivity("task:t1"); a.State != protocol.ActivityBlocked || !strings.Contains(a.Label, "Pause") {
		t.Fatalf("paused: %+v", a)
	}
	h.taskStatusActivity(store.Task{ID: "t1", Status: protocol.TaskStatusRunning})
	if a, _ := h.sessionActivity("task:t1"); a.State != protocol.ActivityThinking || !a.Coarse {
		t.Fatalf("resumed: %+v", a)
	}
	h.taskStatusActivity(store.Task{ID: "t1", Status: protocol.TaskStatusFailed, Summary: "exit 1\nmore"})
	if a, _ := h.sessionActivity("task:t1"); a.State != protocol.ActivityError || a.Label != "exit 1" {
		t.Fatalf("failed: %+v", a)
	}
	// Observed sessions are left to handleExternalSession.
	h.taskStatusActivity(store.Task{ID: "t2", Origin: protocol.TaskOriginExternal, Status: protocol.TaskStatusRunning})
	if _, ok := h.sessionActivity("task:t2"); ok {
		t.Fatal("external task got a status activity")
	}
}

func TestExternalActivityIsCoarse(t *testing.T) {
	cases := []struct {
		p     protocol.ExternalSessionPayload
		state string
		tool  string
	}{
		{protocol.ExternalSessionPayload{Status: "working", Source: "hook", Event: "PreToolUse", Tool: "Bash", Summary: "Bash: go test"}, protocol.ActivityWorking, "Bash"},
		{protocol.ExternalSessionPayload{Status: "working", Source: "hook", Event: "PostToolUse"}, protocol.ActivityThinking, ""},
		{protocol.ExternalSessionPayload{Status: "working", Source: "passive"}, protocol.ActivityWorking, ""},
		{protocol.ExternalSessionPayload{Status: "needs_you", Source: "hook", Summary: "Claude needs your permission"}, protocol.ActivityAsking, ""},
		{protocol.ExternalSessionPayload{Status: "idle", Source: "hook"}, protocol.ActivityDone, ""},
		{protocol.ExternalSessionPayload{Status: "fail", Source: "hook"}, protocol.ActivityError, ""},
	}
	for _, c := range cases {
		a, ok := externalActivity("t1", "w1", c.p)
		if !ok || a.State != c.state || a.Tool != c.tool || !a.Coarse {
			t.Errorf("%+v -> %+v (ok=%v), want %s/%s coarse", c.p, a, ok, c.state, c.tool)
		}
		if c.p.Source == "hook" && a.Source != protocol.ActivitySourceHook {
			t.Errorf("source %s, want hook", a.Source)
		}
	}
}

func TestHeartbeatReportsHeatToBrowsers(t *testing.T) {
	th := newTestHub(t)
	info := testWorkerInfo("w-h", "hot")
	info.MaxConcurrent = 2
	info.Facts = &protocol.MachineFacts{CPUCores: 8}
	w := th.connectWorker(t, info, nil, "w-h")
	browser := th.dialBrowser(t)
	w.send(protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{
		ActiveTaskIDs: []string{},
		Metrics:       &protocol.WorkerMetrics{CPUPercent: 20, LoadAvg: 7, MemUsedMB: 4000, MemTotalMB: 16000},
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		env := browser.expect(protocol.MsgFleetUpdate, time.Until(deadline))
		p, _ := protocol.DecodePayload[struct {
			Kind   string           `json:"kind"`
			Worker WorkerStatusView `json:"worker"`
		}](env)
		if p.Kind != "worker" || p.Worker.Heat == nil {
			continue
		}
		hh := p.Worker.Heat
		// load 7 on 8 cores dominates CPU 20 %.
		if hh.Cause != HeatCauseCPU || hh.Raw != 0.875 || hh.Level == "" {
			t.Fatalf("heat %+v", hh)
		}
		fs, err := th.svc.FleetStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range fs.Workers {
			if v.ID == "w-h" && (v.Heat == nil || v.Heat.Raw != 0.875) {
				t.Fatalf("fleet status heat %+v", v.Heat)
			}
		}
		return
	}
	t.Fatal("no worker update with heat")
}

func TestActivitySummaryShortensPathsAgainstTheCheckout(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	w := th.connectWorker(t, testWorkerInfo("w-1", "one"), nil, "w-1")
	browser := th.dialBrowser(t)
	res, err := th.svc.DispatchTask(ctx, DispatchRequest{WorkerSelector: WorkerSelector{WorkerID: "w-1"}, Agent: "mock", RepoPath: "/home/dev/wc", Prompt: "count words"})
	if err != nil {
		t.Fatal(err)
	}
	key := "task:" + res.TaskID
	nextActivity(t, browser, key, 3*time.Second) // queued
	w.send(protocol.MsgSessionActivity, protocol.SessionActivity{TaskID: res.TaskID, State: protocol.ActivityWorking, Tool: "Edit", Label: "Edit: /home/dev/wc/wc.py", At: time.Now()})
	if a := nextActivity(t, browser, key, 3*time.Second); a.Summary != "editing wc.py" || a.Label != "Edit: /home/dev/wc/wc.py" {
		t.Fatalf("activity %+v, want summary 'editing wc.py' and the raw label", a)
	}
	// Not working: no summary.
	w.send(protocol.MsgSessionActivity, protocol.SessionActivity{TaskID: res.TaskID, State: protocol.ActivityThinking, At: time.Now()})
	if a := nextActivity(t, browser, key, 3*time.Second); a.Summary != "" {
		t.Fatalf("thinking activity has summary %q", a.Summary)
	}
}
