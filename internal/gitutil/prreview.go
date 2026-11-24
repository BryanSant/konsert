package gitutil

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

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

// ghLoginTTL is how long the gh login of this machine is trusted before it is looked up
// again (gh auth switch changes it).
const ghLoginTTL = time.Hour

// Messages of a review on the user's own pull request, which GitHub refuses: the decision
// goes to the PR as a comment instead.
const (
	OwnPRApproveMessage = "GitHub doesn't let you approve your own pull request — added a comment instead."
	OwnPRChangesMessage = "GitHub doesn't let you request changes on your own pull request — added a comment instead."
)

// PRReviewer sends review decisions to pull requests with gh. One per machine: it caches
// the gh login (gh api user) so reviewing your own PR is detected without asking GitHub
// every time.
type PRReviewer struct {
	// GH runs gh; nil = gh from PATH. Tests replace it so the real gh is never called.
	GH GHRunner
	// Now is the clock of the login cache; nil = time.Now.
	Now func() time.Time

	mu      sync.Mutex
	login   string
	loginAt time.Time
}

func (p *PRReviewer) gh() GHRunner {
	if p.GH == nil {
		return ExecGH
	}
	return p.GH
}

func (p *PRReviewer) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// Login returns the GitHub login gh is authenticated as on this machine (cached for
// ghLoginTTL). A failed lookup is not cached.
func (p *PRReviewer) Login(ctx context.Context, dir string) (string, error) {
	p.mu.Lock()
	if p.login != "" && p.now().Sub(p.loginAt) < ghLoginTTL {
		l := p.login
		p.mu.Unlock()
		return l, nil
	}
	p.mu.Unlock()
	out, err := p.gh()(ctx, dir, "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	login := strings.TrimSpace(string(out))
	if login == "" {
		return "", errors.New("gh api user: no login")
	}
	p.mu.Lock()
	p.login, p.loginAt = login, p.now()
	p.mu.Unlock()
	return login, nil
}

// Review approves (or requests changes on) the pull request of the checkout's branch
// with gh: nothing is sent when the checkout has no GitHub remote, gh is missing or the
// branch has no open PR, and the result says which. GitHub does not allow reviewing
// your own pull request, so on your own PR the decision is posted as a comment instead
// (status commented): detected up front from the PR author and the gh login, and from
// GitHub's refusal when the login could not be looked up.
func (p *PRReviewer) Review(ctx context.Context, dir string, req protocol.GitPRReviewRequest) protocol.GitPRReviewResult {
	fail := func(format string, a ...any) protocol.GitPRReviewResult {
		return protocol.GitPRReviewResult{Status: protocol.PRReviewFailed, Message: fmt.Sprintf(format, a...)}
	}
	if req.Action != protocol.PRReviewApprove && req.Action != protocol.PRReviewRequestChanges {
		return fail("unknown review action %q", req.Action)
	}
	gh := p.gh()
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
	raw, err := gh(ctx, dir, "pr", "view", branch, "--json", "number,url,state,author")
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
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil || pr.Number == 0 {
		return fail("gh pr view %s: unexpected output", branch)
	}
	res := protocol.GitPRReviewResult{PRNumber: pr.Number, PRURL: pr.URL}
	if !strings.EqualFold(pr.State, "OPEN") {
		res.Status, res.Message = protocol.PRReviewFailed, fmt.Sprintf("PR #%d is %s", pr.Number, strings.ToLower(pr.State))
		return res
	}
	body := strings.TrimSpace(req.Body)
	if pr.Author.Login != "" {
		if login, err := p.Login(ctx, dir); err == nil && strings.EqualFold(login, pr.Author.Login) {
			return p.commentInstead(ctx, dir, req, body, res)
		}
	}
	args := []string{"pr", "review", fmt.Sprint(pr.Number)}
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
		if IsOwnPRRefusal(err) {
			return p.commentInstead(ctx, dir, req, strings.TrimSpace(req.Body), res)
		}
		res.Status, res.Message = protocol.PRReviewFailed, fmt.Sprintf("gh pr review #%d: %v", pr.Number, err)
		return res
	}
	res.Status = protocol.PRReviewSent
	return res
}

// commentInstead posts the decision on the user's own pull request as a comment.
func (p *PRReviewer) commentInstead(ctx context.Context, dir string, req protocol.GitPRReviewRequest, note string, res protocol.GitPRReviewResult) protocol.GitPRReviewResult {
	body, msg := OwnPRComment(req.Action, req.By, note)
	if _, err := p.gh()(ctx, dir, "pr", "comment", fmt.Sprint(res.PRNumber), "--body", body); err != nil {
		res.Status = protocol.PRReviewFailed
		res.Message = fmt.Sprintf("GitHub doesn't let you review your own pull request, and adding a comment failed: %v", err)
		return res
	}
	res.Status, res.Message = protocol.PRReviewCommented, msg
	return res
}

// OwnPRComment is the comment posted on the user's own pull request for a decision
// (action) by the named person or device, with the user's note, and the message that
// tells the user so.
func OwnPRComment(action, by, note string) (body, message string) {
	by = strings.TrimSpace(by)
	if by == "" {
		by = "a Konsert user"
	}
	if action == protocol.PRReviewApprove {
		body = "Approved in Konsert by " + by + "."
		if note != "" {
			body += "\n\n" + note
		}
		return body, OwnPRApproveMessage
	}
	body = "Changes requested in Konsert by " + by + "."
	if note != "" {
		body += "\n\n" + note
	}
	return body, OwnPRChangesMessage
}

// IsOwnPRRefusal reports whether a gh pr review error is GitHub refusing a review of the
// caller's own pull request ("Can not approve your own pull request", "Can not request
// changes on your own pull request").
func IsOwnPRRefusal(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "your own pull request")
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
