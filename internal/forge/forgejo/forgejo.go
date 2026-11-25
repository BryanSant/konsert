// Package forgejo implements forge.Client for Forgejo and Gitea instances (including
// Codeberg) using REST API v1.
package forgejo

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
	defaultTimeout   = 30 * time.Second
	maxResponseBytes = 4 << 20
	pageLimit        = 50
)

// Client is an authenticated Forgejo/Gitea REST v1 forge.Client. Safe for concurrent use.
type Client struct {
	apiBase    string
	token      string
	httpClient *http.Client
}

// New creates a Forgejo/Gitea Client for r using token.
func New(r forge.Repo, token string, httpClient *http.Client) *Client {
	base := strings.TrimRight(strings.TrimSpace(r.APIBase), "/")
	if base == "" {
		base = forge.APIBaseFor("https", forge.Forgejo, r.Host)
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

func (c *Client) Kind() forge.Kind { return forge.Forgejo }

func repoPath(r forge.Repo) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

func (c *Client) do(ctx context.Context, method, fullURL string, reqBody any) ([]byte, http.Header, int, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("forgejo: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("forgejo: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "syre")
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("forgejo: request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.Header, resp.StatusCode, fmt.Errorf("forgejo: read response: %w", err)
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
	detail := fmt.Sprintf("forgejo %s: HTTP %d", op, status)
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

func (c *Client) RepoInfo(ctx context.Context, r forge.Repo) (forge.RepoInfo, error) {
	data, _, status, err := c.do(ctx, http.MethodGet, c.apiBase+repoPath(r), nil)
	if err != nil {
		return forge.RepoInfo{}, err
	}
	if status != http.StatusOK {
		return forge.RepoInfo{}, mapHTTPError(status, data, "repo info")
	}
	var raw struct {
		DefaultBranch       string `json:"default_branch"`
		AllowSquashMerge    bool   `json:"allow_squash_merge"`
		AllowMergeCommits   bool   `json:"allow_merge_commits"`
		AllowRebase         bool   `json:"allow_rebase"`
		AllowRebaseExplicit bool   `json:"allow_rebase_explicit"`
		Permissions         struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return forge.RepoInfo{}, fmt.Errorf("forgejo: decode repo info: %w", err)
	}

	var methods []forge.MergeMethod
	if raw.AllowSquashMerge {
		methods = append(methods, forge.Squash)
	}
	if raw.AllowMergeCommits {
		methods = append(methods, forge.Merge)
	}
	if raw.AllowRebase || raw.AllowRebaseExplicit {
		methods = append(methods, forge.Rebase)
	}

	info := forge.RepoInfo{
		DefaultBranch:  raw.DefaultBranch,
		AllowedMethods: methods,
		CanPush:        raw.Permissions.Push,
	}
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
		Kind:    forge.Forgejo,
		Host:    r.Host,
		Owner:   user.Login,
		Name:    name,
		APIBase: c.apiBase,
	}
}

type fjPR struct {
	Number   int        `json:"number"`
	Title    string     `json:"title"`
	HTMLURL  string     `json:"html_url"`
	State    string     `json:"state"`
	Draft    bool       `json:"draft"`
	Merged   bool       `json:"merged"`
	MergedAt *time.Time `json:"merged_at"`
	Head     struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p fjPR) toForgePR(checks forge.ChecksState) forge.PR {
	state := forge.PRClosed
	switch {
	case p.Merged || p.MergedAt != nil:
		state = forge.PRMerged
	case strings.EqualFold(p.State, "open"):
		state = forge.PROpen
	}
	draft := p.Draft || strings.HasPrefix(p.Title, "WIP:")
	return forge.PR{
		Number:   p.Number,
		URL:      p.HTMLURL,
		State:    state,
		Draft:    draft,
		Head:     p.Head.Ref,
		Base:     p.Base.Ref,
		Checks:   checks,
		MergedAt: p.MergedAt,
	}
}

func (c *Client) OpenPR(ctx context.Context, r forge.Repo, req forge.OpenPRRequest) (forge.PR, error) {
	head := req.Head
	if req.HeadOwner != "" && !strings.EqualFold(req.HeadOwner, r.Owner) && !strings.Contains(head, ":") {
		head = req.HeadOwner + ":" + head
	}
	title := req.Title
	if req.Draft && !strings.HasPrefix(title, "WIP:") {
		title = "WIP: " + title
	}
	payload := map[string]any{
		"title": title,
		"body":  req.Body,
		"head":  head,
		"base":  req.Base,
	}
	data, _, status, err := c.do(ctx, http.MethodPost, c.apiBase+repoPath(r)+"/pulls", payload)
	if err != nil {
		return forge.PR{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return forge.PR{}, mapHTTPError(status, data, "open PR")
	}
	var raw fjPR
	if err := json.Unmarshal(data, &raw); err != nil {
		return forge.PR{}, fmt.Errorf("forgejo: decode PR: %w", err)
	}
	return raw.toForgePR(forge.ChecksNone), nil
}

func (c *Client) FindPR(ctx context.Context, r forge.Repo, head, base string) (forge.PR, error) {
	wantOwner := ""
	wantBranch := head
	if idx := strings.IndexByte(head, ':'); idx >= 0 {
		wantOwner = head[:idx]
		wantBranch = head[idx+1:]
	}

	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s%s/pulls?state=open&limit=%d&page=%d", c.apiBase, repoPath(r), pageLimit, page)
		data, _, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return forge.PR{}, err
		}
		if status != http.StatusOK {
			return forge.PR{}, mapHTTPError(status, data, "find PR")
		}
		var list []fjPR
		if err := json.Unmarshal(data, &list); err != nil {
			return forge.PR{}, fmt.Errorf("forgejo: decode PR list: %w", err)
		}
		for _, p := range list {
			if p.Head.Ref != wantBranch {
				continue
			}
			if base != "" && p.Base.Ref != base {
				continue
			}
			if wantOwner != "" && p.Head.Repo != nil && !strings.EqualFold(p.Head.Repo.Owner.Login, wantOwner) {
				continue
			}
			checks := c.checksForSHA(ctx, r, p.Head.SHA)
			return p.toForgePR(checks), nil
		}
		if len(list) < pageLimit {
			break
		}
	}
	return forge.PR{}, fmt.Errorf("%w: no open PR for %s -> %s", forge.ErrNotFound, head, base)
}

func (c *Client) GetPR(ctx context.Context, r forge.Repo, number int) (forge.PR, error) {
	endpoint := fmt.Sprintf("%s%s/pulls/%d", c.apiBase, repoPath(r), number)
	data, _, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return forge.PR{}, err
	}
	if status != http.StatusOK {
		return forge.PR{}, mapHTTPError(status, data, "get PR")
	}
	var raw fjPR
	if err := json.Unmarshal(data, &raw); err != nil {
		return forge.PR{}, fmt.Errorf("forgejo: decode PR: %w", err)
	}
	checks := c.checksForSHA(ctx, r, raw.Head.SHA)
	return raw.toForgePR(checks), nil
}

func (c *Client) checksForSHA(ctx context.Context, r forge.Repo, sha string) forge.ChecksState {
	if sha == "" {
		return forge.ChecksNone
	}
	endpoint := fmt.Sprintf("%s%s/commits/%s/status", c.apiBase, repoPath(r), url.PathEscape(sha))
	data, _, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status != http.StatusOK {
		return forge.ChecksNone
	}
	var cs struct {
		State    string `json:"state"`
		Statuses []any  `json:"statuses"`
	}
	if json.Unmarshal(data, &cs) != nil {
		return forge.ChecksNone
	}
	if len(cs.Statuses) == 0 && cs.State == "" {
		return forge.ChecksNone
	}
	switch strings.ToLower(cs.State) {
	case "success":
		return forge.ChecksPassed
	case "failure", "error":
		return forge.ChecksFailed
	case "pending":
		return forge.ChecksPending
	default:
		return forge.ChecksNone
	}
}

func (c *Client) MergePR(ctx context.Context, r forge.Repo, number int, req forge.MergeRequest) (forge.MergeResult, error) {
	method := req.Method
	if method == "" {
		method = forge.Squash
	}

	whenChecks := false
	if req.WhenChecksPass {
		pr, err := c.GetPR(ctx, r, number)
		if err != nil {
			return forge.MergeResult{}, err
		}
		if pr.Checks == forge.ChecksPending {
			whenChecks = true
		}
	}

	payload := map[string]any{
		"Do":                        string(method),
		"merge_when_checks_succeed": whenChecks,
	}
	if req.CommitTitle != "" {
		payload["merge_title_field"] = req.CommitTitle
	}
	if req.CommitMessage != "" {
		payload["merge_message_field"] = req.CommitMessage
	}

	endpoint := fmt.Sprintf("%s%s/pulls/%d/merge", c.apiBase, repoPath(r), number)
	data, _, status, err := c.do(ctx, http.MethodPost, endpoint, payload)
	if err != nil {
		return forge.MergeResult{}, err
	}
	if status == http.StatusOK || status == http.StatusNoContent || status == http.StatusCreated {
		if whenChecks {
			return forge.MergeResult{
				AutoMergeSet: true,
				Method:       method,
				Message:      "auto-merge scheduled when checks succeed",
			}, nil
		}
		return forge.MergeResult{
			Merged:  true,
			Method:  method,
			Message: "merged",
		}, nil
	}

	msg := strings.ToLower(parseErrorMessage(data))
	switch status {
	case http.StatusMethodNotAllowed:
		if strings.Contains(msg, "not allowed") || strings.Contains(msg, "disabled") || strings.Contains(msg, "merge style") {
			return forge.MergeResult{}, fmt.Errorf("%w: %s", forge.ErrMethodNotAllowed, parseErrorMessage(data))
		}
		return forge.MergeResult{}, fmt.Errorf("%w: %s", forge.ErrNotMergeable, parseErrorMessage(data))
	case http.StatusConflict:
		return forge.MergeResult{}, fmt.Errorf("%w: %s", forge.ErrNotMergeable, parseErrorMessage(data))
	default:
		return forge.MergeResult{}, mapHTTPError(status, data, "merge PR")
	}
}

func (c *Client) ReviewComments(ctx context.Context, r forge.Repo, number int, since time.Time) ([]forge.ReviewComment, error) {
	var out []forge.ReviewComment

	// 1. Reviews + their inline comments
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s%s/pulls/%d/reviews?limit=%d&page=%d", c.apiBase, repoPath(r), number, pageLimit, page)
		data, _, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, mapHTTPError(status, data, "list reviews")
		}
		var reviews []struct {
			ID            int64     `json:"id"`
			State         string    `json:"state"`
			Body          string    `json:"body"`
			CommentsCount int       `json:"comments_count"`
			SubmittedAt   time.Time `json:"submitted_at"`
			User          struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := json.Unmarshal(data, &reviews); err != nil {
			return nil, fmt.Errorf("forgejo: decode reviews: %w", err)
		}
		for _, rev := range reviews {
			isChangesReq := strings.EqualFold(rev.State, "REQUEST_CHANGES")
			if rev.SubmittedAt.After(since) && (strings.TrimSpace(rev.Body) != "" || isChangesReq) {
				out = append(out, forge.ReviewComment{
					ID:               "review:" + strconv.FormatInt(rev.ID, 10),
					Author:           rev.User.Login,
					Body:             rev.Body,
					ChangesRequested: isChangesReq,
					CreatedAt:        rev.SubmittedAt,
				})
			}
			if rev.CommentsCount > 0 {
				inline, err := c.reviewInlineComments(ctx, r, number, rev.ID, isChangesReq, since)
				if err != nil {
					return nil, err
				}
				out = append(out, inline...)
			}
		}
		if len(reviews) < pageLimit {
			break
		}
	}

	// 2. Issue comments
	q := url.Values{}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	endpoint := fmt.Sprintf("%s%s/issues/%d/comments", c.apiBase, repoPath(r), number)
	if enc := q.Encode(); enc != "" {
		endpoint += "?" + enc
	}
	data, _, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
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
		return nil, fmt.Errorf("forgejo: decode issue comments: %w", err)
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

	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (c *Client) reviewInlineComments(ctx context.Context, r forge.Repo, number int, reviewID int64, changesReq bool, since time.Time) ([]forge.ReviewComment, error) {
	endpoint := fmt.Sprintf("%s%s/pulls/%d/reviews/%d/comments", c.apiBase, repoPath(r), number, reviewID)
	data, _, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, mapHTTPError(status, data, "list review inline comments")
	}
	var comments []struct {
		ID        int64     `json:"id"`
		Body      string    `json:"body"`
		Path      string    `json:"path"`
		LineNum   int       `json:"new_position"`
		OldLine   int       `json:"old_position"`
		CreatedAt time.Time `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &comments); err != nil {
		return nil, fmt.Errorf("forgejo: decode review inline comments: %w", err)
	}
	var out []forge.ReviewComment
	for _, cm := range comments {
		if !cm.CreatedAt.After(since) {
			continue
		}
		line := cm.LineNum
		if line == 0 {
			line = cm.OldLine
		}
		out = append(out, forge.ReviewComment{
			ID:               "comment:" + strconv.FormatInt(cm.ID, 10),
			Author:           cm.User.Login,
			Body:             cm.Body,
			Path:             cm.Path,
			Line:             line,
			ChangesRequested: changesReq,
			CreatedAt:        cm.CreatedAt,
		})
	}
	return out, nil
}

func (c *Client) CompareURL(r forge.Repo, head, base string) string {
	return fmt.Sprintf("https://%s/%s/%s/compare/%s...%s",
		r.Host, r.Owner, r.Name, url.PathEscape(base), url.PathEscape(head))
}
