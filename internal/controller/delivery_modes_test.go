package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/forge"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// fakeForge is a deterministic in-memory forge.Resolver and forge.Client for tests.
type fakeForge struct {
	mu sync.Mutex

	detectErr error
	clientErr error
	repo      forge.Repo
	repoInfo  forge.RepoInfo

	pr              *forge.PR
	openedReqs      []forge.OpenPRRequest
	mergeReqs       []forge.MergeRequest
	mergeFn         func(forge.MergeRequest) (forge.MergeResult, error)
	reviewComments  []forge.ReviewComment
	compareURLValue string
	baseUpdates     []string
}

func (f *fakeForge) Detect(_ context.Context, remoteURL string) (forge.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.detectErr != nil {
		return forge.Repo{}, f.detectErr
	}
	if f.repo.Host != "" {
		return f.repo, nil
	}
	return forge.Repo{Kind: forge.GitHub, Host: "github.com", Owner: "acme", Name: "app"}, nil
}

func (f *fakeForge) Client(_ context.Context, _ forge.Repo) (forge.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clientErr != nil {
		return nil, f.clientErr
	}
	return f, nil
}

func (f *fakeForge) Kind() forge.Kind { return forge.GitHub }

func (f *fakeForge) RepoInfo(_ context.Context, _ forge.Repo) (forge.RepoInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.repoInfo
	if info.DefaultBranch == "" {
		info.DefaultBranch = "main"
	}
	return info, nil
}

func (f *fakeForge) OpenPR(_ context.Context, _ forge.Repo, req forge.OpenPRRequest) (forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openedReqs = append(f.openedReqs, req)
	pr := forge.PR{
		Number: 12,
		URL:    "https://github.com/acme/app/pull/12",
		State:  forge.PROpen,
		Head:   req.Head,
		Base:   req.Base,
		Checks: forge.ChecksPending,
	}
	f.pr = &pr
	return pr, nil
}

func (f *fakeForge) FindPR(_ context.Context, _ forge.Repo, _, _ string) (forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pr != nil {
		return *f.pr, nil
	}
	return forge.PR{}, forge.ErrNotFound
}

func (f *fakeForge) GetPR(_ context.Context, _ forge.Repo, _ int) (forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pr == nil {
		return forge.PR{}, forge.ErrNotFound
	}
	return *f.pr, nil
}

func (f *fakeForge) MergePR(_ context.Context, _ forge.Repo, _ int, req forge.MergeRequest) (forge.MergeResult, error) {
	f.mu.Lock()
	f.mergeReqs = append(f.mergeReqs, req)
	fn := f.mergeFn
	f.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return forge.MergeResult{Merged: true, Method: req.Method, SHA: "deadbeef"}, nil
}

func (f *fakeForge) ReviewComments(_ context.Context, _ forge.Repo, _ int, _ time.Time) ([]forge.ReviewComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]forge.ReviewComment, len(f.reviewComments))
	copy(out, f.reviewComments)
	return out, nil
}

func (f *fakeForge) CompareURL(r forge.Repo, head, base string) string {
	if f.compareURLValue != "" {
		return f.compareURLValue
	}
	return "https://" + r.Host + "/" + r.FullName() + "/compare/" + base + "..." + head
}

