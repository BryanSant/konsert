package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestProjectForPathAndExternalSessionInference(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()

	if err := th.st.CreateProject(ctx, store.Project{Name: "Alpha"}); err != nil {
		t.Fatal(err)
	}
	projs, _ := th.st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	if err := th.st.CreateRepo(ctx, store.Repo{
		ProjectID: proj.ID,
		Name:      "service",
		RemoteURL: "https://github.com/Acme/Service.git",
	}); err != nil {
		t.Fatal(err)
	}
	repos, _ := th.st.ListRepos(ctx, proj.ID)
	repo := repos[0]

	wInfo := testWorkerInfo("w-inf", "dev-mac")
	wInfo.Facts = &protocol.MachineFacts{
		CPUCores: 8,
		Checkouts: []protocol.ReportedCheckout{
			{Path: "/Users/dev/src/service", RemoteURL: "git@GITHUB.COM:Acme/Service.git", Branch: "main"},
		},
	}
	th.connectWorker(t, wInfo, nil, "w-inf")

	// Exact checkout path and nested subdirectory match; sibling path does not.
	if got := th.h.projectForPath(ctx, "w-inf", "/Users/dev/src/service"); got != proj.ID {
		t.Fatalf("projectForPath(exact) = %q, want %q", got, proj.ID)
	}
	if got := th.h.projectForPath(ctx, "w-inf", "/Users/dev/src/service/cmd/api"); got != proj.ID {
		t.Fatalf("projectForPath(subdir) = %q, want %q", got, proj.ID)
	}
	if got := th.h.projectForPath(ctx, "w-inf", "/Users/dev/src/service2"); got != "" {
		t.Fatalf("projectForPath(sibling) = %q, want empty", got)
	}

	// handleExternalSession infers both ProjectID and RepoID from cwd.
	th.h.handleExternalSession(ctx, "w-inf", protocol.ExternalSessionPayload{
		ExternalID: "ext-1",
		Agent:      "claude",
		Cwd:        "/Users/dev/src/service/internal/pkg",
		Branch:     "main",
		Status:     "working",
		Source:     "hook",
	})
	tasks, err := th.st.ListTasks(ctx, "", "w-inf", 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("expected 1 external task, got %d (err=%v)", len(tasks), err)
	}
	if tasks[0].ProjectID != proj.ID || tasks[0].RepoID != repo.ID {
		t.Fatalf("external task got project=%q repo=%q, want project=%q repo=%q",
			tasks[0].ProjectID, tasks[0].RepoID, proj.ID, repo.ID)
	}
}

