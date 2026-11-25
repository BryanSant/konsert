package github

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/forge"
)

func TestGitHubClient(t *testing.T) {
	ctx := context.Background()
	var (
		checksMode       = "passed" // "none", "pending", "passed", "failed"
		mergeStatus      = http.StatusOK
		mergeErrMsg      = ""
		autoMergeCalled  = false
		autoMergeMethod  = ""
		lastOpenPRHead   = ""
		lastAuthHeader   = ""
		enterpriseServer bool
	)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")

		p := r.URL.Path
		if enterpriseServer {
			if p == "/api/graphql" {
				var body map[string]any
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &body)
				autoMergeCalled = true
				if vars, ok := body["variables"].(map[string]any); ok {
					autoMergeMethod, _ = vars["method"].(string)
				}
				_, _ = w.Write([]byte(`{"data":{"enablePullRequestAutoMerge":{"pullRequest":{"id":"PR_1"}}}}`))
				return
			}
			p = strings.TrimPrefix(p, "/api/v3")
		}

		switch {
		case r.Method == http.MethodGet && p == "/repos/org/app":
			_, _ = w.Write([]byte(`{
				"default_branch": "main",
				"allow_squash_merge": true,
				"allow_merge_commit": false,
				"allow_rebase_merge": true,
				"permissions": {"push": false}
			}`))
		case r.Method == http.MethodGet && p == "/user":
			_, _ = w.Write([]byte(`{"login": "alice"}`))
		case r.Method == http.MethodGet && p == "/repos/alice/app":
			_, _ = w.Write([]byte(`{"fork": true, "name": "app", "parent": {"full_name": "org/app"}}`))
		case r.Method == http.MethodPost && p == "/repos/org/app/pulls":
			var body map[string]any
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			lastOpenPRHead, _ = body["head"].(string)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"number": 42,
				"node_id": "PR_42",
				"html_url": "https://github.com/org/app/pull/42",
				"state": "open",
				"draft": true,
				"head": {"ref": "feat/x", "sha": "abc123"},
				"base": {"ref": "main"}
			}`))
		case r.Method == http.MethodGet && p == "/repos/org/app/pulls":
			if r.URL.Query().Get("head") == "org:missing" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{
				"number": 42,
				"node_id": "PR_42",
				"html_url": "https://github.com/org/app/pull/42",
				"state": "open",
				"head": {"ref": "feat/x", "sha": "abc123"},
				"base": {"ref": "main"}
			}]`))
		case r.Method == http.MethodGet && p == "/repos/org/app/pulls/42":
			_, _ = w.Write([]byte(`{
				"number": 42,
				"node_id": "PR_42",
				"html_url": "https://github.com/org/app/pull/42",
				"state": "open",
				"draft": false,
				"auto_merge": {"enabled_by": {"login": "alice"}},
				"head": {"ref": "feat/x", "sha": "abc123"},
				"base": {"ref": "main"}
			}`))
		case r.Method == http.MethodGet && p == "/repos/org/app/commits/abc123/status":
			switch checksMode {
			case "none":
				_, _ = w.Write([]byte(`{"state":"pending","total_count":0}`))
			case "pending":
				_, _ = w.Write([]byte(`{"state":"pending","total_count":1}`))
			case "failed":
				_, _ = w.Write([]byte(`{"state":"failure","total_count":1}`))
			default:
				_, _ = w.Write([]byte(`{"state":"success","total_count":1}`))
			}
		case r.Method == http.MethodGet && p == "/repos/org/app/commits/abc123/check-runs":
			switch checksMode {
			case "none":
				_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
			case "pending":
				_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"status":"in_progress","conclusion":""}]}`))
			case "failed":
				_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"status":"completed","conclusion":"failure"}]}`))
			default:
				_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"status":"completed","conclusion":"success"}]}`))
			}
		case r.Method == http.MethodPut && p == "/repos/org/app/pulls/42/merge":
			if mergeStatus != http.StatusOK {
				w.WriteHeader(mergeStatus)
				_, _ = w.Write([]byte(`{"message":"` + mergeErrMsg + `"}`))
				return
			}
			_, _ = w.Write([]byte(`{"sha":"deadbeef","merged":true,"message":"Pull Request successfully merged"}`))
		case r.Method == http.MethodPost && p == "/graphql":
			var body map[string]any
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			autoMergeCalled = true
			if vars, ok := body["variables"].(map[string]any); ok {
				autoMergeMethod, _ = vars["method"].(string)
			}
			_, _ = w.Write([]byte(`{"data":{"enablePullRequestAutoMerge":{"pullRequest":{"id":"PR_42"}}}}`))
		case r.Method == http.MethodGet && p == "/repos/org/app/pulls/42/reviews":
			if r.URL.Query().Get("page") == "2" {
				_, _ = w.Write([]byte(`[
					{"id": 102, "state": "CHANGES_REQUESTED", "body": "Please fix lint", "submitted_at": "2026-11-24T10:05:00Z", "user": {"login": "bob"}}
				]`))
				return
			}
			w.Header().Set("Link", `<`+srv.URL+`/repos/org/app/pulls/42/reviews?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`[
				{"id": 101, "state": "COMMENTED", "body": "old review", "submitted_at": "2026-11-24T09:00:00Z", "user": {"login": "carol"}}
			]`))
		case r.Method == http.MethodGet && p == "/repos/org/app/pulls/42/comments":
			_, _ = w.Write([]byte(`[
				{"id": 201, "pull_request_review_id": 102, "body": "Use err check here", "path": "main.go", "line": 17, "created_at": "2026-11-24T10:06:00Z", "user": {"login": "bob"}}
			]`))
		case r.Method == http.MethodGet && p == "/repos/org/app/issues/42/comments":
			_, _ = w.Write([]byte(`[
				{"id": 301, "body": "General note", "created_at": "2026-11-24T10:07:00Z", "user": {"login": "dave"}}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	repo := forge.Repo{Kind: forge.GitHub, Host: "github.com", Owner: "org", Name: "app", APIBase: srv.URL}
	c := New(repo, "ghp_secret", srv.Client())

	if c.Kind() != forge.GitHub {
		t.Fatalf("Kind = %s", c.Kind())
	}

	// RepoInfo + fork detection
	info, err := c.RepoInfo(ctx, repo)
	if err != nil {
		t.Fatalf("RepoInfo: %v", err)
	}
	if lastAuthHeader != "Bearer ghp_secret" {
		t.Fatalf("auth header = %q", lastAuthHeader)
	}
	if info.DefaultBranch != "main" || info.CanPush || len(info.AllowedMethods) != 2 || info.AllowedMethods[0] != forge.Squash || info.AllowedMethods[1] != forge.Rebase {
		t.Fatalf("unexpected RepoInfo: %+v", info)
	}
	if info.Fork == nil || info.Fork.Owner != "alice" || info.Fork.Name != "app" {
		t.Fatalf("expected fork alice/app, got %+v", info.Fork)
	}

	// OpenPR from fork
	pr, err := c.OpenPR(ctx, repo, forge.OpenPRRequest{
		Head: "feat/x", HeadOwner: "alice", Base: "main", Title: "Add X", Body: "details", Draft: true,
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if lastOpenPRHead != "alice:feat/x" || pr.Number != 42 || !pr.Draft {
		t.Fatalf("OpenPR: head=%q pr=%+v", lastOpenPRHead, pr)
	}

	// FindPR found and not found
	found, err := c.FindPR(ctx, repo, "feat/x", "main")
	if err != nil || found.Number != 42 || found.Checks != forge.ChecksPassed {
		t.Fatalf("FindPR: %+v err=%v", found, err)
	}
	if _, err := c.FindPR(ctx, repo, "missing", "main"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("FindPR missing: err=%v, want ErrNotFound", err)
	}

	// GetPR checks states
	for _, tc := range []struct {
		mode string
		want forge.ChecksState
	}{
		{"none", forge.ChecksNone},
		{"pending", forge.ChecksPending},
		{"failed", forge.ChecksFailed},
		{"passed", forge.ChecksPassed},
	} {
		checksMode = tc.mode
		got, err := c.GetPR(ctx, repo, 42)
		if err != nil || got.Checks != tc.want || !got.AutoMerge {
			t.Fatalf("GetPR(%s): %+v err=%v", tc.mode, got, err)
		}
	}

	// MergePR immediate
	res, err := c.MergePR(ctx, repo, 42, forge.MergeRequest{Method: forge.Squash, CommitTitle: "Add X"})
	if err != nil || !res.Merged || res.SHA != "deadbeef" {
		t.Fatalf("MergePR: %+v err=%v", res, err)
	}

	// MergePR auto-merge when checks pending
	checksMode = "pending"
	res, err = c.MergePR(ctx, repo, 42, forge.MergeRequest{Method: forge.Rebase, WhenChecksPass: true})
	if err != nil || !res.AutoMergeSet || !autoMergeCalled || autoMergeMethod != "REBASE" {
		t.Fatalf("MergePR auto-merge: res=%+v called=%v method=%s err=%v", res, autoMergeCalled, autoMergeMethod, err)
	}

	// MergePR method not allowed & not mergeable
	checksMode = "passed"
	mergeStatus = http.StatusMethodNotAllowed
	mergeErrMsg = "Merge commits are not allowed on this repository."
	if _, err := c.MergePR(ctx, repo, 42, forge.MergeRequest{Method: forge.Merge}); !errors.Is(err, forge.ErrMethodNotAllowed) {
		t.Fatalf("expected ErrMethodNotAllowed, got %v", err)
	}
	mergeErrMsg = "Pull Request is not mergeable"
	if _, err := c.MergePR(ctx, repo, 42, forge.MergeRequest{Method: forge.Squash}); !errors.Is(err, forge.ErrNotMergeable) {
		t.Fatalf("expected ErrNotMergeable, got %v", err)
	}

	// ReviewComments with pagination + since filter + CHANGES_REQUESTED propagation
	since := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	comments, err := c.ReviewComments(ctx, repo, 42, since)
	if err != nil {
		t.Fatalf("ReviewComments: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("expected 3 comments after since, got %d: %+v", len(comments), comments)
	}
	if !comments[0].ChangesRequested || comments[0].ID != "review:102" {
		t.Fatalf("unexpected comment[0]: %+v", comments[0])
	}
	if !comments[1].ChangesRequested || comments[1].Path != "main.go" || comments[1].Line != 17 {
		t.Fatalf("unexpected comment[1]: %+v", comments[1])
	}
	if comments[2].ID != "issue:301" || comments[2].ChangesRequested {
		t.Fatalf("unexpected comment[2]: %+v", comments[2])
	}

	// CompareURL
	if u := c.CompareURL(repo, "feat/x", "main"); !strings.Contains(u, "https://github.com/org/app/compare/main...feat%2Fx") {
		t.Fatalf("CompareURL = %q", u)
	}

	// GitHub Enterprise /api/v3 -> /api/graphql
	enterpriseServer = true
	autoMergeCalled = false
	checksMode = "pending"
	gheRepo := forge.Repo{Kind: forge.GitHub, Host: "ghe.corp.example", Owner: "org", Name: "app", APIBase: srv.URL + "/api/v3"}
	gheClient := New(gheRepo, "ghe_tok", srv.Client())
	res, err = gheClient.MergePR(ctx, gheRepo, 42, forge.MergeRequest{Method: forge.Squash, WhenChecksPass: true})
	if err != nil || !res.AutoMergeSet || !autoMergeCalled {
		t.Fatalf("GHE auto-merge failed: res=%+v called=%v err=%v", res, autoMergeCalled, err)
	}
}