func TestDeliverySettingsAndOverrides(t *testing.T) {
	e := newAuditEnv(t)
	ctx := context.Background()

	// Global settings default to auto delivery mode and squash merge style.
	code, raw := e.do(t, credCookie, http.MethodGet, "/api/settings", "")
	if code != http.StatusOK || !strings.Contains(raw, `"default_merge_style":"squash"`) {
		t.Fatalf("GET /api/settings: %d %s", code, raw)
	}
	code, raw = e.do(t, credCookie, http.MethodPut, "/api/settings", `{"default_delivery_mode":"pr_open","default_merge_style":"rebase"}`)
	if code != http.StatusOK || !strings.Contains(raw, `"default_delivery_mode":"pr_open"`) || !strings.Contains(raw, `"default_merge_style":"rebase"`) {
		t.Fatalf("PUT /api/settings: %d %s", code, raw)
	}
	// Invalid delivery mode rejected.
	if code, _ := e.do(t, credCookie, http.MethodPut, "/api/settings", `{"default_delivery_mode":"bogus"}`); code != http.StatusBadRequest {
		t.Fatalf("invalid global delivery_mode status = %d, want 400", code)
	}

	// Project override via PATCH /api/projects/{id}.
	code, raw = e.do(t, credCookie, http.MethodPost, "/api/projects", `{"name":"Web"}`)
	if code != http.StatusCreated {
		t.Fatalf("create project: %d %s", code, raw)
	}
	var proj store.Project
	_ = json.Unmarshal([]byte(raw), &proj)
	code, raw = e.do(t, credCookie, http.MethodPatch, "/api/projects/"+proj.ID, `{"delivery_mode":"direct","merge_style":"merge"}`)
	if code != http.StatusOK || !strings.Contains(raw, `"delivery_mode":"direct"`) || !strings.Contains(raw, `"merge_style":"merge"`) {
		t.Fatalf("PATCH project: %d %s", code, raw)
	}

	// Repo override via PATCH /api/projects/{id}/repos/{repoId}.
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	stop := answerGitRPCs(w, protocol.GitInfoResult{OK: true, IsGitRepo: true, Branch: "syre/t1", DefaultBranch: "main", HasRemote: true, RemoteURL: "https://github.com/acme/app.git"}, nil, nil)
	defer stop()
	code, raw = e.do(t, credCookie, http.MethodPost, "/api/projects/"+proj.ID+"/repos", `{"name":"app","remote_url":"https://github.com/acme/app.git"}`)
	if code != http.StatusCreated {
		t.Fatalf("create repo: %d %s", code, raw)
	}
	var repo store.Repo
	_ = json.Unmarshal([]byte(raw), &repo)
	code, raw = e.do(t, credCookie, http.MethodPatch, "/api/projects/"+proj.ID+"/repos/"+repo.ID, `{"delivery_mode":"keep_branch","merge_style":"squash"}`)
	if code != http.StatusOK || !strings.Contains(raw, `"delivery_mode":"keep_branch"`) {
		t.Fatalf("PATCH repo: %d %s", code, raw)
	}

	// Verify resolution order: repo > project > global.
	task := store.Task{ID: "t1", ProjectID: proj.ID, RepoID: repo.ID, WorkerID: "w1", Workdir: "/src/app", Branch: "syre/t1"}
	_ = e.th.st.CreateTask(ctx, task)
	plan := e.th.h.TaskDeliveryPlan(ctx, task)
	if plan.Mode != store.DeliveryModeKeepBranch || plan.MergeStyle != store.MergeStyleSquash {
		t.Fatalf("repo override plan = %+v", plan)
	}
	// Clear repo override -> falls back to project override.
	_, _ = e.do(t, credCookie, http.MethodPatch, "/api/projects/"+proj.ID+"/repos/"+repo.ID, `{"delivery_mode":"","merge_style":""}`)
	plan = e.th.h.TaskDeliveryPlan(ctx, task)
	if plan.Mode != store.DeliveryModeDirect || plan.MergeStyle != store.MergeStyleMerge {
		t.Fatalf("project override plan = %+v", plan)
	}
	// Clear project override -> falls back to global setting.
	_, _ = e.do(t, credCookie, http.MethodPatch, "/api/projects/"+proj.ID, `{"delivery_mode":"","merge_style":""}`)
	plan = e.th.h.TaskDeliveryPlan(ctx, task)
	if plan.Mode != store.DeliveryModePROpen || plan.MergeStyle != store.MergeStyleRebase {
		t.Fatalf("global override plan = %+v", plan)
	}
}

func TestDeliveryModeKeepBranch(t *testing.T) {
	e := newAuditEnv(t)
	ctx := context.Background()
	_ = e.th.st.SetSettings(ctx, map[string]string{deliveryModeSetting: store.DeliveryModeKeepBranch})
	if err := e.th.st.CreateTask(ctx, store.Task{
		ID: "t-keep", Agent: "mock", Workdir: "/r", Branch: "syre/t-keep",
		Prompt: "experiment", Status: protocol.TaskStatusCompleted,
	}); err != nil {
		t.Fatal(err)
	}

	code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t-keep/review", `{"decision":"approved"}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	rv := decodeReviewResult(t, raw).Review
	if rv.DeliveryMode != store.DeliveryModeKeepBranch || rv.DeliveryStatus != store.DeliveryStatusDelivered ||
		!strings.Contains(rv.DeliveryMessage, "syre/t-keep") {
		t.Fatalf("keep_branch review = %+v", rv)
	}
}

