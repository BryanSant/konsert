package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestSQLiteStoreCRUDAndWAL(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "syre.db")

	st, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer st.Close()

	// Workers
	now := time.Now().UTC()
	w1 := store.Worker{
		ID:             "w1",
		Name:           "fedora-podman-1",
		HostGroup:      "fedora-main",
		OS:             "linux",
		Arch:           "amd64",
		Isolation:      "container",
		PermissionMode: store.PermissionFullAccess,
		Agents:         []string{"claude", "agy"},
		Tags:           map[string]string{"env": "podman"},
		MaxConcurrent:  4,
		Status:         "online",
		LastSeenAt:     now.Add(-60 * time.Second),
	}
	if err := st.UpsertWorker(ctx, w1); err != nil {
		t.Fatalf("UpsertWorker failed: %v", err)
	}
	gotW, err := st.GetWorker(ctx, "w1")
	if err != nil || gotW.Name != "fedora-podman-1" || len(gotW.Agents) != 2 || gotW.Tags["env"] != "podman" {
		t.Fatalf("GetWorker mismatch: %+v err=%v", gotW, err)
	}

	stale, err := st.MarkStaleWorkersOffline(ctx, now.Add(-30*time.Second))
	if err != nil || len(stale) != 1 || stale[0] != "w1" {
		t.Fatalf("expected w1 marked offline, got stale=%v err=%v", stale, err)
	}
	gotW, _ = st.GetWorker(ctx, "w1")
	if gotW.Status != "offline" {
		t.Fatalf("expected status offline, got %s", gotW.Status)
	}

	// Tasks & Events
	task := store.Task{
		ID:             "t1",
		WorkerID:       "w1",
		Agent:          "claude",
		Workdir:        "/repo",
		Branch:         "feat/x",
		Mode:           protocol.TaskModeOneshot,
		PermissionMode: store.PermissionFullAccess,
		Prompt:         "implement feature x",
		Status:         protocol.TaskStatusQueued,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}
	if err := st.UpdateTaskAssignment(ctx, "t1", "w1", "/repo/.syre-worktrees/t1"); err != nil {
		t.Fatalf("UpdateTaskAssignment failed: %v", err)
	}
	if _, err := st.UpdateTaskStatus(ctx, "t1", protocol.TaskStatusRunning, "", "", nil, store.Cause{}); err != nil {
		t.Fatalf("UpdateTaskStatus running failed: %v", err)
	}
	ec := 0
	if _, err := st.UpdateTaskStatus(ctx, "t1", protocol.TaskStatusCompleted, "done!", `{"diff_stat":"1 file changed"}`, &ec, store.Cause{Actor: store.ActorAgent, Confidence: store.ConfidenceHigh}); err != nil {
		t.Fatalf("UpdateTaskStatus completed failed: %v", err)
	}
	gotT, err := st.GetTask(ctx, "t1")
	if err != nil || gotT.Status != protocol.TaskStatusCompleted || gotT.ExitCode == nil || *gotT.ExitCode != 0 || gotT.StartedAt == nil || gotT.CompletedAt == nil {
		t.Fatalf("GetTask mismatch: %+v err=%v", gotT, err)
	}

	evID, err := st.AppendEvent(ctx, store.TaskEvent{TaskID: "t1", Kind: "milestone", Message: "started"})
	if err != nil || evID <= 0 {
		t.Fatalf("AppendEvent failed: id=%d err=%v", evID, err)
	}
	evs, err := st.ListEvents(ctx, "t1", 0, 10)
	if err != nil || len(evs) != 1 || evs[0].Message != "started" {
		t.Fatalf("ListEvents mismatch: %+v err=%v", evs, err)
	}

	// Chat sessions & messages
	cs := store.ChatSession{ID: "s1", Title: "Default", Provider: "openai", Model: "gpt-4o"}
	if err := st.CreateChatSession(ctx, cs); err != nil {
		t.Fatalf("CreateChatSession failed: %v", err)
	}
	if _, err := st.AppendChatMessage(ctx, store.ChatMessage{SessionID: "s1", Role: "user", Content: "check fleet"}); err != nil {
		t.Fatalf("AppendChatMessage failed: %v", err)
	}
	msgs, err := st.ListChatMessages(ctx, "s1")
	if err != nil || len(msgs) != 1 || msgs[0].Content != "check fleet" {
		t.Fatalf("ListChatMessages mismatch: %+v err=%v", msgs, err)
	}

	// DeleteWorker unlinks tasks without deleting task history.
	if err := st.DeleteWorker(ctx, "w1"); err != nil {
		t.Fatalf("DeleteWorker failed: %v", err)
	}
	if _, err := st.GetWorker(ctx, "w1"); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound after DeleteWorker, got %v", err)
	}
	gotTAfter, err := st.GetTask(ctx, "t1")
	if err != nil || gotTAfter.WorkerID != "" {
		t.Fatalf("expected task preserved with empty worker_id, got %+v err=%v", gotTAfter, err)
	}
}

