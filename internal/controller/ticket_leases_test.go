package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/store/sqlite"
)

func createProjectAndRepo(t *testing.T, st store.Store, name string, autopilot, optimistic bool) (store.Project, store.Repo) {
	t.Helper()
	ctx := context.Background()
	opt := optimistic
	if err := st.CreateProject(ctx, store.Project{
		Name:      name,
		Autopilot: autopilot,
		Autonomy:  store.AutonomyPolicy{OptimisticChaining: &opt},
	}); err != nil {
		t.Fatal(err)
	}
	projs, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := projs[len(projs)-1]
	if err := st.CreateRepo(ctx, store.Repo{
		ProjectID: p.ID,
		Name:      "backend",
		RemoteURL: "https://github.com/acme/" + p.ID + ".git",
	}); err != nil {
		t.Fatal(err)
	}
	repos, err := st.ListRepos(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p, repos[0]
}

func TestSchedulerDispatchesReadyTicketOnlyWhenAutopilotEnabled(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()

	// Project starts with autopilot=false (default).
	proj, repo := createProjectAndRepo(t, th.st, "Billing", false, false)
	tk, err := th.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Rotate tokens",
		Body:      "Rotate refresh tokens every 15m.",
		Acceptance: []store.AcceptanceItem{
			{Text: "Invalidate token family on reuse"},
		},
		Priority: "P1",
		Status:   store.TicketStatusReady,
		Hints: store.TicketHints{
			Agent: "mock",
			Model: "mock-fast",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
		TicketID:     tk.ID,
		RepoID:       repo.ID,
		BaseBranch:   "main",
		TargetBranch: "feat/rotate",
	}); err != nil {
		t.Fatal(err)
	}

	fw := th.connectWorker(t, testWorkerInfo("w-1", "box-1"), nil, "w-1")
	th.h.SchedulePending(ctx)
	if got := fw.drain(protocol.MsgAgentChatOpen, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("ticket in autopilot=false project must not dispatch, got %d agent.chat.open", len(got))
	}
	if cur, _ := th.st.GetTicket(ctx, tk.ID); cur.Status != store.TicketStatusReady || cur.Lease != nil {
		t.Fatalf("expected ticket still ready/unleased, got %+v", cur)
	}

	// Enable autopilot on the project -> next scheduler pass dispatches it as an agent chat.
	proj.Autopilot = true
	if err := th.st.UpdateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	th.h.SchedulePending(ctx)

	openEnv := fw.expect(protocol.MsgAgentChatOpen, 3*time.Second)
	open, err := protocol.DecodePayload[protocol.AgentChatOpen](openEnv)
	if err != nil {
		t.Fatal(err)
	}
	promptEnv := fw.expect(protocol.MsgAgentChatPrompt, 3*time.Second)
	prompt, err := protocol.DecodePayload[protocol.AgentChatPrompt](promptEnv)
	if err != nil {
		t.Fatal(err)
	}
	if open.Agent != "mock" || open.Model != "mock-fast" || open.TicketSID != tk.SID || len(open.Bindings) != 1 || open.Bindings[0].TargetBranch != "feat/rotate" {
		t.Fatalf("unexpected AgentChatOpen: %+v", open)
	}
	if prompt.ChatID != open.ChatID || !strings.Contains(prompt.Text, "Rotate tokens") || !strings.Contains(prompt.Text, "Invalidate token family on reuse") {
		t.Fatalf("dispatch prompt missing title/acceptance: %+v", prompt)
	}

	// Verify ticket is running with lease pointing to the created task and chat session bound to ticket.
	leased, err := th.st.GetTicket(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if leased.Status != store.TicketStatusRunning || leased.Lease == nil || leased.Lease.WorkerID != "w-1" || leased.Lease.TaskID == "" {
		t.Fatalf("unexpected leased ticket state: %+v", leased)
	}
	cs, err := th.st.GetChatSession(ctx, open.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	if cs.TicketID != tk.ID || cs.TaskID != leased.Lease.TaskID || cs.FleetTools {
		t.Fatalf("unexpected chat session linkage: %+v", cs)
	}
	task, err := th.st.GetTask(ctx, leased.Lease.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.TicketID != tk.ID || task.ProjectID != proj.ID || task.RepoID != repo.ID {
		t.Fatalf("unexpected task linkage: %+v", task)
	}
}

func TestTicketHeartbeatRenewalAndStatusTransitions(t *testing.T) {
	th := newTestHub(t)
	th.h.TicketLeaseTTL = 30 * time.Second
	ctx := context.Background()

	proj, repo := createProjectAndRepo(t, th.st, "Payments", true, false)
	mkReady := func(title string) store.Ticket {
		tk, err := th.st.CreateTicket(ctx, store.Ticket{
			ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: title,
			Status: store.TicketStatusReady, Hints: store.TicketHints{Agent: "mock"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repo.ID}); err != nil {
			t.Fatal(err)
		}
		return tk
	}

	tkComplete := mkReady("Complete flow")
	fw := th.connectWorker(t, testWorkerInfo("w-1", "box-1"), nil, "w-1")

	openEnv := fw.expect(protocol.MsgAgentChatOpen, 3*time.Second)
	open1, _ := protocol.DecodePayload[protocol.AgentChatOpen](openEnv)
	_ = fw.expect(protocol.MsgAgentChatPrompt, 3*time.Second)

	before, _ := th.st.GetTicket(ctx, tkComplete.ID)
	if before.Lease == nil {
		t.Fatalf("expected active lease")
	}

	// Sleep briefly and send heartbeat with active chat -> lease must extend.
	time.Sleep(50 * time.Millisecond)
	fw.send(protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{
		ActiveChatIDs: []string{open1.ChatID},
		At:            time.Now().UTC(),
	})
	waitFor(t, 2*time.Second, "lease heartbeat to advance", func() bool {
		cur, err := th.st.GetTicket(ctx, tkComplete.ID)
		return err == nil && cur.Lease != nil && cur.Lease.ExpiresAt.After(before.Lease.ExpiresAt)
	})

	// elicitation -> ticket needs_you (lease kept).
	fw.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{
		ChatID:      open1.ChatID,
		Seq:         1,
		Kind:        protocol.EventElicitation,
		Elicitation: &protocol.AgentElicitation{ID: "e-1", Kind: protocol.ElicitQuestion, Title: "Pick color?"},
	})
	waitFor(t, 2*time.Second, "ticket needs_you", func() bool {
		cur, err := th.st.GetTicket(ctx, tkComplete.ID)
		return err == nil && cur.Status == store.TicketStatusNeedsYou && cur.Lease != nil
	})

	// elicitation_resolved -> ticket back to running (lease kept).
	fw.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{
		ChatID:      open1.ChatID,
		Seq:         2,
		Kind:        protocol.EventElicitationOK,
		Elicitation: &protocol.AgentElicitation{ID: "e-1"},
	})
	waitFor(t, 2*time.Second, "ticket back to running", func() bool {
		cur, err := th.st.GetTicket(ctx, tkComplete.ID)
		return err == nil && cur.Status == store.TicketStatusRunning && cur.Lease != nil
	})

	// turn_end (ok) -> ticket review (lease released, chat stays open).
	fw.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{
		ChatID:     open1.ChatID,
		Seq:        3,
		Kind:       protocol.EventTurnEnd,
		StopReason: "end_turn",
		Digest: &protocol.RunDigest{
			TicketSID: tkComplete.SID,
			Status:    protocol.DigestStatusReview,
			Summary:   "Done",
		},
	})
	waitFor(t, 2*time.Second, "ticket review", func() bool {
		cur, err := th.st.GetTicket(ctx, tkComplete.ID)
		return err == nil && cur.Status == store.TicketStatusReview && cur.Lease == nil
	})

	// Engine error -> ticket failed + chat closed.
	// Wait for auto-off on idle after tkComplete reached review, then re-enable Play.
	waitFor(t, 2*time.Second, "autopilot auto-off after tkComplete", func() bool {
		p, err := th.st.GetProject(ctx, proj.ID)
		return err == nil && !p.Autopilot
	})
	if _, err := th.h.SetProjectAutopilot(ctx, proj.ID, true); err != nil {
		t.Fatal(err)
	}
	tkFail := mkReady("Fail flow")
	th.h.SchedulePending(ctx)
	openEnv2 := fw.expect(protocol.MsgAgentChatOpen, 3*time.Second)
	open2, _ := protocol.DecodePayload[protocol.AgentChatOpen](openEnv2)
	_ = fw.expect(protocol.MsgAgentChatPrompt, 3*time.Second)
	fw.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{
		ChatID: open2.ChatID,
		Seq:    1,
		Kind:   protocol.EventError,
		Error:  &protocol.AgentChatError{Kind: protocol.ErrKindAuth, Message: "token expired"},
	})
	waitFor(t, 2*time.Second, "ticket failed", func() bool {
		cur, err := th.st.GetTicket(ctx, tkFail.ID)
		return err == nil && cur.Status == store.TicketStatusFailed && cur.Lease == nil
	})
	_ = fw.expect(protocol.MsgAgentChatClose, 3*time.Second)

	// Cancelled turn -> ticket canceled + chat closed.
	waitFor(t, 2*time.Second, "autopilot auto-off after tkFail", func() bool {
		p, err := th.st.GetProject(ctx, proj.ID)
		return err == nil && !p.Autopilot
	})
	if _, err := th.h.SetProjectAutopilot(ctx, proj.ID, true); err != nil {
		t.Fatal(err)
	}
	tkCancel := mkReady("Cancel flow")
	th.h.SchedulePending(ctx)
	openEnv3 := fw.expect(protocol.MsgAgentChatOpen, 3*time.Second)
	open3, _ := protocol.DecodePayload[protocol.AgentChatOpen](openEnv3)
	_ = fw.expect(protocol.MsgAgentChatPrompt, 3*time.Second)
	fw.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{
		ChatID:     open3.ChatID,
		Seq:        1,
		Kind:       protocol.EventTurnEnd,
		StopReason: "cancelled",
	})
	waitFor(t, 2*time.Second, "ticket canceled", func() bool {
		cur, err := th.st.GetTicket(ctx, tkCancel.ID)
		return err == nil && cur.Status == store.TicketStatusCanceled && cur.Lease == nil
	})
	_ = fw.expect(protocol.MsgAgentChatClose, 3*time.Second)
}