func TestDeliveryModeDirectConflictReturnsTaskToAgent(t *testing.T) {
	e := newAuditEnv(t)
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	ctx := context.Background()
	_ = e.th.st.CreateTask(ctx, store.Task{
		ID: "t-conf", WorkerID: "w1", Agent: "mock", Workdir: "/r", Branch: "syre/t-conf",
		Title: "Fix parser", Prompt: "fix parser", Status: protocol.TaskStatusCompleted,
	})

	stop := answerGitRPCs(w,
		protocol.GitInfoResult{OK: true, IsGitRepo: true, Branch: "syre/t-conf", DefaultBranch: "main", HasRemote: false},
		func(req protocol.GitMergeRequest) protocol.GitMergeResult {
			return protocol.GitMergeResult{
				OK:            false,
				Conflict:      true,
				ConflictFiles: []string{"parser.go"},
				TargetBranch:  "main",
				Error:         "squash-merge conflict merging syre/t-conf into main (parser.go)",
			}
		},
		nil,
	)
	defer stop()

	code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t-conf/review", `{"decision":"approved"}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	res := decodeReviewResult(t, raw)
	if res.Review.DeliveryStatus != store.DeliveryStatusConflict || res.Review.Decision != store.ReviewChangesRequested ||
		res.FollowUpTaskID == "" || !strings.Contains(res.Review.Note, "parser.go") {
		t.Fatalf("conflict result = %+v", res)
	}
	follow := mustTask(t, e.th.st, res.FollowUpTaskID)
	if !strings.Contains(follow.Prompt, "parser.go") {
		t.Fatalf("follow-up prompt missing conflict detail: %q", follow.Prompt)
	}
}

func TestDeliveryModePRMergeOpensOnReviewAndMergesWithFallback(t *testing.T) {
	e := newAuditEnv(t)
	ctx := context.Background()
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")

	ff := &fakeForge{
		repoInfo: forge.RepoInfo{
			DefaultBranch:  "main",
			AllowedMethods: []forge.MergeMethod{forge.Merge, forge.Rebase}, // squash NOT allowed
			CanPush:        true,
		},
		mergeFn: func(req forge.MergeRequest) (forge.MergeResult, error) {
			return forge.MergeResult{AutoMergeSet: true, Method: req.Method}, nil
		},
	}
	e.th.h.SetForge(ff)

	var pushed []protocol.GitPushRequest
	stop := answerGitRPCs(w,
		protocol.GitInfoResult{OK: true, IsGitRepo: true, Branch: "syre/t-pr", DefaultBranch: "main", HasRemote: true, RemoteURL: "https://github.com/acme/app.git"},
		nil,
		func(req protocol.GitPushRequest) protocol.GitPushResult {
			pushed = append(pushed, req)
			return protocol.GitPushResult{OK: true, Branch: "syre/t-pr", RemoteURL: "https://github.com/acme/app.git", HeadSHA: "111"}
		},
	)
	defer stop()

	_ = e.th.st.CreateTask(ctx, store.Task{
		ID: "t-pr", WorkerID: "w1", Agent: "mock", Workdir: "/r", Branch: "syre/t-pr",
		Title: "Add feature", Summary: "Implemented feature X.",
		Prompt: "Add feature\n- [x] write code\n- [ ] add tests", Status: protocol.TaskStatusCompleted,
	})

	// Simulate task entering Review in pr_merge mode.
	e.th.h.prepareReviewPR(ctx, mustTask(t, e.th.st, "t-pr"))

	if len(pushed) != 1 || len(ff.openedReqs) != 1 {
		t.Fatalf("expected 1 push and 1 OpenPR, got pushes=%d opens=%d", len(pushed), len(ff.openedReqs))
	}
	if !strings.Contains(ff.openedReqs[0].Body, "Implemented feature X.") || !strings.Contains(ff.openedReqs[0].Body, "- [x] write code") {
		t.Fatalf("PR body missing summary/acceptance: %q", ff.openedReqs[0].Body)
	}

	// Preview shows merge PR #12 when checks pass.
	plan := e.th.h.TaskDeliveryPlan(ctx, mustTask(t, e.th.st, "t-pr"))
	if !strings.Contains(plan.ApproveLabel, "merge PR #12 (squash) when checks pass") {
		t.Fatalf("ApproveLabel = %q", plan.ApproveLabel)
	}

	// Approve merges PR #12 with WhenChecksPass=true and falls back from squash to merge.
	code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t-pr/review", `{"decision":"approved"}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	rv := decodeReviewResult(t, raw).Review
	if rv.DeliveryStatus != store.DeliveryStatusAutoMerge || !rv.PRAutoMerge || rv.MergeMethod != string(forge.Merge) ||
		!strings.Contains(rv.DeliveryMessage, "not allowed") {
		t.Fatalf("pr_merge fallback review = %+v", rv)
	}
	if len(ff.mergeReqs) != 1 || !ff.mergeReqs[0].WhenChecksPass || ff.mergeReqs[0].Method != forge.Merge {
		t.Fatalf("mergeReqs = %+v", ff.mergeReqs)
	}
}

