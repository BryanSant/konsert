package sqlite

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

func setupTicketLeaseFixture(t *testing.T, st *SQLiteStore, optimistic bool) (store.Project, store.Repo) {
	t.Helper()
	ctx := context.Background()
	opt := optimistic
	proj := store.Project{
		Name:      "AutopilotProj",
		Autopilot: true,
		Autonomy:  store.AutonomyPolicy{OptimisticChaining: &opt},
	}
	if err := st.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	projs, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	proj = projs[len(projs)-1]
	if err := st.CreateRepo(ctx, store.Repo{
		ProjectID: proj.ID,
		Name:      "api",
		RemoteURL: "https://github.com/acme/api-" + proj.ID + ".git",
	}); err != nil {
		t.Fatal(err)
	}
	repos, err := st.ListRepos(ctx, proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	return proj, repos[0]
}

func TestClaimTicketConcurrentSingleWinner(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	proj, repo := setupTicketLeaseFixture(t, st, false)

	tk, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Race ticket",
		Status:    store.TicketStatusReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repo.ID}); err != nil {
		t.Fatal(err)
	}

	const goroutines = 32
	now := time.Now().UTC().Truncate(time.Second)
	var (
		wg      sync.WaitGroup
		winners atomic.Int32
		winnerW atomic.Value
	)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		wid := fmt.Sprintf("worker-%02d", i)
		tid := fmt.Sprintf("task-%02d", i)
		go func() {
			defer wg.Done()
			<-start
			claimed, ok, err := st.ClaimTicket(ctx, tk.ID, wid, tid, now, 10*time.Minute)
			if err != nil {
				t.Errorf("ClaimTicket(%s) err = %v", wid, err)
				return
			}
			if ok {
				winners.Add(1)
				winnerW.Store(wid)
				if claimed.Status != store.TicketStatusRunning || claimed.Lease == nil || claimed.Lease.WorkerID != wid || claimed.Lease.TaskID != tid {
					t.Errorf("unexpected claimed ticket: %+v", claimed)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := winners.Load(); got != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", got)
	}
	gotTk, err := st.GetTicket(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTk.Status != store.TicketStatusRunning || gotTk.Lease == nil || gotTk.Lease.WorkerID != winnerW.Load().(string) {
		t.Fatalf("persisted ticket mismatch: %+v", gotTk)
	}
	if !gotTk.Lease.ExpiresAt.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("expires_at = %v, want %v", gotTk.Lease.ExpiresAt, now.Add(10*time.Minute))
	}
}

func TestDispatchableBlockersAndOptimisticChaining(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Project 1: optimistic_chaining = false
	pStrict, rStrict := setupTicketLeaseFixture(t, st, false)
	blocker, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: pStrict.ID, Kind: store.TicketKindFeature, Title: "Blocker", Status: store.TicketStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: pStrict.ID, Kind: store.TicketKindFeature, Title: "Dependent", Status: store.TicketStatusReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Ticket with no repo binding is never dispatchable.
	unbound, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: pStrict.ID, Kind: store.TicketKindFeature, Title: "Unbound", Status: store.TicketStatusReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = unbound
	if err := st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: dependent.ID, RepoID: rStrict.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTicketEdge(ctx, dependent.ID, blocker.ID); err != nil {
		t.Fatal(err)
	}

	// While blocker is running, dependent is not dispatchable.
	list, err := st.ListDispatchableTickets(ctx, pStrict.ID, 10)
	if err != nil || len(list) != 0 {
		t.Fatalf("expected 0 dispatchable while blocker running, got %d (%v)", len(list), err)
	}
	if _, ok, err := st.ClaimTicket(ctx, dependent.ID, "w1", "t1", time.Now().UTC(), 5*time.Minute); err != nil || ok {
		t.Fatalf("blocked ticket must not be claimable: ok=%v err=%v", ok, err)
	}

	// Move blocker to review: still blocked when optimistic_chaining=false.
	blocker.Status = store.TicketStatusReview
	if _, err := st.UpdateTicket(ctx, blocker); err != nil {
		t.Fatal(err)
	}
	list, err = st.ListDispatchableTickets(ctx, pStrict.ID, 10)
	if err != nil || len(list) != 0 {
		t.Fatalf("expected 0 dispatchable while blocker in review (strict), got %d (%v)", len(list), err)
	}

	// Move blocker to done: now dependent is dispatchable and claimable.
	blocker.Status = store.TicketStatusDone
	if _, err := st.UpdateTicket(ctx, blocker); err != nil {
		t.Fatal(err)
	}
	list, err = st.ListDispatchableTickets(ctx, pStrict.ID, 10)
	if err != nil || len(list) != 1 || list[0].ID != dependent.ID {
		t.Fatalf("expected dependent dispatchable once blocker done, got %+v (%v)", list, err)
	}

	// Project 2: optimistic_chaining = true
	pOpt, rOpt := setupTicketLeaseFixture(t, st, true)
	optBlocker, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: pOpt.ID, Kind: store.TicketKindFeature, Title: "OptBlocker", Status: store.TicketStatusReview,
	})
	if err != nil {
		t.Fatal(err)
	}
	optDep, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: pOpt.ID, Kind: store.TicketKindFeature, Title: "OptDependent", Status: store.TicketStatusReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: optDep.ID, RepoID: rOpt.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTicketEdge(ctx, optDep.ID, optBlocker.ID); err != nil {
		t.Fatal(err)
	}
	list, err = st.ListDispatchableTickets(ctx, pOpt.ID, 10)
	if err != nil || len(list) != 1 || list[0].ID != optDep.ID {
		t.Fatalf("expected optimistic chaining to unblock review blocker, got %+v (%v)", list, err)
	}
	if _, ok, err := st.ClaimTicket(ctx, optDep.ID, "w1", "t-opt", time.Now().UTC(), 5*time.Minute); err != nil || !ok {
		t.Fatalf("expected ClaimTicket to succeed under optimistic chaining: ok=%v err=%v", ok, err)
	}
}

