package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	commanderSchemaMaxTokens     = 2500
	fleetStatus20MachineMaxToken = 400
)

func connectInMemoryMCP(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	cTrans, sTrans := mcp.NewInMemoryTransports()
	srvSess, err := srv.Connect(ctx, sTrans, nil)
	if err != nil {
		t.Fatalf("mcp server connect: %v", err)
	}
	t.Cleanup(func() { _ = srvSess.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "surface-test", Version: "1.0"}, nil)
	sess, err := client.Connect(ctx, cTrans, nil)
	if err != nil {
		t.Fatalf("mcp client connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func TestCommanderToolListAndSchemaTokenBudget(t *testing.T) {
	cs := newChatServer(t)
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)
	_, open, _ := openChat(t, cs, w, b, map[string]any{"fleet_tools": true})
	tok := strings.TrimPrefix(open.MCPServers[0].Headers["Authorization"], "Bearer ")
	sess, err := connectChatMCP(t, w.mcpURL(open.MCPServers[0]), tok)
	if err != nil {
		t.Fatalf("connect commander MCP: %v", err)
	}
	defer sess.Close()

	listed, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	gotNames := make([]string, 0, len(listed.Tools))
	for _, tl := range listed.Tools {
		gotNames = append(gotNames, tl.Name)
	}
	slices.Sort(gotNames)

	wantCommander := []string{
		"answer_question",
		"ask_user",
		"backlog",
		"control",
		"fleet_status",
		"inspect",
		"manage_work",
		"memory",
		"reply",
		"request_override",
		"set_policy",
	}
	if !slices.Equal(gotNames, wantCommander) {
		t.Fatalf("commander tool list = %v, want %v", gotNames, wantCommander)
	}

	raw, err := json.Marshal(listed.Tools, protocol.JSONOptions)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	tokens := protocol.EstimateTokens(string(raw))
	t.Logf("Commander serialized tool schemas: %d chars, %d tokens (budget <= %d)", len(raw), tokens, commanderSchemaMaxTokens)
	if tokens > commanderSchemaMaxTokens {
		t.Fatalf("commander tool schemas = %d tokens (%d chars), exceeds budget %d", tokens, len(raw), commanderSchemaMaxTokens)
	}

	// External /mcp surface (with commander service attached) has the 8 core tools + ask_commander.
	extSrv := NewMCPServer(cs.svc, "test", cs.srv.Commander())
	extSess := connectInMemoryMCP(t, extSrv)
	extListed, err := extSess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ext ListTools: %v", err)
	}
	extNames := make([]string, 0, len(extListed.Tools))
	for _, tl := range extListed.Tools {
		extNames = append(extNames, tl.Name)
	}
	slices.Sort(extNames)
	wantExt := []string{
		"ask_commander",
		"backlog",
		"control",
		"fleet_status",
		"inspect",
		"manage_work",
		"memory",
		"reply",
		"set_policy",
	}
	if !slices.Equal(extNames, wantExt) {
		t.Fatalf("external /mcp tool list = %v, want %v", extNames, wantExt)
	}

	// API-key commander backend ToolDefinitions matches the exact 8 core fleet tools.
	defs := cs.svc.ToolDefinitions()
	apiNames := make([]string, 0, len(defs))
	for _, d := range defs {
		apiNames = append(apiNames, d.Name)
	}
	slices.Sort(apiNames)
	wantAPI := []string{
		"backlog",
		"control",
		"fleet_status",
		"inspect",
		"manage_work",
		"memory",
		"reply",
		"set_policy",
	}
	if !slices.Equal(apiNames, wantAPI) {
		t.Fatalf("API-key commander tool list = %v, want %v", apiNames, wantAPI)
	}
}

