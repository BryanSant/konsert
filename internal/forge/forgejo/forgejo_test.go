package forgejo

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/forge"
)

func TestForgejoClient(t *testing.T) {
	ctx := context.Background()
	var (
		statusState   = "success"
		mergeStatus   = http.StatusOK
		mergeErrMsg   = ""
		lastMergeBody map[string]any
		lastOpenHead  string
		lastAuth      string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path

		switch {
		case r.Method == http.MethodGet && p == "/api/v1/repos/org/demo":
			_, _ = w.Write([]byte(`{
				"default_branch": "main",
				"allow_squash_merge": true,
				"allow_merge_commits": true,
				"allow_rebase": false,
				"allow_rebase_explicit": true,
				"permissions": {"push": false}
			}`))
		case r.Method == http.MethodGet && p == "/api/v1/user":
			_, _ = w.Write([]byte(`{"login":"alice"}`))
		case r.Method == http.MethodGet && p == "/api/v1/repos/alice/demo":
			_, _ = w.Write([]byte(`{"fork":true,"name":"demo","parent":{"full_name":"org/demo"}}`))
		case r.Method == http.MethodPost && p == "/api/v1/repos/org/demo/pulls":
			var body map[string]any
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			lastOpenHead, _ = body["head"].(string)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"number": 11,
				"title": "WIP: Add Z",
				"html_url": "https://codeberg.org/org/demo/pulls/11",
				"state": "open",
				"head": {"ref": "feat/z", "sha": "111222"},
				"base": {"ref": "main"}
			}`))
		case r.Method == http.MethodGet && p == "/api/v1/repos/org/demo/pulls":
			_, _ = w.Write([]byte(`[{
				"number": 11,
				"title": "Add Z",
				"html_url": "https://codeberg.org/org/demo/pulls/11",
				"state": "open",
				"head": {"ref": "feat/z", "sha": "111222", "repo": {"owner": {"login": "alice"}}},
				"base": {"ref": "main"}
			}]`))
		case r.Method == http.MethodGet && p == "/api/v1/repos/org/demo/pulls/11":
			_, _ = w.Write([]byte(`{
				"number": 11,
				"title": "Add Z",
				"html_url": "https://codeberg.org/org/demo/pulls/11",
				"state": "open",
				"head": {"ref": "feat/z", "sha": "111222"},
				"base": {"ref": "main"}
			}`))
		case r.Method == http.MethodGet && p == "/api/v1/repos/org/demo/commits/111222/status":
			_, _ = w.Write([]byte(`{"state":"` + statusState + `","statuses":[{"status":"` + statusState + `"}]}`))
		case r.Method == http.MethodPost && p == "/api/v1/repos/org/demo/pulls/11/merge":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &lastMergeBody)
			if mergeStatus != http.StatusOK {
				w.WriteHeader(mergeStatus)
				_, _ = w.Write([]byte(`{"message":"` + mergeErrMsg + `"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && p == "/api/v1/repos/org/demo/pulls/11/reviews":
			_, _ = w.Write([]byte(`[
				{"id": 5, "state": "REQUEST_CHANGES", "body": "Needs tests", "comments_count": 1, "submitted_at": "2026-11-24T11:00:00Z", "user": {"login": "bob"}}
			]`))
		case r.Method == http.MethodGet && p == "/api/v1/repos/org/demo/pulls/11/reviews/5/comments":
			_, _ = w.Write([]byte(`[
				{"id": 50, "body": "Handle nil", "path": "x.go", "new_position": 9, "created_at": "2026-11-24T11:01:00Z", "user": {"login": "bob"}}
			]`))
		case r.Method == http.MethodGet && p == "/api/v1/repos/org/demo/issues/11/comments":
			_, _ = w.Write([]byte(`[
				{"id": 60, "body": "Looks promising", "created_at": "2026-11-24T11:02:00Z", "user": {"login": "carol"}}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	repo := forge.Repo{Kind: forge.Forgejo, Host: "codeberg.org", Owner: "org", Name: "demo", APIBase: srv.URL + "/api/v1"}
	c := New(repo, "fj-tok", srv.Client())

	if c.Kind() != forge.Forgejo {
		t.Fatalf("Kind = %s", c.Kind())
	}

	// RepoInfo
	info, err := c.RepoInfo(ctx, repo)
	if err != nil {
		t.Fatalf("RepoInfo: %v", err)
	}
	if lastAuth != "token fj-tok" {
		t.Fatalf("auth header = %q", lastAuth)
	}
	if info.DefaultBranch != "main" || info.CanPush || len(info.AllowedMethods) != 3 {
		t.Fatalf("unexpected RepoInfo: %+v", info)
	}
	if info.Fork == nil || info.Fork.Owner != "alice" {
		t.Fatalf("expected fork alice, got %+v", info.Fork)
	}

	// OpenPR from fork (draft)
	pr, err := c.OpenPR(ctx, repo, forge.OpenPRRequest{
		Head: "feat/z", HeadOwner: "alice", Base: "main", Title: "Add Z", Body: "body", Draft: true,
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if lastOpenHead != "alice:feat/z" || pr.Number != 11 || !pr.Draft {
		t.Fatalf("OpenPR: head=%q pr=%+v", lastOpenHead, pr)
	}

	// FindPR found and not found
	found, err := c.FindPR(ctx, repo, "alice:feat/z", "main")
	if err != nil || found.Number != 11 || found.Checks != forge.ChecksPassed {
		t.Fatalf("FindPR: %+v err=%v", found, err)
	}
	if _, err := c.FindPR(ctx, repo, "other", "main"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("FindPR missing: err=%v, want ErrNotFound", err)
	}

	// MergePR immediate
	res, err := c.MergePR(ctx, repo, 11, forge.MergeRequest{Method: forge.Squash, CommitTitle: "Add Z"})
	if err != nil || !res.Merged || lastMergeBody["Do"] != "squash" {
		t.Fatalf("MergePR: res=%+v body=%+v err=%v", res, lastMergeBody, err)
	}

	// MergePR when checks pending -> merge_when_checks_succeed
	statusState = "pending"
	res, err = c.MergePR(ctx, repo, 11, forge.MergeRequest{Method: forge.Rebase, WhenChecksPass: true})
	if err != nil || !res.AutoMergeSet || lastMergeBody["Do"] != "rebase" || lastMergeBody["merge_when_checks_succeed"] != true {
		t.Fatalf("MergePR auto-merge: res=%+v body=%+v err=%v", res, lastMergeBody, err)
	}

	// MergePR errors
	statusState = "success"
	mergeStatus = http.StatusMethodNotAllowed
	mergeErrMsg = "merge style not allowed"
	if _, err := c.MergePR(ctx, repo, 11, forge.MergeRequest{Method: forge.Merge}); !errors.Is(err, forge.ErrMethodNotAllowed) {
		t.Fatalf("expected ErrMethodNotAllowed, got %v", err)
	}
	mergeStatus = http.StatusConflict
	mergeErrMsg = "pull request has conflicts"
	if _, err := c.MergePR(ctx, repo, 11, forge.MergeRequest{Method: forge.Squash}); !errors.Is(err, forge.ErrNotMergeable) {
		t.Fatalf("expected ErrNotMergeable, got %v", err)
	}

	// ReviewComments
	since := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	comments, err := c.ReviewComments(ctx, repo, 11, since)
	if err != nil {
		t.Fatalf("ReviewComments: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("expected 3 comments, got %d: %+v", len(comments), comments)
	}
	if !comments[0].ChangesRequested || !comments[1].ChangesRequested || comments[1].Path != "x.go" || comments[1].Line != 9 {
		t.Fatalf("unexpected comments: %+v", comments)
	}

	// CompareURL
	if u := c.CompareURL(repo, "feat/z", "main"); u != "https://codeberg.org/org/demo/compare/main...feat%2Fz" {
		t.Fatalf("CompareURL = %q", u)
	}
}
