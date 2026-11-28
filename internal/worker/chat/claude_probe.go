package chat

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

const (
	claudeUsageURL        = "https://api.anthropic.com/api/oauth/usage"
	claudeOAuthBeta       = "oauth-2025-04-20"
	claudeUsageMinPeriod  = 5 * time.Minute
	claudeUsageDisableEnv = "SYRE_DISABLE_CLAUDE_USAGE"
	claudeExpiringWithin  = 24 * time.Hour
)

// claudeUsageEntry caches usage per account: the last fetched snapshot (polled at most
// every claudeUsageMinPeriod) and the windows the account's own sessions reported in
// rate_limit_event lines (free, and the only source for some setup-token accounts).
type claudeUsageEntry struct {
	fetchedAt time.Time
	limits    []protocol.AgentRateLimit
	note      string
	// blockedUntil holds off the usage endpoint after a 429 (its Retry-After) or a
	// 403 (the token cannot read usage): polling again only renews the limit.
	blockedUntil time.Time

	stream   map[string]protocol.AgentRateLimit
	streamAt map[string]time.Time
}

// recordStreamLimits remembers windows seen in a session's rate_limit_event.
func (e *claudeEngine) recordStreamLimits(acctID string, limits []protocol.AgentRateLimit) {
	if len(limits) == 0 {
		return
	}
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	ent := e.usageCache[acctID]
	if ent.stream == nil {
		ent.stream = map[string]protocol.AgentRateLimit{}
		ent.streamAt = map[string]time.Time{}
	}
	now := e.now()
	for _, l := range limits {
		ent.stream[l.Window] = l
		ent.streamAt[l.Window] = now
	}
	e.usageCache[acctID] = ent
}

// --- Probe --------------------------------------------------------------------------------

func (e *claudeEngine) Probe(ctx context.Context, acct Account) protocol.AgentAccount {
	out := protocol.AgentAccount{
		ID:              acct.ID,
		Agent:           "claude",
		Label:           acct.Label,
		IsDefault:       acct.IsDefault,
		Approvals:       claudeApprovalModes(),
		DefaultApproval: "default",
		ChatSupported:   true,
		Fork:            true, // --resume <id> --fork-session
		CheckedAt:       e.now().UTC(),
	}
	if out.Label == "" {
		out.Label = claudeLabel(acct)
	}
	if v, code, err := e.run(ctx, e.spec(acct, e.probeDir, "--version")); err != nil {
		out.ChatSupported = false
		out.Auth = protocol.AgentAuthStatus{State: protocol.AuthUnknown, Detail: "claude is not installed or not runnable: " + err.Error()}
		return out
	} else if code == 0 {
		out.Version = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(string(v)), "(Claude Code)"))
	}

	auth := e.authStatus(ctx, acct)
	if auth.State == protocol.AuthOK {
		auth.Method = claudeAuthMode(auth.Method, acct)
	}
	out.Auth = auth.AgentAuthStatus
	out.Plan = auth.plan

	init, usage, initErr := e.probeInit(ctx, acct, auth.State == protocol.AuthOK && e.usageDue(acct.ID))
	if initErr == nil && len(init.Models) > 0 {
		out.Models, out.DefaultModel = claudeModels(init.Models)
		out.ModelsSource = "discovered"
		if out.Plan == "" {
			out.Plan = init.Account.SubscriptionType
		}
		if out.Auth.Email == "" && auth.State == protocol.AuthOK {
			out.Auth.Email = init.Account.Email
		}
	} else {
		out.Models, out.DefaultModel = claudeFallbackModels()
		out.ModelsSource = "fallback"
	}

	if auth.State != protocol.AuthOK {
		out.UsageNote = "Sign in to see usage."
		return out
	}
	if auth.Method == "api_key" {
		// Pay per token: no subscription windows; the hub shows what chats spent.
		out.UsageNote = "API billing: pay per token, no subscription windows."
		return out
	}
	out.Usage, out.UsageNote = e.usage(ctx, acct, usage)
	return out
}

type claudeAuth struct {
	protocol.AgentAuthStatus
	plan string
}

// authStatus runs `claude auth status` (JSON; exit 1 when logged out).
func (e *claudeEngine) authStatus(ctx context.Context, acct Account) claudeAuth {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stdout, code, err := e.run(cctx, e.spec(acct, e.probeDir, "auth", "status"))
	if err != nil {
		return claudeAuth{AgentAuthStatus: protocol.AgentAuthStatus{State: protocol.AuthUnknown, Detail: "claude auth status failed: " + err.Error()}}
	}
	return claudeParseAuth(stdout, code, acct, e.now())
}

