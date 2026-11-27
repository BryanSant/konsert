package tracker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/store/sqlite"
)

type env struct {
	st   *sqlite.SQLiteStore
	fake *Fake
	sy   *Syncer
	trk  store.Tracker
	proj store.Project
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "syre.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateProject(ctx, store.Project{Name: "billing"}); err != nil {
		t.Fatal(err)
	}
	projs, _ := st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	trk := store.Tracker{ID: "fake-1", ProjectID: proj.ID, Kind: "fake", Enabled: true}
	if err := st.CreateTracker(ctx, trk); err != nil {
		t.Fatal(err)
	}
	fake := NewFake()
	sy := &Syncer{Store: st, Factories: map[string]Factory{"fake": func(context.Context, store.Tracker) (Tracker, error) { return fake, nil }}}
	return &env{st: st, fake: fake, sy: sy, trk: trk, proj: proj}
}

func (e *env) sync(t *testing.T) {
	t.Helper()
	trk, err := e.st.GetTracker(context.Background(), e.trk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.sy.SyncOne(context.Background(), trk); err != nil {
		t.Fatal(err)
	}
}

func (e *env) ticket(t *testing.T, ext string) store.Ticket {
	t.Helper()
	tk, err := e.st.TicketByExternalRef(context.Background(), e.trk.ID, ext)
	if err != nil {
		t.Fatalf("ticket for %s: %v", ext, err)
	}
	return tk
}

func TestPullCreatesTicketsWithHierarchyAndDAG(t *testing.T) {
	e := newEnv(t)
	e.fake.Put(Item{ExternalID: "K-1", Kind: "epic", Title: "Rotation", Status: "ready"})
	e.fake.Put(Item{ExternalID: "K-2", Title: "API", Status: "ready", Priority: "P1", ParentExternalID: "K-1",
		Acceptance: []store.AcceptanceItem{{Text: "rotates"}}})
	e.fake.Put(Item{ExternalID: "K-3", Title: "Web", Status: "ready", ParentExternalID: "K-1", BlockedBy: []string{"K-2"}})
	e.sync(t)

	epic, api, web := e.ticket(t, "K-1"), e.ticket(t, "K-2"), e.ticket(t, "K-3")
	if epic.Kind != "epic" || api.ParentID != epic.ID || web.ParentID != epic.ID || api.Priority != "P1" || len(api.Acceptance) != 1 {
		t.Fatalf("pulled tickets: epic=%+v api=%+v web=%+v", epic, api, web)
	}
	edges, _ := e.st.ListTicketEdges(context.Background(), web.ID)
	if len(edges) != 1 || edges[0].BlockedByID != api.ID {
		t.Fatalf("web blocked_by = %+v, want api", edges)
	}
	trk, _ := e.st.GetTracker(context.Background(), e.trk.ID)
	if trk.Cursor == "" || trk.LastPullAt.IsZero() || trk.LastError != "" {
		t.Fatalf("tracker after sync: %+v", trk)
	}
	// Nothing changed: the next cycle pushes nothing back.
	pushes := len(e.fake.Pushes)
	e.sync(t)
	if len(e.fake.Pushes) != pushes {
		t.Fatalf("an unchanged cycle pushed %d batches", len(e.fake.Pushes)-pushes)
	}
}

func TestFieldOwnershipOnPull(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.fake.Put(Item{ExternalID: "K-1", Title: "Old title", Status: "ready", Acceptance: []store.AcceptanceItem{{Text: "a"}, {Text: "b"}}})
	e.sync(t)

	// Syre works the ticket and checks an item.
	tk := e.ticket(t, "K-1")
	tk.Status = store.TicketStatusReview
	tk.Acceptance[0].Done = true
	if _, err := e.st.UpdateTicket(ctx, tk); err != nil {
		t.Fatal(err)
	}
	// Meanwhile a human retitles it, adds an item and moves it back to ready in the tracker.
	e.fake.Put(Item{ExternalID: "K-1", Title: "New title", Status: "ready", Acceptance: []store.AcceptanceItem{{Text: "a"}, {Text: "b"}, {Text: "c"}}})
	e.sync(t)

	got := e.ticket(t, "K-1")
	if got.Title != "New title" {
		t.Fatalf("title %q: human-owned fields take the tracker's value", got.Title)
	}
	if got.Status != store.TicketStatusReview {
		t.Fatalf("status %q: Syre owns the status while it works the ticket", got.Status)
	}
	if len(got.Acceptance) != 3 || !got.Acceptance[0].Done || got.Acceptance[1].Done {
		t.Fatalf("acceptance %+v: text from the tracker, done ORed with Syre's", got.Acceptance)
	}
	// Syre's review status (and done flag) reached the tracker in the same cycle.
	it, _ := e.fake.Get("K-1")
	if it.Status != store.TicketStatusReview || !it.Acceptance[0].Done {
		t.Fatalf("pushed item = %+v", it)
	}
}

func TestPushCreatesAndUpdatesSyreTickets(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	epic, err := e.st.CreateTicket(ctx, store.Ticket{ProjectID: e.proj.ID, Kind: store.TicketKindEpic, Title: "Epic", Status: store.TicketStatusReady})
	if err != nil {
		t.Fatal(err)
	}
	tk, err := e.st.CreateTicket(ctx, store.Ticket{ProjectID: e.proj.ID, ParentID: epic.ID, Kind: store.TicketKindFeature, Title: "Work", Status: store.TicketStatusReady})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.CreateRepo(ctx, store.Repo{ProjectID: e.proj.ID, Name: "api", RemoteURL: "https://example.com/api.git"}); err != nil {
		t.Fatal(err)
	}
	repos, _ := e.st.ListRepos(ctx, e.proj.ID)
	if err := e.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repos[0].ID}); err != nil {
		t.Fatal(err)
	}
	e.sync(t)

	refs, _ := e.st.ListExternalRefsByTracker(ctx, e.trk.ID)
	if len(refs) != 2 {
		t.Fatalf("refs after first push = %+v", refs)
	}
	epicExt := e.extOf(t, epic.ID)
	item, _ := e.fake.Get(e.extOf(t, tk.ID))
	if item.Title != "Work" || item.ParentExternalID != epicExt {
		t.Fatalf("pushed child %+v, want parent %s", item, epicExt)
	}

	// Syre leases the ticket: the push carries a claim with the syre authority.
	time.Sleep(10 * time.Millisecond)
	tk, _ = e.st.GetTicket(ctx, tk.ID)
	if _, claimed, err := e.st.ClaimTicket(ctx, tk.ID, "w1", "task-1", time.Now().UTC(), time.Minute); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	e.sync(t)
	item, _ = e.fake.Get(e.extOf(t, tk.ID))
	if item.Claim == nil || item.Claim.Authority != SyreAuthority || item.Status != store.TicketStatusRunning {
		t.Fatalf("pushed leased item = %+v", item)
	}
}

