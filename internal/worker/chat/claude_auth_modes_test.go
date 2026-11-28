package chat

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

func readModeFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "claude_modes", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The three ways a Claude account authenticates, from recorded `claude auth status`
// output (SYR-BA6HV). A long-lived token is a subscription, never an API key.
func TestClaudeAuthModes(t *testing.T) {
	now := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	for _, c := range []struct {
		fixture string
		env     map[string]string
		want    string
		plan    string
	}{
		{"auth_status_oauth.json", nil, "oauth", "max"},
		{"auth_status_token.json", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-fake"}, "setup_token", ""},
		{"auth_status_apikey.json", map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api03-fake"}, "api_key", ""},
		// An API key put in the token variable is billed per token.
		{"auth_status_token.json", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-api03-fake"}, "api_key", ""},
		// A token account whose CLI reports something odd is still a subscription.
		{"auth_status_apikey.json", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-fake"}, "setup_token", ""},
	} {
		acct := Account{ID: "a", Env: c.env}
		a := claudeParseAuth(readModeFixture(t, c.fixture), 0, acct, now)
		if a.State != protocol.AuthOK {
			t.Fatalf("%s: state %s", c.fixture, a.State)
		}
		if got := claudeAuthMode(a.Method, acct); got != c.want || a.plan != c.plan {
			t.Errorf("%s %v: mode %q plan %q, want %q %q", c.fixture, c.env, got, a.plan, c.want, c.plan)
		}
	}
}

// Token mode: the CLI reports no usage, the endpoint answers 429 with Retry-After, and
// the windows the account's own sessions report in-stream are what the user sees, with
// when they were seen.
func TestClaudeTokenUsageBacksOffAndShowsStreamWindows(t *testing.T) {
	t.Setenv(claudeUsageDisableEnv, "")
	if _, err := claudeParseCLIUsage(readModeFixture(t, "get_usage_token.json")); err == nil {
		t.Fatal("token-mode get_usage parsed as windows")
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "1775")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"Rate limited. Please try again later."}}`))
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	e := newClaudeEngine()
	e.now = func() time.Time { return now }
	e.usageURL = srv.URL
	e.tokenReader = func(context.Context, Account) (string, error) { return "sk-ant-oat01-fake", nil }
	acct := Account{ID: "claude2"}

	limits, note := e.usage(context.Background(), acct, readModeFixture(t, "get_usage_token.json"))
	if calls != 1 || len(limits) != 0 || !strings.Contains(note, "rate-limited (429) until") || strings.Contains(note, "sk-ant") {
		t.Fatalf("first poll: calls %d limits %v note %q", calls, limits, note)
	}
	// Polling again within Retry-After does not ask again.
	now = now.Add(10 * time.Minute)
	if e.usageDue(acct.ID) {
		t.Fatal("usage due inside Retry-After")
	}
	_, _ = e.usage(context.Background(), acct, nil)
	if calls != 1 {
		t.Fatalf("asked again inside Retry-After: %d calls", calls)
	}

	// A session on the account reports its windows in-stream.
	var line claudeLine
	if err := json.Unmarshal(readModeFixture(t, "rate_limit_event_token.json"), &line); err != nil {
		t.Fatal(err)
	}
	e.recordStreamLimits(acct.ID, claudeRateLimits(line.RateLimitInfo))
	seen := now
	now = now.Add(2 * time.Minute)
	limits, _ = e.usage(context.Background(), acct, nil)
	if len(limits) != 2 || limits[0].Window != "five_hour" || limits[1].Window != "seven_day" {
		t.Fatalf("stream windows = %+v", limits)
	}
	for _, l := range limits {
		if l.ObservedAt == nil || !l.ObservedAt.Equal(seen) || l.UsedPercent < 20 {
			t.Fatalf("window %+v: want observed_at %s", l, seen)
		}
	}
	// After Retry-After it may ask again.
	now = now.Add(30 * time.Minute)
	if !e.usageDue(acct.ID) {
		t.Fatal("usage not due after Retry-After")
	}
}

func TestClaudeRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"":                              30 * time.Minute,
		"120":                           2 * time.Minute,
		"junk":                          30 * time.Minute,
		"999999":                        24 * time.Hour,
		"Fri, 27 Nov 2026 12:10:00 GMT": 10 * time.Minute,
	} {
		if got := claudeRetryAfter(v, now); got != want {
			t.Errorf("%q: %s, want %s", v, got, want)
		}
	}
}