func claudeParseAuth(stdout []byte, code int, acct Account, now time.Time) claudeAuth {
	var st struct {
		LoggedIn         bool    `json:"loggedIn"`
		AuthMethod       string  `json:"authMethod"`
		Email            string  `json:"email"`
		LoginExpiresAt   *string `json:"loginExpiresAt"`
		SubscriptionType string  `json:"subscriptionType"`
		PlanTier         string  `json:"planTier"`
	}
	parseErr := unmarshal(stdout, &st)
	if code != 0 || (parseErr == nil && !st.LoggedIn) {
		return claudeAuth{AgentAuthStatus: protocol.AgentAuthStatus{
			State:   protocol.AuthLoggedOut,
			Detail:  "Not signed in.",
			Relogin: claudeRelogin(acct),
		}}
	}
	if parseErr != nil {
		return claudeAuth{AgentAuthStatus: protocol.AgentAuthStatus{State: protocol.AuthUnknown, Detail: "could not parse claude auth status"}}
	}
	a := claudeAuth{AgentAuthStatus: protocol.AgentAuthStatus{State: protocol.AuthOK, Email: st.Email}, plan: st.SubscriptionType}
	switch st.AuthMethod {
	case "claude.ai", "oauth":
		a.Method = "oauth"
	case "oauth_token":
		a.Method = "setup_token"
	case "api_key", "console", "apiKey":
		a.Method = "api_key"
	default:
		a.Method = st.AuthMethod
	}
	if st.LoginExpiresAt != nil && *st.LoginExpiresAt != "" {
		if ts, err := time.Parse(time.RFC3339, *st.LoginExpiresAt); err == nil {
			ts = ts.UTC()
			a.ExpiresAt = &ts
			if ts.Sub(now) < claudeExpiringWithin {
				a.State = protocol.AuthExpiring
				a.Detail = "The sign-in expires soon; Claude Code normally refreshes it on use."
				a.Relogin = claudeRelogin(acct)
			}
		}
	}
	return a
}

