package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/store/sqlite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAutopilotAutoOffOnIdleWithFinalPulseAndAudit(t *testing.T) {
	cs := newChatServer(t)
	cs.h.PulseDebounce = 80 * time.Millisecond
	ctx := context.Background()

	// Connect external MCP commander subscribed to syre://pulse.
	mcpSrv := NewMCPServer(cs.svc, "test")
	cTrans, sTrans := mcp.NewInMemoryTransports()
	if _, err := mcpSrv.Connect(ctx, sTrans, nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var pulses []string
	var sess *mcp.ClientSession
	client := mcp.NewClient(&mcp.Implementation{Name: "autopilot-commander", Version: "1.0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(hctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			if req.Params.URI != PulseResourceURI {
				return
			}
			if res, err := sess.ReadResource(hctx, &mcp.ReadResourceParams{URI: PulseResourceURI}); err == nil && len(res.Contents) > 0 {
				mu.Lock()
				pulses = append(pulses, res.Contents[0].Text)
				mu.Unlock()
			}
		},
	})
	var err error
	sess, err = client.Connect(ctx, cTrans, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Subscribe(ctx, &mcp.SubscribeParams{URI: PulseResourceURI}); err != nil {
		t.Fatal(err)
	}

	proj, repo := createProjectAndRepo(t, cs.st, "AutoFinish", true, false)
	tk, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Single autopilot ticket",
		Status:    store.TicketStatusReady,
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repo.ID}); err != nil {
		t.Fatal(err)
	}

	fw := cs.connectAgentWorker(t, "w-auto", "box-auto", claudeAccount("mock", true))
	cs.h.SchedulePending(ctx)

	openEnv := fw.expect(protocol.MsgAgentChatOpen, 3*time.Second)
	open, _ := protocol.DecodePayload[protocol.AgentChatOpen](openEnv)
	_ = fw.expect(protocol.MsgAgentChatPrompt, 3*time.Second)

	// While running: project is playing, counts show 1 running.
	stRunning, err := cs.h.ProjectAutopilotState(ctx, proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stRunning.Play || stRunning.State != AutopilotStatePlaying || stRunning.Counts.Running != 1 || stRunning.Counts.Dispatchable != 0 {
		t.Fatalf("unexpected running state: %+v", stRunning)
	}

	// Elicitation -> needs_you keeps autopilot ON (needs_you counts as work left).
	fw.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{
		ChatID:      open.ChatID,
		Seq:         1,
		Kind:        protocol.EventElicitation,
		Elicitation: &protocol.AgentElicitation{ID: "e-1", Kind: protocol.ElicitQuestion, Title: "Confirm?"},
	})
	waitFor(t, 3*time.Second, "ticket needs_you", func() bool {
		cur, err := cs.st.GetTicket(ctx, tk.ID)
		return err == nil && cur.Status == store.TicketStatusNeedsYou
	})
	if pNow, _ := cs.st.GetProject(ctx, proj.ID); !pNow.Autopilot {
		t.Fatalf("autopilot must stay on while a ticket is in needs_you")
	}

	// Turn ends -> ticket moves to review (review does NOT count as work left for autopilot).
	fw.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{
		ChatID:     open.ChatID,
		Seq:        2,
		Kind:       protocol.EventTurnEnd,
		StopReason: "end_turn",
		Digest:     &protocol.RunDigest{TicketSID: tk.SID, Status: protocol.DigestStatusReview, Summary: "Done"},
	})
	waitFor(t, 3*time.Second, "ticket reaches review and autopilot turns off", func() bool {
		pNow, err := cs.st.GetProject(ctx, proj.ID)
		return err == nil && !pNow.Autopilot
	})

	stDone, err := cs.h.ProjectAutopilotState(ctx, proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stDone.Play || stDone.State != AutopilotStateFinished || stDone.Counts.Dispatchable != 0 || stDone.Counts.Running != 0 || stDone.Counts.NeedsYou != 0 || stDone.Counts.Review != 1 {
		t.Fatalf("unexpected finished state: %+v", stDone)
	}

	// Final pulse fires once with board_idle and final autopilot off state.
	waitFor(t, 3*time.Second, "final board_idle pulse", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range pulses {
			if strings.Contains(p, "idle") {
				return true
			}
		}
		return false
	})
	time.Sleep(250 * time.Millisecond)

	mu.Lock()
	idlePulses := 0
	var lastIdle string
	for _, p := range pulses {
		if strings.Contains(p, "idle") {
			idlePulses++
			lastIdle = p
		}
	}
	mu.Unlock()
	if idlePulses != 1 {
		t.Fatalf("expected exactly 1 board_idle pulse, got %d (%v)", idlePulses, pulses)
	}
	if !strings.HasPrefix(lastIdle, "[SYRE PULSE | autopilot off |") {
		t.Fatalf("expected final pulse to report autopilot off, got:\n%s", lastIdle)
	}

	// Audit log and fleet events both record "autopilot finished".
	audits, err := cs.st.ListAudit(ctx, store.AuditFilter{Action: AuditAutopilotFinish, Limit: 10})
	if err != nil || len(audits) == 0 {
		t.Fatalf("expected %s audit event, got %+v (err=%v)", AuditAutopilotFinish, audits, err)
	}
	if !strings.Contains(strings.ToLower(audits[0].Message), "autopilot finished") {
		t.Fatalf("unexpected audit message: %q", audits[0].Message)
	}
	events, err := cs.st.ListFleetEvents(ctx, store.FleetEventFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	sawFinishedEvent := false
	for _, ev := range events {
		if ev.Action == "autopilot_finished" && strings.Contains(strings.ToLower(ev.Summary), "autopilot finished") {
			sawFinishedEvent = true
			break
		}
	}
	if !sawFinishedEvent {
		t.Fatalf("expected autopilot_finished fleet event, got %+v", events)
	}
}

