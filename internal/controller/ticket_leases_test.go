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
	if got := fw.drain(protocol.MsgTaskStart, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("ticket in autopilot=false project must not dispatch, got %d task.start", len(got))
	}
	if cur, _ := th.st.GetTicket(ctx, tk.ID); cur.Status != store.TicketStatusReady || cur.Lease != nil {
		t.Fatalf("expected ticket still ready/unleased, got %+v", cur)
	}

	// Enable autopilot on the project -> next scheduler pass dispatches it.
	proj.Autopilot = true
	if err := th.st.UpdateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	th.h.SchedulePending(ctx)

	env := fw.expect(protocol.MsgTaskStart, 3*time.Second)
	p, err := protocol.DecodePayload[protocol.TaskStartPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	if p.Agent != "mock" || p.Model != "mock-fast" || p.Branch != "feat/rotate" {
		t.Fatalf("unexpected TaskStartPayload: %+v", p)
	}
	if !strings.Contains(p.Prompt, "Rotate tokens") || !strings.Contains(p.Prompt, "Invalidate token family on reuse") {
		t.Fatalf("placeholder prompt missing title/acceptance: %q", p.Prompt)
	}

	// Verify ticket is running with lease pointing to the created task.
	leased, err := th.st.GetTicket(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if leased.Status != store.TicketStatusRunning || leased.Lease == nil || leased.Lease.WorkerID != "w-1" || leased.Lease.TaskID != p.TaskID {
		t.Fatalf("unexpected leased ticket state: %+v", leased)
	}
	task, err := th.st.GetTask(ctx, p.TaskID)
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

	env := fw.expect(protocol.MsgTaskStart, 3*time.Second)
	p, _ := protocol.DecodePayload[protocol.TaskStartPayload](env)

	before, _ := th.st.GetTicket(ctx, tkComplete.ID)
	if before.Lease == nil {
		t.Fatalf("expected active lease")
	}

	// Sleep briefly and send heartbeat with active task -> lease must extend.
	time.Sleep(50 * time.Millisecond)
	fw.send(protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{
		ActiveTaskIDs: []string{p.TaskID},
		At:            time.Now().UTC(),
	})
	waitFor(t, 2*time.Second, "lease heartbeat to advance", func() bool {
		cur, err := th.st.GetTicket(ctx, tkComplete.ID)
		return err == nil && cur.Lease != nil && cur.Lease.ExpiresAt.After(before.Lease.ExpiresAt)
	})

	// waiting_input -> ticket needs_you (lease kept).
	fw.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: p.TaskID, Status: protocol.TaskStatusWaitingInput})
	waitFor(t, 2*time.Second, "ticket needs_you", func() bool {
		cur, err := th.st.GetTicket(ctx, tkComplete.ID)
		return err == nil && cur.Status == store.TicketStatusNeedsYou && cur.Lease != nil
	})

	// Back to running -> ticket running (lease kept).
	fw.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: p.TaskID, Status: protocol.TaskStatusRunning})
	waitFor(t, 2*time.Second, "ticket back to running", func() bool {
		cur, err := th.st.GetTicket(ctx, tkComplete.ID)
		return err == nil && cur.Status == store.TicketStatusRunning && cur.Lease != nil
	})

	// completed -> ticket review (lease released).
	fw.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: p.TaskID, Status: protocol.TaskStatusCompleted})
	waitFor(t, 2*time.Second, "ticket review", func() bool {
		cur, err := th.st.GetTicket(ctx, tkComplete.ID)
		return err == nil && cur.Status == store.TicketStatusReview && cur.Lease == nil
	})

	// Failed task -> ticket failed.
	tkFail := mkReady("Fail flow")
	th.h.SchedulePending(ctx)
	env2 := fw.expect(protocol.MsgTaskStart, 3*time.Second)
	p2, _ := protocol.DecodePayload[protocol.TaskStartPayload](env2)
	code := 1
	fw.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: p2.TaskID, Status: protocol.TaskStatusFailed, ExitCode: &code})
	waitFor(t, 2*time.Second, "ticket failed", func() bool {
		cur, err := th.st.GetTicket(ctx, tkFail.ID)
		return err == nil && cur.Status == store.TicketStatusFailed && cur.Lease == nil
	})

	// Canceled task -> ticket canceled.
	tkCancel := mkReady("Cancel flow")
	th.h.SchedulePending(ctx)
	env3 := fw.expect(protocol.MsgTaskStart, 3*time.Second)
	p3, _ := protocol.DecodePayload[protocol.TaskStartPayload](env3)
	fw.send(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: p3.TaskID, Status: protocol.TaskStatusCanceled})
	waitFor(t, 2*time.Second, "ticket canceled", func() bool {
		cur, err := th.st.GetTicket(ctx, tkCancel.ID)
		return err == nil && cur.Status == store.TicketStatusCanceled && cur.Lease == nil
	})
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
	env1 := fw1.expect(protocol.MsgTaskStart, 3*time.Second)
	p1, _ := protocol.DecodePayload[protocol.TaskStartPayload](env1)

	// Connect a second worker, close w-1, then expire the lease past its TTL.
	fw2 := th.connectWorker(t, testWorkerInfo("w-2", "box-2"), nil, "w-2")
	fw1.close()
	waitFor(t, 3*time.Second, "w-1 disconnect", func() bool { return !th.h.IsWorkerConnected("w-1") })

	th.h.expireTicketLeases(ctx, time.Now().UTC().Add(20*time.Second))

	env2 := fw2.expect(protocol.MsgTaskStart, 3*time.Second)
	p2, _ := protocol.DecodePayload[protocol.TaskStartPayload](env2)
	if p2.TaskID == p1.TaskID {
		t.Fatalf("re-dispatched ticket should get a new task id, got %s", p2.TaskID)
	}
	got, _ := th.st.GetTicket(ctx, tk.ID)
	if got.Status != store.TicketStatusRunning || got.Lease == nil || got.Lease.WorkerID != "w-2" || got.Lease.TaskID != p2.TaskID {
		t.Fatalf("expected ticket re-leased on w-2: %+v", got)
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