func TestProjectDefaultsReposAndFacts(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer st.Close()

	// 1. Default Project exists automatically and cannot be deleted.
	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects failed: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != store.DefaultProjectID || !projects[0].IsDefault {
		t.Fatalf("expected built-in Default project, got %+v", projects)
	}
	if err := st.DeleteProject(ctx, store.DefaultProjectID); err != store.ErrProtectedDefault {
		t.Fatalf("expected ErrProtectedDefault deleting default project, got %v", err)
	}

	// 2. Multi-repo Project with registry entries keyed by remote_url.
	now := time.Now().UTC()
	for _, wid := range []string{"w-mac", "w-linux"} {
		if err := st.UpsertWorker(ctx, store.Worker{ID: wid, Name: wid, HostGroup: wid, OS: "linux", Arch: "amd64", Status: "online", LastSeenAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateProject(ctx, store.Project{ID: "proj-app1", Name: "App1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRepo(ctx, store.Repo{ID: "repo-api", ProjectID: "proj-app1", Name: "app1-api", RemoteURL: "https://github.com/org/app1-api.git"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRepo(ctx, store.Repo{ID: "repo-web", ProjectID: "proj-app1", Name: "app1-web", RemoteURL: "https://github.com/org/app1-web.git"}); err != nil {
		t.Fatal(err)
	}

	repos, err := st.ListRepos(ctx, "proj-app1")
	if err != nil || len(repos) != 2 {
		t.Fatalf("expected 2 repos under multi-repo project, got %+v err=%v", repos, err)
	}

	// Bind a task to project + repo + unified session model fields.
	if err := st.CreateTask(ctx, store.Task{
		ID:          "t-tax-1",
		WorkerID:    "w-linux",
		ProjectID:   "proj-app1",
		RepoID:      "repo-api",
		Agent:       "claude",
		Model:       "claude-sonnet-4-5",
		SessionKind: "chat",
		Workdir:     "/srv/app1-api/.wt/gcp",
		Branch:      "feat/gcp",
		Status:      protocol.TaskStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	gotT, err := st.GetTask(ctx, "t-tax-1")
	if err != nil || gotT.ProjectID != "proj-app1" || gotT.RepoID != "repo-api" || gotT.Branch != "feat/gcp" || gotT.Model != "claude-sonnet-4-5" || gotT.SessionKind != "chat" {
		t.Fatalf("unexpected task work/session fields: %+v err=%v", gotT, err)
	}

	// Deleting custom project cascades repos and re-homes tasks to Default.
	if err := st.DeleteProject(ctx, "proj-app1"); err != nil {
		t.Fatal(err)
	}
	gotRehomed, err := st.GetTask(ctx, "t-tax-1")
	if err != nil || gotRehomed.ProjectID != store.DefaultProjectID || gotRehomed.RepoID != "" {
		t.Fatalf("expected task re-homed to Default after project delete, got %+v err=%v", gotRehomed, err)
	}

	// 3. Cached machine facts & Commander persistent memory
	if err := st.UpdateWorkerFacts(ctx, "w-linux", `{"os_name":"Fedora Linux","cpu_cores":16,"tools":{"postgres":"18.1"}}`, now); err != nil {
		t.Fatalf("UpdateWorkerFacts failed: %v", err)
	}
	gotW, err := st.GetWorker(ctx, "w-linux")
	if err != nil || !strings.Contains(gotW.FactsJSON, `"postgres":"18.1"`) || gotW.FactsUpdatedAt == nil {
		t.Fatalf("expected persisted facts on worker, got %+v err=%v", gotW, err)
	}

	if err := st.UpsertMemory(ctx, store.MemoryEntry{Scope: "fleet", Key: "deploy-host", Summary: "Use foo1 for GCP deploys"}); err != nil {
		t.Fatalf("UpsertMemory failed: %v", err)
	}
	mems, err := st.ListMemory(ctx, "fleet", 10)
	if err != nil || len(mems) != 1 || mems[0].Summary != "Use foo1 for GCP deploys" {
		t.Fatalf("ListMemory mismatch: %+v err=%v", mems, err)
	}
	if err := st.DeleteMemory(ctx, "fleet", "deploy-host"); err != nil {
		t.Fatalf("DeleteMemory failed: %v", err)
	}
}
