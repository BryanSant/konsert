// Package github implements forge.Client for GitHub and GitHub Enterprise Server
// using the GitHub REST API (and GraphQL only for enablePullRequestAutoMerge).
package github

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/forge"
)

const (
	apiVersionHeader = "2022-11-28"
	defaultTimeout   = 30 * time.Second
	maxResponseBytes = 4 << 20
)

// Client is an authenticated GitHub forge.Client. Safe for concurrent use.
type Client struct {
	apiBase    string
	token      string
	httpClient *http.Client
}

// New creates a GitHub Client for r using token. If httpClient is nil, a default client
// with a 30s timeout is used.
func New(r forge.Repo, token string, httpClient *http.Client) *Client {
	base := strings.TrimRight(strings.TrimSpace(r.APIBase), "/")
	if base == "" {
		base = forge.APIBaseFor("https", forge.GitHub, r.Host)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		apiBase:    base,
		token:      token,
		httpClient: httpClient,
	}
}

// Factory adapts New to forge.ClientFactory.
func Factory(r forge.Repo, token string, httpClient *http.Client) forge.Client {
	return New(r, token, httpClient)
}

func (c *Client) Kind() forge.Kind { return forge.GitHub }

func (c *Client) graphqlURL() string {
	if strings.HasSuffix(c.apiBase, "/api/v3") {
		return strings.TrimSuffix(c.apiBase, "/api/v3") + "/api/graphql"
	}
	return c.apiBase + "/graphql"
}

func (c *Client) do(ctx context.Context, method, fullURL string, reqBody any) ([]byte, http.Header, int, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("github: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersionHeader)
	req.Header.Set("User-Agent", "syre")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("github: request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.Header, resp.StatusCode, fmt.Errorf("github: read response: %w", err)
	}
	return data, resp.Header, resp.StatusCode, nil
}

func parseErrorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return e.Message
	}
	return ""
}

func mapHTTPError(status int, body []byte, op string) error {
	msg := parseErrorMessage(body)
	detail := fmt.Sprintf("github %s: HTTP %d", op, status)
	if msg != "" {
		detail += ": " + msg
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", forge.ErrForbidden, detail)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", forge.ErrNotFound, detail)
	default:
		return errors.New(detail)
	}
}

func repoPath(r forge.Repo) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

func (c *Client) RepoInfo(ctx context.Context, r forge.Repo) (forge.RepoInfo, error) {
	data, _, status, err := c.do(ctx, http.MethodGet, c.apiBase+repoPath(r), nil)
	if err != nil {
		return forge.RepoInfo{}, err
	}
	if status != http.StatusOK {
		return forge.RepoInfo{}, mapHTTPError(status, data, "repo info")
	}

	var raw struct {
		DefaultBranch    string `json:"default_branch"`
		AllowSquashMerge bool   `json:"allow_squash_merge"`
		AllowMergeCommit bool   `json:"allow_merge_commit"`
		AllowRebaseMerge bool   `json:"allow_rebase_merge"`
		Permissions      struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return forge.RepoInfo{}, fmt.Errorf("github: decode repo info: %w", err)
	}

	var methods []forge.MergeMethod
	if raw.AllowSquashMerge {
		methods = append(methods, forge.Squash)
	}
	if raw.AllowMergeCommit {
		methods = append(methods, forge.Merge)
	}
	if raw.AllowRebaseMerge {
		methods = append(methods, forge.Rebase)
	}

	info := forge.RepoInfo{
		DefaultBranch:  raw.DefaultBranch,
		AllowedMethods: methods,
		CanPush:        raw.Permissions.Push,
	}

	// Check for the authenticated user's fork when they don't own the repo.
	if fork := c.findUserFork(ctx, r); fork != nil {
		info.Fork = fork
	}
	return info, nil
}

