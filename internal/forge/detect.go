package forge

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultProbeTimeout = 3 * time.Second

// Detector identifies the forge kind and API root from a git remote URL.
type Detector struct {
	// Overrides maps lowercased hostnames (e.g. "git.corp.example") to a forge Kind,
	// bypassing version-endpoint probing.
	Overrides map[string]Kind
	// HTTPClient is used to probe unknown hosts. If nil, a client with ProbeTimeout is used.
	HTTPClient *http.Client
	// ProbeTimeout bounds each probe request (default 3s).
	ProbeTimeout time.Duration
	// Scheme overrides the URL scheme used for APIBase and probing (default "https").
	// Primarily intended for tests against httptest servers.
	Scheme string
}

// Detect parses remoteURL using default detection settings (known hosts + HTTPS probing).
func Detect(ctx context.Context, remoteURL string) (Repo, error) {
	return (&Detector{}).Detect(ctx, remoteURL)
}

// Detect parses remoteURL into a Repo, identifying the forge by per-host override,
// well-known hostname, or version-endpoint probing.
func (d *Detector) Detect(ctx context.Context, remoteURL string) (Repo, error) {
	host, owner, name, err := ParseRemoteURL(remoteURL)
	if err != nil {
		return Repo{}, err
	}

	scheme := d.scheme()
	if kind, ok := d.overrideFor(host); ok {
		return Repo{
			Kind:    kind,
			Host:    host,
			Owner:   owner,
			Name:    name,
			APIBase: APIBaseFor(scheme, kind, host),
		}, nil
	}

	if kind, ok := KnownHostKind(host); ok {
		return Repo{
			Kind:    kind,
			Host:    host,
			Owner:   owner,
			Name:    name,
			APIBase: APIBaseFor(scheme, kind, host),
		}, nil
	}

	kind, ok := d.probeHost(ctx, scheme, host)
	if !ok {
		return Repo{}, fmt.Errorf("%w: %s", ErrUnknownForge, host)
	}
	return Repo{
		Kind:    kind,
		Host:    host,
		Owner:   owner,
		Name:    name,
		APIBase: APIBaseFor(scheme, kind, host),
	}, nil
}

// KnownHostKind returns the forge Kind for well-known public forge hosts.
func KnownHostKind(host string) (Kind, bool) {
	switch NormalizeHost(host) {
	case "github.com":
		return GitHub, true
	case "gitlab.com":
		return GitLab, true
	case "codeberg.org":
		return Forgejo, true
	default:
		return "", false
	}
}

// APIBaseFor builds the API root URL for kind and host using scheme ("https" if empty).
func APIBaseFor(scheme string, kind Kind, host string) string {
	if scheme == "" {
		scheme = "https"
	}
	host = NormalizeHost(host)
	switch kind {
	case GitHub:
		if host == "github.com" {
			return scheme + "://api.github.com"
		}
		return scheme + "://" + host + "/api/v3"
	case GitLab:
		return scheme + "://" + host + "/api/v4"
	case Forgejo:
		return scheme + "://" + host + "/api/v1"
	default:
		return scheme + "://" + host
	}
}

// NormalizeHost lowercases and trims a hostname (preserving non-default ports).
func NormalizeHost(host string) string {
	return strings.ToLower(strings.TrimSpace(host))
}

// ParseRemoteURL parses an https, http, ssh, git, or SCP-style git remote URL into
// (host, owner, name). Nested group paths (GitLab) are preserved in owner.
func ParseRemoteURL(remoteURL string) (host, owner, name string, err error) {
	raw := strings.TrimSpace(remoteURL)
	if raw == "" {
		return "", "", "", fmt.Errorf("%w: empty remote URL", ErrUnknownForge)
	}

	var rawPath string
	if strings.Contains(raw, "://") {
		u, parseErr := url.Parse(raw)
		if parseErr != nil || u.Host == "" {
			return "", "", "", fmt.Errorf("%w: invalid remote URL %q", ErrUnknownForge, raw)
		}
		switch strings.ToLower(u.Scheme) {
		case "https", "http":
			host = stripDefaultPort(strings.ToLower(u.Host), strings.ToLower(u.Scheme))
		case "ssh", "git":
			// SSH/git ports (e.g. :2222) are not HTTPS API ports.
			host = strings.ToLower(u.Hostname())
		default:
			return "", "", "", fmt.Errorf("%w: unsupported scheme %q", ErrUnknownForge, u.Scheme)
		}
		rawPath = u.Path
	} else {
		// SCP-style: [user@]host:owner/repo[.git]
		colon := strings.IndexByte(raw, ':')
		if colon <= 0 {
			return "", "", "", fmt.Errorf("%w: unrecognized remote URL %q", ErrUnknownForge, raw)
		}
		hostPart := raw[:colon]
		if at := strings.LastIndexByte(hostPart, '@'); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		hostPart = strings.TrimSpace(hostPart)
		if hostPart == "" || strings.ContainsAny(hostPart, "/\\ ") {
			return "", "", "", fmt.Errorf("%w: invalid host in remote URL %q", ErrUnknownForge, raw)
		}
		host = strings.ToLower(hostPart)
		rawPath = raw[colon+1:]
	}

	owner, name, err = splitRepoPath(rawPath)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: %v", ErrUnknownForge, err)
	}
	return host, owner, name, nil
}