func TestDeliveryModePRMergeNoCredentialFallbackShowsCompareURL(t *testing.T) {
	e := newAuditEnv(t)
	ctx := context.Background()
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	// Nil forge resolver -> no forge available.
	e.th.h.SetForge(nil)

	stop := answerGitRPCs(w,
		protocol.GitInfoResult{OK: true, IsGitRepo: true, Branch: "syre/t-nocred", DefaultBranch: "main", HasRemote: true, RemoteURL: "https://github.com/acme/app.git"},
		nil,
		func(req protocol.GitPushRequest) protocol.GitPushResult {
			return protocol.GitPushResult{OK: true, Branch: "syre/t-nocred", HeadSHA: "222"}
		},
	)
	defer stop()

	_ = e.th.st.CreateTask(ctx, store.Task{
		ID: "t-nocred", WorkerID: "w1", Agent: "mock", Workdir: "/r", Branch: "syre/t-nocred",
		Prompt: "do work", Status: protocol.TaskStatusCompleted,
	})

	plan := e.th.h.TaskDeliveryPlan(ctx, mustTask(t, e.th.st, "t-nocred"))
	if !strings.Contains(plan.ApproveLabel, "compare URL") {
		t.Fatalf("ApproveLabel = %q", plan.ApproveLabel)
	}

	code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t-nocred/review", `{"decision":"approved"}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	rv := decodeReviewResult(t, raw).Review
	if rv.DeliveryStatus != store.DeliveryStatusManual || !strings.Contains(rv.CompareURL, "https://github.com/acme/app/compare/main...syre/t-nocred") {
		t.Fatalf("no-cred review = %+v", rv)
	}
}

func TestDeliveryModePROpenForkPathAndTrackingAndCommentDedupe(t *testing.T) {
	e := newAuditEnv(t)
	ctx := context.Background()
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	_ = e.th.st.SetSettings(ctx, map[string]string{deliveryModeSetting: store.DeliveryModePROpen})

	ff := &fakeForge{
		repo: forge.Repo{Kind: forge.GitHub, Host: "github.com", Owner: "upstream", Name: "lib"},
		repoInfo: forge.RepoInfo{
			DefaultBranch: "main",
			CanPush:       false,
			Fork:          &forge.Repo{Kind: forge.GitHub, Host: "github.com", Owner: "alice", Name: "lib"},
		},
	}
	e.th.h.SetForge(ff)

	var pushReqs []protocol.GitPushRequest
	stop := answerGitRPCs(w,
		protocol.GitInfoResult{OK: true, IsGitRepo: true, Branch: "syre/t-fork", DefaultBranch: "main", HasRemote: true, RemoteURL: "https://github.com/upstream/lib.git"},
		nil,
		func(req protocol.GitPushRequest) protocol.GitPushResult {
			pushReqs = append(pushReqs, req)
			return protocol.GitPushResult{OK: true, Branch: "syre/t-fork", RemoteURL: req.RemoteURL, HeadSHA: "333"}
		},
	)
	defer stop()

	_ = e.th.st.CreateTask(ctx, store.Task{
		ID: "t-fork", WorkerID: "w1", Agent: "mock", Workdir: "/r", Branch: "syre/t-fork",
		Title: "Upstream fix", Prompt: "fix bug", Status: protocol.TaskStatusCompleted,
	})

	// Nothing pushed before Approve in pr_open mode.
	e.th.h.prepareReviewPR(ctx, mustTask(t, e.th.st, "t-fork"))
	if len(pushReqs) != 0 {
		t.Fatalf("pr_open pushed before approve: %+v", pushReqs)
	}

	// Plan mentions fork.
	plan := e.th.h.TaskDeliveryPlan(ctx, mustTask(t, e.th.st, "t-fork"))
	if !strings.Contains(plan.ApproveLabel, "fork") {
		t.Fatalf("fork ApproveLabel = %q", plan.ApproveLabel)
	}

	// Approve pushes to fork and opens PR against upstream with HeadOwner = alice.
	code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t-fork/review", `{"decision":"approved"}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	rv := decodeReviewResult(t, raw).Review
	if rv.DeliveryStatus != store.DeliveryStatusPROpen || rv.PRNumber != 12 || rv.PRState != string(forge.PROpen) {
		t.Fatalf("pr_open review = %+v", rv)
	}
	if len(pushReqs) != 1 || pushReqs[0].RemoteURL != "https://github.com/alice/lib.git" {
		t.Fatalf("expected push to fork, got %+v", pushReqs)
	}
	if len(ff.openedReqs) != 1 || ff.openedReqs[0].HeadOwner != "alice" {
		t.Fatalf("expected HeadOwner=alice, got %+v", ff.openedReqs)
	}

	// Review comments with ChangesRequested=true are delivered once and deduped persistently.
	t1 := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	ff.mu.Lock()
	ff.reviewComments = []forge.ReviewComment{
		{ID: "cm-1", Author: "bob", Body: "Looks fine overall", ChangesRequested: false, CreatedAt: t1},
		{ID: "cm-2", Author: "bob", Body: "Handle nil pointer", Path: "lib.go", Line: 42, ChangesRequested: true, CreatedAt: t1},
	}
	ff.mu.Unlock()

	e.th.h.PollOpenPRs(ctx)
	rv1, _ := e.th.st.GetReview(ctx, "t-fork", "")
	if rv1.FollowUpTaskID == "" || !strings.Contains(rv1.Note, "Handle nil pointer") || !strings.Contains(rv1.Note, "lib.go:42") {
		t.Fatalf("expected review comment follow-up, got %+v", rv1)
	}
	firstFollow := rv1.FollowUpTaskID

	// Polling again with the same comment ID must NOT create another follow-up.
	e.th.h.PollOpenPRs(ctx)
	rv2, _ := e.th.st.GetReview(ctx, "t-fork", "")
	if rv2.FollowUpTaskID != firstFollow {
		t.Fatalf("duplicate follow-up created: %q vs %q", rv2.FollowUpTaskID, firstFollow)
	}

	// Polling when PR is merged updates PRState to merged and DeliveryStatus to delivered.
	ff.mu.Lock()
	ff.pr.State = forge.PRMerged
	ff.pr.Checks = forge.ChecksPassed
	ff.mu.Unlock()

	e.th.h.PollOpenPRs(ctx)
	rv3, _ := e.th.st.GetReview(ctx, "t-fork", "")
	if rv3.PRState != string(forge.PRMerged) || rv3.DeliveryStatus != store.DeliveryStatusDelivered || rv3.PRChecks != string(forge.ChecksPassed) {
		t.Fatalf("merged PR tracking = %+v", rv3)
	}
}

