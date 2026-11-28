package chat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

func usageEngine(hook func(context.Context, Account) acpUsage) *acpEngine {
	return &acpEngine{ad: &acpAdapter{Agent: "grok", usage: hook}, probes: map[string]*acpProbeEntry{}, usages: map[string]*acpUsageEntry{}}
}

func TestACPUsageHookRunsOnlySignedInOnItsOwnSchedule(t *testing.T) {
	var calls atomic.Int32
	e := usageEngine(func(context.Context, Account) acpUsage {
		calls.Add(1)
		return acpUsage{Windows: []protocol.AgentRateLimit{{Window: "monthly_cursor_models", Label: "Cursor Models", UsedPercent: 42}}, Plan: "Pro"}
	})
	acct := Account{ID: "grok"}
	ok := protocol.AgentAccount{ID: "grok", Auth: protocol.AgentAuthStatus{State: protocol.AuthOK}}
	out := protocol.AgentAccount{ID: "grok", Auth: protocol.AgentAuthStatus{State: protocol.AuthLoggedOut}}
	if got := e.withUsage(t.Context(), acct, "k", out); len(got.Usage) != 0 || calls.Load() != 0 {
		t.Fatalf("signed out: %+v (%d calls)", got, calls.Load())
	}
	got := e.withUsage(t.Context(), acct, "k", ok)
	if len(got.Usage) != 1 || got.Usage[0].Label != "Cursor Models" || got.Plan != "Pro" || calls.Load() != 1 {
		t.Fatalf("first: %+v (%d calls)", got, calls.Load())
	}
	// Within acpUsageTTL the reading is reused, whatever the handshake cache does.
	for range 3 {
		e.withUsage(t.Context(), acct, "k", ok)
	}
	if calls.Load() != 1 {
		t.Fatalf("hook ran %d times within its TTL", calls.Load())
	}
}

func TestACPUsageFailedFetchKeepsLastGoodNeverZero(t *testing.T) {
	fail := false
	e := usageEngine(func(context.Context, Account) acpUsage {
		if fail {
			return acpUsage{Err: errors.New("usage service answered 503")}
		}
		return acpUsage{Windows: []protocol.AgentRateLimit{{Window: "monthly", UsedPercent: 70}}}
	})
	ok := protocol.AgentAccount{ID: "a", Auth: protocol.AgentAuthStatus{State: protocol.AuthOK}}
	e.withUsage(t.Context(), Account{ID: "a"}, "k", ok)
	fail = true
	e.usages["k"].at = time.Now().Add(-acpUsageTTL) // due again
	got := e.withUsage(t.Context(), Account{ID: "a"}, "k", ok)
	if len(got.Usage) != 1 || got.Usage[0].UsedPercent != 70 || !strings.HasPrefix(got.UsageNote, "As of ") || !strings.Contains(got.UsageNote, "503") {
		t.Fatalf("after a failed fetch: %+v", got)
	}
	// A first fetch that fails reports why, with no numbers (not 0%).
	e2 := usageEngine(func(context.Context, Account) acpUsage {
		return acpUsage{Err: errors.New("usage service answered 401")}
	})
	got = e2.withUsage(t.Context(), Account{ID: "b"}, "k2", ok)
	if len(got.Usage) != 0 || !strings.Contains(got.UsageNote, "401") {
		t.Fatalf("first fetch failed: %+v", got)
	}
}

func TestUsageClientBacksOffAndCaps(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			w.Write([]byte(strings.Repeat("x", usageMaxBody+10)))
		case "/away":
			http.Redirect(w, r, "http://example.invalid/steal", http.StatusFound)
		default:
			if s := int(status.Load()); s != 200 {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(s)
				return
			}
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()
	c := newUsageClient()
	now := time.Now()
	c.Now = func() time.Time { return now }
	if body, err := c.Get(t.Context(), "acct-a", srv.URL+"/u", nil); err != nil || string(body) != `{"ok":true}` {
		t.Fatalf("ok: %q %v", body, err)
	}
	if _, err := c.Get(t.Context(), "acct-a", srv.URL+"/big", nil); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("size cap: %v", err)
	}
	if _, err := c.Get(t.Context(), "acct-a", srv.URL+"/away", nil); err == nil {
		t.Fatal("followed a redirect to another host")
	}
	status.Store(429)
	if _, err := c.Get(t.Context(), "acct-a", srv.URL+"/u", nil); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("429: %v", err)
	}
	status.Store(200)
	// Backing off this account for Retry-After (120 s); another account is unaffected.
	if _, err := c.Get(t.Context(), "acct-a", srv.URL+"/u", nil); !errors.Is(err, errUsageBackoff) {
		t.Fatalf("not backing off: %v", err)
	}
	if _, err := c.Get(t.Context(), "acct-b", srv.URL+"/u", nil); err != nil {
		t.Fatalf("other account: %v", err)
	}
	now = now.Add(121 * time.Second)
	if _, err := c.Get(t.Context(), "acct-a", srv.URL+"/u", nil); err != nil {
		t.Fatalf("after Retry-After: %v", err)
	}
}

// Each account reads its own credentials: two grok accounts, two homes.
func TestAccountDirPerAccount(t *testing.T) {
	a := Account{ID: "grok", Env: map[string]string{"HOME": "/home/u"}}
	b := Account{ID: "grok2", Env: map[string]string{"GROK_HOME": "/home/u/.grok2"}}
	c := Account{ID: "cursor2", Env: map[string]string{"HOME": "/home/u", "CURSOR_CONFIG_DIR": "~/.cursor2"}}
	if got := accountDir(a, "GROK_HOME", ".grok"); got != filepath.Join("/home/u", ".grok") {
		t.Fatalf("a: %s", got)
	}
	if got := accountDir(b, "GROK_HOME", ".grok"); got != "/home/u/.grok2" {
		t.Fatalf("b: %s", got)
	}
	if got := accountDir(c, "CURSOR_CONFIG_DIR", ".cursor"); got != filepath.Join("/home/u", ".cursor2") {
		t.Fatalf("c: %s", got)
	}
}
