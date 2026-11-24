package gitutil

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// prRepo makes a git checkout on branch feat/x with the given origin URL ("" = none).
func prRepo(t *testing.T, origin string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "feat/x")
	if origin != "" {
		run("remote", "add", "origin", origin)
	}
	return dir
}

// fakeGH records gh calls and answers them from a script.
type fakeGH struct {
	calls    [][]string
	view     string
	viewE    error
	revE     error
	login    string
	loginE   error
	commentE error
}

func (f *fakeGH) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	switch {
	case args[0] == "api":
		return []byte(f.login + "\n"), f.loginE
	case len(args) > 1 && args[1] == "view":
		return []byte(f.view), f.viewE
	case len(args) > 1 && args[1] == "comment":
		return []byte("https://github.com/acme/wc/pull/12#issuecomment-1\n"), f.commentE
	}
	return nil, f.revE
}

func TestReviewPR(t *testing.T) {
	ctx := context.Background()
	open := `{"number":12,"url":"https://github.com/acme/wc/pull/12","state":"OPEN"}`
	ownPR := `{"number":12,"url":"https://github.com/acme/wc/pull/12","state":"OPEN","author":{"login":"bryan"}}`
	cases := []struct {
		name     string
		origin   string
		req      protocol.GitPRReviewRequest
		gh       fakeGH
		status   string
		msg      string
		lastCall []string
	}{
		{
			name: "approve", origin: "git@github.com:acme/wc.git",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewApprove}, gh: fakeGH{view: open},
			status: protocol.PRReviewSent, lastCall: []string{"pr", "review", "12", "--approve"},
		},
		{
			name: "approve with note on a named branch", origin: "https://github.com/acme/wc.git",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewApprove, Branch: "konsert/t-abc", Body: "LGTM"}, gh: fakeGH{view: open},
			status: protocol.PRReviewSent, lastCall: []string{"pr", "review", "12", "--approve", "--body", "LGTM"},
		},
		{
			name: "request changes", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewRequestChanges, Body: "Handle empty input"}, gh: fakeGH{view: open},
			status: protocol.PRReviewSent, lastCall: []string{"pr", "review", "12", "--request-changes", "--body", "Handle empty input"},
		},
		{
			name: "request changes needs a body", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewRequestChanges}, gh: fakeGH{view: open},
			status: protocol.PRReviewSent, lastCall: []string{"pr", "review", "12", "--request-changes", "--body", defaultChangesBody},
		},
		{
			name: "no remote", origin: "",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewApprove}, status: protocol.PRReviewNoGitHub,
		},
		{
			name: "not github", origin: "https://gitlab.com/acme/wc.git",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewApprove}, status: protocol.PRReviewNoGitHub,
		},
		{
			name: "no gh", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewApprove}, gh: fakeGH{viewE: ErrNoGH}, status: protocol.PRReviewNoGH,
		},
		{
			name: "no PR", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewApprove},
			gh:  fakeGH{viewE: errors.New(`no pull requests found for branch "feat/x"`)}, status: protocol.PRReviewNoPR,
		},
		{
			name: "merged PR", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewApprove},
			gh:  fakeGH{view: `{"number":3,"url":"u","state":"MERGED"}`}, status: protocol.PRReviewFailed, msg: "PR #3 is merged",
		},
		{
			name: "GitHub refuses", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewApprove},
			gh:  fakeGH{view: open, revE: errors.New("Pull request is in draft state")}, status: protocol.PRReviewFailed,
			msg: "draft",
		},
		{
			name: "own PR approve comments instead", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewApprove, By: "Bryan's iPhone"},
			gh:  fakeGH{view: ownPR, login: "Bryan"}, status: protocol.PRReviewCommented, msg: "doesn't let you approve your own pull request",
			lastCall: []string{"pr", "comment", "12", "--body", "Approved in Konsert by Bryan's iPhone."},
		},
		{
			name: "own PR request changes posts the note", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewRequestChanges, Body: "Handle empty input", By: "laptop"},
			gh:  fakeGH{view: ownPR, login: "bryan"}, status: protocol.PRReviewCommented, msg: "request changes on your own pull request",
			lastCall: []string{"pr", "comment", "12", "--body", "Changes requested in Konsert by laptop.\n\nHandle empty input"},
		},
		{
			name: "someone else's PR is reviewed", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewApprove},
			gh:  fakeGH{view: ownPR, login: "alice"}, status: protocol.PRReviewSent,
			lastCall: []string{"pr", "review", "12", "--approve"},
		},
		{
			name: "login unknown: GitHub's refusal falls back to a comment", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewApprove, Body: "Ship it"},
			gh: fakeGH{view: ownPR, loginE: errors.New("HTTP 401"),
				revE: errors.New("failed to create review: GraphQL: Review Can not approve your own pull request (addPullRequestReview)")},
			status: protocol.PRReviewCommented, msg: "added a comment instead",
			lastCall: []string{"pr", "comment", "12", "--body", "Approved in Konsert by a Konsert user.\n\nShip it"},
		},
		{
			name: "request changes refusal falls back too", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewRequestChanges, Body: "Rename it"},
			gh: fakeGH{view: open,
				revE: errors.New("GraphQL: Review Can not request changes on your own pull request (addPullRequestReview)")},
			status: protocol.PRReviewCommented, msg: "request changes on your own pull request",
			lastCall: []string{"pr", "comment", "12", "--body", "Changes requested in Konsert by a Konsert user.\n\nRename it"},
		},
		{
			name: "own PR comment fails", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewApprove},
			gh:  fakeGH{view: ownPR, login: "bryan", commentE: errors.New("HTTP 403")}, status: protocol.PRReviewFailed,
			msg: "adding a comment failed: HTTP 403",
		},
		{
			name: "auth failure on view", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: protocol.PRReviewApprove},
			gh:  fakeGH{viewE: errors.New("gh auth login required")}, status: protocol.PRReviewFailed, msg: "gh auth login required",
		},
		{
			name: "bad action", origin: "https://github.com/acme/wc",
			req: protocol.GitPRReviewRequest{Action: "merge"}, status: protocol.PRReviewFailed, msg: "unknown review action",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := prRepo(t, tc.origin)
			gh := tc.gh
			res := (&PRReviewer{GH: gh.run}).Review(ctx, dir, tc.req)
			if res.Status != tc.status || !strings.Contains(res.Message, tc.msg) {
				t.Fatalf("ReviewPR = %+v, want status %s with %q", res, tc.status, tc.msg)
			}
			if tc.lastCall != nil {
				if len(gh.calls) == 0 || !slices.Equal(gh.calls[len(gh.calls)-1], tc.lastCall) {
					t.Fatalf("gh calls %q, want last %q", gh.calls, tc.lastCall)
				}
				wantBranch := tc.req.Branch
				if wantBranch == "" {
					wantBranch = "feat/x"
				}
				if gh.calls[0][2] != wantBranch {
					t.Fatalf("gh pr view of %q, want %q", gh.calls[0][2], wantBranch)
				}
				if res.PRNumber != 12 || res.PRURL == "" {
					t.Fatalf("result %+v lacks the PR", res)
				}
			}
			if tc.status == protocol.PRReviewNoGitHub && len(gh.calls) > 0 {
				t.Fatalf("gh was called without a GitHub remote: %q", gh.calls)
			}
		})
	}
}

// The gh login is looked up once per machine (per reviewer) and again after the TTL;
// failures are not cached.
func TestPRReviewerCachesLogin(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{loginE: errors.New("offline")}
	p := &PRReviewer{GH: gh.run, Now: func() time.Time { return now }}
	if _, err := p.Login(ctx, "."); err == nil {
		t.Fatal("failed lookup returned a login")
	}
	gh.loginE, gh.login = nil, "bryan"
	for range 3 {
		if l, err := p.Login(ctx, "."); err != nil || l != "bryan" {
			t.Fatalf("Login = %q, %v", l, err)
		}
	}
	if len(gh.calls) != 2 {
		t.Fatalf("gh api user ran %d times, want 2 (one failure, one cached success)", len(gh.calls))
	}
	gh.login = "alice"
	now = now.Add(ghLoginTTL + time.Second)
	if l, _ := p.Login(ctx, "."); l != "alice" {
		t.Fatalf("Login after TTL = %q, want a fresh lookup", l)
	}
	if !slices.Equal(gh.calls[0], []string{"api", "user", "--jq", ".login"}) {
		t.Fatalf("login call %q", gh.calls[0])
	}
}