type claudeInitModel struct {
	Value                 string   `json:"value"`
	ResolvedModel         string   `json:"resolvedModel"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
}

type claudeInitResponse struct {
	Models  []claudeInitModel `json:"models"`
	Account struct {
		Email            string `json:"email"`
		SubscriptionType string `json:"subscriptionType"`
	} `json:"account"`
	CurrentPermissionMode string `json:"current_permission_mode"`
}

// probeInit runs a short-lived stream-json process that never receives a user message:
// initialize gives the models; get_usage (when wantUsage) the subscription windows, read
// by the CLI with its own (refreshed) credentials.
func (e *claudeEngine) probeInit(ctx context.Context, acct Account, wantUsage bool) (claudeInitResponse, jsontext.Value, error) {
	var init claudeInitResponse
	cctx, cancel := context.WithTimeout(ctx, e.initTimeout)
	defer cancel()
	proc, err := e.launch(cctx, e.spec(acct, e.probeDir,
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--no-session-persistence"))
	if err != nil {
		return init, nil, err
	}
	defer proc.Kill()

	type resp struct {
		id   string
		ok   bool
		body jsontext.Value
	}
	ch := make(chan resp, 4)
	go func() {
		sc := ccLineReader(proc.Stdout())
		for sc.Scan() {
			var l claudeLine
			if unmarshal(sc.Bytes(), &l) != nil || l.Type != "control_response" {
				continue
			}
			var r struct {
				Subtype   string         `json:"subtype"`
				RequestID string         `json:"request_id"`
				Response  jsontext.Value `json:"response"`
			}
			if unmarshal(l.Response, &r) == nil {
				ch <- resp{id: r.RequestID, ok: r.Subtype == "success", body: r.Response}
			}
		}
		close(ch)
	}()
	send := func(id, subtype string) error {
		b, _ := marshalLine(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{"subtype": subtype}})
		_, err := proc.Stdin().Write(append(b, '\n'))
		return err
	}
	wait := func(id string) (resp, error) {
		for {
			select {
			case r, ok := <-ch:
				if !ok {
					return resp{}, errors.New("claude exited during probe")
				}
				if r.id == id {
					return r, nil
				}
			case <-cctx.Done():
				return resp{}, cctx.Err()
			}
		}
	}
	if err := send("probe-init", "initialize"); err != nil {
		return init, nil, err
	}
	r, err := wait("probe-init")
	if err != nil {
		return init, nil, err
	}
	if !r.ok {
		return init, nil, errors.New("initialize failed")
	}
	if err := unmarshal(r.body, &init); err != nil {
		return init, nil, err
	}
	var usage jsontext.Value
	if wantUsage {
		if send("probe-usage", "get_usage") == nil {
			if r, err := wait("probe-usage"); err == nil && r.ok {
				usage = r.body
			}
		}
	}
	_ = proc.Stdin().Close()
	return init, usage, nil
}

func claudeModels(ms []claudeInitModel) ([]protocol.AgentModel, string) {
	out := make([]protocol.AgentModel, 0, len(ms))
	def := ""
	for _, m := range ms {
		am := protocol.AgentModel{ID: m.Value, DisplayName: m.DisplayName, Description: m.Description}
		if am.DisplayName == "" {
			am.DisplayName = m.Value
		}
		if m.Value == "default" {
			am.IsDefault = true
			def = m.Value
		}
		for _, lv := range m.SupportedEffortLevels {
			label, short := ccEffortLabel(lv)
			am.Efforts = append(am.Efforts, protocol.AgentEffort{ID: lv, Label: label, Short: short})
		}
		out = append(out, am)
	}
	return out, def
}

func claudeFallbackModels() ([]protocol.AgentModel, string) {
	return []protocol.AgentModel{
		{ID: "default", DisplayName: "Default (recommended)", IsDefault: true},
		{ID: "opus", DisplayName: "Opus"},
		{ID: "sonnet", DisplayName: "Sonnet"},
		{ID: "haiku", DisplayName: "Haiku"},
	}, "default"
}

// --- Usage ----------------------------------------------------------------------------------

func (e *claudeEngine) usageDisabled() bool {
	v := os.Getenv(claudeUsageDisableEnv)
	return v == "1" || strings.EqualFold(v, "true")
}

// usageDue reports whether the account's usage snapshot is older than the poll period.
func (e *claudeEngine) usageDue(acctID string) bool {
	if e.usageDisabled() {
		return false
	}
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	ent, ok := e.usageCache[acctID]
	if ok && e.now().Before(ent.blockedUntil) {
		return false
	}
	return !ok || ent.fetchedAt.IsZero() || e.now().Sub(ent.fetchedAt) >= claudeUsageMinPeriod
}

// usage returns the account's windows: a fresh fetch when due (get_usage from the probe
// process, else the OAuth usage endpoint), merged with what sessions reported in-stream.
func (e *claudeEngine) usage(ctx context.Context, acct Account, fromCLI jsontext.Value) ([]protocol.AgentRateLimit, string) {
	if !e.usageDisabled() && e.usageDue(acct.ID) {
		var limits []protocol.AgentRateLimit
		note := ""
		var err error
		if len(fromCLI) > 0 {
			limits, err = claudeParseCLIUsage(fromCLI)
		}
		if len(fromCLI) == 0 || err != nil {
			limits, err = e.fetchUsage(ctx, acct)
		}
		var block *claudeUsageBlock
		if err != nil {
			note = "Usage unavailable (" + err.Error() + "); showing what chats report."
			limits = nil
			errors.As(err, &block)
		}
		e.usageMu.Lock()
		ent := e.usageCache[acct.ID]
		if block != nil {
			// Keep the last good numbers (they say how old they are) and wait it out.
			ent.blockedUntil, ent.note = block.until, note
			if ent.fetchedAt.IsZero() {
				ent.fetchedAt = e.now()
			}
		} else {
			ent.fetchedAt, ent.limits, ent.note = e.now(), limits, note
		}
		e.usageCache[acct.ID] = ent
		e.usageMu.Unlock()
	}
	e.usageMu.Lock()
	ent := e.usageCache[acct.ID]
	e.usageMu.Unlock()
	merged := claudeMergeLimits(ent, e.now())
	note := ent.note
	if e.usageDisabled() {
		note = "Usage polling is disabled (" + claudeUsageDisableEnv + "); showing what chats report."
	}
	if len(merged) > 0 && note != "" && len(ent.limits) == 0 {
		note = strings.Replace(note, "showing what chats report", "showing what the last chat reported", 1)
	}
	if len(merged) == 0 && note == "" {
		note = "No usage reported yet."
	}
	return merged, note
}

// claudeMergeLimits combines the fetched snapshot with in-stream windows: a stream
// window replaces the fetched one when it is newer, and an exhausted stream window
// stays until its reset time passes.
func claudeMergeLimits(ent claudeUsageEntry, now time.Time) []protocol.AgentRateLimit {
	byWin := map[string]protocol.AgentRateLimit{}
	var order []string
	fetched := ent.fetchedAt
	for _, l := range ent.limits {
		if _, ok := byWin[l.Window]; !ok {
			order = append(order, l.Window)
		}
		if l.ObservedAt == nil && !fetched.IsZero() {
			at := fetched.UTC()
			l.ObservedAt = &at
		}
		byWin[l.Window] = l
	}
	for w, l := range ent.stream {
		if l.ResetsAt != nil && l.ResetsAt.Before(now) {
			continue // that window has reset since
		}
		cur, ok := byWin[w]
		if ok && !ent.streamAt[w].After(ent.fetchedAt) && l.State != "exhausted" {
			continue
		}
		if ok && l.UsedPercent == 0 && cur.UsedPercent > 0 && l.State != "exhausted" {
			l.UsedPercent = cur.UsedPercent
		}
		if !ok {
			order = append(order, w)
		}
		if at := ent.streamAt[w]; !at.IsZero() {
			at = at.UTC()
			l.ObservedAt = &at
		}
		byWin[w] = l
	}
	sort.SliceStable(order, func(i, j int) bool { return claudeWindowRank(order[i]) < claudeWindowRank(order[j]) })
	out := make([]protocol.AgentRateLimit, 0, len(order))
	for _, w := range order {
		out = append(out, byWin[w])
	}
	return out
}

func claudeWindowRank(w string) int {
	switch w {
	case "five_hour":
		return 0
	case "seven_day":
		return 1
	case "seven_day_opus":
		return 2
	case "seven_day_sonnet":
		return 3
	}
	return 10
}

type claudeUsageBucket struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// claudeBucketWindows are the usage-response buckets surfaced as windows.
var claudeBucketWindows = []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet"}

func claudeBucketLimit(window string, b *claudeUsageBucket) (protocol.AgentRateLimit, bool) {
	if b == nil || b.Utilization == nil {
		return protocol.AgentRateLimit{}, false
	}
	rl := protocol.AgentRateLimit{Window: window, UsedPercent: *b.Utilization, State: claudePercentState(*b.Utilization)}
	if b.ResetsAt != nil {
		if ts, err := time.Parse(time.RFC3339Nano, *b.ResetsAt); err == nil {
			ts = ts.UTC()
			rl.ResetsAt = &ts
		}
	}
	return rl, true
}

func claudePercentState(pct float64) string {
	switch {
	case pct >= 100:
		return "exhausted"
	case pct >= 80:
		return "warning"
	}
	return "ok"
}

// claudeParseUsageBuckets maps the /api/oauth/usage shape (also inside get_usage's
// rate_limits) to windows.
func claudeParseUsageBuckets(raw jsontext.Value) ([]protocol.AgentRateLimit, error) {
	var buckets map[string]jsontext.Value
	if err := unmarshal(raw, &buckets); err != nil {
		return nil, fmt.Errorf("unexpected usage response")
	}
	var out []protocol.AgentRateLimit
	for _, w := range claudeBucketWindows {
		var b claudeUsageBucket
		if v, ok := buckets[w]; !ok || string(v) == "null" || unmarshal(v, &b) != nil {
			continue
		}
		if rl, ok := claudeBucketLimit(w, &b); ok {
			out = append(out, rl)
		}
	}
	// Model-scoped weekly limits ("Fable 52%").
	if v, ok := buckets["model_scoped"]; ok {
		var scoped []struct {
			DisplayName string `json:"display_name"`
			claudeUsageBucket
		}
		if unmarshal(v, &scoped) == nil {
			for _, s := range scoped {
				name := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s.DisplayName), " ", "_"))
				if name == "" {
					continue
				}
				b := s.claudeUsageBucket
				if rl, ok := claudeBucketLimit("seven_day_"+name, &b); ok {
					dup := false
					for _, o := range out {
						dup = dup || o.Window == rl.Window
					}
					if !dup {
						out = append(out, rl)
					}
				}
			}
		}
	}
	return out, nil
}

// claudeParseCLIUsage reads the get_usage control response.
func claudeParseCLIUsage(raw jsontext.Value) ([]protocol.AgentRateLimit, error) {
	var r struct {
		Available  *bool          `json:"rate_limits_available"`
		RateLimits jsontext.Value `json:"rate_limits"`
	}
	if err := unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if (r.Available != nil && !*r.Available) || len(r.RateLimits) == 0 || string(r.RateLimits) == "null" {
		return nil, errors.New("the CLI reports no rate limits for this account")
	}
	return claudeParseUsageBuckets(r.RateLimits)
}

// fetchUsage calls GET /api/oauth/usage with the account's OAuth access token. The
// token is used in-process only: never logged, never included in errors.
func (e *claudeEngine) fetchUsage(ctx context.Context, acct Account) ([]protocol.AgentRateLimit, error) {
	token, err := e.tokenReader(ctx, acct)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("no OAuth token found for this account")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "syre")
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, errors.New("usage endpoint unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("usage endpoint refused the token (401)")
	case resp.StatusCode == http.StatusForbidden:
		return nil, &claudeUsageBlock{until: e.now().Add(claudeUsageScopeWait), reason: "the token cannot read usage (403)"}
	case resp.StatusCode == http.StatusTooManyRequests:
		wait := claudeRetryAfter(resp.Header.Get("Retry-After"), e.now())
		until := e.now().Add(wait)
		return nil, &claudeUsageBlock{until: until, reason: "usage endpoint rate-limited (429) until " + until.Local().Format("15:04")}
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("usage endpoint returned HTTP %d", resp.StatusCode)
	}
	return claudeParseUsageBuckets(body)
}

// claudeReadOAuthToken finds the account's OAuth access token: CLAUDE_CODE_OAUTH_TOKEN
// (setup-token accounts), the macOS Keychain item of the default config dir, or
// $CLAUDE_CONFIG_DIR/.credentials.json.
func claudeReadOAuthToken(ctx context.Context, acct Account) (string, error) {
	if t := ccEnvValue(acct.Env, "CLAUDE_CODE_OAUTH_TOKEN"); t != "" {
		return t, nil
	}
	home, _ := os.UserHomeDir()
	cfg := acct.Env["CLAUDE_CONFIG_DIR"]
	if cfg == "" {
		cfg = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	defaultDir := cfg == "" || filepath.Clean(cfg) == filepath.Join(home, ".claude")
	if runtime.GOOS == "darwin" {
		if !defaultDir {
			return "", errors.New("no readable OAuth token for a non-default config dir on macOS")
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, "security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
		if err != nil {
			return "", errors.New("could not read the Claude Code Keychain item")
		}
		return claudeTokenFromCredentials(out)
	}
	if cfg == "" {
		cfg = filepath.Join(home, ".claude")
	}
	data, err := os.ReadFile(filepath.Join(cfg, ".credentials.json"))
	if err != nil {
		return "", errors.New("no .credentials.json in the account's config dir")
	}
	return claudeTokenFromCredentials(data)
}

func claudeTokenFromCredentials(data []byte) (string, error) {
	var c struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := unmarshal(data, &c); err != nil || c.ClaudeAiOauth.AccessToken == "" {
		return "", errors.New("credentials hold no OAuth access token")
	}
	return c.ClaudeAiOauth.AccessToken, nil
}

// claudeUsageScopeWait is how long a token that may not read usage (403) waits before
// the endpoint is asked again.
const claudeUsageScopeWait = 24 * time.Hour

// claudeUsageBlock is a usage endpoint answer to wait out (429, 403).
type claudeUsageBlock struct {
	until  time.Time
	reason string
}

func (b *claudeUsageBlock) Error() string { return b.reason }

// claudeRetryAfter reads a Retry-After header (seconds or an HTTP date); 30 minutes
// when absent or unreadable, at most a day.
func claudeRetryAfter(v string, now time.Time) time.Duration {
	const def, most = 30 * time.Minute, 24 * time.Hour
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return min(time.Duration(secs)*time.Second, most)
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return min(t.Sub(now), most)
	}
	return def
}

// claudeAuthMode tells the three ways a Claude account authenticates (SYR-BA6HV):
// "oauth" (an interactive subscription login), "setup_token" (a long-lived subscription
// token, CLAUDE_CODE_OAUTH_TOKEN) and "api_key" (pay per token). The token prefix is
// only checked locally, never logged or sent.
func claudeAuthMode(reported string, acct Account) string {
	if t := ccEnvValue(acct.Env, "CLAUDE_CODE_OAUTH_TOKEN"); t != "" {
		if strings.HasPrefix(t, "sk-ant-api") {
			return "api_key" // an API key put in the token variable
		}
		return "setup_token"
	}
	if reported == "oauth" || reported == "setup_token" {
		return reported
	}
	if ccEnvValue(acct.Env, "ANTHROPIC_API_KEY") != "" || reported == "api_key" {
		return "api_key"
	}
	return reported
}
