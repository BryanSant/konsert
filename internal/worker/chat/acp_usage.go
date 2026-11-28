package chat

// Plan usage for ACP agents (SYR-W1GXW). An adapter's optional usage hook fetches the
// account's windows (one vendor HTTP call or a short CLI run). It runs only for a
// signed-in account and on its own schedule (acpUsageTTL), separate from the ACP
// handshake cache. A failed fetch keeps the last good windows with their as-of time
// and says why in usage_note; it never reports 0%.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// acpUsageTTL is how long a usage reading is reused before the hook runs again.
const acpUsageTTL = 5 * time.Minute

// acpUsage is what a usage hook found. Err set (or no windows with a Note) is a failed
// fetch: the last good windows stay.
type acpUsage struct {
	Windows []protocol.AgentRateLimit
	Plan    string
	Note    string
	Err     error
}

type acpUsageEntry struct {
	mu   sync.Mutex
	at   time.Time // last fetch attempt
	good time.Time // last fetch with windows
	last acpUsage  // the last good reading
	note string    // why the latest fetch failed ("" when it didn't)
}

// withUsage adds the account's plan usage to a probe result: from the usage hook,
// cached for acpUsageTTL per account, only when the account is signed in.
func (e *acpEngine) withUsage(ctx context.Context, acct Account, key string, res protocol.AgentAccount) protocol.AgentAccount {
	if e.ad.usage == nil || res.Auth.State != protocol.AuthOK {
		return res
	}
	e.usageMu.Lock()
	ent := e.usages[key]
	if ent == nil {
		ent = &acpUsageEntry{}
		e.usages[key] = ent
	}
	e.usageMu.Unlock()

	ent.mu.Lock()
	defer ent.mu.Unlock()
	now := time.Now()
	if ent.at.IsZero() || now.Sub(ent.at) >= acpUsageTTL {
		u := e.ad.usage(ctx, acct)
		ent.at = now
		switch {
		case u.Err == nil && len(u.Windows) > 0:
			ent.last, ent.good, ent.note = u, now, ""
		case u.Err != nil:
			ent.note = u.Err.Error()
		default:
			ent.note = cmpOrString(u.Note, "no usage reported")
		}
	}
	if len(ent.last.Windows) == 0 {
		if ent.note != "" {
			res.UsageNote = ent.note
		}
		return res
	}
	res.Usage = append([]protocol.AgentRateLimit(nil), ent.last.Windows...)
	if ent.last.Plan != "" && res.Plan == "" {
		res.Plan = ent.last.Plan
	}
	res.UsageNote = ent.last.Note
	if ent.note != "" {
		// The latest fetch failed: the numbers are the last good ones, as of then.
		res.UsageNote = "As of " + ent.good.Local().Format("15:04") + ": " + ent.note
	}
	return res
}

func cmpOrString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// --- vendor HTTP calls ---------------------------------------------------------------

// usageHTTPTimeout and usageMaxBody bound one vendor usage call.
const (
	usageHTTPTimeout = 10 * time.Second
	usageMaxBody     = 1 << 20
)

// usageClient fetches vendor usage endpoints for every account: a timeout, a body cap,
// no redirect to another host, and per-account backoff after 401/403/429 (honouring
// Retry-After). It never logs tokens, cookies or bodies: errors carry status codes only.
type usageClient struct {
	HTTP *http.Client
	Now  func() time.Time

	mu      sync.Mutex
	backoff map[string]time.Time // account key → no calls before
}

func newUsageClient() *usageClient {
	return &usageClient{Now: time.Now, backoff: map[string]time.Time{}, HTTP: &http.Client{
		Timeout: usageHTTPTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				return errors.New("redirect to another host refused")
			}
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}}
}

// errUsageBackoff says a call was skipped while the account backs off.
var errUsageBackoff = errors.New("usage service asked to wait")

// Get fetches u for account key with the given headers and returns the body.
func (c *usageClient) Get(ctx context.Context, key, u string, header http.Header) ([]byte, error) {
	return c.do(ctx, key, http.MethodGet, u, "", header)
}

// Post sends a JSON body to u for account key and returns the answer's body.
func (c *usageClient) Post(ctx context.Context, key, u, body string, header http.Header) ([]byte, error) {
	return c.do(ctx, key, http.MethodPost, u, body, header)
}

func (c *usageClient) do(ctx context.Context, key, method, u, body string, header http.Header) ([]byte, error) {
	now := c.Now()
	c.mu.Lock()
	until := c.backoff[key]
	c.mu.Unlock()
	if now.Before(until) {
		return nil, fmt.Errorf("%w until %s", errUsageBackoff, until.Local().Format("15:04"))
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("usage request failed: %v", ue.Err) // the URL may carry nothing secret, but say less
		}
		return nil, errors.New("usage request failed")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		wait := retryAfter(resp.Header.Get("Retry-After"), now)
		if wait <= 0 {
			wait = 5 * time.Minute
			if resp.StatusCode == http.StatusTooManyRequests {
				wait = time.Minute
			}
		}
		c.mu.Lock()
		c.backoff[key] = now.Add(wait)
		c.mu.Unlock()
		return nil, fmt.Errorf("usage service answered %d", resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("usage service answered %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, usageMaxBody+1))
	if err != nil {
		return nil, errors.New("usage response unreadable")
	}
	if len(data) > usageMaxBody {
		return nil, errors.New("usage response too large")
	}
	return data, nil
}

// retryAfter parses a Retry-After header (seconds, or an HTTP date).
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return t.Sub(now)
	}
	return 0
}

// --- per-account credentials -----------------------------------------------------------

// accountDir is a directory the account's CLI keeps its state in: the account's own
// envKey (GROK_HOME, CURSOR_CONFIG_DIR, …), else defaultRel under the account's HOME,
// else under the user's home. Never the global default when the account points elsewhere.
func accountDir(acct Account, envKey, defaultRel string) string {
	if v := strings.TrimSpace(acct.Env[envKey]); v != "" {
		return accountExpandHome(v, acct)
	}
	home := strings.TrimSpace(acct.Env["HOME"])
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, defaultRel)
}

func accountExpandHome(p string, acct Account) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home := strings.TrimSpace(acct.Env["HOME"])
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, p[2:])
}