func TestTicketLeaseExpiryRequeuesAndRedispaches(t *testing.T) {
	th := newTestHub(t)
	th.h.TicketLeaseTTL = 10 * time.Second
	ctx := context.Background()

	proj, repo := createProjectAndRepo(t, th.st, "ExpiryProj", true, false)
	tk, err := th.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "Recoverable",
		Status: store.TicketStatusReady, OrphanPolicy: store.TicketOrphanPolicyRequeue,
		Hints: store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repo.ID}); err != nil {
		t.Fatal(err)
	}

	fw1 := th.connectWorker(t, testWorkerInfo("w-1", "box-1"), nil, "w-1")
	env1 := fw1.expect(protocol.MsgAgentChatOpen, 3*time.Second)
	open1, _ := protocol.DecodePayload[protocol.AgentChatOpen](env1)
	_ = fw1.expect(protocol.MsgAgentChatPrompt, 3*time.Second)
	firstLeased, _ := th.st.GetTicket(ctx, tk.ID)

	// Connect a second worker, close w-1, then expire the lease past its TTL.
	fw2 := th.connectWorker(t, testWorkerInfo("w-2", "box-2"), nil, "w-2")
	fw1.close()
	waitFor(t, 3*time.Second, "w-1 disconnect", func() bool { return !th.h.IsWorkerConnected("w-1") })

	th.h.expireTicketLeases(ctx, time.Now().UTC().Add(20*time.Second))

	env2 := fw2.expect(protocol.MsgAgentChatOpen, 3*time.Second)
	open2, _ := protocol.DecodePayload[protocol.AgentChatOpen](env2)
	_ = fw2.expect(protocol.MsgAgentChatPrompt, 3*time.Second)
	if open2.ChatID == open1.ChatID {
		t.Fatalf("re-dispatched ticket should get a new chat id, got %s", open2.ChatID)
	}
	got, _ := th.st.GetTicket(ctx, tk.ID)
	if got.Status != store.TicketStatusRunning || got.Lease == nil || got.Lease.WorkerID != "w-2" || got.Lease.TaskID == firstLeased.Lease.TaskID {
		t.Fatalf("expected ticket re-leased on w-2 with new task: %+v", got)
	}
}

