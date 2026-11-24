package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
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

// answerPRReview makes the fake worker answer one git.pr.review with res and returns the
// request it got.
func answerPRReview(w *fakeWorker, res protocol.GitPRReviewResult) <-chan protocol.GitPRReviewRequest {
	got := make(chan protocol.GitPRReviewRequest, 1)
	go func() {
		env := w.expect(protocol.MsgGitPRReview, 5*time.Second)
		req, _ := protocol.DecodePayload[protocol.GitPRReviewRequest](env)
		got <- req
		raw, _ := protocol.NewEnvelope(protocol.MsgGitPRReviewResult, env.ID, res)
		_ = w.conn.Write(w.ctx, websocket.MessageText, raw)
	}()
	return got
}

func decodeReviewResult(t *testing.T, raw string) ReviewResult {
	t.Helper()
	var res ReviewResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return res
}

func TestReviewApproveRunsGHOnTheMachineThatOwnsTheCheckout(t *testing.T) {
	e := newAuditEnv(t)
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	ctx := context.Background()
	if err := e.th.st.CreateTask(ctx, store.Task{ID: "t1", WorkerID: "w1", Agent: "mock", Workdir: "/r", EffectiveDir: "/r/wt", Branch: "konsert/t1",
		Prompt: "count words", Status: protocol.TaskStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	reqs := answerPRReview(w, protocol.GitPRReviewResult{Status: protocol.PRReviewSent, PRNumber: 7, PRURL: "https://github.com/acme/wc/pull/7"})
	code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t1/review", `{"decision":"approved"}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	req := <-reqs
	if req.Action != protocol.PRReviewApprove || req.Workdir != "/r/wt" || req.Branch != "konsert/t1" {
		t.Fatalf("git.pr.review request %+v", req)
	}
	res := decodeReviewResult(t, raw)
	rv := res.Review
	if rv.Decision != store.ReviewApproved || rv.GitHubStatus != protocol.PRReviewSent || rv.GitHubURL == "" ||
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
	// The task timeline records the decision.
	evs, _ := e.th.st.ListEvents(ctx, "t1", 0, 50)
	if !slices.ContainsFunc(evs, func(ev store.TaskEvent) bool { return ev.Kind == "review" && ev.Message == "Approved" }) {
		t.Fatalf("no review event in %+v", evs)
	}
}

func TestReviewRequestChangesOnFinishedWorkStartsAFollowUp(t *testing.T) {
	e := newAuditEnv(t)
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	ctx := context.Background()
	if err := e.th.st.CreateTask(ctx, store.Task{ID: "t1", WorkerID: "w1", Agent: "mock", Workdir: "/r", Title: "Word count",
		Prompt: "count words", Status: protocol.TaskStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	reqs := answerPRReview(w, protocol.GitPRReviewResult{Status: protocol.PRReviewNoPR, Message: "no pull request for branch main"})
	code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t1/review", `{"decision":"changes_requested","note":"Handle empty files"}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if req := <-reqs; req.Action != protocol.PRReviewRequestChanges || req.Body != "Handle empty files" {
		t.Fatalf("git.pr.review request %+v", req)
	}
	res := decodeReviewResult(t, raw)
	if res.Review.Decision != store.ReviewChangesRequested || res.Review.Note != "Handle empty files" ||
		res.Review.GitHubStatus != protocol.PRReviewNoPR || res.Delivered != "follow_up" || res.FollowUpTaskID == "" ||
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
	// One reader: the PR review comes first, then the note is typed into the session.
	env := w.expect(protocol.MsgGitPRReview, 5*time.Second)
	raw, _ := protocol.NewEnvelope(protocol.MsgGitPRReviewResult, env.ID, protocol.GitPRReviewResult{Status: protocol.PRReviewNoGitHub, Message: "the checkout has no GitHub remote"})
	_ = w.conn.Write(w.ctx, websocket.MessageText, raw)
	steer := w.expect(protocol.MsgTaskSteer, 5*time.Second)
	p, _ := protocol.DecodePayload[protocol.TaskSteerPayload](steer)
	if p.TaskID != "t1" || p.InputText != "Use a map" || !p.AppendNewline {
		t.Fatalf("steer %+v", p)
	}
	if res := <-done; res.Delivered != "steered" || res.Review.GitHubStatus != protocol.PRReviewNoGitHub {
		t.Fatalf("review result %+v", res)
	}
}

func TestReviewOfflineMachineIsRecordedButNotSent(t *testing.T) {
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
	if rv.Decision != store.ReviewApproved || rv.GitHubStatus != protocol.PRReviewOffline || !strings.Contains(rv.GitHubMessage, "far is offline") {
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
		if id != "KON-BM8JR" {
			return nil, context.DeadlineExceeded
		}
		return []byte(`{"id":"KON-BM8JR","title":"Store reviews on the hub","acceptance_items":[
			{"text":"Decisions persist on the hub","done":true},{"text":"Approve uses gh","done":false}]}`), nil
	}
	got := th.h.TaskAcceptance(ctx, store.Task{TicketID: "KON-BM8JR", Prompt: "- prompt bullet"})
	want := []AcceptanceItem{{Text: "Decisions persist on the hub", Done: true}, {Text: "Approve uses gh"}}
	if got.Source != "ticket" || got.TicketID != "KON-BM8JR" || got.TicketTitle != "Store reviews on the hub" || !slices.Equal(got.Items, want) {
		t.Fatalf("ticket acceptance %+v", got)
	}
	// An unknown ticket (or ka missing) falls back to the prompt.
	got = th.h.TaskAcceptance(ctx, store.Task{TicketID: "KON-ZZZZZ", Prompt: "Do it\n- prompt bullet"})
	if got.Source != "prompt" || !slices.Equal(got.Items, []AcceptanceItem{{Text: "prompt bullet"}}) {
		t.Fatalf("fallback acceptance %+v", got)
	}
}

func TestTaskAcceptanceRoute(t *testing.T) {
	e := newAuditEnv(t)
	ctx := context.Background()
	e.th.h.kaShowFn = func(context.Context, string) ([]byte, error) {
		return []byte(`{"title":"T","acceptance_items":[{"text":"from the ticket","done":true}]}`), nil
	}
	if err := e.th.st.CreateTask(ctx, store.Task{ID: "t1", Agent: "mock", Workdir: "/r", Prompt: "Do KON-BM8JR\n- a bullet", Status: protocol.TaskStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if err := e.th.st.SetTaskTicket(ctx, "t1", "KON-BM8JR", "T"); err != nil {
		t.Fatal(err)
	}
	code, raw := e.do(t, credCookie, http.MethodGet, "/api/tasks/t1/acceptance", "")
	if code != http.StatusOK || !strings.Contains(raw, `"source":"ticket"`) || !strings.Contains(raw, `{"text":"from the ticket","done":true}`) {
		t.Fatalf("GET acceptance: %d %s", code, raw)
	}
	if code, _ := e.do(t, credCookie, http.MethodGet, "/api/tasks/nope/acceptance", ""); code != http.StatusNotFound {
		t.Fatalf("missing task: %d", code)
	}
}

func TestAcceptanceFromPrompt(t *testing.T) {
	cases := []struct {
		name, prompt, source string
		items                []AcceptanceItem
	}{
		{"bullets", "Fix the counter.\n\n- counts words\n* counts lines\n• handles empty input", "prompt",
			[]AcceptanceItem{{Text: "counts words"}, {Text: "counts lines"}, {Text: "handles empty input"}}},
		{"numbered", "1. first\n2) second", "prompt", []AcceptanceItem{{Text: "first"}, {Text: "second"}}},
		{"checkboxes", "- [x] done one\n- [ ] open one\n[X] bare done", "prompt",
			[]AcceptanceItem{{Text: "done one", Done: true}, {Text: "open one"}, {Text: "bare done", Done: true}}},
		{"no bullets: the goal", "Make wc count bytes\nmore detail", "goal", []AcceptanceItem{{Text: "Make wc count bytes"}}},
		{"empty", "", "goal", []AcceptanceItem{}},
		{"dash without space is not a bullet", "-flag\n--help", "goal", []AcceptanceItem{{Text: "-flag"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := acceptanceFromPrompt(c.prompt)
			if got.Source != c.source || !slices.Equal(got.Items, c.items) {
				t.Fatalf("acceptanceFromPrompt(%q) = %+v, want %s %+v", c.prompt, got, c.source, c.items)
			}
		})
	}
	var long strings.Builder
	for range maxAcceptance + 5 {
		long.WriteString("- item\n")
	}
	if got := acceptanceFromPrompt(long.String()); len(got.Items) != maxAcceptance {
		t.Fatalf("%d items, want the cap %d", len(got.Items), maxAcceptance)
	}
}
