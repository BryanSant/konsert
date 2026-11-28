package chat

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func cursorFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/cursor/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func windowsOf(u acpUsage) map[string]string {
	out := map[string]string{}
	for _, w := range u.Windows {
		r := "-"
		if w.ResetsAt != nil {
			r = w.ResetsAt.Format("2006-01-02")
		}
		out[w.Window] = fmt.Sprintf("%s %.1f %s", w.Label, w.UsedPercent, r)
	}
	return out
}

func TestParseCursorUsageForms(t *testing.T) {
	// An individual plan (recorded from cursor.com, 2026-11-28): pools by model class.
	u, err := parseCursorUsageSummary(cursorFixture(t, "usage-summary-individual.json"))
	if err != nil || u.Plan != "premium" {
		t.Fatalf("individual: %+v %v", u, err)
	}
	got := windowsOf(u)
	if got["monthly"] != "Plan total 0.0 2026-12-01" || got["monthly_cursor_models"] != "Cursor Models 0.0 2026-12-01" ||
		got["monthly_other_models"] != "Other Models 0.0 2026-12-01" || len(got) != 3 {
		t.Fatalf("individual windows %v", got)
	}
	// A team plan: overall used/limit plus the team's on-demand pool.
	u, err = parseCursorUsageSummary(cursorFixture(t, "usage-summary-team.json"))
	got = windowsOf(u)
	if err != nil || u.Plan != "enterprise" || got["monthly"] != "Plan total 70.4 2026-12-01" || got["monthly_on_demand"] != "On-demand 12.0 2026-12-01" {
		t.Fatalf("team: %v %v", got, err)
	}
	// Unlimited: no windows, said so.
	u, err = parseCursorUsageSummary([]byte(`{"isUnlimited":true,"membershipType":"enterprise"}`))
	if err != nil || len(u.Windows) != 0 || u.Note != "Unlimited plan" {
		t.Fatalf("unlimited: %+v %v", u, err)
	}
	// Unified plans: get-current-period-usage, cycle end in Unix ms.
	u, err = parseCursorPeriodUsage(cursorFixture(t, "current-period-usage.json"))
	got = windowsOf(u)
	if err != nil || got["monthly"] != "Plan total 91.0 2026-12-01" || got["monthly_other_models"] != "Other Models 71.2 2026-12-01" {
		t.Fatalf("period: %v %v", got, err)
	}
	// Recorded (cursor.com, 2026-11-28): the period's limit is hit while usage-summary
	// still showed the billing cycle at 0%: both pools read exhausted.
	u, err = parseCursorPeriodUsage(cursorFixture(t, "current-period-usage-limit.json"))
	if err != nil || len(u.Windows) != 2 || u.Windows[0].State != "exhausted" || u.Windows[1].Window != "monthly_cursor_models" {
		t.Fatalf("period at its limit: %+v %v", u, err)
	}
	// Legacy request-based plans: per-model requests against a monthly maximum.
	w, err := parseCursorLegacyUsage(cursorFixture(t, "usage-legacy.json"))
	if err != nil || len(w) != 1 || w[0].Window != "monthly_gpt_4" || w[0].UsedPercent != 84 || w[0].ResetsAt == nil {
		t.Fatalf("legacy: %+v %v", w, err)
	}
}

func fakeJWT(sub string, exp time.Time) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"none"}`) + "." + enc(fmt.Sprintf(`{"sub":%q,"exp":%d}`, sub, exp.Unix())) + ".sig"
}

func TestCursorUsageCallsTheDashboardWithTheAccountLogin(t *testing.T) {
	var cookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie = r.Header.Get("Cookie")
		if r.URL.Path == "/api/usage-summary" && r.Header.Get("Origin") == "https://cursor.com" {
			w.Write(cursorFixture(t, "usage-summary-team.json"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	prevBase, prevTok, prevHTTP := cursorBase, cursorToken, cursorHTTP
	defer func() { cursorBase, cursorToken, cursorHTTP = prevBase, prevTok, prevHTTP }()
	cursorBase, cursorHTTP = srv.URL, newUsageClient()
	tok := fakeJWT("google-oauth2|user_123", time.Now().Add(time.Hour))
	cursorToken = func(context.Context, Account) (string, string) { return tok, "" }

	u := cursorUsage(t.Context(), Account{ID: "cursor"})
	if u.Err != nil || len(u.Windows) != 2 || u.Plan != "enterprise" {
		t.Fatalf("usage: %+v", u)
	}
	if !strings.HasPrefix(cookie, "WorkosCursorSessionToken=user_123%3A%3A") || !strings.HasSuffix(cookie, tok) {
		t.Fatalf("cookie %q", cookie)
	}
	// An expired login says how to fix it; nothing is called.
	cookie = ""
	cursorToken = func(context.Context, Account) (string, string) { return fakeJWT("x|u", time.Now().Add(-time.Hour)), "" }
	if u := cursorUsage(t.Context(), Account{ID: "cursor"}); u.Err == nil || !strings.Contains(u.Err.Error(), "cursor-agent login") || cookie != "" {
		t.Fatalf("expired: %+v (called: %v)", u, cookie != "")
	}
	// No token: the note, no call.
	cursorToken = func(context.Context, Account) (string, string) { return "", "keychain note" }
	if u := cursorUsage(t.Context(), Account{ID: "cursor"}); u.Note != "keychain note" || len(u.Windows) != 0 {
		t.Fatalf("no token: %+v", u)
	}
}

// Each account reads its own auth.json (CURSOR_CONFIG_DIR).
func TestCursorTokenPerAccountFile(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	os.WriteFile(a+"/auth.json", []byte(`{"accessToken":"tok-a"}`), 0o600)
	os.WriteFile(b+"/auth.json", []byte(`{"accessToken":"tok-b"}`), 0o600)
	if tok, _ := cursorToken(t.Context(), Account{ID: "cursor", Env: map[string]string{"CURSOR_CONFIG_DIR": a}}); tok != "tok-a" {
		t.Fatalf("a: %q", tok)
	}
	if tok, _ := cursorToken(t.Context(), Account{ID: "cursor2", Env: map[string]string{"CURSOR_CONFIG_DIR": b}}); tok != "tok-b" {
		t.Fatalf("b: %q", tok)
	}
}