func TestDeliveryModeDirectPassesTaskSIDAndSurfacesMergeMessage(t *testing.T) {
	e := newAuditEnv(t)
	w := e.th.connectWorker(t, testWorkerInfo("w1", "alpha"), nil, "w1")
	ctx := context.Background()
	_ = e.th.st.CreateTask(ctx, store.Task{
		ID: "t-dirmsg", SID: "S42", WorkerID: "w1", Agent: "mock",
		Workdir: "/r/.syre-worktrees/S42", Branch: "syre/S42",
		Title: "Local merge", Prompt: "local merge", Status: protocol.TaskStatusCompleted,
	})

	var gotReq protocol.GitMergeRequest
	stop := answerGitRPCs(w,
		protocol.GitInfoResult{OK: true, IsGitRepo: true, Branch: "syre/S42", DefaultBranch: "main", HasRemote: false},
		func(req protocol.GitMergeRequest) protocol.GitMergeResult {
			gotReq = req
			return protocol.GitMergeResult{
				OK:           true,
				Merged:       true,
				TargetBranch: "main",
				MergeSHA:     "abcdef123",
				MergedBranch: "syre/merged/S42",
				Message:      "main is checked out with uncommitted changes; the merge is ready on branch syre/merged/S42 — fast-forward main when you're ready",
			}
		},
		nil,
	)
	defer stop()

	code, raw := e.do(t, credCookie, http.MethodPost, "/api/tasks/t-dirmsg/review", `{"decision":"approved"}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if gotReq.TaskSID != "S42" {
		t.Fatalf("GitMergeRequest.TaskSID = %q, want S42", gotReq.TaskSID)
	}
	rv := decodeReviewResult(t, raw).Review
	if rv.DeliveryStatus != store.DeliveryStatusDelivered || !strings.Contains(rv.DeliveryMessage, "syre/merged/S42") {
		t.Fatalf("direct merge review = %+v", rv)
	}
}

func (f *fakeForge) UpdatePRBase(_ context.Context, _ forge.Repo, _ int, base string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.baseUpdates = append(f.baseUpdates, base)
	if f.pr != nil {
		f.pr.Base = base
	}
	return nil
}
