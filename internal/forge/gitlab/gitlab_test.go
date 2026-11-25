package gitlab

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

func TestGitLabClient(t *testing.T) {
	ctx := context.Background()
	var (
		pipelineStatus = "success"
		mergeStatus    = http.StatusOK
		mergeErrMsg    = ""
		lastMergeBody  map[string]any
		lastForkMRPath string
		lastTargetID   float64
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rawPath := r.URL.EscapedPath()

		switch {
		case r.Method == http.MethodGet && rawPath == "/api/v4/projects/group%2Fsub%2Fapp":
			_, _ = w.Write([]byte(`{
				"id": 99,
				"path": "app",
				"default_branch": "main",
				"merge_method": "rebase_merge",
				"squash_option": "default_on",
				"namespace": {"full_path": "group/sub"},
				"permissions": {"project_access": {"access_level": 20}, "group_access": {"access_level": 30}}
			}`))
		case r.Method == http.MethodGet && rawPath == "/api/v4/projects/group%2Fsub%2Fapp/forks":
			_, _ = w.Write([]byte(`[{
				"id": 100,
				"path": "app",
				"namespace": {"full_path": "alice"}
			}]`))
		case r.Method == http.MethodPost && rawPath == "/api/v4/projects/alice%2Fapp/merge_requests":
			lastForkMRPath = rawPath
			var body map[string]any
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			lastTargetID, _ = body["target_project_id"].(float64)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"iid": 7,
				"web_url": "https://gitlab.example.com/group/sub/app/-/merge_requests/7",
				"state": "opened",
				"draft": true,
				"source_branch": "feat/y",
				"target_branch": "main"
			}`))
		case r.Method == http.MethodGet && rawPath == "/api/v4/projects/group%2Fsub%2Fapp/merge_requests":
			if r.URL.Query().Get("source_branch") == "missing" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{
				"iid": 7,
				"web_url": "https://gitlab.example.com/group/sub/app/-/merge_requests/7",
				"state": "opened",
				"source_branch": "feat/y",
				"target_branch": "main",
				"head_pipeline": {"status": "` + pipelineStatus + `"}
			}]`))
		case r.Method == http.MethodGet && rawPath == "/api/v4/projects/group%2Fsub%2Fapp/merge_requests/7":
			_, _ = w.Write([]byte(`{
				"iid": 7,
				"web_url": "https://gitlab.example.com/group/sub/app/-/merge_requests/7",
				"state": "opened",
				"draft": false,
				"source_branch": "feat/y",
				"target_branch": "main",
				"merge_when_pipeline_succeeds": false,
				"head_pipeline": {"status": "` + pipelineStatus + `"}
			}`))
		case r.Method == http.MethodPut && rawPath == "/api/v4/projects/group%2Fsub%2Fapp/merge_requests/7/merge":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &lastMergeBody)
			if mergeStatus != http.StatusOK {
				w.WriteHeader(mergeStatus)
				_, _ = w.Write([]byte(`{"message":"` + mergeErrMsg + `"}`))
				return
			}
			if mwps, _ := lastMergeBody["merge_when_pipeline_succeeds"].(bool); mwps {
				_, _ = w.Write([]byte(`{"iid": 7, "state": "opened", "merge_when_pipeline_succeeds": true}`))
				return
			}
			_, _ = w.Write([]byte(`{"iid": 7, "state": "merged", "merge_commit_sha": "cafebabe"}`))
		case r.Method == http.MethodGet && rawPath == "/api/v4/projects/group%2Fsub%2Fapp/merge_requests/7/discussions":
			if r.URL.Query().Get("page") == "2" {
				_, _ = w.Write([]byte(`[
					{"id":"d2","notes":[
						{"id":2,"body":"Please rename","system":false,"resolvable":true,"resolved":false,"created_at":"2026-11-24T10:10:00Z","author":{"username":"bob"},"position":{"new_path":"pkg/a.go","new_line":42}}
					]}
				]`))
				return
			}
			w.Header().Set("X-Next-Page", "2")
			_, _ = w.Write([]byte(`[
				{"id":"d1","notes":[
					{"id":1,"body":"system event","system":true,"resolvable":false,"resolved":false,"created_at":"2026-11-24T10:05:00Z","author":{"username":"bot"}},
					{"id":3,"body":"LGTM note","system":false,"resolvable":true,"resolved":true,"created_at":"2026-11-24T10:06:00Z","author":{"username":"carol"}}
				]}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	repo := forge.Repo{Kind: forge.GitLab, Host: "gitlab.example.com", Owner: "group/sub", Name: "app", APIBase: srv.URL + "/api/v4"}
	c := New(repo, "glpat-secret", srv.Client())

	if c.Kind() != forge.GitLab {
		t.Fatalf("Kind = %s", c.Kind())
	}

	// RepoInfo
	info, err := c.RepoInfo(ctx, repo)
	if err != nil {
		t.Fatalf("RepoInfo: %v", err)
	}
	if info.DefaultBranch != "main" || !info.CanPush || len(info.AllowedMethods) != 2 || info.AllowedMethods[0] != forge.Squash || info.AllowedMethods[1] != forge.Rebase {
		t.Fatalf("unexpected RepoInfo: %+v", info)
	}
	if info.Fork == nil || info.Fork.Owner != "alice" {
		t.Fatalf("expected fork alice, got %+v", info.Fork)
	}

	// OpenPR from fork
	pr, err := c.OpenPR(ctx, repo, forge.OpenPRRequest{
		Head: "feat/y", HeadOwner: "alice", Base: "main", Title: "Add Y", Body: "desc", Draft: true,
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if lastForkMRPath != "/api/v4/projects/alice%2Fapp/merge_requests" || lastTargetID != 99 || pr.Number != 7 || !pr.Draft {
		t.Fatalf("fork OpenPR: path=%q target=%v pr=%+v", lastForkMRPath, lastTargetID, pr)
	}

	// FindPR
	found, err := c.FindPR(ctx, repo, "feat/y", "main")
	if err != nil || found.Number != 7 || found.Checks != forge.ChecksPassed {
		t.Fatalf("FindPR: %+v err=%v", found, err)
	}
	if _, err := c.FindPR(ctx, repo, "missing", "main"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("FindPR missing: err=%v, want ErrNotFound", err)
	}

	// MergePR immediate
	res, err := c.MergePR(ctx, repo, 7, forge.MergeRequest{Method: forge.Squash, CommitTitle: "Add Y"})
	if err != nil || !res.Merged || res.SHA != "cafebabe" {
		t.Fatalf("MergePR: %+v err=%v", res, err)
	}

	// MergePR when pipeline running -> MWPS
	pipelineStatus = "running"
	res, err = c.MergePR(ctx, repo, 7, forge.MergeRequest{Method: forge.Squash, WhenChecksPass: true})
	if err != nil || !res.AutoMergeSet {
		t.Fatalf("MergePR MWPS: %+v err=%v", res, err)
	}

	// MergePR method not allowed & not mergeable
	pipelineStatus = "success"
	mergeStatus = http.StatusUnprocessableEntity
	mergeErrMsg = "Squash is not allowed for this project"
	if _, err := c.MergePR(ctx, repo, 7, forge.MergeRequest{Method: forge.Squash}); !errors.Is(err, forge.ErrMethodNotAllowed) {
		t.Fatalf("expected ErrMethodNotAllowed, got %v", err)
	}
	mergeStatus = http.StatusMethodNotAllowed
	mergeErrMsg = "Branch cannot be merged due to conflicts"
	if _, err := c.MergePR(ctx, repo, 7, forge.MergeRequest{Method: forge.Merge}); !errors.Is(err, forge.ErrNotMergeable) {
		t.Fatalf("expected ErrNotMergeable, got %v", err)
	}

	// ReviewComments paginated discussions
	since := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	comments, err := c.ReviewComments(ctx, repo, 7, since)
	if err != nil {
		t.Fatalf("ReviewComments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 non-system notes, got %d: %+v", len(comments), comments)
	}
	if comments[0].ChangesRequested || comments[0].Author != "carol" {
		t.Fatalf("unexpected comment[0]: %+v", comments[0])
	}
	if !comments[1].ChangesRequested || comments[1].Path != "pkg/a.go" || comments[1].Line != 42 {
		t.Fatalf("unexpected comment[1]: %+v", comments[1])
	}

	// CompareURL
	if u := c.CompareURL(repo, "feat/y", "main"); !strings.Contains(u, "https://gitlab.example.com/group/sub/app/-/merge_requests/new") {
		t.Fatalf("CompareURL = %q", u)
	}
}