func TestTicketLeaseBreakRESTAndAutopilotPatch(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "lease-rest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req := func(method, path, body string) (int, map[string]any) {
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

	// Create project (autopilot defaults to false) and toggle via PATCH /api/projects/{id}.
	code, proj := req(http.MethodPost, "/api/projects", `{"name":"WebApp"}`)
	if code != http.StatusCreated || proj["autopilot"] != false {
		t.Fatalf("POST /api/projects = %d %v", code, proj)
	}
	projID := proj["id"].(string)

	code, patched := req(http.MethodPatch, "/api/projects/"+projID, `{"autopilot":true}`)
	if code != http.StatusOK || patched["autopilot"] != true {
		t.Fatalf("PATCH /api/projects autopilot = %d %v", code, patched)
	}

	// Settings: update ticket_lease_ttl_seconds.
	code, settingsResp := req(http.MethodPut, "/api/settings", `{"ticket_lease_ttl_seconds":120}`)
	if code != http.StatusOK || int(settingsResp["ticket_lease_ttl_seconds"].(float64)) != 120 {
		t.Fatalf("PUT /api/settings = %d %v", code, settingsResp)
	}

	// Create repo & ticket, claim it, then break lease via /requeue and /cancel.
	code, repo := req(http.MethodPost, "/api/projects/"+projID+"/repos", `{"name":"app","remote_url":"https://github.com/acme/app.git"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST repo = %d %v", code, repo)
	}
	repoID := repo["id"].(string)

	code, tk := req(http.MethodPost, "/api/tickets", `{"project_id":"`+projID+`","kind":"feature","title":"Break lease","status":"ready"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST ticket = %d %v", code, tk)
	}
	tkID := tk["id"].(string)
	code, _ = req(http.MethodPut, "/api/tickets/"+tkID+"/repos/"+repoID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("PUT binding = %d", code)
	}

	if _, ok, err := st.ClaimTicket(ctx, tkID, "w-offline", "", time.Now().UTC(), 5*time.Minute); err != nil || !ok {
		t.Fatalf("ClaimTicket: ok=%v err=%v", ok, err)
	}

	// POST /api/tickets/{id}/requeue breaks lease and audits.
	code, reqOut := req(http.MethodPost, "/api/tickets/"+tkID+"/requeue", `{"note":"worker stuck"}`)
	if code != http.StatusOK || reqOut["status"] != store.TicketStatusReady || reqOut["lease"] != nil {
		t.Fatalf("POST requeue = %d %v", code, reqOut)
	}

	// Claim again and POST /api/tickets/{id}/cancel.
	if _, ok, err := st.ClaimTicket(ctx, tkID, "w-offline", "", time.Now().UTC(), 5*time.Minute); err != nil || !ok {
		t.Fatalf("ClaimTicket 2: ok=%v err=%v", ok, err)
	}
	code, canOut := req(http.MethodPost, "/api/tickets/"+tkID+"/cancel", `{"note":"no longer needed"}`)
	if code != http.StatusOK || canOut["status"] != store.TicketStatusCanceled || canOut["lease"] != nil {
		t.Fatalf("POST cancel = %d %v", code, canOut)
	}

	// Audit log contains both ticket.requeue and ticket.cancel.
	audits, err := st.ListAudit(ctx, store.AuditFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, a := range audits {
		seen[a.Action] = true
	}
	if !seen[AuditTicketRequeue] || !seen[AuditTicketCancel] {
		t.Fatalf("expected %s and %s in audit log, got %+v", AuditTicketRequeue, AuditTicketCancel, audits)
	}
}
