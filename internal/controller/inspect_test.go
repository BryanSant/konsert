package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestInspect(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	const (
		w1 = "w-inspect-1"
		w2 = "w-inspect-2"
	)
	fw1 := th.connectWorker(t, testWorkerInfo(w1, "box-a"), nil, w1)
	fw2 := th.connectWorker(t, testWorkerInfo(w2, "box-b"), nil, w2)
	if err := th.st.UpsertWorker(ctx, store.Worker{ID: "w-off", Name: "offline-box", HostGroup: "offline-box", Status: "offline"}); err != nil {
		t.Fatal(err)
	}

	t.Run("routes diff to the worker that ran it", func(t *testing.T) {
		tk, task := seedInspectTask(t, th, w1, "route diff", store.Task{
			Worktrees: []store.TaskWorktree{
				{RepoName: "api", WorktreePath: "/wt/api"},
				{RepoName: "web", WorktreePath: "/wt/web"},
			},
		})
		dropFrames(fw1)
		dropFrames(fw2)
		got := make(chan protocol.ArtifactFetchRequest, 1)
		go func() {
			env := fw1.expect(protocol.MsgArtifactFetch, 5*time.Second)
			req, err := protocol.DecodePayload[protocol.ArtifactFetchRequest](env)
			if err != nil {
				t.Errorf("decode artifact.fetch: %v", err)
				return
			}
			got <- req
			fw1.sendWithID(protocol.MsgArtifactFetchResult, env.ID, protocol.ArtifactFetchResult{
				Kind: protocol.InspectKindDiff, Text: "diff-body", MaxLines: req.MaxLines, Offset: req.Offset,
			})
		}()
		callCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		res, err := th.svc.Inspect(callCtx, InspectRequest{Target: tk.SID, Kind: "diff", Repo: "WEB", MaxLines: 50})
		if err != nil {
			t.Fatal(err)
		}
		if res.Text != "diff-body" || res.TaskID != task.ID {
			t.Fatalf("result=%+v", res)
		}
		select {
		case req := <-got:
			if req.Workdir != "/wt/web" || req.Kind != protocol.InspectKindDiff || req.TaskID != task.ID || req.MaxLines != 50 {
				t.Fatalf("request=%+v", req)
			}
		default:
			t.Fatal("worker did not receive artifact.fetch")
		}
		if extra := bufferedFrames(fw2, protocol.MsgArtifactFetch); len(extra) != 0 {
			t.Fatalf("other worker got %d artifact.fetch frames", len(extra))
		}
	})

	t.Run("digest stays on the hub", func(t *testing.T) {
		raw, err := json.Marshal(protocol.RunDigest{Status: protocol.DigestStatusCompleted, Summary: "shipped the button"}, protocol.JSONOptions)
		if err != nil {
			t.Fatal(err)
		}
		_, task := seedInspectTask(t, th, w1, "digest me", store.Task{DigestJSON: string(raw)})
		dropFrames(fw1)
		res, err := th.svc.Inspect(ctx, InspectRequest{Target: task.SID, Kind: protocol.InspectKindDigest})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Text, "summary: shipped the button") {
			t.Fatalf("digest text=%q", res.Text)
		}
		if extra := bufferedFrames(fw1, protocol.MsgArtifactFetch); len(extra) != 0 {
			t.Fatal("digest must not fetch from the daemon")
		}
	})

	t.Run("digest includes the attempts table", func(t *testing.T) {
		raw, err := json.Marshal(protocol.RunDigest{Status: protocol.DigestStatusReview, Summary: "shipped the button"}, protocol.JSONOptions)
		if err != nil {
			t.Fatal(err)
		}
		tk, task := seedInspectTask(t, th, w1, "attempts table", store.Task{DigestJSON: string(raw)})
		dur := int64(7)
		if _, err := th.st.UpsertAttempt(ctx, store.Attempt{
			TicketID: tk.ID, TaskID: task.ID, Attempt: 1, WorkerID: w1,
			Agent: "mock", Model: "m", Status: store.AttemptReview, DurationS: &dur,
			VerifyExit: intPtr(0),
		}); err != nil {
			t.Fatal(err)
		}
		res, err := th.svc.Inspect(ctx, InspectRequest{Target: task.SID, Kind: protocol.InspectKindDigest})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Text, "summary: shipped the button") || !strings.Contains(res.Text, "box-a") ||
			!strings.Contains(res.Text, "mock/m") || !strings.Contains(res.Text, "7s") || !strings.Contains(res.Text, "pass") {
			t.Fatalf("digest text=%q", res.Text)
		}
	})

	t.Run("offline", func(t *testing.T) {
		_, task := seedInspectTask(t, th, "w-off", "offline run", store.Task{})
		_, err := th.svc.Inspect(ctx, InspectRequest{Target: task.SID, Kind: protocol.InspectKindLog})
		var fail *inspectFailure
		if !errors.As(err, &fail) || fail.Status != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "offline") {
			t.Fatalf("err=%v", err)
		}
		if !strings.Contains(err.Error(), "offline-box") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("cleaned up", func(t *testing.T) {
		_, task := seedInspectTask(t, th, w1, "cleaned run", store.Task{
			Worktrees: []store.TaskWorktree{{RepoName: "web", WorktreePath: "/wt/web"}},
		})
		if err := th.st.MarkTaskWorktreesCleaned(ctx, task.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
		dropFrames(fw1)
		_, err := th.svc.Inspect(ctx, InspectRequest{Target: task.SID, Kind: protocol.InspectKindDiff})
		var fail *inspectFailure
		if !errors.As(err, &fail) || fail.Status != http.StatusConflict || !strings.Contains(err.Error(), "cleaned") {
			t.Fatalf("err=%v", err)
		}
		if extra := bufferedFrames(fw1, protocol.MsgArtifactFetch); len(extra) != 0 {
			t.Fatal("cleaned worktree must not fetch")
		}
	})

	t.Run("daemon cleaned", func(t *testing.T) {
		tk, _ := seedInspectTask(t, th, w1, "daemon cleaned", store.Task{
			Worktrees: []store.TaskWorktree{{RepoName: "web", WorktreePath: "/wt/missing"}},
		})
		dropFrames(fw1)
		go func() {
			env := fw1.expect(protocol.MsgArtifactFetch, 5*time.Second)
			fw1.sendWithID(protocol.MsgArtifactFetchResult, env.ID, protocol.ArtifactFetchResult{Error: "worktree was cleaned up"})
		}()
		callCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		_, err := th.svc.Inspect(callCtx, InspectRequest{Target: tk.SID, Kind: protocol.InspectKindDiff})
		var fail *inspectFailure
		if !errors.As(err, &fail) || fail.Status != http.StatusConflict || !strings.Contains(err.Error(), "cleaned up") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("transcript", func(t *testing.T) {
		tk, task := seedInspectTask(t, th, w1, "transcript", store.Task{})
		cs := seedInspectChat(t, th, task, tk.ID)
		appendInspectEvent(t, th, cs.ID, 1, protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, Text: "DELTA-SKIP"})
		appendInspectEvent(t, th, cs.ID, 2, protocol.AgentChatEvent{
			Kind:     protocol.EventToolCall,
			ToolCall: &protocol.AgentToolCall{ID: "run", Name: "Bash", Title: "still going", Status: "running", Output: "RUNNING-SENTINEL"},
		})
		appendInspectEvent(t, th, cs.ID, 3, protocol.AgentChatEvent{Kind: eventUserMessage, Text: "hello\nthere"})
		appendInspectEvent(t, th, cs.ID, 4, protocol.AgentChatEvent{
			Kind:     protocol.EventToolCall,
			ToolCall: &protocol.AgentToolCall{ID: "done", Name: "Bash", Title: "Run tests", Status: "completed", Output: "SENTINEL-OUTPUT"},
		})
		appendInspectEvent(t, th, cs.ID, 5, protocol.AgentChatEvent{Kind: protocol.EventMessage, Text: "world"})
		res, err := th.svc.Inspect(ctx, InspectRequest{Target: tk.SID, Kind: protocol.InspectKindTranscript, MaxLines: 2})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Truncated || res.NextOffset != 2 {
			t.Fatalf("page=%+v", res)
		}
		if !strings.Contains(res.Text, "user: hello there") || !strings.Contains(res.Text, "tool: Bash Run tests") {
			t.Fatalf("text=%q", res.Text)
		}
		for _, hidden := range []string{"SENTINEL-OUTPUT", "RUNNING-SENTINEL", "DELTA-SKIP", "world"} {
			if strings.Contains(res.Text, hidden) {
				t.Fatalf("text contains %s: %q", hidden, res.Text)
			}
		}
	})

	t.Run("transcript byte cap", func(t *testing.T) {
		tk, task := seedInspectTask(t, th, w1, "transcript bytes", store.Task{})
		cs := seedInspectChat(t, th, task, tk.ID)
		appendInspectEvent(t, th, cs.ID, 1, protocol.AgentChatEvent{
			Kind: protocol.EventMessage, Text: strings.Repeat("a", 200*1024) + "ENDMARKER",
		})
		res, err := th.svc.Inspect(ctx, InspectRequest{Target: task.SID, Kind: protocol.InspectKindTranscript, MaxLines: 2000})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Truncated || len(res.Text) > protocol.InspectByteCap || strings.Contains(res.Text, "ENDMARKER") {
			t.Fatalf("len=%d truncated=%v note=%q", len(res.Text), res.Truncated, res.Note)
		}
		if !strings.Contains(res.Note, "offset=") {
			t.Fatalf("note=%q", res.Note)
		}
	})
}

func seedInspectTask(t *testing.T, th *testHub, workerID, title string, task store.Task) (store.Ticket, store.Task) {
	t.Helper()
	ctx := context.Background()
	tk, err := th.st.CreateTicket(ctx, store.Ticket{Title: title, ProjectID: store.DefaultProjectID})
	if err != nil {
		t.Fatal(err)
	}
	wts := task.Worktrees
	task.WorkerID = workerID
	task.TicketID = tk.ID
	task.ProjectID = store.DefaultProjectID
	if task.Agent == "" {
		task.Agent = "mock"
	}
	if task.Workdir == "" {
		task.Workdir = "/work"
	}
	if task.Prompt == "" {
		task.Prompt = "work"
	}
	if task.Status == "" {
		task.Status = protocol.TaskStatusCompleted
	}
	task.Worktrees = nil
	if err := th.st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	got, err := th.st.ListTasksByTicket(ctx, tk.ID)
	if err != nil || len(got) == 0 {
		t.Fatalf("list tasks: %v n=%d", err, len(got))
	}
	if len(wts) > 0 {
		if err := th.st.UpdateTaskWorktrees(ctx, got[0].ID, wts); err != nil {
			t.Fatal(err)
		}
		got[0].Worktrees = wts
	}
	return tk, got[0]
}

func seedInspectChat(t *testing.T, th *testHub, task store.Task, ticketID string) store.ChatSession {
	t.Helper()
	ctx := context.Background()
	if err := th.st.CreateChatSession(ctx, store.ChatSession{
		Title: "inspect", Provider: "agent", Model: "mock", Backend: store.ChatBackendAgent,
		WorkerID: task.WorkerID, TaskID: task.ID, TicketID: ticketID, Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	cs, err := th.st.GetChatSessionByTaskID(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func appendInspectEvent(t *testing.T, th *testHub, chatID string, seq int64, ev protocol.AgentChatEvent) {
	t.Helper()
	ev.ChatID = chatID
	ev.Seq = seq
	raw, err := json.Marshal(ev, protocol.JSONOptions)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := th.st.AppendChatEvent(context.Background(), store.ChatEvent{
		ChatID: chatID, Seq: seq, TurnID: ev.TurnID, Kind: ev.Kind, JSON: string(raw),
	})
	if err != nil || !ok {
		t.Fatalf("append event %d: %v ok=%v", seq, err, ok)
	}
}

func dropFrames(fw *fakeWorker) {
	for {
		select {
		case <-fw.frames:
		default:
			return
		}
	}
}

func bufferedFrames(fw *fakeWorker, msgType string) []protocol.Envelope {
	var out []protocol.Envelope
	for {
		select {
		case env := <-fw.frames:
			if env.Type == msgType {
				out = append(out, env)
			}
		default:
			return out
		}
	}
}
