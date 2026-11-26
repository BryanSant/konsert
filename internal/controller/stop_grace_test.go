package controller

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func decodeCancelPayload(t *testing.T, env protocol.Envelope) protocol.TaskCancelPayload {
	t.Helper()
	p, err := protocol.DecodePayload[protocol.TaskCancelPayload](env)
	if err != nil {
		t.Fatalf("decode task.cancel: %v", err)
	}
	return p
}

func decodeChatClosePayload(t *testing.T, env protocol.Envelope) protocol.AgentChatClose {
	t.Helper()
	p, err := protocol.DecodePayload[protocol.AgentChatClose](env)
	if err != nil {
		t.Fatalf("decode agent.chat.close: %v", err)
	}
	return p
}

func connectStopWorker(t *testing.T, th *testHub, id, name string) *fakeWorker {
	t.Helper()
	return th.connectWorker(t, protocol.WorkerInfo{
		ID:            id,
		Name:          name,
		OS:            "linux",
		Arch:          "amd64",
		Agents:        []string{"mock", "claude"},
		MaxConcurrent: 4,
	}, nil, id)
}

func TestControlStopScopesWithGrace(t *testing.T) {
	ctx := userCtx()

	t.Run("session_task_and_chat", func(t *testing.T) {
		th := newTestHub(t)
		w := connectStopWorker(t, th, "w-sess", "box-sess")
		defer w.close()

		// Running PTY task.
		task, err := th.svc.DispatchTask(ctx, DispatchRequest{
			Agent:    "mock",
			RepoPath: t.TempDir(),
			Prompt:   "run",
			Mode:     protocol.TaskModeOneshot,
		})
		if err != nil {
			t.Fatal(err)
		}
		w.expect(protocol.MsgTaskStart, 3*time.Second)

		if _, err := th.svc.Control(ctx, ControlInput{
			Scope:  "session",
			Target: task.TaskSID,
			Action: "stop",
			GraceS: 25,
		}, nil); err != nil {
			t.Fatalf("control session task stop: %v", err)
		}
		p := decodeCancelPayload(t, w.expect(protocol.MsgTaskCancel, 3*time.Second))
		if p.TaskID != task.TaskID || p.GraceS != 25 {
			t.Fatalf("task.cancel = %+v, want task=%s grace=25", p, task.TaskID)
		}

		// Busy agent chat.
		cs := store.ChatSession{
			ID:       "chat-sess-1",
			SID:      "c-s0001",
			Title:    "Agent chat",
			Backend:  store.ChatBackendAgent,
			WorkerID: "w-sess",
			Agent:    "claude",
			Status:   protocol.ChatStatusWorking,
		}
		if err := th.st.CreateChatSession(ctx, cs); err != nil {
			t.Fatal(err)
		}
		if _, err := th.svc.Control(ctx, ControlInput{
			Scope:  "session",
			Target: cs.ID,
			Action: "stop",
			GraceS: 15,
		}, nil); err != nil {
			t.Fatalf("control session chat stop: %v", err)
		}
		cp := decodeChatClosePayload(t, w.expect(protocol.MsgAgentChatClose, 3*time.Second))
		if cp.ChatID != cs.ID || !cp.CancelTurn || cp.GraceS != 15 {
			t.Fatalf("agent.chat.close = %+v, want chat=%s cancel_turn=true grace=15", cp, cs.ID)
		}

		audits, _ := th.st.ListAudit(ctx, store.AuditFilter{Limit: 20})
		foundTaskAudit, foundChatAudit := false, false
		for _, a := range audits {
			if a.Action == AuditCancel && strings.Contains(a.MetaJSON, `"grace_s":25`) {
				foundTaskAudit = true
			}
			if a.Action == AuditStopChat && strings.Contains(a.MetaJSON, `"grace_s":15`) {
				foundChatAudit = true
			}
		}
		if !foundTaskAudit || !foundChatAudit {
			t.Fatalf("expected audit rows with grace_s: task=%v chat=%v audits=%+v", foundTaskAudit, foundChatAudit, audits)
		}
	})

	t.Run("ticket_scope_stops_chat_and_task", func(t *testing.T) {
		th := newTestHub(t)
		w := connectStopWorker(t, th, "w-tk", "box-tk")
		defer w.close()

		if err := th.st.CreateRepo(ctx, store.Repo{
			ProjectID: store.DefaultProjectID,
			Name:      "app",
			RemoteURL: "https://example.com/app.git",
		}); err != nil {
			t.Fatal(err)
		}
		repos, _ := th.st.ListRepos(ctx, store.DefaultProjectID)
		tk, err := th.st.CreateTicket(ctx, store.Ticket{
			ProjectID: store.DefaultProjectID,
			Title:     "Stop my ticket",
			Status:    store.TicketStatusReady,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
			TicketID: tk.ID,
			RepoID:   repos[0].ID,
		}); err != nil {
			t.Fatal(err)
		}
		task, err := th.svc.DispatchTask(ctx, DispatchRequest{
			Agent:    "mock",
			RepoPath: t.TempDir(),
			Prompt:   "ticket work",
			Mode:     protocol.TaskModeOneshot,
		})
		if err != nil {
			t.Fatal(err)
		}
		w.expect(protocol.MsgTaskStart, 3*time.Second)

		claimedTk, claimed, err := th.st.ClaimTicket(ctx, tk.ID, "w-tk", task.TaskID, time.Now().UTC(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if !claimed || claimedTk.Lease == nil {
			t.Fatalf("expected ticket %s to be claimed, got claimed=%v tk=%+v", tk.SID, claimed, claimedTk)
		}
		cs := store.ChatSession{
			ID:       "chat-tk-1",
			SID:      "ch-tk001",
			Title:    "Ticket chat",
			Backend:  store.ChatBackendAgent,
			WorkerID: "w-tk",
			TicketID: tk.ID,
			Agent:    "claude",
			Status:   protocol.ChatStatusWorking,
		}
		if err := th.st.CreateChatSession(ctx, cs); err != nil {
			t.Fatal(err)
		}

		if _, err := th.svc.Control(ctx, ControlInput{
			Scope:  "ticket",
			Target: tk.SID,
			Action: "stop",
			Reason: "no longer needed",
			GraceS: 20,
		}, nil); err != nil {
			t.Fatalf("control ticket stop: %v", err)
		}

		cp := decodeChatClosePayload(t, w.expect(protocol.MsgAgentChatClose, 3*time.Second))
		if cp.ChatID != cs.ID || !cp.CancelTurn || cp.GraceS != 20 {
			t.Fatalf("ticket chat close = %+v, want chat=%s cancel_turn=true grace=20", cp, cs.ID)
		}
		tp := decodeCancelPayload(t, w.expect(protocol.MsgTaskCancel, 3*time.Second))
		if tp.TaskID != task.TaskID || tp.GraceS != 20 {
			t.Fatalf("ticket task cancel = %+v, want task=%s grace=20", tp, task.TaskID)
		}
		gotTk, err := th.st.GetTicket(ctx, tk.ID)
		if err != nil || gotTk.Status != store.TicketStatusCanceled {
			t.Fatalf("ticket status = %q (err=%v), want canceled", gotTk.Status, err)
		}
	})

	t.Run("machine_scope_stops_all_managed_sessions", func(t *testing.T) {
		th := newTestHub(t)
		w := connectStopWorker(t, th, "w-mach", "box-mach")
		defer w.close()

		task, err := th.svc.DispatchTask(ctx, DispatchRequest{
			Agent:    "mock",
			RepoPath: t.TempDir(),
			Prompt:   "machine work",
			Mode:     protocol.TaskModeOneshot,
		})
		if err != nil {
			t.Fatal(err)
		}
		w.expect(protocol.MsgTaskStart, 3*time.Second)

		cs := store.ChatSession{
			ID:       "chat-mach-1",
			SID:      "ch-m0001",
			Title:    "Machine chat",
			Backend:  store.ChatBackendAgent,
			WorkerID: "w-mach",
			Agent:    "claude",
			Status:   protocol.ChatStatusWorking,
		}
		if err := th.st.CreateChatSession(ctx, cs); err != nil {
			t.Fatal(err)
		}

		res, err := th.svc.Control(ctx, ControlInput{
			Scope:  "machine",
			Target: "box-mach",
			Action: "stop",
			GraceS: 30,
		}, nil)
		if err != nil {
			t.Fatalf("control machine stop: %v", err)
		}
		wr := res.(WorkerControlResult)
		if wr.Affected != 2 || wr.GraceS != 30 {
			t.Fatalf("machine stop result = %+v, want affected=2 grace_s=30", wr)
		}
		tp := decodeCancelPayload(t, w.expect(protocol.MsgTaskCancel, 3*time.Second))
		if tp.TaskID != task.TaskID || tp.GraceS != 30 {
			t.Fatalf("machine task cancel = %+v", tp)
		}
		cp := decodeChatClosePayload(t, w.expect(protocol.MsgAgentChatClose, 3*time.Second))
		if cp.ChatID != cs.ID || !cp.CancelTurn || cp.GraceS != 30 {
			t.Fatalf("machine chat close = %+v", cp)
		}
	})

	t.Run("project_scope_stops_only_target_project", func(t *testing.T) {
		th := newTestHub(t)
		w := connectStopWorker(t, th, "w-proj", "box-proj")
		defer w.close()

		if err := th.st.CreateProject(ctx, store.Project{Name: "Alpha"}); err != nil {
			t.Fatal(err)
		}
		if err := th.st.CreateProject(ctx, store.Project{Name: "Beta"}); err != nil {
			t.Fatal(err)
		}
		projs, _ := th.st.ListProjects(ctx)
		var alpha, beta store.Project
		for _, p := range projs {
			if p.Name == "Alpha" {
				alpha = p
			} else if p.Name == "Beta" {
				beta = p
			}
		}

		tAlpha, err := th.svc.DispatchTask(ctx, DispatchRequest{
			ProjectID: alpha.ID,
			Agent:     "mock",
			RepoPath:  t.TempDir(),
			Prompt:    "alpha work",
			Mode:      protocol.TaskModeOneshot,
		})
		if err != nil {
			t.Fatal(err)
		}
		w.expect(protocol.MsgTaskStart, 3*time.Second)

		tBeta, err := th.svc.DispatchTask(ctx, DispatchRequest{
			ProjectID: beta.ID,
			Agent:     "mock",
			RepoPath:  t.TempDir(),
			Prompt:    "beta work",
			Mode:      protocol.TaskModeOneshot,
		})
		if err != nil {
			t.Fatal(err)
		}
		w.expect(protocol.MsgTaskStart, 3*time.Second)

		tkAlpha, err := th.st.CreateTicket(ctx, store.Ticket{ProjectID: alpha.ID, Title: "Alpha tk", Status: store.TicketStatusRunning})
		if err != nil {
			t.Fatal(err)
		}
		chatAlpha := store.ChatSession{
			ID:       "chat-alpha-1",
			SID:      "ch-a0001",
			Title:    "Alpha chat",
			Backend:  store.ChatBackendAgent,
			WorkerID: "w-proj",
			TicketID: tkAlpha.ID,
			Agent:    "claude",
			Status:   protocol.ChatStatusWorking,
		}
		if err := th.st.CreateChatSession(ctx, chatAlpha); err != nil {
			t.Fatal(err)
		}

		res, err := th.svc.Control(ctx, ControlInput{
			Scope:  "project",
			Target: "Alpha",
			Action: "stop",
			GraceS: 45,
		}, nil)
		if err != nil {
			t.Fatalf("control project stop: %v", err)
		}
		pr := res.(ProjectStopResult)
		if pr.ProjectID != alpha.ID || pr.Affected != 2 || pr.GraceS != 45 {
			t.Fatalf("project stop result = %+v, want affected=2 grace_s=45", pr)
		}

		tp := decodeCancelPayload(t, w.expect(protocol.MsgTaskCancel, 3*time.Second))
		if tp.TaskID != tAlpha.TaskID || tp.GraceS != 45 {
			t.Fatalf("project task cancel = %+v, want alpha task %s grace 45", tp, tAlpha.TaskID)
		}
		cp := decodeChatClosePayload(t, w.expect(protocol.MsgAgentChatClose, 3*time.Second))
		if cp.ChatID != chatAlpha.ID || !cp.CancelTurn || cp.GraceS != 45 {
			t.Fatalf("project chat close = %+v", cp)
		}

		// Beta task must remain untouched.
		gotBeta, _ := th.st.GetTask(ctx, tBeta.TaskID)
		if protocol.IsTerminalTaskStatus(gotBeta.Status) {
			t.Fatalf("beta task should not have been stopped, status=%s", gotBeta.Status)
		}
	})

	t.Run("fleet_scope_stops_everything_clamps_grace_and_pauses_dispatch", func(t *testing.T) {
		th := newTestHub(t)
		w := connectStopWorker(t, th, "w-fl", "box-fl")
		defer w.close()

		task, err := th.svc.DispatchTask(ctx, DispatchRequest{
			Agent:    "mock",
			RepoPath: t.TempDir(),
			Prompt:   "fleet work",
			Mode:     protocol.TaskModeOneshot,
		})
		if err != nil {
			t.Fatal(err)
		}
		w.expect(protocol.MsgTaskStart, 3*time.Second)

		cs := store.ChatSession{
			ID:       "chat-fl-1",
			SID:      "ch-f0001",
			Title:    "Fleet chat",
			Backend:  store.ChatBackendAgent,
			WorkerID: "w-fl",
			Agent:    "claude",
			Status:   protocol.ChatStatusWorking,
		}
		if err := th.st.CreateChatSession(ctx, cs); err != nil {
			t.Fatal(err)
		}

		// Request grace_s=600 -> clamped to 300.
		res, err := th.svc.Control(ctx, ControlInput{
			Scope:  "fleet",
			Action: "stop",
			GraceS: 600,
		}, nil)
		if err != nil {
			t.Fatalf("control fleet stop: %v", err)
		}
		fp := res.(FleetPauseState)
		if !fp.Paused || fp.Affected != 2 || fp.GraceS != protocol.MaxStopGraceS {
			t.Fatalf("fleet stop = %+v, want paused=true affected=2 grace_s=300", fp)
		}
		tp := decodeCancelPayload(t, w.expect(protocol.MsgTaskCancel, 3*time.Second))
		if tp.TaskID != task.TaskID || tp.GraceS != protocol.MaxStopGraceS {
			t.Fatalf("fleet task cancel = %+v, want grace=300", tp)
		}
		cp := decodeChatClosePayload(t, w.expect(protocol.MsgAgentChatClose, 3*time.Second))
		if cp.ChatID != cs.ID || !cp.CancelTurn || cp.GraceS != protocol.MaxStopGraceS {
			t.Fatalf("fleet chat close = %+v, want grace=300", cp)
		}

		// Dispatch must now be paused.
		if !th.h.fleetPaused(ctx) {
			t.Fatal("expected fleetPaused to be true after fleet stop")
		}

		// Fleet events + audit recorded.
		events, _ := th.st.ListFleetEvents(ctx, store.FleetEventFilter{Limit: 20})
		foundFleetEvent := false
		for _, ev := range events {
			if ev.Action == "stop_fleet" {
				foundFleetEvent = true
			}
		}
		if !foundFleetEvent {
			t.Fatalf("expected stop_fleet fleet event, got %+v", events)
		}
		audits, _ := th.st.ListAudit(ctx, store.AuditFilter{Limit: 20})
		foundAudit := false
		for _, a := range audits {
			if a.Action == AuditStopFleet && strings.Contains(a.MetaJSON, `"grace_s":300`) {
				foundAudit = true
			}
		}
		if !foundAudit {
			t.Fatalf("expected fleet.stop audit with grace_s=300, got %+v", audits)
		}
	})

	t.Run("rest_endpoints_accept_grace_s", func(t *testing.T) {
		cs := newChatServer(t)
		code, raw := cs.api(t, http.MethodPost, "/api/fleet/stop", map[string]any{"grace_s": 0})
		if code != http.StatusOK {
			t.Fatalf("POST /api/fleet/stop = %d: %s", code, raw)
		}
		var fp FleetPauseState
		if err := json.Unmarshal(raw, &fp); err != nil {
			t.Fatal(err)
		}
		if !fp.Paused || fp.GraceS != protocol.DefaultStopGraceS {
			t.Fatalf("POST /api/fleet/stop resp = %+v, want paused=true grace_s=10", fp)
		}
	})
}
