package gitutil

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/BryanSant/konsert/internal/protocol"
)

// GHRunner runs gh with args in dir and returns its stdout; a failure's error carries
// gh's stderr. Replaceable so tests never call the real gh or GitHub.
type GHRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// ErrNoGH means gh is not installed on this machine.
var ErrNoGH = errors.New("gh is not installed on this machine")

// ExecGH runs gh from PATH.
func ExecGH(ctx context.Context, dir string, args ...string) ([]byte, error) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return nil, ErrNoGH
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, errors.New(msg)
	}
	return out, nil
}

// defaultChangesBody is the review body when changes are requested without a note (gh
// requires one).
const defaultChangesBody = "Changes requested in Konsert."

// ReviewPR approves (or requests changes on) the pull request of the checkout's branch
// with gh: nothing is sent when the checkout has no GitHub remote, gh is missing or the
// branch has no open PR, and the result says which.
func ReviewPR(ctx context.Context, dir string, req protocol.GitPRReviewRequest, gh GHRunner) protocol.GitPRReviewResult {
	fail := func(format string, a ...any) protocol.GitPRReviewResult {
		return protocol.GitPRReviewResult{Status: protocol.PRReviewFailed, Message: fmt.Sprintf(format, a...)}
	}
	if req.Action != protocol.PRReviewApprove && req.Action != protocol.PRReviewRequestChanges {
		return fail("unknown review action %q", req.Action)
	}
	if gh == nil {
		gh = ExecGH
	}
	remotes, err := runGit(ctx, dir, "remote", "-v")
	if err != nil {
		return fail("%v", err)
	}
	if !hasGitHubRemote(remotes) {
		return protocol.GitPRReviewResult{Status: protocol.PRReviewNoGitHub, Message: "the checkout has no GitHub remote"}
	}
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		if branch, err = runGit(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD"); err != nil || branch == "" {
			return fail("the checkout is not on a branch (detached HEAD)")
		}
	}
	raw, err := gh(ctx, dir, "pr", "view", branch, "--json", "number,url,state")
	switch {
	case errors.Is(err, ErrNoGH):
		return protocol.GitPRReviewResult{Status: protocol.PRReviewNoGH, Message: ErrNoGH.Error()}
	case err != nil && strings.Contains(strings.ToLower(err.Error()), "no pull requests found"):
		return protocol.GitPRReviewResult{Status: protocol.PRReviewNoPR, Message: "no pull request for branch " + branch}
	case err != nil:
		return fail("gh pr view %s: %v", branch, err)
	}
	var pr struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil || pr.Number == 0 {
		return fail("gh pr view %s: unexpected output", branch)
	}
	res := protocol.GitPRReviewResult{PRNumber: pr.Number, PRURL: pr.URL}
	if !strings.EqualFold(pr.State, "OPEN") {
		res.Status, res.Message = protocol.PRReviewFailed, fmt.Sprintf("PR #%d is %s", pr.Number, strings.ToLower(pr.State))
		return res
	}
	args := []string{"pr", "review", fmt.Sprint(pr.Number)}
	body := strings.TrimSpace(req.Body)
	if req.Action == protocol.PRReviewApprove {
		args = append(args, "--approve")
	} else {
		args = append(args, "--request-changes")
		if body == "" {
			body = defaultChangesBody
		}
	}
	if body != "" {
		args = append(args, "--body", body)
	}
	if _, err := gh(ctx, dir, args...); err != nil {
		res.Status, res.Message = protocol.PRReviewFailed, fmt.Sprintf("gh pr review #%d: %v", pr.Number, err)
		return res
	}
	res.Status = protocol.PRReviewSent
	return res
}

// hasGitHubRemote reports whether `git remote -v` output names a github.com remote.
func hasGitHubRemote(remotes string) bool {
	for line := range strings.SplitSeq(remotes, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && (strings.Contains(f[1], "github.com:") || strings.Contains(f[1], "github.com/")) {
			return true
		}
	}
	return false
}