func TestFleetStatusSummary20MachineTokenBudgetAndDetail(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()

	// Seed 20 machines across several capability groups (with RAM, GPU, online/offline/drained).
	now := time.Now().UTC()
	for i := 1; i <= 20; i++ {
		osName, arch, ramMB := "linux", "amd64", uint64(64*1024)
		tags := map[string]string{}
		if i <= 6 {
			tags["gpu"] = "true"
		} else if i <= 14 {
			osName, arch, ramMB = "darwin", "arm64", uint64(32*1024)
		} else {
			osName, arch, ramMB = "windows", "amd64", uint64(16*1024)
		}
		factsRaw, _ := json.Marshal(protocol.MachineFacts{
			CPUCores:   16,
			RAMTotalMB: ramMB,
			DiskFreeGB: 200,
		}, protocol.JSONOptions)
		w := store.Worker{
			ID:            fmt.Sprintf("w-%02d", i),
			SID:           fmt.Sprintf("m-%02d", i),
			Name:          fmt.Sprintf("node-%02d", i),
			OS:            osName,
			Arch:          arch,
			Agents:        []string{"claude", "codex"},
			Tags:          tags,
			MaxConcurrent: 2,
			Status:        "offline",
			Drained:       i == 20,
			FactsJSON:     string(factsRaw),
			LastSeenAt:    now,
		}
		if err := th.st.UpsertWorker(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	// Add a directive to verify it appears in summary.
	until := now.Add(2 * time.Hour)
	if _, err := th.h.CreateDirective(ctx, DirectiveInput{
		Kind:   store.DirectiveProviderDisable,
		Target: "codex",
		Until:  &until,
		Reason: "quota",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := th.svc.FleetStatusView(ctx, FleetStatusInput{})
	if err != nil {
		t.Fatalf("FleetStatusView summary: %v", err)
	}
	raw, err := json.Marshal(res, protocol.JSONOptions)
	if err != nil {
		t.Fatal(err)
	}
	tokens := protocol.EstimateTokens(string(raw))
	t.Logf("20-machine fleet_status summary: %d chars, %d tokens (budget <= %d): %s", len(raw), tokens, fleetStatus20MachineMaxToken, raw)
	if tokens > fleetStatus20MachineMaxToken {
		t.Fatalf("20-machine fleet_status summary = %d tokens, exceeds budget %d", tokens, fleetStatus20MachineMaxToken)
	}
	sum := res.(FleetSummaryResult)
	if sum.Total != 20 || len(sum.Groups) == 0 || len(sum.Directives) != 1 {
		t.Fatalf("unexpected summary: %+v", sum)
	}

	// detail=machines with machine filter returns per-machine facts.
	mRes, err := th.svc.FleetStatusView(ctx, FleetStatusInput{Machine: "node-01"})
	if err != nil {
		t.Fatalf("FleetStatusView machine: %v", err)
	}
	mMap := mRes.(map[string]any)
	rows := mMap["machines"].([]MachineDetailRow)
	if len(rows) != 1 || rows[0].WorkerName != "node-01" || rows[0].Facts == nil || rows[0].Facts.RAMTotalMB != 64*1024 {
		t.Fatalf("unexpected machine detail: %+v", rows)
	}
}

func TestSurfaceBacklogManageWorkControlMemoryInspect(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()

	// 1. manage_work: create_project + add_repo + batch create tickets with blocked_by, repos, hints
	projRes, err := th.svc.ManageWork(ctx, ManageWorkInput{
		Action: "create_project",
		Items:  []WorkItemInput{{Title: "Billing", VerifyCmd: "go test ./..."}},
	})
	if err != nil || len(projRes.Results) != 1 {
		t.Fatalf("create_project: %v %+v", err, projRes)
	}
	repoRes, err := th.svc.ManageWork(ctx, ManageWorkInput{
		Action: "add_repo",
		Items:  []WorkItemInput{{Project: "Billing", Title: "api", RemoteURL: "https://example.com/billing-api.git"}},
	})
	if err != nil || len(repoRes.Results) != 1 {
		t.Fatalf("add_repo: %v %+v", err, repoRes)
	}

	createRes, err := th.svc.ManageWork(ctx, ManageWorkInput{
		Action: "create",
		Items: []WorkItemInput{
			{
				Project:    "Billing",
				Title:      "Add refresh endpoint",
				Status:     store.TicketStatusReady,
				Acceptance: []string{"Rotates tokens"},
				Repos:      []string{"api"},
				Hints:      &store.TicketHints{Agent: "claude"},
			},
			{
				Project:   "Billing",
				Title:     "Retry on 401 in client",
				Status:    store.TicketStatusReady,
				BlockedBy: []string{"Add refresh endpoint"},
				Repos:     []string{"api"},
			},
		},
	})
	if err != nil || len(createRes.Results) != 2 {
		t.Fatalf("batch create: %v %+v", err, createRes)
	}
	t1SID := createRes.Results[0].(map[string]any)["sid"].(string)
	t2SID := createRes.Results[1].(map[string]any)["sid"].(string)

	// 2. backlog shows projects/repos and tickets with blockers.
	bl, err := th.svc.Backlog(ctx, BacklogInput{Project: "Billing"})
	if err != nil {
		t.Fatalf("backlog: %v", err)
	}
	if len(bl.Projects) != 1 || len(bl.Projects[0].Repos) != 1 {
		t.Fatalf("backlog projects: %+v", bl.Projects)
	}
	if len(bl.Tickets) != 2 {
		t.Fatalf("backlog tickets: %+v", bl.Tickets)
	}
	foundBlocker := false
	for _, tk := range bl.Tickets {
		if tk.SID == t2SID && slices.Contains(tk.Blockers, t1SID) {
			foundBlocker = true
		}
	}
	if !foundBlocker {
		t.Fatalf("expected %s to be blocked by %s; got %+v", t2SID, t1SID, bl.Tickets)
	}

	// 3. manage_work update + requeue + cancel
	if _, err := th.svc.ManageWork(ctx, ManageWorkInput{
		Action: "update",
		Items:  []WorkItemInput{{ID: t1SID, Priority: "P1"}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := th.svc.ManageWork(ctx, ManageWorkInput{
		Action: "requeue",
		Items:  []WorkItemInput{{ID: t1SID, Note: "retry"}},
	}); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if _, err := th.svc.ManageWork(ctx, ManageWorkInput{
		Action: "cancel",
		Items:  []WorkItemInput{{ID: t2SID, Note: "superseded"}},
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// 4. control: autopilot play/pause and refuse-on-/mcp for resume/undrain
	if _, err := th.svc.Control(ctx, ControlInput{Scope: "project", Target: "Billing", Action: "autopilot_play"}, nil); err != nil {
		t.Fatalf("control autopilot_play: %v", err)
	}
	if _, err := th.svc.Control(ctx, ControlInput{Scope: "project", Target: "Billing", Action: "autopilot_pause"}, nil); err != nil {
		t.Fatalf("control autopilot_pause: %v", err)
	}
	if _, err := th.svc.Control(ctx, ControlInput{Scope: "fleet", Action: "pause"}, nil); err != nil {
		t.Fatalf("control fleet pause: %v", err)
	}
	if _, err := th.svc.Control(ctx, ControlInput{Scope: "fleet", Action: "resume"}, nil); err == nil {
		t.Fatal("expected control fleet resume without confirm to refuse")
	}
	if _, err := th.svc.Control(ctx, ControlInput{Scope: "machine", Target: "box", Action: "undrain"}, nil); err == nil {
		t.Fatal("expected control machine undrain without confirm to refuse")
	}

	// 5. memory save & recall
	if _, err := th.svc.Memory(ctx, MemoryInput{Action: "save", Scope: "fleet", Key: "k1", Summary: "remember this"}); err != nil {
		t.Fatalf("memory save: %v", err)
	}
	rec, err := th.svc.Memory(ctx, MemoryInput{Action: "recall", Scope: "fleet"})
	if err != nil {
		t.Fatalf("memory recall: %v", err)
	}
	if entries := rec.(map[string]any)["entries"].([]store.MemoryEntry); len(entries) != 1 || entries[0].Key != "k1" {
		t.Fatalf("unexpected memory entries: %+v", entries)
	}

	// 6. inspect kind=events and kind=audit
	evRes, err := th.svc.Inspect(ctx, InspectRequest{Kind: protocol.InspectKindEvents})
	if err != nil || !strings.Contains(evRes.Text, "events") {
		t.Fatalf("inspect events: %v %+v", err, evRes)
	}
	auRes, err := th.svc.Inspect(ctx, InspectRequest{Kind: protocol.InspectKindAudit})
	if err != nil || !strings.Contains(auRes.Text, "events") {
		t.Fatalf("inspect audit: %v %+v", err, auRes)
	}
}

func TestReplyEnforcesAnswerQuestionRules(t *testing.T) {
	cs := newChatServer(t)
	cs.h.AskUserTimeout = 10 * time.Second
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)
	_, openWorker, _ := openChat(t, cs, w, b, nil)
	_, openCmdr, _ := openChat(t, cs, w, b, map[string]any{"fleet_tools": true})

	workerSess, err := connectChatMCP(t, w.mcpURL(openWorker.MCPServers[0]), strings.TrimPrefix(openWorker.MCPServers[0].Headers["Authorization"], "Bearer "))
	if err != nil {
		t.Fatal(err)
	}
	defer workerSess.Close()
	cmdrSess, err := connectChatMCP(t, w.mcpURL(openCmdr.MCPServers[0]), strings.TrimPrefix(openCmdr.MCPServers[0].Headers["Authorization"], "Bearer "))
	if err != nil {
		t.Fatal(err)
	}
	defer cmdrSess.Close()

	// Worker asks an open question (NOT delegated).
	go func() {
		_, _ = workerSess.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "ask_user",
			Arguments: map[string]any{
				"question": "Deploy now?",
				"context":  "Testing reply rules",
				"why_now":  "Need go/no-go",
				"options": []map[string]any{
					{"label": "Yes", "consequence": "Deploys"},
					{"label": "No", "consequence": "Waits"},
				},
			},
		})
	}()
	var q store.Question
	waitFor(t, 5*time.Second, "open question", func() bool {
		qs, _ := cs.st.ListQuestions(context.Background(), store.QuestionFilter{Statuses: []string{store.QuestionOpen}})
		if len(qs) == 1 {
			q = qs[0]
			return true
		}
		return false
	})

	// Commander calling reply on a non-delegated question MUST be refused.
	res, err := cmdrSess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "reply",
		Arguments: map[string]any{"target": q.SID, "message": "Yes"},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("expected reply on non-delegated question to be refused, got %+v", res)
	}

	// Clean up open question so the worker goroutine exits cleanly.
	_, _ = cs.api(t, http.MethodPost, "/api/questions/"+q.SID+"/answer", map[string]any{"option_id": q.Options[0].ID})
}
