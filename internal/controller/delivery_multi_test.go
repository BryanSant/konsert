package controller

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// A two-repo ticket delivers both repos; one repo conflicting is reported without
// losing the other repo's merge.
func TestDeliveryMultiRepoDirectReportsEachRepo(t *testing.T) {
	e := newAuditEnv(t)
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	ctx := context.Background()
	if err := e.th.st.CreateProject(ctx, store.Project{Name: "multi", DeliveryMode: store.DeliveryModeDirect}); err != nil {
		t.Fatal(err)
	}
	projs, _ := e.th.st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	for _, name := range []string{"api", "web"} {
		if err := e.th.st.CreateRepo(ctx, store.Repo{ProjectID: proj.ID, Name: name, RemoteURL: "https://example.com/" + name + ".git"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = e.th.st.CreateTask(ctx, store.Task{
		ID: "t-multi", SID: "M1", WorkerID: "w1", Agent: "mock", ProjectID: proj.ID,
		Workdir: "/r/.syre/tickets/tk-1", Branch: "syre/tk-1",
		Title: "Cross-repo", Prompt: "cross-repo", Status: protocol.TaskStatusCompleted,
	})
	if err := e.th.st.UpdateTaskWorktrees(ctx, "t-multi", []store.TaskWorktree{
		{RepoName: "api", WorktreePath: "/r/.syre/tickets/tk-1/api", Branch: "syre/tk-1"},
		{RepoName: "web", WorktreePath: "/r/.syre/tickets/tk-1/web", Branch: "syre/tk-1"},
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var merged []string
	stop := answerGitRPCs(w,
		protocol.GitInfoResult{OK: true, IsGitRepo: true, Branch: "syre/tk-1", DefaultBranch: "main"},
		func(req protocol.GitMergeRequest) protocol.GitMergeResult {
			mu.Lock()
			merged = append(merged, req.Workdir)
			mu.Unlock()
			if strings.HasSuffix(req.Workdir, "/web") {
				return protocol.GitMergeResult{OK: false, Conflict: true, TargetBranch: "main", ConflictFiles: []string{"index.ts"}}
			}
			return protocol.GitMergeResult{OK: true, Merged: true, TargetBranch: "main", MergeSHA: "abc1234"}
		},
		nil,
	)
	defer stop()

	code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t-multi/review", `{"decision":"approved"}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	mu.Lock()
	gotMerged := append([]string(nil), merged...)
	mu.Unlock()
	if len(gotMerged) != 2 || !strings.HasSuffix(gotMerged[0], "/api") || !strings.HasSuffix(gotMerged[1], "/web") {
		t.Fatalf("merge requests for %v, want api then web", gotMerged)
	}
	rv := decodeReviewResult(t, raw).Review
	if rv.DeliveryStatus != store.DeliveryStatusConflict {
		t.Fatalf("status %q, want conflict (the worst repo)", rv.DeliveryStatus)
	}
	if !strings.Contains(rv.DeliveryMessage, "api:") || !strings.Contains(rv.DeliveryMessage, "web:") {
		t.Fatalf("message %q should name each repo", rv.DeliveryMessage)
	}
}
