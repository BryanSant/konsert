package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func cappedProject(t *testing.T, st store.Store, name string, limit int) store.Project {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateProject(ctx, store.Project{Name: name, MaxConcurrent: limit}); err != nil {
		t.Fatal(err)
	}
	projs, _ := st.ListProjects(ctx)
	for _, p := range projs {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("project %s not created", name)
	return store.Project{}
}

func TestProjectCapHoldsQueuedAndAutomaticWork(t *testing.T) {
	cs, _ := scopeTestFleet(t)
	ctx := context.Background()
	capped := cappedProject(t, cs.st, "Capped", 1)
	free := cappedProject(t, cs.st, "Free", 0)

	// REST validates the cap.
	if code, _ := cs.api(t, http.MethodPatch, "/api/projects/"+capped.ID, map[string]any{"max_concurrent": -1}); code != http.StatusBadRequest {
		t.Fatalf("negative cap accepted: %d", code)
	}
	if code, raw := cs.api(t, http.MethodPatch, "/api/projects/"+capped.ID, map[string]any{"max_concurrent": 1}); code != http.StatusOK || decodeInto[store.Project](t, raw).MaxConcurrent != 1 {
		t.Fatalf("set cap: %d %s", code, raw)
	}

	queue := func(id, project string) {
		t.Helper()
		if err := cs.st.CreateTask(ctx, store.Task{ID: id, SID: "t-" + id, ProjectID: project, Agent: "mock", Workdir: "/repo", Prompt: "p",
			Mode: protocol.TaskModeOneshot, Status: protocol.TaskStatusQueued, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	queue("c-1", capped.ID)
	queue("c-2", capped.ID)
	queue("c-3", capped.ID)
	queue("f-1", free.ID)
	queue("f-2", free.ID)
	cs.h.SchedulePending(ctx)
	placed := func(ids ...string) int {
		n := 0
		for _, id := range ids {
			if mustTask(t, cs.st, id).Status != protocol.TaskStatusQueued {
				n++
			}
		}
		return n
	}
	if n := placed("c-1", "c-2", "c-3"); n != 1 {
		t.Fatalf("capped project placed %d, want 1", n)
	}
	if n := placed("f-1", "f-2"); n != 2 {
		t.Fatalf("uncapped project placed %d, want 2", n)
	}
	// Oldest first: c-1 runs, the others wait.
	if mustTask(t, cs.st, "c-1").Status == protocol.TaskStatusQueued {
		t.Fatal("the oldest task did not get the slot")
	}

	// The Commander's dispatch waits too, saying why; the user's starts.
	mkChat(t, cs.st, "c-cmd", true)
	res, err := cs.svc.DispatchTask(commanderCtx("c-cmd"), DispatchRequest{ProjectID: capped.ID, Agent: "mock", RepoPath: "/repo", Prompt: "p"})
	if err != nil || res.WorkerID != "" || !strings.Contains(strings.Join(res.Warnings, " "), "project Capped is at its limit of 1 running at once") {
		t.Fatalf("automatic dispatch at cap: %+v %v", res, err)
	}
	if res, err := cs.svc.DispatchTask(userCtx(), DispatchRequest{ProjectID: capped.ID, Agent: "mock", RepoPath: "/repo", Prompt: "p"}); err != nil || res.WorkerID == "" {
		t.Fatalf("user dispatch at cap: %+v %v", res, err)
	}

	// Raising the cap lets the queue move.
	if code, _ := cs.api(t, http.MethodPatch, "/api/projects/"+capped.ID, map[string]any{"max_concurrent": 0}); code != http.StatusOK {
		t.Fatalf("clear cap: %d", code)
	}
	waitFor(t, 3*time.Second, "queued work placed after the cap was lifted", func() bool { return placed("c-1", "c-2", "c-3") == 3 })
}

func TestProjectCapHoldsTicketRuns(t *testing.T) {
	cs, _ := scopeTestFleet(t)
	ctx := context.Background()
	proj := cappedProject(t, cs.st, "Tickets", 1)
	if err := cs.st.CreateRepo(ctx, store.Repo{ProjectID: proj.ID, Name: "api", RemoteURL: "https://github.com/acme/api.git", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	repos, _ := cs.st.ListRepos(ctx, proj.ID)
	var tickets []store.Ticket
	for i := 0; i < 3; i++ {
		tk, err := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "T", Status: store.TicketStatusReady, Hints: store.TicketHints{Agent: "mock"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repos[0].ID, TargetBranch: "feat/x"})
		tickets = append(tickets, tk)
	}
	if _, err := cs.h.SetProjectAutopilot(ctx, proj.ID, true); err != nil {
		t.Fatal(err)
	}
	running := func() int {
		n := 0
		for _, tk := range tickets {
			if cur, _ := cs.st.GetTicket(ctx, tk.ID); cur.Status == store.TicketStatusRunning {
				n++
			}
		}
		return n
	}
	waitFor(t, 3*time.Second, "one ticket running", func() bool { return running() == 1 })
	cs.h.SchedulePending(ctx)
	time.Sleep(200 * time.Millisecond)
	if n := running(); n != 1 {
		t.Fatalf("%d tickets running, cap 1", n)
	}
}