func TestTicketPlacementLocalityPreference(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()

	if err := th.st.CreateProject(ctx, store.Project{Name: "Multi", Autopilot: false}); err != nil {
		t.Fatal(err)
	}
	projs, _ := th.st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	if err := th.st.CreateRepo(ctx, store.Repo{
		ProjectID:     proj.ID,
		Name:          "api",
		RemoteURL:     "https://github.com/acme/api.git",
		DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	if err := th.st.CreateRepo(ctx, store.Repo{
		ProjectID:     proj.ID,
		Name:          "web",
		RemoteURL:     "https://github.com/acme/web.git",
		DefaultBranch: "develop",
	}); err != nil {
		t.Fatal(err)
	}
	repos, _ := th.st.ListRepos(ctx, proj.ID)
	var apiRepo, webRepo store.Repo
	for _, r := range repos {
		if r.Name == "api" {
			apiRepo = r
		} else {
			webRepo = r
		}
	}

	tk, err := th.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Cross-repo change",
		Priority:  "P1",
		Status:    store.TicketStatusReady,
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: apiRepo.ID, TargetBranch: "feat/x"})
	_ = th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: webRepo.ID, TargetBranch: "feat/x"})

	// Worker w-remote has higher disk free (would normally win tie-break) but only 1 checkout.
	wRemoteInfo := testWorkerInfo("w-remote", "remote-box")
	wRemoteInfo.Facts = &protocol.MachineFacts{
		DiskFreeGB: 500,
		Checkouts: []protocol.ReportedCheckout{
			{Path: "/src/api", RemoteURL: "github.com/acme/api"},
		},
	}
	fwRemote := th.connectWorker(t, wRemoteInfo, nil, "w-remote")

	// Worker w-local has lower disk free but checkouts of BOTH bound repos -> must win by locality.
	wLocalInfo := testWorkerInfo("w-local", "local-box")
	wLocalInfo.Facts = &protocol.MachineFacts{
		DiskFreeGB: 50,
		Checkouts: []protocol.ReportedCheckout{
			{Path: "/work/api", RemoteURL: "git@github.com:acme/api.git"},
			{Path: "/work/web", RemoteURL: "https://github.com/acme/web"},
		},
	}
	fwLocal := th.connectWorker(t, wLocalInfo, nil, "w-local")

	// Enable autopilot only after both workers have registered so the scheduler compares both.
	proj.Autopilot = true
	if err := th.st.UpdateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	th.h.SchedulePending(ctx)

	env := fwLocal.expect(protocol.MsgTaskStart, 3*time.Second)
	if got := fwRemote.drain(protocol.MsgTaskStart, 150*time.Millisecond); len(got) != 0 {
		t.Fatalf("expected w-local to win by locality, but w-remote received task.start")
	}
	p, err := protocol.DecodePayload[protocol.TaskStartPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	if p.TicketSID != tk.SID {
		t.Fatalf("TicketSID = %q, want %q", p.TicketSID, tk.SID)
	}
	if len(p.Bindings) != 2 {
		t.Fatalf("expected 2 bindings, got %+v", p.Bindings)
	}
	byName := map[string]protocol.TaskRepoBinding{}
	for _, b := range p.Bindings {
		byName[b.RepoName] = b
	}
	if byName["api"].CheckoutPath != "/work/api" || byName["api"].BaseBranch != "main" {
		t.Errorf("unexpected api binding: %+v", byName["api"])
	}
	if byName["web"].CheckoutPath != "/work/web" || byName["web"].BaseBranch != "develop" {
		t.Errorf("unexpected web binding: %+v", byName["web"])
	}
}

