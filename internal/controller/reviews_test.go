package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/coder/websocket"
)

func TestDispatchOrderPutsExpeditedFirst(t *testing.T) {
	// ListTasks order: newest first.
	tasks := []store.Task{{ID: "d"}, {ID: "c"}, {ID: "b"}, {ID: "a"}}
	ids := func(ts []store.Task) []string {
		var out []string
		for _, t := range ts {
			out = append(out, t.ID)
		}
		return out
	}
	if got := ids(dispatchOrder(tasks, nil)); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("no expedite: %v", got)
	}
	if got := ids(dispatchOrder(tasks, map[string]bool{"c": true, "d": true})); !slices.Equal(got, []string{"c", "d", "a", "b"}) {
		t.Fatalf("expedited c, d: %v", got)
	}
}

func TestExpeditedQueuedTaskIsPlacedFirst(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Minute).UTC()
	for i, id := range []string{"task-a", "task-b", "task-c"} {
		// Distinct creation times: a is the oldest.
		if err := th.st.CreateTask(ctx, store.Task{
			ID: id, Agent: "mock", Workdir: "/repo", Prompt: "do it", Status: protocol.TaskStatusQueued,
			OrphanPolicy: protocol.OrphanPolicyRequeue, CreatedAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		payJSON, _ := json.Marshal(protocol.TaskStartPayload{TaskID: id, Agent: "mock", Workdir: "/repo", Prompt: "do it", Mode: protocol.TaskModeOneshot, TimeoutSeconds: 60})
		if err := th.st.SetTaskStartSpec(ctx, id, store.TaskStartSpec{SelectorJSON: "{}", PayloadJSON: string(payJSON)}); err != nil {
			t.Fatal(err)
		}
	}
	browser := th.dialBrowser(t)
	if _, err := th.h.SetExpedite(ctx, "task-c", "", true); err != nil {
		t.Fatal(err)
	}
	// Every browser hears about it.
	env := browser.expect(protocol.MsgFleetUpdate, 3*time.Second)
	for {
		p, _ := protocol.DecodePayload[struct {
			Kind   string       `json:"kind"`
			Review store.Review `json:"review"`
		}](env)
		if p.Kind == "review" {
			if p.Review.TaskID != "task-c" || !p.Review.Expedite || p.Review.SID == "" {
				t.Fatalf("review update %+v", p.Review)
			}
			break
		}
		env = browser.expect(protocol.MsgFleetUpdate, 3*time.Second)
	}

	// One slot: the expedited task goes first even though it is the newest.
	info := testWorkerInfo("w-1", "one")
	info.MaxConcurrent = 1
	w := th.connectWorker(t, info, nil, "w-1")
	th.h.SchedulePending(ctx)
	start := w.expect(protocol.MsgTaskStart, 3*time.Second)
	p, _ := protocol.DecodePayload[protocol.TaskStartPayload](start)
	if p.TaskID != "task-c" {
		t.Fatalf("first placed %s, want the expedited task-c", p.TaskID)
	}
}

func decodeReviewResult(t *testing.T, raw string) ReviewResult {
	t.Helper()
	var res ReviewResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return res
}

// answerGitRPCs answers git.info / git.merge / git.push messages sent to w until stopped.
func answerGitRPCs(w *fakeWorker, info protocol.GitInfoResult, mergeFn func(protocol.GitMergeRequest) protocol.GitMergeResult, pushFn func(protocol.GitPushRequest) protocol.GitPushResult) func() {
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case env, ok := <-w.frames:
				if !ok {
					return
				}
				switch env.Type {
				case protocol.MsgGitInfo:
					raw, _ := protocol.NewEnvelope(protocol.MsgGitInfoResult, env.ID, info)
					_ = w.conn.Write(w.ctx, websocket.MessageText, raw)
				case protocol.MsgGitMerge:
					req, _ := protocol.DecodePayload[protocol.GitMergeRequest](env)
					res := protocol.GitMergeResult{OK: true, Merged: true, TargetBranch: "main", MergeSHA: "abc1234"}
					if mergeFn != nil {
						res = mergeFn(req)
					}
					raw, _ := protocol.NewEnvelope(protocol.MsgGitMergeResult, env.ID, res)
					_ = w.conn.Write(w.ctx, websocket.MessageText, raw)
				case protocol.MsgGitPush:
					req, _ := protocol.DecodePayload[protocol.GitPushRequest](env)
					res := protocol.GitPushResult{OK: true, Branch: req.Branch, HeadSHA: "def5678"}
					if pushFn != nil {
						res = pushFn(req)
					}
					raw, _ := protocol.NewEnvelope(protocol.MsgGitPushResult, env.ID, res)
					_ = w.conn.Write(w.ctx, websocket.MessageText, raw)
				}
			}
		}
	}()
	return func() { close(done) }
}

