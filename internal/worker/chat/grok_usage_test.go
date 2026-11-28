package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseGrokBillingForms(t *testing.T) {
	// Recorded (SuperGrok, 2026-11-28): the weekly credits are spent, wrapped in "config".
	raw, _ := os.ReadFile("testdata/grok/billing-credits.json")
	u, ok := parseGrokBilling(raw)
	if !ok || len(u.Windows) != 1 {
		t.Fatalf("credits: %+v %v", u, ok)
	}
	w := u.Windows[0]
	if w.Window != "seven_day" || w.UsedPercent != 100 || w.State != "exhausted" || w.ResetsAt == nil || w.ResetsAt.Format(time.RFC3339) != "2026-11-30T11:07:16Z" {
		t.Fatalf("credits window: %+v", w)
	}
	// The monthly form, values as strings or numbers.
	raw, _ = os.ReadFile("testdata/grok/billing-monthly.json")
	u, ok = parseGrokBilling(raw)
	if !ok || len(u.Windows) != 1 || u.Windows[0].Window != "monthly" || u.Windows[0].UsedPercent != 25 || u.Windows[0].State != "ok" {
		t.Fatalf("monthly: %+v %v", u, ok)
	}
	// No limit: nothing to show.
	if _, ok := parseGrokBilling([]byte(`{"config":{"monthlyLimit":{"val":0},"used":{"val":0}}}`)); ok {
		t.Fatal("a zero limit read as usage")
	}
}

func TestGrokLoginExpiredOrMissing(t *testing.T) {
	home := t.TempDir()
	acct := Account{ID: "grok", Env: map[string]string{"GROK_HOME": home}}
	if _, note := grokLogin(acct, time.Now()); !strings.Contains(note, "Not signed in") {
		t.Fatalf("missing: %q", note)
	}
	soon := time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)
	os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"https://auth.x.ai::abc":{"key":"k1","user_id":"u","expires_at":"`+soon+`"}}`), 0o600)
	if a, note := grokLogin(acct, time.Now()); a.Key != "" || !strings.Contains(note, "run grok once") {
		t.Fatalf("expiring: %+v %q", a, note)
	}
	later := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"https://auth.x.ai":{"key":"k2","user_id":"u2","expires_at":"`+later+`"}}`), 0o600)
	if a, _ := grokLogin(acct, time.Now()); a.Key != "k2" || a.UserID != "u2" {
		t.Fatalf("valid: %+v", a)
	}
	if u := grokUsage(t.Context(), Account{ID: "g", Env: map[string]string{"GROK_HOME": t.TempDir()}}); u.Note == "" || len(u.Windows) != 0 {
		t.Fatalf("no login: %+v", u)
	}
}
