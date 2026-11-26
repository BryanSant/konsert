package controller

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/BryanSant/syre/internal/forge"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// A dependent's PR opens against its blocker's branch while the blocker's PR is open,
// and is rebased and retargeted onto main once the blocker merges.
func TestStackedPROpensOnBlockerBranchAndRetargetsAfterMerge(t *testing.T) {
	e := newAuditEnv(t)
	ctx := context.Background()
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	_ = e.th.st.SetSettings(ctx, map[string]string{deliveryModeSetting: store.DeliveryModePROpen})
	ff := &fakeForge{repoInfo: forge.RepoInfo{DefaultBranch: "main", CanPush: true}}
	e.th.h.SetForge(ff)

	if err := e.th.st.CreateProject(ctx, store.Project{Name: "stacked"}); err != nil {
		t.Fatal(err)
	}
	projs, _ := e.th.st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	if err := e.th.st.CreateRepo(ctx, store.Repo{ProjectID: proj.ID, Name: "app", RemoteURL: "https://github.com/acme/app.git", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	repos, _ := e.th.st.ListRepos(ctx, proj.ID)
	repo := repos[0]
	mk := func(title string) store.Ticket {
		tk, err := e.th.st.CreateTicket(ctx, store.Ticket{ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: title, Status: store.TicketStatusReview})
		if err != nil {
			t.Fatal(err)
		}
		_ = e.th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repo.ID, TargetBranch: "syre/" + tk.SID})
		return tk
	}
	a, b := mk("Blocker"), mk("Dependent")
	if err := e.th.st.AddTicketEdge(ctx, b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	mkTask := func(id string, tk store.Ticket) {
		_ = e.th.st.CreateTask(ctx, store.Task{ID: id, WorkerID: "w1", Agent: "mock", ProjectID: proj.ID, RepoID: repo.ID, TicketID: tk.ID,
			Workdir: "/r/" + id, Branch: "syre/" + tk.SID, Title: tk.Title, Prompt: tk.Title, Status: protocol.TaskStatusCompleted})
		_ = e.th.st.UpdateTaskWorktrees(ctx, id, []store.TaskWorktree{{RepoName: "app", WorktreePath: "/r/" + id, Branch: "syre/" + tk.SID, BaseSHA: "base-" + id}})
	}
	mkTask("t-a", a)
	mkTask("t-b", b)
	// The blocker's PR is open (delivered as pr_open, not merged).
	if _, err := e.th.st.SaveReview(ctx, store.Review{TaskID: "t-a", Decision: store.ReviewApproved, DeliveryMode: store.DeliveryModePROpen,
		DeliveryStatus: store.DeliveryStatusPROpen, PRNumber: 7, PRState: string(forge.PROpen), PRRemoteURL: "https://github.com/acme/app.git"}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var pushes []protocol.GitPushRequest
	stop := answerGitRPCs(w,
		protocol.GitInfoResult{OK: true, IsGitRepo: true, Branch: "syre/" + b.SID, DefaultBranch: "main", HasRemote: true, RemoteURL: "https://github.com/acme/app.git"},
		nil,
		func(req protocol.GitPushRequest) protocol.GitPushResult {
			mu.Lock()
			pushes = append(pushes, req)
			mu.Unlock()
			return protocol.GitPushResult{OK: true, Branch: req.Branch, HeadSHA: "head"}
		},
	)
	defer stop()

	code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t-b/review", `{"decision":"approved"}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if len(ff.openedReqs) != 1 || ff.openedReqs[0].Base != "syre/"+a.SID {
		t.Fatalf("dependent PR base = %+v, want the blocker's branch syre/%s", ff.openedReqs, a.SID)
	}
	if rv, _ := e.th.st.GetReview(ctx, "t-b", ""); rv.PRBase != "syre/"+a.SID {
		t.Fatalf("PRBase = %q", rv.PRBase)
	}

	// The blocker's PR merges: the next poll rebases the dependent onto main and retargets it.
	if _, err := e.th.st.SaveReview(ctx, store.Review{TaskID: "t-a", Decision: store.ReviewApproved, DeliveryMode: store.DeliveryModePROpen,
		DeliveryStatus: store.DeliveryStatusDelivered, PRNumber: 7, PRState: string(forge.PRMerged), PRRemoteURL: "https://github.com/acme/app.git"}); err != nil {
		t.Fatal(err)
	}
	e.th.h.PollOpenPRs(ctx)

	ff.mu.Lock()
	updates := append([]string(nil), ff.baseUpdates...)
	ff.mu.Unlock()
	if len(updates) != 1 || updates[0] != "main" {
		t.Fatalf("base updates = %v, want [main]", updates)
	}
	mu.Lock()
	last := pushes[len(pushes)-1]
	mu.Unlock()
	if last.RebaseOnto != "main" {
		t.Fatalf("retarget push = %+v, want a rebase onto main", last)
	}
	if rv, _ := e.th.st.GetReview(ctx, "t-b", ""); rv.PRBase != "main" {
		t.Fatalf("PRBase after retarget = %q", rv.PRBase)
	}
}