func TestAutopilotPrecedenceFleetPauseAndProjectPause(t *testing.T) {
	th := newTestHub(t)
	ctx := withAuditActor(context.Background(), "tester", auditSourceREST)

	// Two projects: A (default off) and B (default off), each with one ready repo-bound ticket.
	projA, repoA := createProjectAndRepo(t, th.st, "Alpha", false, false)
	projB, repoB := createProjectAndRepo(t, th.st, "Beta", false, false)

	mkTicket := func(projID, repoID, title string) store.Ticket {
		tk, err := th.st.CreateTicket(ctx, store.Ticket{
			ProjectID: projID, Kind: store.TicketKindFeature, Title: title,
			Status: store.TicketStatusReady, Hints: store.TicketHints{Agent: "mock"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repoID}); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	tkA := mkTicket(projA.ID, repoA.ID, "Alpha work")
	tkB := mkTicket(projB.ID, repoB.ID, "Beta work")

	fw := th.connectWorker(t, testWorkerInfo("w-1", "box-1"), nil, "w-1")

	// 1. Explicitly pause project B, turn global autopilot ON, and also Pause everything (fleet pause).
	if _, err := th.h.SetProjectAutopilot(ctx, projB.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := th.h.SetGlobalAutopilot(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := th.h.PauseEverything(ctx); err != nil {
		t.Fatal(err)
	}

	// Fleet pause blocks dispatch even though global autopilot is ON.
	th.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgAgentChatOpen, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("fleet pause must block autopilot dispatch, got %d opens", len(got))
	}
	stA, _ := th.h.ProjectAutopilotState(ctx, projA.ID)
	if stA.Play || !stA.FleetPaused || stA.State != AutopilotStatePaused {
		t.Fatalf("expected Alpha paused while fleet paused, got %+v", stA)
	}

	// 2. Resume fleet: Alpha dispatches (global ON, not paused), Beta stays ready (explicitly paused > global).
	if _, err := th.h.ResumeEverything(ctx); err != nil {
		t.Fatal(err)
	}
	openEnv := fw.expect(protocol.MsgAgentChatOpen, 3*time.Second)
	open, _ := protocol.DecodePayload[protocol.AgentChatOpen](openEnv)
	_ = fw.expect(protocol.MsgAgentChatPrompt, 3*time.Second)
	if open.TicketSID != tkA.SID {
		t.Fatalf("expected Alpha ticket %s to dispatch, got %s", tkA.SID, open.TicketSID)
	}
	if extra := fw.drain(protocol.MsgAgentChatOpen, 200*time.Millisecond); len(extra) != 0 {
		t.Fatalf("explicitly paused project Beta must not dispatch when global is on, got %d extra opens", len(extra))
	}
	curB, _ := th.st.GetTicket(ctx, tkB.ID)
	if curB.Status != store.TicketStatusReady {
		t.Fatalf("expected Beta ticket still ready, got %s", curB.Status)
	}
	stB, _ := th.h.ProjectAutopilotState(ctx, projB.ID)
	if stB.Play || !stB.AutopilotPaused || !stB.Global || stB.State != AutopilotStatePaused {
		t.Fatalf("expected Beta explicitly paused under global=true, got %+v", stB)
	}
}

func TestAutopilotRESTMCPAndAuditAttribution(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "autopilot-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	doReq := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		var r *http.Request
		if body != "" {
			r, _ = http.NewRequest(method, ts.URL+path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		} else {
			r, _ = http.NewRequest(method, ts.URL+path, nil)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.UnmarshalRead(resp.Body, &out)
		return resp.StatusCode, out
	}

	// Create project via REST.
	code, proj := doReq(http.MethodPost, "/api/projects", `{"name":"Billing"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/projects = %d %v", code, proj)
	}
	projID := proj["id"].(string)
	projSID := proj["sid"].(string)

	// GET /api/projects/{id}/autopilot (works with SID too).
	code, ap0 := doReq(http.MethodGet, "/api/projects/"+projSID+"/autopilot", "")
	if code != http.StatusOK || ap0["play"] != false || ap0["state"] != AutopilotStatePaused {
		t.Fatalf("GET project autopilot = %d %v", code, ap0)
	}

	// POST /api/projects/{id}/autopilot {"play": true} (user via UI/REST).
	code, ap1 := doReq(http.MethodPost, "/api/projects/"+projID+"/autopilot", `{"play":true}`)
	if code != http.StatusOK || ap1["play"] != true || ap1["state"] != AutopilotStatePlaying {
		t.Fatalf("POST project autopilot play=true = %d %v", code, ap1)
	}

	// Global via PUT /api/settings {"autopilot_global": true}.
	code, setResp := doReq(http.MethodPut, "/api/settings", `{"autopilot_global":true}`)
	if code != http.StatusOK || setResp["autopilot_global"] != true {
		t.Fatalf("PUT /api/settings autopilot_global = %d %v", code, setResp)
	}
	code, fleetAP := doReq(http.MethodGet, "/api/autopilot", "")
	if code != http.StatusOK || fleetAP["global"] != true || fleetAP["state"] != AutopilotStatePlaying {
		t.Fatalf("GET /api/autopilot = %d %v", code, fleetAP)
	}

	// MCP tool: autopilot(project, action: pause|play|status) attributed to commander.
	mcpSrv := NewMCPServer(serverControlPlane{FleetService: srv.Service(), srv: srv, admin: false}, "test")
	cTrans, sTrans := mcp.NewInMemoryTransports()
	if _, err := mcpSrv.Connect(ctx, sTrans, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "commander", Version: "1.0"}, nil)
	sess, err := client.Connect(ctx, cTrans, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Call MCP autopilot(project="Billing", action="pause") with commander context via hub directly too.
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "autopilot",
		Arguments: map[string]any{"project": "Billing", "action": "pause"},
	})
	if err != nil || res.IsError {
		t.Fatalf("MCP autopilot pause failed: err=%v res=%+v", err, res)
	}
	var mcpState AutopilotProjectState
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &mcpState); err != nil {
		t.Fatal(err)
	}
	if mcpState.Play || !mcpState.AutopilotPaused || mcpState.State != AutopilotStatePaused {
		t.Fatalf("unexpected MCP pause result: %+v", mcpState)
	}

	// Commander-attributed call (mirrors per-chat commander MCP context).
	cmdCtx := withAuditActor(ctx, "commander", auditSourceCommander)
	if _, err := srv.Hub().Autopilot(cmdCtx, AutopilotInput{Project: projSID, Action: "play"}); err != nil {
		t.Fatalf("commander autopilot play: %v", err)
	}
	// Global status via MCP.
	resStatus, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "autopilot",
		Arguments: map[string]any{"action": "status"},
	})
	if err != nil || resStatus.IsError {
		t.Fatalf("MCP autopilot status failed: err=%v res=%+v", err, resStatus)
	}
	var globalOut AutopilotState
	if err := json.Unmarshal([]byte(resStatus.Content[0].(*mcp.TextContent).Text), &globalOut); err != nil {
		t.Fatal(err)
	}
	if !globalOut.Global || globalOut.State != AutopilotStatePlaying {
		t.Fatalf("unexpected MCP global status: %+v", globalOut)
	}

	// Verify audit attribution: REST ("rest"), MCP ("mcp"), and commander ("commander").
	audits, err := st.ListAudit(ctx, store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]bool{}
	for _, a := range audits {
		if a.Action == AuditAutopilotPlay || a.Action == AuditAutopilotPause {
			sources[a.Source] = true
		}
	}
	for _, want := range []string{auditSourceREST, auditSourceMCP, auditSourceCommander} {
		if !sources[want] {
			t.Fatalf("expected audit source %q in autopilot audits, got sources=%v audits=%+v", want, sources, audits)
		}
	}
}