func TestTicketWorktreeCleanupAfterDoneAndFailedRetention(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()

	proj, repo := createProjectAndRepo(t, th.st, "CleanupProj", true, false)
	fw := th.connectWorker(t, testWorkerInfo("w-clean", "clean-box"), nil, "w-clean")

	// Helper to respond OK to git.worktree.cleanup RPCs and record cleaned paths.
	cleanedCh := make(chan protocol.GitWorktreeCleanupRequest, 10)
	stopResponder := make(chan struct{})
	t.Cleanup(func() { close(stopResponder) })
	go func() {
		for {
			select {
			case <-stopResponder:
				return
			case <-fw.ctx.Done():
				return
			case env := <-fw.frames:
				if env.Type != protocol.MsgGitWorktreeCleanup {
					continue
				}
				req, _ := protocol.DecodePayload[protocol.GitWorktreeCleanupRequest](env)
				cleanedCh <- req
				fw.sendWithID(protocol.MsgGitWorktreeCleanupResult, env.ID, protocol.GitWorktreeCleanupResult{OK: true})
			}
		}
	}()

	// 1. Ticket reaching done -> immediate cleanup via PATCH /api/tickets/{id}.
	doneTk, err := th.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Done ticket",
		Status:    store.TicketStatusReview,
	})
	if err != nil {
		t.Fatal(err)
	}
	doneTask := store.Task{
		ID:        "task-done-wt",
		SID:       "t-dwt",
		WorkerID:  "w-clean",
		ProjectID: proj.ID,
		RepoID:    repo.ID,
		Agent:     "mock",
		Workdir:   "/work/" + doneTk.SID,
		Prompt:    "p",
		Status:    protocol.TaskStatusCompleted,
		TicketID:  doneTk.ID,
	}
	if err := th.st.CreateTask(ctx, doneTask); err != nil {
		t.Fatal(err)
	}
	if err := th.st.UpdateTaskWorktrees(ctx, doneTask.ID, []store.TaskWorktree{
		{RepoName: "backend", CheckoutPath: "/src/backend", WorktreePath: "/work/" + doneTk.SID + "/backend", Branch: "feat/done"},
	}); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(config.ControllerConfig{NoAuth: true}, th.st, nil, "test")
	srv.hub.Close()
	srv.hub = th.h
	req := httptest.NewRequest(http.MethodPatch, "/api/tickets/"+doneTk.ID, strings.NewReader(`{"status":"done"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH ticket done: %d %s", rec.Code, rec.Body.String())
	}

	select {
	case got := <-cleanedCh:
		if got.Workdir != "/work/"+doneTk.SID+"/backend" || got.RepoRoot != "/src/backend" || !got.PruneEmptyParent {
			t.Fatalf("unexpected cleanup request: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for git.worktree.cleanup after ticket done")
	}

	// Wait briefly for MarkTaskWorktreesCleaned to persist.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tRow, _ := th.st.GetTask(ctx, doneTask.ID); tRow.WorktreesCleanedAt != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if tRow, _ := th.st.GetTask(ctx, doneTask.ID); tRow.WorktreesCleanedAt == nil {
		t.Fatal("expected WorktreesCleanedAt set on done ticket task")
	}

	// 2. Failed ticket -> kept during retention period, cleaned after retention elapses.
	if err := th.st.SetSettings(ctx, map[string]string{worktreeRetentionDaysSetting: "7"}); err != nil {
		t.Fatal(err)
	}
	failTk, err := th.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindBug,
		Title:     "Failed ticket",
		Status:    store.TicketStatusFailed,
	})
	if err != nil {
		t.Fatal(err)
	}
	failTask := store.Task{
		ID:        "task-fail-wt",
		SID:       "t-fwt",
		WorkerID:  "w-clean",
		ProjectID: proj.ID,
		RepoID:    repo.ID,
		Agent:     "mock",
		Workdir:   "/work/" + failTk.SID,
		Prompt:    "p",
		Status:    protocol.TaskStatusFailed,
		TicketID:  failTk.ID,
	}
	if err := th.st.CreateTask(ctx, failTask); err != nil {
		t.Fatal(err)
	}
	if err := th.st.UpdateTaskWorktrees(ctx, failTask.ID, []store.TaskWorktree{
		{RepoName: "backend", CheckoutPath: "/src/backend", WorktreePath: "/work/" + failTk.SID + "/backend", Branch: "feat/fail"},
	}); err != nil {
		t.Fatal(err)
	}

	// Sweep at +3 days: must NOT clean up yet.
	th.h.sweepTicketWorktrees(ctx, failTk.UpdatedAt.Add(3*24*time.Hour))
	select {
	case got := <-cleanedCh:
		t.Fatalf("failed ticket worktree cleaned too early: %+v", got)
	case <-time.After(150 * time.Millisecond):
	}
	if tRow, _ := th.st.GetTask(ctx, failTask.ID); tRow.WorktreesCleanedAt != nil {
		t.Fatal("failed ticket worktree should not be marked cleaned before retention")
	}

	// Sweep at +8 days: must clean up now.
	th.h.sweepTicketWorktrees(ctx, failTk.UpdatedAt.Add(8*24*time.Hour))
	select {
	case got := <-cleanedCh:
		if got.Workdir != "/work/"+failTk.SID+"/backend" {
			t.Fatalf("unexpected failed cleanup request: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for failed ticket worktree cleanup after retention")
	}
	if tRow, _ := th.st.GetTask(ctx, failTask.ID); tRow.WorktreesCleanedAt == nil {
		t.Fatal("expected WorktreesCleanedAt set on failed ticket task after retention")
	}
}