func (c *Client) findUserFork(ctx context.Context, r forge.Repo) *forge.Repo {
	userData, _, status, err := c.do(ctx, http.MethodGet, c.apiBase+"/user", nil)
	if err != nil || status != http.StatusOK {
		return nil
	}
	var user struct {
		Login string `json:"login"`
	}
	if json.Unmarshal(userData, &user) != nil || user.Login == "" {
		return nil
	}
	if strings.EqualFold(user.Login, r.Owner) {
		return nil
	}
	forkURL := c.apiBase + "/repos/" + url.PathEscape(user.Login) + "/" + url.PathEscape(r.Name)
	forkData, _, fStatus, fErr := c.do(ctx, http.MethodGet, forkURL, nil)
	if fErr != nil || fStatus != http.StatusOK {
		return nil
	}
	var forkRepo struct {
		Fork   bool   `json:"fork"`
		Name   string `json:"name"`
		Parent *struct {
			FullName string `json:"full_name"`
		} `json:"parent"`
	}
	if json.Unmarshal(forkData, &forkRepo) != nil || !forkRepo.Fork || forkRepo.Parent == nil {
		return nil
	}
	if !strings.EqualFold(forkRepo.Parent.FullName, r.FullName()) {
		return nil
	}
	name := forkRepo.Name
	if name == "" {
		name = r.Name
	}
	return &forge.Repo{
		Kind:    forge.GitHub,
		Host:    r.Host,
		Owner:   user.Login,
		Name:    name,
		APIBase: c.apiBase,
	}
}

type ghPR struct {
	Number    int        `json:"number"`
	NodeID    string     `json:"node_id"`
	HTMLURL   string     `json:"html_url"`
	State     string     `json:"state"`
	Draft     bool       `json:"draft"`
	Merged    bool       `json:"merged"`
	MergedAt  *time.Time `json:"merged_at"`
	AutoMerge any        `json:"auto_merge"`
	Head      struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p ghPR) toForgePR(checks forge.ChecksState) forge.PR {
	state := forge.PRClosed
	switch {
	case p.Merged || p.MergedAt != nil:
		state = forge.PRMerged
	case strings.EqualFold(p.State, "open"):
		state = forge.PROpen
	}
	return forge.PR{
		Number:    p.Number,
		URL:       p.HTMLURL,
		State:     state,
		Draft:     p.Draft,
		Head:      p.Head.Ref,
		Base:      p.Base.Ref,
		Checks:    checks,
		AutoMerge: p.AutoMerge != nil,
		MergedAt:  p.MergedAt,
	}
}

func (c *Client) OpenPR(ctx context.Context, r forge.Repo, req forge.OpenPRRequest) (forge.PR, error) {
	head := req.Head
	if req.HeadOwner != "" && !strings.EqualFold(req.HeadOwner, r.Owner) && !strings.Contains(head, ":") {
		head = req.HeadOwner + ":" + head
	}
	payload := map[string]any{
		"title": req.Title,
		"body":  req.Body,
		"head":  head,
		"base":  req.Base,
		"draft": req.Draft,
	}
	data, _, status, err := c.do(ctx, http.MethodPost, c.apiBase+repoPath(r)+"/pulls", payload)
	if err != nil {
		return forge.PR{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return forge.PR{}, mapHTTPError(status, data, "open PR")
	}
	var raw ghPR
	if err := json.Unmarshal(data, &raw); err != nil {
		return forge.PR{}, fmt.Errorf("github: decode PR: %w", err)
	}
	return raw.toForgePR(forge.ChecksNone), nil
}

func (c *Client) FindPR(ctx context.Context, r forge.Repo, head, base string) (forge.PR, error) {
	headFilter := head
	if !strings.Contains(headFilter, ":") {
		headFilter = r.Owner + ":" + headFilter
	}
	q := url.Values{}
	q.Set("state", "open")
	q.Set("head", headFilter)
	if base != "" {
		q.Set("base", base)
	}
	data, _, status, err := c.do(ctx, http.MethodGet, c.apiBase+repoPath(r)+"/pulls?"+q.Encode(), nil)
	if err != nil {
		return forge.PR{}, err
	}
	if status != http.StatusOK {
		return forge.PR{}, mapHTTPError(status, data, "find PR")
	}
	var list []ghPR
	if err := json.Unmarshal(data, &list); err != nil {
		return forge.PR{}, fmt.Errorf("github: decode PR list: %w", err)
	}
	if len(list) == 0 {
		return forge.PR{}, fmt.Errorf("%w: no open PR for %s -> %s", forge.ErrNotFound, head, base)
	}
	checks := c.checksForSHA(ctx, r, list[0].Head.SHA)
	return list[0].toForgePR(checks), nil
}

func (c *Client) getPRRaw(ctx context.Context, r forge.Repo, number int) (ghPR, error) {
	endpoint := fmt.Sprintf("%s%s/pulls/%d", c.apiBase, repoPath(r), number)
	data, _, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ghPR{}, err
	}
	if status != http.StatusOK {
		return ghPR{}, mapHTTPError(status, data, "get PR")
	}
	var raw ghPR
	if err := json.Unmarshal(data, &raw); err != nil {
		return ghPR{}, fmt.Errorf("github: decode PR: %w", err)
	}
	return raw, nil
}

