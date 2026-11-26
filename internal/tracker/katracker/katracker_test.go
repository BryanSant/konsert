package katracker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/KanArtist/ka/pkg/kaservice"

	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/store/sqlite"
	"github.com/BryanSant/syre/internal/tracker"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// newStoreRemote creates a bare git remote holding a fresh ka store with project syre.
func newStoreRemote(t *testing.T) string {
	t.Helper()
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.com",
		"GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.com",
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
	} {
		t.Setenv(k, v)
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", remote)
	seed := filepath.Join(root, "seed")
	if _, err := kaservice.InitStore(context.Background(), kaservice.InitOptions{Dir: seed, Name: "test", Project: "syre", Prefix: "SY"}); err != nil {
		t.Fatal(err)
	}
	// Initiatives and epics need clients that understand them (ka 1.3.0+).
	f, err := os.OpenFile(filepath.Join(seed, ".ka", "store.yml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("requires_ka: 1.3.0\n")
	f.Close()
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "init")
	git(t, seed, "remote", "add", "origin", remote)
	git(t, seed, "push", "-q", "-u", "origin", "main")
	return remote
}

// human opens another clone of the store, as a person using ka elsewhere.
func human(t *testing.T, remote string) *kaservice.Service {
	t.Helper()
	tr, err := Open(context.Background(), Config{StoreURL: remote, Project: "syre"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return tr.svc
}

func TestKaTrackerRoundTrip(t *testing.T) {
	ctx := context.Background()
	remote := newStoreRemote(t)
	bob := kaservice.Caller{Actor: "bob"}

	// A human files an epic with two tickets, the second blocked by the first.
	h := human(t, remote)
	epic, err := h.Create(ctx, bob, []kaservice.CreateIn{{Project: "syre", Title: "Calc fixes", Kind: "epic", Status: "ready"}})
	if err != nil {
		t.Fatal(err)
	}
	epicID := epic.Created[0].ID
	a, err := h.Create(ctx, bob, []kaservice.CreateIn{{Project: "syre", Title: "Fix Add", Kind: "bug", Status: "ready", Priority: "P1", Parent: epicID, Acceptance: []string{"Add returns a+b"}}})
	if err != nil {
		t.Fatal(err)
	}
	aID := a.Created[0].ID
	b, err := h.Create(ctx, bob, []kaservice.CreateIn{{Project: "syre", Title: "Add Sub", Status: "ready", Parent: epicID, BlockedBy: []string{aID}}})
	if err != nil {
		t.Fatal(err)
	}
	bID := b.Created[0].ID

	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "syre.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateProject(ctx, store.Project{Name: "calc"}); err != nil {
		t.Fatal(err)
	}
	projs, _ := st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	trk := store.Tracker{ID: "ka-test", ProjectID: proj.ID, Kind: Kind, Enabled: true,
		ConfigJSON: `{"store_url":"` + remote + `","project":"syre","clone_dir":"` + filepath.Join(t.TempDir(), "syre-clone") + `"}`}
	if err := st.CreateTracker(ctx, trk); err != nil {
		t.Fatal(err)
	}
	sy := &tracker.Syncer{Store: st, Factories: map[string]tracker.Factory{Kind: Factory}}
	syncOnce := func() {
		t.Helper()
		cur, _ := st.GetTracker(ctx, trk.ID)
		if err := sy.SyncOne(ctx, cur); err != nil {
			t.Fatal(err)
		}
	}
	syncOnce()

	tkA, err := st.TicketByExternalRef(ctx, trk.ID, aID)
	if err != nil {
		t.Fatal(err)
	}
	tkB, _ := st.TicketByExternalRef(ctx, trk.ID, bID)
	tkEpic, _ := st.TicketByExternalRef(ctx, trk.ID, epicID)
	if tkA.Title != "Fix Add" || tkA.Kind != "bug" || tkA.Status != store.TicketStatusReady || tkA.ParentID != tkEpic.ID || len(tkA.Acceptance) != 1 {
		t.Fatalf("pulled A = %+v", tkA)
	}
	if edges, _ := st.ListTicketEdges(ctx, tkB.ID); len(edges) != 1 || edges[0].BlockedByID != tkA.ID {
		t.Fatalf("B's blockers = %+v", edges)
	}

	// Syre leases A: ka shows it in progress with a read-only syre claim.
	if err := st.CreateRepo(ctx, store.Repo{ProjectID: proj.ID, Name: "demo", RemoteURL: "https://example.com/demo.git"}); err != nil {
		t.Fatal(err)
	}
	repos, _ := st.ListRepos(ctx, proj.ID)
	_ = st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tkA.ID, RepoID: repos[0].ID})
	time.Sleep(10 * time.Millisecond)
	if _, ok, err := st.ClaimTicket(ctx, tkA.ID, "w1", "task-1", time.Now().UTC(), time.Minute); err != nil || !ok {
		t.Fatalf("lease A: %v %v", ok, err)
	}
	syncOnce()
	h2 := human(t, remote)
	d, err := h2.Get(kaservice.GetIn{ID: aID})
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "in_progress" || d.Claim == nil || d.Claim.Authority != "syre" {
		t.Fatalf("ka sees A as status=%s claim=%+v", d.Status, d.Claim)
	}
	if _, err := h2.Claim(ctx, kaservice.Caller{Actor: "agent/claude"}, aID, true); !kaservice.IsCode(err, kaservice.CodeClaimLost) {
		t.Fatalf("a ka agent stealing Syre's claim: %v", err)
	}

	// A human retitles B in ka; Syre pulls the new title.
	if _, err := h2.Update(ctx, bob, []kaservice.UpdateIn{{ID: bID, Set: map[string]any{"title": "Add Sub with tests"}}}); err != nil {
		t.Fatal(err)
	}
	syncOnce()
	if got, _ := st.GetTicket(ctx, tkB.ID); got.Title != "Add Sub with tests" {
		t.Fatalf("B title after pull = %q", got.Title)
	}

	// A ticket created in Syre appears in ka.
	created, err := st.CreateTicket(ctx, store.Ticket{ProjectID: proj.ID, ParentID: tkEpic.ID, Kind: store.TicketKindChore, Title: "Followup from Syre", Status: store.TicketStatusBacklog})
	if err != nil {
		t.Fatal(err)
	}
	syncOnce()
	refs, _ := st.ListExternalRefs(ctx, created.ID)
	if len(refs) != 1 {
		t.Fatalf("Syre ticket refs after push = %+v", refs)
	}
	d, err = human(t, remote).Get(kaservice.GetIn{ID: refs[0].ExternalID})
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Followup from Syre" || d.ParentRef == nil || d.ParentRef.ID != epicID || d.Status != "backlog" {
		t.Fatalf("ka sees the Syre ticket as %+v", d.Summary)
	}
}
