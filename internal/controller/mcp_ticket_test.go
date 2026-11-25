package controller

import (
	"context"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTicketMCPToolsOnlyOnTicketSessions(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)

	// 1. Plain agent chat (no task/ticket) -> no ticket tools.
	_, openPlain, _ := openChat(t, cs, w, b, nil)
	plainSess, err := connectChatMCP(t, w.mcpURL(openPlain.MCPServers[0]), strings.TrimPrefix(openPlain.MCPServers[0].Headers["Authorization"], "Bearer "))
	if err != nil {
		t.Fatal(err)
	}
	defer plainSess.Close()
	plainTools, err := plainSess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var plainNames []string
	for _, tl := range plainTools.Tools {
		plainNames = append(plainNames, tl.Name)
	}
	slices.Sort(plainNames)
	wantPlain := []string{"ask_user", "recent_events", "release_hold", "request_user_action"}
	if !slices.Equal(plainNames, wantPlain) {
		t.Fatalf("plain chat tools = %v, want %v", plainNames, wantPlain)
	}

	// 2. Ticket-backed chat session (via task_id with task.TicketID set) -> includes ticket tools.
	proj, repo := createProjectAndRepo(t, cs.st, "Billing", false, false)
	tk, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Ticket session test",
		Status:    store.TicketStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repo.ID})
	task := store.Task{
		ID:        "t-mcp-1",
		WorkerID:  "w1",
		ProjectID: proj.ID,
		Agent:     "claude",
		Workdir:   "/work",
		Prompt:    "work",
		Status:    protocol.TaskStatusRunning,
		TicketID:  tk.ID,
	}
	if err := cs.st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	_, openTk, _ := openChat(t, cs, w, b, map[string]any{"task_id": task.ID})
	tkSess, err := connectChatMCP(t, w.mcpURL(openTk.MCPServers[0]), strings.TrimPrefix(openTk.MCPServers[0].Headers["Authorization"], "Bearer "))
	if err != nil {
		t.Fatal(err)
	}
	defer tkSess.Close()
	tkTools, err := tkSess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tkNames []string
	for _, tl := range tkTools.Tools {
		tkNames = append(tkNames, tl.Name)
	}
	slices.Sort(tkNames)
	wantTk := []string{"ask_user", "recent_events", "release_hold", "request_user_action", "ticket_check", "ticket_comment", "ticket_followup", "ticket_get"}
	if !slices.Equal(tkNames, wantTk) {
		t.Fatalf("ticket chat tools = %v, want %v", tkNames, wantTk)
	}
}

func TestTicketMCPToolsScoping(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)

	proj, repo := createProjectAndRepo(t, cs.st, "Billing", false, false)
	tkA, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "Ticket A",
		Acceptance: []store.AcceptanceItem{{Text: "Item A1"}},
		Status:     store.TicketStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	tkB, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "Ticket B",
		Acceptance: []store.AcceptanceItem{{Text: "Item B1"}},
		Status:     store.TicketStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tkA.ID, RepoID: repo.ID})
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tkB.ID, RepoID: repo.ID})

	taskA := store.Task{ID: "t-scope-a", WorkerID: "w1", ProjectID: proj.ID, Agent: "claude", Workdir: "/work", Prompt: "a", Status: protocol.TaskStatusRunning, TicketID: tkA.ID}
	taskB := store.Task{ID: "t-scope-b", WorkerID: "w1", ProjectID: proj.ID, Agent: "claude", Workdir: "/work", Prompt: "b", Status: protocol.TaskStatusRunning, TicketID: tkB.ID}
	if err := cs.st.CreateTask(ctx, taskA); err != nil {
		t.Fatal(err)
	}
	if err := cs.st.CreateTask(ctx, taskB); err != nil {
		t.Fatal(err)
	}

	_, openA, _ := openChat(t, cs, w, b, map[string]any{"task_id": taskA.ID})
	sessA, err := connectChatMCP(t, w.mcpURL(openA.MCPServers[0]), strings.TrimPrefix(openA.MCPServers[0].Headers["Authorization"], "Bearer "))
	if err != nil {
		t.Fatal(err)
	}
	defer sessA.Close()

	// Session A tries to touch Ticket B via ticket_check, ticket_comment, ticket_followup, ticket_get -> all rejected.
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"ticket_check", map[string]any{"ticket_id": tkB.SID, "index": 0}},
		{"ticket_check", map[string]any{"ticket_id": tkB.ID, "text": "Item B1"}},
		{"ticket_comment", map[string]any{"ticket_id": tkB.SID, "text": "hijack"}},
		{"ticket_followup", map[string]any{"ticket_id": tkB.SID, "title": "hijack"}},
		{"ticket_get", map[string]any{"ticket_id": tkB.SID}},
	} {
		res, err := sessA.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("session A %s(%v) should have been rejected, got: %s", tc.name, tc.args, toolText(res))
		}
	}

	// Verify Ticket B was completely untouched.
	gotB, err := cs.st.GetTicket(ctx, tkB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotB.Acceptance[0].Done {
		t.Fatalf("Ticket B acceptance item was modified by session A")
	}
	if commentsB, _ := cs.st.ListTicketComments(ctx, tkB.ID); len(commentsB) != 0 {
		t.Fatalf("Ticket B comments modified by session A: %+v", commentsB)
	}
	if gotTaskB, _ := cs.st.GetTask(ctx, taskB.ID); len(gotTaskB.Followups) != 0 {
		t.Fatalf("Task B followups modified by session A: %+v", gotTaskB.Followups)
	}
}