func TestReviewApproveDirectLocalOnlyRecordsDecisionAndTimeline(t *testing.T) {
	e := newAuditEnv(t)
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	ctx := context.Background()
	if err := e.th.st.CreateTask(ctx, store.Task{ID: "t1", WorkerID: "w1", Agent: "mock", Workdir: "/r", EffectiveDir: "/r/wt", Branch: "syre/t1",
		Prompt: "count words", Status: protocol.TaskStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	stop := answerGitRPCs(w, protocol.GitInfoResult{OK: true, IsGitRepo: true, Branch: "syre/t1", DefaultBranch: "main", HasRemote: false}, nil, nil)
	defer stop()

	code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t1/review", `{"decision":"approved"}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	res := decodeReviewResult(t, raw)
	rv := res.Review
	if rv.Decision != store.ReviewApproved || rv.DeliveryMode != store.DeliveryModeDirect ||
		rv.DeliveryStatus != store.DeliveryStatusDelivered || rv.MergeMethod != store.MergeStyleSquash ||
		rv.DecidedAt == nil || rv.DecidedBy != e.user || res.FollowUpTaskID != "" {
		t.Fatalf("review result %+v", res)
	}
	// Stored on the hub: the list and the fleet snapshot carry it.
	code, raw = e.do(t, credCookie, http.MethodGet, "/api/reviews", "")
	if code != http.StatusOK || !strings.Contains(raw, `"decision":"approved"`) {
		t.Fatalf("GET /api/reviews: %d %s", code, raw)
	}
	code, raw = e.do(t, credCookie, http.MethodGet, "/api/fleet", "")
	if code != http.StatusOK || !strings.Contains(raw, `"reviews":[{`) {
		t.Fatalf("GET /api/fleet lacks reviews: %d %s", code, raw)
	}
	// Task timeline and audit log record the decision and delivery outcome.
	evs, _ := e.th.st.ListEvents(ctx, "t1", 0, 50)
	if !slices.ContainsFunc(evs, func(ev store.TaskEvent) bool { return ev.Kind == "review" && ev.Message == "Approved" }) {
		t.Fatalf("no review event in %+v", evs)
	}
	if !slices.ContainsFunc(evs, func(ev store.TaskEvent) bool { return ev.Kind == "delivery" }) {
		t.Fatalf("no delivery event in %+v", evs)
	}
	audits, _ := e.th.st.ListAudit(ctx, store.AuditFilter{TaskID: "t1"})
	if !slices.ContainsFunc(audits, func(a store.AuditEvent) bool { return a.Action == AuditTaskDeliver }) {
		t.Fatalf("no task.deliver audit event in %+v", audits)
	}
}

func TestReviewRequestChangesOnFinishedWorkStartsAFollowUp(t *testing.T) {
	e := newAuditEnv(t)
	_ = e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	ctx := context.Background()
	if err := e.th.st.CreateTask(ctx, store.Task{ID: "t1", WorkerID: "w1", Agent: "mock", Workdir: "/r", Title: "Word count",
		Prompt: "count words", Status: protocol.TaskStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t1/review", `{"decision":"changes_requested","note":"Handle empty files"}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	res := decodeReviewResult(t, raw)
	if res.Review.Decision != store.ReviewChangesRequested || res.Review.Note != "Handle empty files" ||
		res.Delivered != "follow_up" || res.FollowUpTaskID == "" ||
		res.Review.FollowUpTaskID != res.FollowUpTaskID {
		t.Fatalf("review result %+v", res)
	}
	follow := mustTask(t, e.th.st, res.FollowUpTaskID)
	if !strings.Contains(follow.Prompt, "Handle empty files") || !strings.Contains(follow.Prompt, "count words") ||
		follow.WorkerID != "w1" || follow.Workdir != "/r" {
		t.Fatalf("follow-up task %+v", follow)
	}
}

func TestReviewRequestChangesOnARunningTaskSteersIt(t *testing.T) {
	e := newAuditEnv(t)
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	e.seedTask(t, "t1", "w1") // running on w1
	done := make(chan ReviewResult, 1)
	go func() {
		_, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t1/review", `{"decision":"changes_requested","note":"Use a map"}`)
		var res ReviewResult
		_ = json.Unmarshal([]byte(raw), &res)
		done <- res
	}()
	steer := w.expect(protocol.MsgTaskSteer, 5*time.Second)
	p, _ := protocol.DecodePayload[protocol.TaskSteerPayload](steer)
	if p.TaskID != "t1" || p.InputText != "Use a map" || !p.AppendNewline {
		t.Fatalf("steer %+v", p)
	}
	if res := <-done; res.Delivered != "steered" {
		t.Fatalf("review result %+v", res)
	}
}

func TestReviewOfflineMachineIsRecordedAsOffline(t *testing.T) {
	e := newAuditEnv(t)
	ctx := context.Background()
	if err := e.th.st.UpsertWorker(ctx, store.Worker{ID: "w9", Name: "far", HostGroup: "g", OS: "linux", Arch: "amd64", Status: "offline", LastSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := e.th.st.CreateTask(ctx, store.Task{ID: "t1", WorkerID: "w9", Agent: "mock", Workdir: "/r", Prompt: "x", Status: protocol.TaskStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t1/review", `{"decision":"approved"}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	rv := decodeReviewResult(t, raw).Review
	if rv.Decision != store.ReviewApproved || rv.DeliveryStatus != store.DeliveryStatusOffline || !strings.Contains(rv.DeliveryMessage, "far is offline") {
		t.Fatalf("review %+v", rv)
	}
}

func TestReviewAndExpediteValidation(t *testing.T) {
	e := newAuditEnv(t)
	ctx := context.Background()
	if err := e.th.st.CreateTask(ctx, store.Task{ID: "t1", Agent: "mock", Workdir: "/r", Prompt: "x", Status: protocol.TaskStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/api/tasks/t1/review", `{"decision":"merge"}`, http.StatusBadRequest},
		{http.MethodPost, "/api/tasks/t1/review", `{"decision":"changes_requested","note":"  "}`, http.StatusBadRequest},
		{http.MethodPost, "/api/tasks/nope/review", `{"decision":"approved"}`, http.StatusNotFound},
		{http.MethodPut, "/api/tasks/nope/expedite", `{"expedite":true}`, http.StatusNotFound},
		{http.MethodPut, "/api/chats/nope/expedite", `{"expedite":true}`, http.StatusNotFound},
		{http.MethodPut, "/api/tasks/t1/expedite", `{"expedite":true}`, http.StatusOK},
	} {
		if code, raw := e.do(t, credCookie, c.method, c.path, c.body); code != c.want {
			t.Errorf("%s %s %s: %d %s, want %d", c.method, c.path, c.body, code, raw, c.want)
		}
	}
	rv, err := e.th.st.GetReview(ctx, "t1", "")
	if err != nil || !rv.Expedite || rv.ExpeditedAt == nil || rv.Decision != "" {
		t.Fatalf("expedite stored %+v, %v", rv, err)
	}
	// Taking it out clears the flag; the task sid works as the path id.
	task := mustTask(t, e.th.st, "t1")
	if code, raw := e.do(t, credCookie, http.MethodPut, "/api/tasks/"+task.SID+"/expedite", `{"expedite":false}`); code != http.StatusOK {
		t.Fatalf("un-expedite: %d %s", code, raw)
	}
	if rv, _ := e.th.st.GetReview(ctx, "t1", ""); rv.Expedite || rv.ExpeditedAt != nil {
		t.Fatalf("un-expedited review %+v", rv)
	}
}

func TestTaskAcceptanceComesFromTheTicketElseThePrompt(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	th.h.kaShowFn = func(_ context.Context, id string) ([]byte, error) {
		if id != "SYR-H50YW" {
			return nil, context.DeadlineExceeded
		}
		return []byte(`{"id":"SYR-H50YW","title":"Delivery modes","acceptance_items":[
			{"text":"Request changes returns to agent","done":true},{"text":"Approve runs delivery mode","done":false}]}`), nil
	}
	got := th.h.TaskAcceptance(ctx, store.Task{TicketID: "SYR-H50YW", Prompt: "- prompt bullet"})
	want := []AcceptanceItem{{Text: "Request changes returns to agent", Done: true}, {Text: "Approve runs delivery mode"}}
	if got.Source != "ticket" || got.TicketID != "SYR-H50YW" || got.TicketTitle != "Delivery modes" || !slices.Equal(got.Items, want) {
		t.Fatalf("ticket acceptance %+v", got)
	}
	// An unknown ticket (or ka missing) falls back to the prompt.
	got = th.h.TaskAcceptance(ctx, store.Task{TicketID: "SYR-ZZZZZ", Prompt: "Do it\n- prompt bullet"})
	if got.Source != "prompt" || !slices.Equal(got.Items, []AcceptanceItem{{Text: "prompt bullet"}}) {
		t.Fatalf("fallback acceptance %+v", got)
	}
	// No bullets: the first line is the goal.
	got = th.h.TaskAcceptance(ctx, store.Task{Prompt: "Fix the race in hub"})
	if got.Source != "goal" || !slices.Equal(got.Items, []AcceptanceItem{{Text: "Fix the race in hub"}}) {
		t.Fatalf("goal acceptance %+v", got)
	}
}