func stripDefaultPort(host, scheme string) string {
	if scheme == "https" && strings.HasSuffix(host, ":443") {
		return strings.TrimSuffix(host, ":443")
	}
	if scheme == "http" && strings.HasSuffix(host, ":80") {
		return strings.TrimSuffix(host, ":80")
	}
	return host
}

func splitRepoPath(rawPath string) (owner, name string, err error) {
	p := strings.Trim(strings.TrimSpace(rawPath), "/")
	p = strings.TrimSuffix(p, ".git")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "", "", fmt.Errorf("missing repository path")
	}
	parts := strings.Split(p, "/")
	clean := make([]string, 0, len(parts))
	for _, seg := range parts {
		if seg != "" {
			clean = append(clean, seg)
		}
	}
	if len(clean) < 2 {
		return "", "", fmt.Errorf("repository path %q must include owner and name", rawPath)
	}
	name = clean[len(clean)-1]
	owner = strings.Join(clean[:len(clean)-1], "/")
	return owner, name, nil
}

func (d *Detector) scheme() string {
	if d != nil && d.Scheme != "" {
		return d.Scheme
	}
	return "https"
}

func (d *Detector) overrideFor(host string) (Kind, bool) {
	if d == nil || len(d.Overrides) == 0 {
		return "", false
	}
	if k, ok := d.Overrides[host]; ok && k != "" {
		return k, true
	}
	for k, v := range d.Overrides {
		if NormalizeHost(k) == host && v != "" {
			return v, true
		}
	}
	return "", false
}

func (d *Detector) httpClient() *http.Client {
	if d != nil && d.HTTPClient != nil {
		return d.HTTPClient
	}
	return &http.Client{Timeout: d.probeTimeout()}
}

func (d *Detector) probeTimeout() time.Duration {
	if d != nil && d.ProbeTimeout > 0 {
		return d.ProbeTimeout
	}
	return defaultProbeTimeout
}

func (d *Detector) probeHost(ctx context.Context, scheme, host string) (Kind, bool) {
	client := d.httpClient()
	timeout := d.probeTimeout()

	// 1. GitLab: GET /api/v4/version
	if d.probeGitLab(ctx, client, timeout, scheme+"://"+host+"/api/v4/version") {
		return GitLab, true
	}
	// 2. Forgejo / Gitea: GET /api/v1/version
	if d.probeForgejo(ctx, client, timeout, scheme+"://"+host+"/api/v1/version") {
		return Forgejo, true
	}
	// 3. GitHub Enterprise: GET /api/v3/meta
	if d.probeGitHubEnterprise(ctx, client, timeout, scheme+"://"+host+"/api/v3/meta") {
		return GitHub, true
	}
	return "", false
}

func (d *Detector) probeGitLab(ctx context.Context, client *http.Client, timeout time.Duration, endpoint string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "konsert")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusOK {
		var payload struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(body, &payload) == nil && strings.TrimSpace(payload.Version) != "" {
			return true
		}
	}
	// Many GitLab instances require authentication on /api/v4/version and return 401
	// alongside GitLab headers.
	if resp.StatusCode == http.StatusUnauthorized {
		if resp.Header.Get("X-Gitlab-Meta") != "" || resp.Header.Get("X-Gitlab-Feature-Category") != "" {
			return true
		}
	}
	return false
}

func (d *Detector) probeForgejo(ctx context.Context, client *http.Client, timeout time.Duration, endpoint string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "konsert")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var payload struct {
		Version string `json:"version"`
	}
	return json.Unmarshal(body, &payload) == nil && strings.TrimSpace(payload.Version) != ""
}

func (d *Detector) probeGitHubEnterprise(ctx context.Context, client *http.Client, timeout time.Duration, endpoint string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "konsert")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.Header.Get("X-GitHub-Enterprise-Version") != "" {
		return true
	}
	if resp.StatusCode == http.StatusOK && resp.Header.Get("X-GitHub-Request-Id") != "" {
		return true
	}
	return false
}