func TestTicketMCPToolsPersistenceAndDigestMerge(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)

	proj, repo := createProjectAndRepo(t, cs.st, "Billing", false, false)
	tk, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Rotate refresh tokens",
		Body:      "Rotate refresh tokens every 15m.",
		Acceptance: []store.AcceptanceItem{
			{Text: "Rotate on /auth/refresh"},
			{Text: "Web fetch wrapper retries once on 401"},
		},
		VerifyCmd: "go test ./...",
		Status:    store.TicketStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
		TicketID: tk.ID, RepoID: repo.ID, TargetBranch: "feat/rotate",
	})

	task := store.Task{
		ID:        "t-persist-1",
		WorkerID:  "w1",
		ProjectID: proj.ID,
		Agent:     "claude",
		Model:     "opus",
		Workdir:   "/work",
		Prompt:    "work",
		Status:    protocol.TaskStatusRunning,
		TicketID:  tk.ID,
	}
	if err := cs.st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	chat, open, _ := openChat(t, cs, w, b, map[string]any{"task_id": task.ID})
	sess, err := connectChatMCP(t, w.mcpURL(open.MCPServers[0]), strings.TrimPrefix(open.MCPServers[0].Headers["Authorization"], "Bearer "))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// 1. ticket_get returns packet with both unchecked acceptance items.
	getRes, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "ticket_get", Arguments: map[string]any{}})
	if err != nil || getRes.IsError {
		t.Fatalf("ticket_get failed: %v %s", err, toolText(getRes))
	}
	pktText := toolText(getRes)
	if !strings.Contains(pktText, "ticket: "+tk.SID) ||
		!strings.Contains(pktText, "Rotate on /auth/refresh") ||
		!strings.Contains(pktText, "Web fetch wrapper retries once on 401") {
		t.Fatalf("unexpected ticket_get output:\n%s", pktText)
	}

	// 2. ticket_check by index (0) and by text substring.
	chk1, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "ticket_check", Arguments: map[string]any{"index": 0}})
	if err != nil || chk1.IsError {
		t.Fatalf("ticket_check(index=0) failed: %v %s", err, toolText(chk1))
	}
	chk2, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "ticket_check", Arguments: map[string]any{"text": "retries once on 401"}})
	if err != nil || chk2.IsError {
		t.Fatalf("ticket_check(text) failed: %v %s", err, toolText(chk2))
	}
	updatedTk, err := cs.st.GetTicket(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !updatedTk.Acceptance[0].Done || !updatedTk.Acceptance[1].Done {
		t.Fatalf("expected both acceptance items checked, got: %+v", updatedTk.Acceptance)
	}

	// Subsequent ticket_get omits checked acceptance items.
	getRes2, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "ticket_get", Arguments: map[string]any{}})
	if err != nil || getRes2.IsError {
		t.Fatalf("second ticket_get failed: %v %s", err, toolText(getRes2))
	}
	if strings.Contains(toolText(getRes2), "acceptance:") {
		t.Fatalf("expected checked acceptance items omitted from ticket_get:\n%s", toolText(getRes2))
	}

	// 3. ticket_comment persists comment with session SID as author; rejects >500 chars.
	tooLong := strings.Repeat("x", 501)
	badComment, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "ticket_comment", Arguments: map[string]any{"text": tooLong}})
	if err == nil && (badComment == nil || !badComment.IsError) {
		t.Fatalf("expected >500 char comment to be rejected, got: %s", toolText(badComment))
	}
	okComment, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "ticket_comment", Arguments: map[string]any{"text": "Rotation tests green."}})
	if err != nil || okComment.IsError {
		t.Fatalf("ticket_comment failed: %v %s", err, toolText(okComment))
	}
	comments, err := cs.st.ListTicketComments(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Author != chat.SID || comments[0].Body != "Rotation tests green." {
		t.Fatalf("unexpected persisted comments: %+v", comments)
	}

	// 4. ticket_followup persists to task.followups_json and merges into stored digest on completion.
	fuRes, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "ticket_followup", Arguments: map[string]any{
		"title": "Deprecate /v1/token static refresh",
		"why":   "/v1/token still issues static refresh tokens",
		"scope": "api",
	}})
	if err != nil || fuRes.IsError {
		t.Fatalf("ticket_followup failed: %v %s", err, toolText(fuRes))
	}
	storedTask, err := cs.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(storedTask.Followups) != 1 || storedTask.Followups[0].Title != "Deprecate /v1/token static refresh" {
		t.Fatalf("unexpected task followups: %+v", storedTask.Followups)
	}

	// Simulate daemon finishing the task and reporting a RunDigest.
	code := 0
	w.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{
		TaskID:   task.ID,
		Status:   protocol.TaskStatusCompleted,
		ExitCode: &code,
		Digest: &protocol.RunDigest{
			TicketSID: tk.SID,
			Status:    protocol.DigestStatusReview,
			Machine:   "box",
			Agent:     "claude",
			Model:     "opus",
			Followups: []string{"existing daemon followup"},
			Summary:   "Rotation implemented; tests pass.",
		},
	})

	waitFor(t, 3*time.Second, "task digest persisted with merged followups", func() bool {
		got, err := cs.st.GetTask(ctx, task.ID)
		return err == nil && got.Status == protocol.TaskStatusCompleted && got.DigestJSON != ""
	})

	finalTask, err := cs.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var mergedDigest protocol.RunDigest
	if err := json.Unmarshal([]byte(finalTask.DigestJSON), &mergedDigest, protocol.JSONOptions); err != nil {
		t.Fatal(err)
	}
	wantEntry := "Deprecate /v1/token static refresh — /v1/token still issues static refresh tokens [api]"
	if !slices.Contains(mergedDigest.Followups, "existing daemon followup") || !slices.Contains(mergedDigest.Followups, wantEntry) {
		t.Fatalf("merged digest followups = %#v, want both daemon and ticket_followup entries", mergedDigest.Followups)
	}
}
