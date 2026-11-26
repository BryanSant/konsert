// Package gitlab implements forge.Client for GitLab (gitlab.com and self-hosted)
// using GitLab REST API v4.
package gitlab

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
	// GitLab Developer access level (minimum needed to push branches).
	accessLevelDeveloper = 30
)

// Client is an authenticated GitLab REST v4 forge.Client. Safe for concurrent use.
type Client struct {
	apiBase    string
	token      string
	httpClient *http.Client
}

// New creates a GitLab Client for r using token.
func New(r forge.Repo, token string, httpClient *http.Client) *Client {
	base := strings.TrimRight(strings.TrimSpace(r.APIBase), "/")
	if base == "" {
		base = forge.APIBaseFor("https", forge.GitLab, r.Host)
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

func (c *Client) Kind() forge.Kind { return forge.GitLab }

func projectPath(fullName string) string {
	return "/projects/" + url.PathEscape(fullName)
}

func (c *Client) do(ctx context.Context, method, fullURL string, reqBody any) ([]byte, http.Header, int, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("gitlab: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("gitlab: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "syre")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("gitlab: request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.Header, resp.StatusCode, fmt.Errorf("gitlab: read response: %w", err)
	}
	return data, resp.Header, resp.StatusCode, nil
}

func parseErrorMessage(body []byte) string {
	var e struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil {
		if s, ok := e.Message.(string); ok && s != "" {
			return s
		}
		if e.Error != "" {
			return e.Error
		}
	}
	return ""
}

func mapHTTPError(status int, body []byte, op string) error {
	msg := parseErrorMessage(body)
	detail := fmt.Sprintf("gitlab %s: HTTP %d", op, status)
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

type glProject struct {
	ID            int64  `json:"id"`
	Path          string `json:"path"`
	DefaultBranch string `json:"default_branch"`
	MergeMethod   string `json:"merge_method"`  // "merge", "rebase_merge", "ff"
	SquashOption  string `json:"squash_option"` // "never", "default_off", "default_on", "always"
	Namespace     struct {
		FullPath string `json:"full_path"`
	} `json:"namespace"`
	Permissions struct {
		ProjectAccess *struct {
			AccessLevel int `json:"access_level"`
		} `json:"project_access"`
		GroupAccess *struct {
			AccessLevel int `json:"access_level"`
		} `json:"group_access"`
	} `json:"permissions"`
}

func (c *Client) getProject(ctx context.Context, fullName string) (glProject, error) {
	data, _, status, err := c.do(ctx, http.MethodGet, c.apiBase+projectPath(fullName), nil)
	if err != nil {
		return glProject{}, err
	}
	if status != http.StatusOK {
		return glProject{}, mapHTTPError(status, data, "project info")
	}
	var proj glProject
	if err := json.Unmarshal(data, &proj); err != nil {
		return glProject{}, fmt.Errorf("gitlab: decode project: %w", err)
	}
	return proj, nil
}

func (c *Client) RepoInfo(ctx context.Context, r forge.Repo) (forge.RepoInfo, error) {
	proj, err := c.getProject(ctx, r.FullName())
	if err != nil {
		return forge.RepoInfo{}, err
	}

	var methods []forge.MergeMethod
	if proj.SquashOption != "never" {
		methods = append(methods, forge.Squash)
	}
	if proj.SquashOption != "always" {
		switch proj.MergeMethod {
		case "rebase_merge", "ff":
			methods = append(methods, forge.Rebase)
		default:
			methods = append(methods, forge.Merge)
		}
	}

	canPush := false
	if proj.Permissions.ProjectAccess != nil && proj.Permissions.ProjectAccess.AccessLevel >= accessLevelDeveloper {
		canPush = true
	}
	if proj.Permissions.GroupAccess != nil && proj.Permissions.GroupAccess.AccessLevel >= accessLevelDeveloper {
		canPush = true
	}

	info := forge.RepoInfo{
		DefaultBranch:  proj.DefaultBranch,
		AllowedMethods: methods,
		CanPush:        canPush,
	}

	if fork := c.findUserFork(ctx, r); fork != nil {
		info.Fork = fork
	}
	return info, nil
}

func (c *Client) findUserFork(ctx context.Context, r forge.Repo) *forge.Repo {
	endpoint := c.apiBase + projectPath(r.FullName()) + "/forks?owned=true&per_page=10"
	data, _, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status != http.StatusOK {
		return nil
	}
	var forks []glProject
	if json.Unmarshal(data, &forks) != nil || len(forks) == 0 {
		return nil
	}
	f := forks[0]
	if f.Namespace.FullPath == "" || f.Path == "" {
		return nil
	}
	return &forge.Repo{
		Kind:    forge.GitLab,
		Host:    r.Host,
		Owner:   f.Namespace.FullPath,
		Name:    f.Path,
		APIBase: c.apiBase,
	}
}

type glMR struct {
	IID                       int        `json:"iid"`
	WebURL                    string     `json:"web_url"`
	State                     string     `json:"state"` // "opened", "merged", "closed"
	Draft                     bool       `json:"draft"`
	WorkInProgress            bool       `json:"work_in_progress"`
	SourceBranch              string     `json:"source_branch"`
	TargetBranch              string     `json:"target_branch"`
	MergeWhenPipelineSucceeds bool       `json:"merge_when_pipeline_succeeds"`
	AutoMergeEnabled          bool       `json:"auto_merge_enabled"`
	MergedAt                  *time.Time `json:"merged_at"`
	MergeCommitSHA            string     `json:"merge_commit_sha"`
	SquashCommitSHA           string     `json:"squash_commit_sha"`
	HeadPipeline              *struct {
		Status string `json:"status"`
	} `json:"head_pipeline"`
}

func mapPipelineStatus(status string) forge.ChecksState {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "":
		return forge.ChecksNone
	case "success":
		return forge.ChecksPassed
	case "failed", "canceled":
		return forge.ChecksFailed
	default:
		// running, pending, created, waiting_for_resource, preparing, scheduled, manual
		return forge.ChecksPending
	}
}

func (m glMR) toForgePR(checks forge.ChecksState) forge.PR {
	state := forge.PRClosed
	switch strings.ToLower(m.State) {
	case "opened":
		state = forge.PROpen
	case "merged":
		state = forge.PRMerged
	}
	return forge.PR{
		Number:    m.IID,
		URL:       m.WebURL,
		State:     state,
		Draft:     m.Draft || m.WorkInProgress,
		Head:      m.SourceBranch,
		Base:      m.TargetBranch,
		Checks:    checks,
		AutoMerge: m.MergeWhenPipelineSucceeds || m.AutoMergeEnabled,
		MergedAt:  m.MergedAt,
	}
}

func (c *Client) OpenPR(ctx context.Context, r forge.Repo, req forge.OpenPRRequest) (forge.PR, error) {
	title := req.Title
	if req.Draft && !strings.HasPrefix(title, "Draft:") {
		title = "Draft: " + title
	}
	payload := map[string]any{
		"source_branch": req.Head,
		"target_branch": req.Base,
		"title":         title,
		"description":   req.Body,
	}

	targetProject := r.FullName()
	if req.HeadOwner != "" && !strings.EqualFold(req.HeadOwner, r.Owner) {
		// On GitLab, fork MRs are created on the source (fork) project with target_project_id.
		upstream, err := c.getProject(ctx, r.FullName())
		if err != nil {
			return forge.PR{}, err
		}
		payload["target_project_id"] = upstream.ID
		targetProject = req.HeadOwner + "/" + r.Name
	}

	endpoint := c.apiBase + projectPath(targetProject) + "/merge_requests"
	data, _, status, err := c.do(ctx, http.MethodPost, endpoint, payload)
	if err != nil {
		return forge.PR{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return forge.PR{}, mapHTTPError(status, data, "open MR")
	}
	var raw glMR
	if err := json.Unmarshal(data, &raw); err != nil {
		return forge.PR{}, fmt.Errorf("gitlab: decode MR: %w", err)
	}
	checks := forge.ChecksNone
	if raw.HeadPipeline != nil {
		checks = mapPipelineStatus(raw.HeadPipeline.Status)
	}
	return raw.toForgePR(checks), nil
}

func (c *Client) FindPR(ctx context.Context, r forge.Repo, head, base string) (forge.PR, error) {
	q := url.Values{}
	q.Set("state", "opened")
	q.Set("source_branch", head)
	if base != "" {
		q.Set("target_branch", base)
	}
	endpoint := c.apiBase + projectPath(r.FullName()) + "/merge_requests?" + q.Encode()
	data, _, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return forge.PR{}, err
	}
	if status != http.StatusOK {
		return forge.PR{}, mapHTTPError(status, data, "find MR")
	}
	var list []glMR
	if err := json.Unmarshal(data, &list); err != nil {
		return forge.PR{}, fmt.Errorf("gitlab: decode MR list: %w", err)
	}
	if len(list) == 0 {
		return forge.PR{}, fmt.Errorf("%w: no open MR for %s -> %s", forge.ErrNotFound, head, base)
	}
	checks := c.checksForMR(ctx, r, list[0])
	return list[0].toForgePR(checks), nil
}

func (c *Client) GetPR(ctx context.Context, r forge.Repo, number int) (forge.PR, error) {
	endpoint := fmt.Sprintf("%s%s/merge_requests/%d", c.apiBase, projectPath(r.FullName()), number)
	data, _, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return forge.PR{}, err
	}
	if status != http.StatusOK {
		return forge.PR{}, mapHTTPError(status, data, "get MR")
	}
	var raw glMR
	if err := json.Unmarshal(data, &raw); err != nil {
		return forge.PR{}, fmt.Errorf("gitlab: decode MR: %w", err)
	}
	checks := c.checksForMR(ctx, r, raw)
	return raw.toForgePR(checks), nil
}

func (c *Client) checksForMR(ctx context.Context, r forge.Repo, mr glMR) forge.ChecksState {
	if mr.HeadPipeline != nil && mr.HeadPipeline.Status != "" {
		return mapPipelineStatus(mr.HeadPipeline.Status)
	}
	endpoint := fmt.Sprintf("%s%s/merge_requests/%d/pipelines?per_page=1", c.apiBase, projectPath(r.FullName()), mr.IID)
	data, _, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status != http.StatusOK {
		return forge.ChecksNone
	}
	var pipelines []struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(data, &pipelines) != nil || len(pipelines) == 0 {
		return forge.ChecksNone
	}
	return mapPipelineStatus(pipelines[0].Status)
}

func (c *Client) MergePR(ctx context.Context, r forge.Repo, number int, req forge.MergeRequest) (forge.MergeResult, error) {
	method := req.Method
	if method == "" {
		method = forge.Squash
	}

	mwps := false
	if req.WhenChecksPass {
		pr, err := c.GetPR(ctx, r, number)
		if err != nil {
			return forge.MergeResult{}, err
		}
		if pr.Checks == forge.ChecksPending {
			mwps = true
		}
	}

	payload := map[string]any{
		"squash": method == forge.Squash,
	}
	if mwps {
		payload["merge_when_pipeline_succeeds"] = true
	}
	commitMsg := strings.TrimSpace(req.CommitTitle)
	if req.CommitMessage != "" {
		if commitMsg != "" {
			commitMsg += "\n\n" + req.CommitMessage
		} else {
			commitMsg = req.CommitMessage
		}
	}
	if commitMsg != "" {
		if method == forge.Squash {
			payload["squash_commit_message"] = commitMsg
		} else {
			payload["merge_commit_message"] = commitMsg
		}
	}

	endpoint := fmt.Sprintf("%s%s/merge_requests/%d/merge", c.apiBase, projectPath(r.FullName()), number)
	data, _, status, err := c.do(ctx, http.MethodPut, endpoint, payload)
	if err != nil {
		return forge.MergeResult{}, err
	}
	if status == http.StatusOK {
		var resp glMR
		if err := json.Unmarshal(data, &resp); err != nil {
			return forge.MergeResult{}, fmt.Errorf("gitlab: decode merge result: %w", err)
		}
		if (resp.MergeWhenPipelineSucceeds || resp.AutoMergeEnabled) && !strings.EqualFold(resp.State, "merged") {
			return forge.MergeResult{
				AutoMergeSet: true,
				Method:       method,
				Message:      "merge when pipeline succeeds enabled",
			}, nil
		}
		sha := resp.MergeCommitSHA
		if sha == "" {
			sha = resp.SquashCommitSHA
		}
		return forge.MergeResult{
			Merged:  true,
			Method:  method,
			SHA:     sha,
			Message: "merged",
		}, nil
	}

	msg := strings.ToLower(parseErrorMessage(data))
	switch status {
	case http.StatusMethodNotAllowed, http.StatusNotAcceptable, http.StatusConflict, http.StatusUnprocessableEntity:
		if strings.Contains(msg, "squash") || strings.Contains(msg, "merge method") || strings.Contains(msg, "not allowed") {
			return forge.MergeResult{}, fmt.Errorf("%w: %s", forge.ErrMethodNotAllowed, parseErrorMessage(data))
		}
		return forge.MergeResult{}, fmt.Errorf("%w: %s", forge.ErrNotMergeable, parseErrorMessage(data))
	default:
		return forge.MergeResult{}, mapHTTPError(status, data, "merge MR")
	}
}

func (c *Client) ReviewComments(ctx context.Context, r forge.Repo, number int, since time.Time) ([]forge.ReviewComment, error) {
	var out []forge.ReviewComment

	page := "1"
	for page != "" {
		endpoint := fmt.Sprintf("%s%s/merge_requests/%d/discussions?per_page=100&page=%s",
			c.apiBase, projectPath(r.FullName()), number, url.QueryEscape(page))
		data, hdr, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, mapHTTPError(status, data, "list discussions")
		}
		var discussions []struct {
			ID    string `json:"id"`
			Notes []struct {
				ID         int64     `json:"id"`
				Body       string    `json:"body"`
				System     bool      `json:"system"`
				Resolvable bool      `json:"resolvable"`
				Resolved   bool      `json:"resolved"`
				CreatedAt  time.Time `json:"created_at"`
				Author     struct {
					Username string `json:"username"`
				} `json:"author"`
				Position *struct {
					NewPath string `json:"new_path"`
					OldPath string `json:"old_path"`
					NewLine int    `json:"new_line"`
					OldLine int    `json:"old_line"`
				} `json:"position"`
			} `json:"notes"`
		}
		if err := json.Unmarshal(data, &discussions); err != nil {
			return nil, fmt.Errorf("gitlab: decode discussions: %w", err)
		}
		for _, disc := range discussions {
			for _, note := range disc.Notes {
				if note.System || !note.CreatedAt.After(since) {
					continue
				}
				var filePath string
				var line int
				if note.Position != nil {
					filePath = note.Position.NewPath
					if filePath == "" {
						filePath = note.Position.OldPath
					}
					line = note.Position.NewLine
					if line == 0 {
						line = note.Position.OldLine
					}
				}
				out = append(out, forge.ReviewComment{
					ID:               "note:" + strconv.FormatInt(note.ID, 10),
					Author:           note.Author.Username,
					Body:             note.Body,
					Path:             filePath,
					Line:             line,
					ChangesRequested: note.Resolvable && !note.Resolved,
					CreatedAt:        note.CreatedAt,
				})
			}
		}
		page = strings.TrimSpace(hdr.Get("X-Next-Page"))
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
	return fmt.Sprintf("https://%s/%s/-/merge_requests/new?merge_request[source_branch]=%s&merge_request[target_branch]=%s",
		r.Host, r.FullName(), url.QueryEscape(head), url.QueryEscape(base))
}

// UpdatePRBase changes the MR's base branch.
func (c *Client) UpdatePRBase(ctx context.Context, r forge.Repo, number int, base string) error {
	endpoint := fmt.Sprintf("%s%s/merge_requests/%d", c.apiBase, projectPath(r.FullName()), number)
	data, _, status, err := c.do(ctx, http.MethodPut, endpoint, map[string]string{"target_branch": base})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return mapHTTPError(status, data, "update MR base")
	}
	return nil
}
