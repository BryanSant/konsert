package gitutil

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

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
	calls [][]string
	view  string
	viewE error
	revE  error
}

func (f *fakeGH) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if len(args) > 1 && args[1] == "view" {
		return []byte(f.view), f.viewE
	}
	return nil, f.revE
}

func TestReviewPR(t *testing.T) {
	ctx := context.Background()
	open := `{"number":12,"url":"https://github.com/acme/wc/pull/12","state":"OPEN"}`
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
			gh:  fakeGH{view: open, revE: errors.New("Can not approve your own pull request")}, status: protocol.PRReviewFailed,
			msg: "Can not approve your own pull request",
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
			res := ReviewPR(ctx, dir, tc.req, gh.run)
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