func (c *Client) GetPR(ctx context.Context, r forge.Repo, number int) (forge.PR, error) {
	raw, err := c.getPRRaw(ctx, r, number)
	if err != nil {
		return forge.PR{}, err
	}
	checks := c.checksForSHA(ctx, r, raw.Head.SHA)
	return raw.toForgePR(checks), nil
}

func (c *Client) checksForSHA(ctx context.Context, r forge.Repo, sha string) forge.ChecksState {
	if sha == "" {
		return forge.ChecksNone
	}

	var hasAny, hasPending, hasFailed bool

	// 1. Combined commit status
	statusURL := fmt.Sprintf("%s%s/commits/%s/status", c.apiBase, repoPath(r), url.PathEscape(sha))
	if data, _, code, err := c.do(ctx, http.MethodGet, statusURL, nil); err == nil && code == http.StatusOK {
		var cs struct {
			State      string `json:"state"`
			TotalCount int    `json:"total_count"`
		}
		if json.Unmarshal(data, &cs) == nil && cs.TotalCount > 0 {
			hasAny = true
			switch cs.State {
			case "failure", "error":
				hasFailed = true
			case "pending":
				hasPending = true
			}
		}
	}

	// 2. Check runs
	runsURL := fmt.Sprintf("%s%s/commits/%s/check-runs", c.apiBase, repoPath(r), url.PathEscape(sha))
	if data, _, code, err := c.do(ctx, http.MethodGet, runsURL, nil); err == nil && code == http.StatusOK {
		var cr struct {
			TotalCount int `json:"total_count"`
			CheckRuns  []struct {
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if json.Unmarshal(data, &cr) == nil && (cr.TotalCount > 0 || len(cr.CheckRuns) > 0) {
			hasAny = true
			for _, run := range cr.CheckRuns {
				if run.Status != "completed" {
					hasPending = true
					continue
				}
				switch run.Conclusion {
				case "failure", "timed_out", "cancelled", "action_required", "startup_failure":
					hasFailed = true
				}
			}
		}
	}

	switch {
	case !hasAny:
		return forge.ChecksNone
	case hasFailed:
		return forge.ChecksFailed
	case hasPending:
		return forge.ChecksPending
	default:
		return forge.ChecksPassed
	}
}

func (c *Client) MergePR(ctx context.Context, r forge.Repo, number int, req forge.MergeRequest) (forge.MergeResult, error) {
	method := req.Method
	if method == "" {
		method = forge.Squash
	}

	if req.WhenChecksPass {
		raw, err := c.getPRRaw(ctx, r, number)
		if err != nil {
			return forge.MergeResult{}, err
		}
		checks := c.checksForSHA(ctx, r, raw.Head.SHA)
		if checks == forge.ChecksPending {
			if err := c.enableAutoMerge(ctx, raw.NodeID, method, req.CommitTitle, req.CommitMessage); err != nil {
				return forge.MergeResult{}, err
			}
			return forge.MergeResult{
				AutoMergeSet: true,
				Method:       method,
				Message:      "auto-merge enabled; will merge when checks pass",
			}, nil
		}
	}

	payload := map[string]any{
		"merge_method": string(method),
	}
	if req.CommitTitle != "" {
		payload["commit_title"] = req.CommitTitle
	}
	if req.CommitMessage != "" {
		payload["commit_message"] = req.CommitMessage
	}

	endpoint := fmt.Sprintf("%s%s/pulls/%d/merge", c.apiBase, repoPath(r), number)
	data, _, status, err := c.do(ctx, http.MethodPut, endpoint, payload)
	if err != nil {
		return forge.MergeResult{}, err
	}
	if status == http.StatusOK {
		var resp struct {
			SHA     string `json:"sha"`
			Merged  bool   `json:"merged"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return forge.MergeResult{}, fmt.Errorf("github: decode merge result: %w", err)
		}
		return forge.MergeResult{
			Merged:  resp.Merged,
			Method:  method,
			SHA:     resp.SHA,
			Message: resp.Message,
		}, nil
	}

	msg := strings.ToLower(parseErrorMessage(data))
	switch status {
	case http.StatusMethodNotAllowed:
		if strings.Contains(msg, "not allowed") || strings.Contains(msg, "merge method") {
			return forge.MergeResult{}, fmt.Errorf("%w: %s", forge.ErrMethodNotAllowed, parseErrorMessage(data))
		}
		return forge.MergeResult{}, fmt.Errorf("%w: %s", forge.ErrNotMergeable, parseErrorMessage(data))
	case http.StatusConflict:
		return forge.MergeResult{}, fmt.Errorf("%w: %s", forge.ErrNotMergeable, parseErrorMessage(data))
	case http.StatusUnprocessableEntity:
		if strings.Contains(msg, "method") || strings.Contains(msg, "not allowed") {
			return forge.MergeResult{}, fmt.Errorf("%w: %s", forge.ErrMethodNotAllowed, parseErrorMessage(data))
		}
		return forge.MergeResult{}, fmt.Errorf("%w: %s", forge.ErrNotMergeable, parseErrorMessage(data))
	default:
		return forge.MergeResult{}, mapHTTPError(status, data, "merge PR")
	}
}

func (c *Client) enableAutoMerge(ctx context.Context, nodeID string, method forge.MergeMethod, title, body string) error {
	if nodeID == "" {
		return errors.New("github: PR node_id is missing; cannot enable auto-merge")
	}
	gqlMethod := "SQUASH"
	switch method {
	case forge.Merge:
		gqlMethod = "MERGE"
	case forge.Rebase:
		gqlMethod = "REBASE"
	}
	vars := map[string]any{
		"id":     nodeID,
		"method": gqlMethod,
	}
	if title != "" {
		vars["headline"] = title
	}
	if body != "" {
		vars["body"] = body
	}
	gqlReq := map[string]any{
		"query": `mutation EnableAutoMerge($id: ID!, $method: PullRequestMergeMethod, $headline: String, $body: String) {
			enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: $method, commitHeadline: $headline, commitBody: $body}) {
				pullRequest { id }
			}
		}`,
		"variables": vars,
	}
	data, _, status, err := c.do(ctx, http.MethodPost, c.graphqlURL(), gqlReq)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return mapHTTPError(status, data, "enable auto-merge")
	}
	var resp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("github: decode graphql response: %w", err)
	}
	if len(resp.Errors) > 0 {
		msg := resp.Errors[0].Message
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "not allowed") || strings.Contains(lower, "merge method") {
			return fmt.Errorf("%w: %s", forge.ErrMethodNotAllowed, msg)
		}
		if strings.Contains(lower, "not mergeable") || strings.Contains(lower, "protected branch") || strings.Contains(lower, "clean status") {
			return fmt.Errorf("%w: %s", forge.ErrNotMergeable, msg)
		}
		return fmt.Errorf("github: enable auto-merge: %s", msg)
	}
	return nil
}

func (c *Client) ReviewComments(ctx context.Context, r forge.Repo, number int, since time.Time) ([]forge.ReviewComment, error) {
	var out []forge.ReviewComment
	changesRequestedReviews := make(map[int64]bool)

	// 1. Reviews (paginated)
	nextURL := fmt.Sprintf("%s%s/pulls/%d/reviews?per_page=100", c.apiBase, repoPath(r), number)
	for nextURL != "" {
		data, hdr, status, err := c.do(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, mapHTTPError(status, data, "list reviews")
		}
		var reviews []struct {
			ID          int64     `json:"id"`
			State       string    `json:"state"`
			Body        string    `json:"body"`
			SubmittedAt time.Time `json:"submitted_at"`
			User        struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := json.Unmarshal(data, &reviews); err != nil {
			return nil, fmt.Errorf("github: decode reviews: %w", err)
		}
		for _, rev := range reviews {
			isChangesReq := strings.EqualFold(rev.State, "CHANGES_REQUESTED")
			if isChangesReq {
				changesRequestedReviews[rev.ID] = true
			}
			if !rev.SubmittedAt.After(since) {
				continue
			}
			if strings.TrimSpace(rev.Body) == "" && !isChangesReq {
				continue
			}
			out = append(out, forge.ReviewComment{
				ID:               "review:" + strconv.FormatInt(rev.ID, 10),
				Author:           rev.User.Login,
				Body:             rev.Body,
				ChangesRequested: isChangesReq,
				CreatedAt:        rev.SubmittedAt,
			})
		}
		nextURL = nextLinkURL(hdr.Get("Link"))
	}

	// 2. Inline pull review comments (paginated)
	q := url.Values{"per_page": {"100"}}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	nextURL = fmt.Sprintf("%s%s/pulls/%d/comments?%s", c.apiBase, repoPath(r), number, q.Encode())
	for nextURL != "" {
		data, hdr, status, err := c.do(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, mapHTTPError(status, data, "list review comments")
		}
		var comments []struct {
			ID                  int64     `json:"id"`
			PullRequestReviewID int64     `json:"pull_request_review_id"`
			Body                string    `json:"body"`
			Path                string    `json:"path"`
			Line                int       `json:"line"`
			OriginalLine        int       `json:"original_line"`
			CreatedAt           time.Time `json:"created_at"`
			User                struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := json.Unmarshal(data, &comments); err != nil {
			return nil, fmt.Errorf("github: decode review comments: %w", err)
		}
		for _, cm := range comments {
			if !cm.CreatedAt.After(since) {
				continue
			}
			line := cm.Line
			if line == 0 {
				line = cm.OriginalLine
			}
			out = append(out, forge.ReviewComment{
				ID:               "comment:" + strconv.FormatInt(cm.ID, 10),
				Author:           cm.User.Login,
				Body:             cm.Body,
				Path:             cm.Path,
				Line:             line,
				ChangesRequested: changesRequestedReviews[cm.PullRequestReviewID],
				CreatedAt:        cm.CreatedAt,
			})
		}
		nextURL = nextLinkURL(hdr.Get("Link"))
	}

	// 3. Issue comments on the PR (paginated)
	nextURL = fmt.Sprintf("%s%s/issues/%d/comments?%s", c.apiBase, repoPath(r), number, q.Encode())
	for nextURL != "" {
		data, hdr, status, err := c.do(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, mapHTTPError(status, data, "list issue comments")
		}
		var comments []struct {
			ID        int64     `json:"id"`
			Body      string    `json:"body"`
			CreatedAt time.Time `json:"created_at"`
			User      struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := json.Unmarshal(data, &comments); err != nil {
			return nil, fmt.Errorf("github: decode issue comments: %w", err)
		}
		for _, cm := range comments {
			if !cm.CreatedAt.After(since) {
				continue
			}
			out = append(out, forge.ReviewComment{
				ID:        "issue:" + strconv.FormatInt(cm.ID, 10),
				Author:    cm.User.Login,
				Body:      cm.Body,
				CreatedAt: cm.CreatedAt,
			})
		}
		nextURL = nextLinkURL(hdr.Get("Link"))
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (c *Client) CompareURL(r forge.Repo, head, base string) string {
	return fmt.Sprintf("https://%s/%s/%s/compare/%s...%s?expand=1",
		r.Host, r.Owner, r.Name, url.PathEscape(base), url.PathEscape(head))
}

func nextLinkURL(linkHeader string) string {
	for _, part := range strings.Split(linkHeader, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.IndexByte(part, '<')
		end := strings.IndexByte(part, '>')
		if start >= 0 && end > start+1 {
			return part[start+1 : end]
		}
	}
	return ""
}

// UpdatePRBase changes the PR's base branch.
func (c *Client) UpdatePRBase(ctx context.Context, r forge.Repo, number int, base string) error {
	endpoint := fmt.Sprintf("%s%s/pulls/%d", c.apiBase, repoPath(r), number)
	data, _, status, err := c.do(ctx, http.MethodPatch, endpoint, map[string]string{"base": base})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return mapHTTPError(status, data, "update PR base")
	}
	return nil
}