func (e *env) extOf(t *testing.T, ticketID string) string {
	t.Helper()
	refs, _ := e.st.ListExternalRefs(context.Background(), ticketID)
	for _, r := range refs {
		if r.Tracker == e.trk.ID {
			return r.ExternalID
		}
	}
	t.Fatalf("ticket %s has no ref", ticketID)
	return ""
}

func TestDeletedInTrackerCancelsIdleTicket(t *testing.T) {
	e := newEnv(t)
	e.fake.Put(Item{ExternalID: "K-1", Title: "Gone soon", Status: "ready"})
	e.sync(t)
	id := e.ticket(t, "K-1").ID
	e.fake.Delete("K-1")
	e.sync(t)
	tk, _ := e.st.GetTicket(context.Background(), id)
	if tk.Status != store.TicketStatusCanceled {
		t.Fatalf("status %q after the tracker deleted it", tk.Status)
	}
	pushes := len(e.fake.Pushes)
	e.sync(t)
	if len(e.fake.Pushes) != pushes {
		t.Fatal("a ticket the tracker deleted was pushed back")
	}
}

// Live-run regression: Syre marks a ticket done; the tracker echoes Syre's earlier push.
// The echo must not revert done, and done must be pushed. A concurrent human status
// change in the tracker loses to Syre's unpushed change.
func TestSyreChangeSurvivesEchoAndConcurrentTrackerStatus(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.fake.EchoPushes = true
	e.fake.Put(Item{ExternalID: "K-1", Title: "Work", Status: "review"})
	e.sync(t)

	mark := func(status string) {
		t.Helper()
		time.Sleep(5 * time.Millisecond)
		tk := e.ticket(t, "K-1")
		tk.Status = status
		if _, err := e.st.UpdateTicket(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	mark(store.TicketStatusDone)
	e.sync(t) // pushes done; the fake echoes it
	e.sync(t) // pulls the echo
	if got := e.ticket(t, "K-1").Status; got != store.TicketStatusDone {
		t.Fatalf("Syre status after echo = %q, want done", got)
	}
	if it, _ := e.fake.Get("K-1"); it.Status != store.TicketStatusDone {
		t.Fatalf("tracker status = %q, want done pushed", it.Status)
	}

	// Concurrent edits: Syre cancels while a human moves it to ready in the tracker.
	mark(store.TicketStatusCanceled)
	e.fake.Put(Item{ExternalID: "K-1", Title: "Work", Status: "ready"})
	e.sync(t)
	if got := e.ticket(t, "K-1").Status; got != store.TicketStatusCanceled {
		t.Fatalf("Syre status = %q, want its unpushed canceled to win", got)
	}
	if it, _ := e.fake.Get("K-1"); it.Status != store.TicketStatusCanceled {
		t.Fatalf("tracker status = %q, want canceled pushed", it.Status)
	}
}