func TestListDispatchableOrderAndHolds(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	proj, repo := setupTicketLeaseFixture(t, st, false)

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	mk := func(title, prio string, offset time.Duration) store.Ticket {
		tk, err := st.CreateTicket(ctx, store.Ticket{
			ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: title,
			Priority: prio, Status: store.TicketStatusReady, CreatedAt: base.Add(offset),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repo.ID}); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	tP2Old := mk("P2 old", "P2", 0)
	tP0Late := mk("P0 late", "P0", 2*time.Minute)
	tP0Early := mk("P0 early", "P0", 1*time.Minute)
	tP1 := mk("P1", "P1", 30*time.Second)

	list, err := st.ListDispatchableTickets(ctx, proj.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{tP0Early.ID, tP0Late.ID, tP1.ID, tP2Old.ID}
	if len(list) != len(want) {
		t.Fatalf("len = %d, want %d", len(list), len(want))
	}
	for i, id := range want {
		if list[i].ID != id {
			t.Fatalf("index %d: got %s (%s), want %s", i, list[i].ID, list[i].Title, id)
		}
	}

	// Machine hold blocks ClaimTicket for that worker.
	if _, err := st.CreateHold(ctx, store.Hold{
		ScopeKind: store.HoldScopeMachine, WorkerID: "w-held", Summary: "maintenance",
		Cause: store.Cause{Actor: store.ActorUser, Confidence: store.ConfidenceCertain},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.ClaimTicket(ctx, tP0Early.ID, "w-held", "task-1", base, 5*time.Minute); err != nil || ok {
		t.Fatalf("ClaimTicket must reject held worker: ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.ClaimTicket(ctx, tP0Early.ID, "w-free", "task-1", base, 5*time.Minute); err != nil || !ok {
		t.Fatalf("ClaimTicket should succeed on unheld worker: ok=%v err=%v", ok, err)
	}
}

func TestRenewAndExpireLeases(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	proj, repo := setupTicketLeaseFixture(t, st, false)

	mk := func(title, orphan string) store.Ticket {
		tk, err := st.CreateTicket(ctx, store.Ticket{
			ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: title,
			Status: store.TicketStatusReady, OrphanPolicy: orphan,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repo.ID}); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	tkRequeue := mk("Requeue on expiry", store.TicketOrphanPolicyRequeue)
	tkFail := mk("Fail on expiry", store.TicketOrphanPolicyFail)
	tkRenewed := mk("Renewed", store.TicketOrphanPolicyRequeue)

	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		tk  store.Ticket
		tid string
	}{
		{tkRequeue, "task-req"},
		{tkFail, "task-fail"},
		{tkRenewed, "task-ren"},
	} {
		if _, ok, err := st.ClaimTicket(ctx, tc.tk.ID, "w1", tc.tid, t0, 5*time.Minute); err != nil || !ok {
			t.Fatalf("claim %s: ok=%v err=%v", tc.tk.Title, ok, err)
		}
	}

	// Renew only tkRenewed at t0+4m for another 5m (expires at t0+9m).
	t1 := t0.Add(4 * time.Minute)
	n, err := st.RenewLeases(ctx, "w1", []string{tkRenewed.ID}, t1, 5*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("RenewLeases = %d, %v; want 1", n, err)
	}
	// Wrong worker cannot renew.
	n, err = st.RenewLeases(ctx, "w-other", []string{tkRequeue.ID}, t1, 5*time.Minute)
	if err != nil || n != 0 {
		t.Fatalf("RenewLeases(wrong worker) = %d, %v; want 0", n, err)
	}

	// At t0+6m, tkRequeue and tkFail expire; tkRenewed remains running.
	expired, err := st.ExpireLeases(ctx, t0.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 2 {
		t.Fatalf("expected 2 expired leases, got %d: %+v", len(expired), expired)
	}
	gotReq, _ := st.GetTicket(ctx, tkRequeue.ID)
	if gotReq.Status != store.TicketStatusReady || gotReq.Lease != nil {
		t.Fatalf("requeue-policy ticket should be ready with nil lease: %+v", gotReq)
	}
	gotFail, _ := st.GetTicket(ctx, tkFail.ID)
	if gotFail.Status != store.TicketStatusFailed || gotFail.Lease != nil {
		t.Fatalf("fail-policy ticket should be failed with nil lease: %+v", gotFail)
	}
	gotRen, _ := st.GetTicket(ctx, tkRenewed.ID)
	if gotRen.Status != store.TicketStatusRunning || gotRen.Lease == nil || !gotRen.Lease.ExpiresAt.Equal(t1.Add(5*time.Minute)) {
		t.Fatalf("renewed ticket should still be running with extended lease: %+v", gotRen)
	}
}
