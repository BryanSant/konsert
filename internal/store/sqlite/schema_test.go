package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

func TestOpenRefusesOlderSchema(t *testing.T) {
	ctx := context.Background()
	for name, ddl := range map[string]string{
		// A migration this build no longer ships.
		"retired migration": `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL, checksum TEXT NOT NULL DEFAULT '');
			INSERT INTO schema_migrations VALUES ('001_init.sql', '2026-01-01T00:00:00Z', '')`,
		// 001_schema.sql from before checksums were recorded.
		"no checksum column": `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL);
			INSERT INTO schema_migrations VALUES ('001_schema.sql', '2026-01-01T00:00:00Z')`,
		// 001_schema.sql edited in place since the database was created.
		"edited schema": `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL, checksum TEXT NOT NULL DEFAULT '');
			INSERT INTO schema_migrations VALUES ('001_schema.sql', '2026-01-01T00:00:00Z', 'deadbeef')`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.db")
			db, err := sql.Open("sqlite", buildDSN(path))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, ddl); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			if _, err := Open(ctx, path); !errors.Is(err, ErrIncompatibleSchema) {
				t.Fatalf("Open(old db) err = %v, want ErrIncompatibleSchema", err)
			}
		})
	}
}

func TestReopenCurrentSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "k.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = st.Close()
}

func TestNewSIDAndDefaults(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].ID != store.DefaultProjectID || projects[0].SID != ids.DefaultProjectSID {
		t.Fatalf("defaults = %+v", projects)
	}
	for kind := range idTables {
		sid, err := st.NewSID(ctx, kind)
		if err != nil || !ids.Valid(kind, sid) {
			t.Fatalf("NewSID(%s) = %q, %v", kind, sid, err)
		}
	}
	if _, err := st.NewSID(ctx, "NOPE"); err == nil {
		t.Fatal("NewSID(unknown kind) succeeded")
	}
}

// Every Create* mints a UUIDv7 id and a valid sid of its kind when both are empty, and
// ResolveID finds the record by either.
func TestCreateMintsIDAndSIDAndResolvesBoth(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.UpsertWorker(ctx, store.Worker{ID: ids.UUID(), Name: "w", HostGroup: "w", OS: "linux", Arch: "amd64", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	workers, _ := st.ListWorkers(ctx)
	w := workers[0]
	if err := st.CreateProject(ctx, store.Project{Name: "p"}); err != nil {
		t.Fatal(err)
	}
	ps, _ := st.ListProjects(ctx)
	p := ps[len(ps)-1]
	if err := st.CreateRepo(ctx, store.Repo{ProjectID: p.ID, Name: "r", RemoteURL: "https://github.com/org/r.git"}); err != nil {
		t.Fatal(err)
	}
	rs, _ := st.ListRepos(ctx, p.ID)
	r := rs[0]
	tk, err := st.CreateTicket(ctx, store.Ticket{ProjectID: p.ID, Kind: store.TicketKindFeature, Title: "ship it"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, store.Task{WorkerID: w.ID, Agent: "claude", Workdir: "/r", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	ts, _ := st.ListTasks(ctx, "", "", 1)
	task := ts[0]
	if err := st.CreateChatSession(ctx, store.ChatSession{Title: "c", Provider: "claude", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	cs, _ := st.ListChatSessions(ctx, 1)
	chat := cs[0]

	for _, rec := range []struct {
		kind    ids.Kind
		id, sid string
	}{
		{ids.Machine, w.ID, w.SID},
		{ids.Project, p.ID, p.SID},
		{ids.Repo, r.ID, r.SID},
		{ids.Ticket, tk.ID, tk.SID},
		{ids.Task, task.ID, task.SID},
		{ids.Chat, chat.ID, chat.SID},
	} {
		if !ids.IsUUIDv7(rec.id) || !ids.Valid(rec.kind, rec.sid) {
			t.Errorf("%s: id %q / sid %q", rec.kind, rec.id, rec.sid)
			continue
		}
		for _, ref := range []string{rec.id, rec.sid} {
			if got, err := st.ResolveID(ctx, rec.kind, ref); err != nil || got != rec.id {
				t.Errorf("ResolveID(%s, %q) = %q, %v; want %q", rec.kind, ref, got, err, rec.id)
			}
		}
	}

	// Unknown refs and sids of the wrong kind are not found.
	for _, ref := range []string{"", ids.UUID(), "t-zzzzz", chat.SID} {
		if _, err := st.ResolveID(ctx, ids.Task, ref); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ResolveID(task, %q) err = %v, want ErrNotFound", ref, err)
		}
	}
	if got, err := st.ResolveID(ctx, ids.Project, ids.DefaultProjectSID); err != nil || got != store.DefaultProjectID {
		t.Errorf("ResolveID(default project sid) = %q, %v", got, err)
	}
}

// A worker's sid is minted on first upsert and survives later upserts.
func TestUpsertWorkerKeepsSID(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	w := store.Worker{ID: ids.UUID(), Name: "w", HostGroup: "w", OS: "linux", Arch: "amd64", Status: "online"}
	if err := st.UpsertWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
	first, _ := st.GetWorker(ctx, w.ID)
	if !ids.Valid(ids.Machine, first.SID) {
		t.Fatalf("worker sid %q", first.SID)
	}
	w.Name = "renamed"
	if err := st.UpsertWorker(ctx, w); err != nil {
		t.Fatal(err)
	}
	again, _ := st.GetWorker(ctx, w.ID)
	if again.SID != first.SID || again.Name != "renamed" {
		t.Fatalf("sid changed or update lost: %+v vs %+v", again, first)
	}
}

// A discovered session maps (worker_id, external_id) to one task with a t- sid.
func TestUpsertExternalTaskStableIdentity(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	wid := ids.UUID()
	if err := st.UpsertWorker(ctx, store.Worker{ID: wid, Name: "w", HostGroup: "w", OS: "linux", Arch: "amd64", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	ext := store.Task{WorkerID: wid, ExternalID: "claude:abc", Agent: "claude", Workdir: "/x", Prompt: "p", Status: "running"}
	id1, _, err := st.UpsertExternalTask(ctx, ext)
	if err != nil {
		t.Fatal(err)
	}
	ext.Status = "completed"
	id2, _, err := st.UpsertExternalTask(ctx, ext)
	if err != nil || id2 != id1 {
		t.Fatalf("repeat upsert = %q, %v; want %q", id2, err, id1)
	}
	got, err := st.GetTask(ctx, id1)
	if err != nil || !ids.IsUUIDv7(got.ID) || !ids.Valid(ids.Task, got.SID) || got.Status != "completed" {
		t.Fatalf("external task = %+v, %v", got, err)
	}
}
