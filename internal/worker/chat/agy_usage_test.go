package chat

import (
	"os"
	"path/filepath"
	"testing"
)

// The recorded output of agy 1.5.18's /usage (bocamacmini, 2026-11-28).
func TestParseAgyUsage(t *testing.T) {
	raw, err := os.ReadFile("testdata/agy/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	w, err := parseAgyUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, x := range w {
		r := "-"
		if x.ResetsAt != nil {
			r = x.ResetsAt.Format("2006-01-02T15:04")
		}
		got[x.Window] = x.Label + "|" + r
		if x.Window == "seven_day_gemini" && x.UsedPercent != 13.7 {
			t.Errorf("gemini weekly used %.1f, want 13.7", x.UsedPercent)
		}
	}
	want := map[string]string{
		"seven_day_gemini":     "Gemini Models · weekly|2026-12-03T07:01",
		"five_hour_gemini":     "Gemini Models · 5-hour|2026-11-28T10:16",
		"seven_day_claude_gpt": "Claude and GPT models · weekly|-", // untouched: no reset time
		"five_hour_claude_gpt": "Claude and GPT models · 5-hour|-",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("windows %v", got)
	}
}

func TestParseAgyUsageUnknownBucketIsNotExhausted(t *testing.T) {
	raw := `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"Gemini Models","buckets":[
	  {"id":"gemini-weekly","window":"weekly","reset_time":"2026-12-03T07:01:50Z"},
	  {"id":"gemini-5h","window":"5h","remaining_fraction":0,"reset_time":"2026-11-28T10:16:56Z"}]}]}}}`
	w, err := parseAgyUsage([]byte(raw))
	if err != nil || len(w) != 1 || w[0].Window != "five_hour_gemini" || w[0].State != "exhausted" || w[0].UsedPercent != 100 {
		t.Fatalf("%+v %v", w, err)
	}
	if _, err := parseAgyUsage([]byte("Gemini Models\tWeekly\t86%")); err == nil {
		t.Fatal("text output accepted")
	}
}

// Copilot's plan quotas come back through the usage hook (SYR-PFBW5).
func TestCopilotUsageFromPlanCache(t *testing.T) {
	dir := t.TempDir()
	cache := "// copilot cache\n" + `{"copilotUserCache":{"github.com:u":{"retrievedAt":"2026-11-27T20:00:00Z","response":{"quota_reset_date_utc":"2026-12-01T00:00:00Z",
	  "quota_snapshots":{"premium_interactions":{"entitlement":300,"percent_remaining":40,"unlimited":false},
	                     "chat":{"entitlement":0,"percent_remaining":100,"unlimited":true}}}}}}`
	if err := os.WriteFile(filepath.Join(dir, "copilot-user-cache.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	u := copilotUsage(t.Context(), Account{ID: "copilot", Env: map[string]string{"COPILOT_CACHE_HOME": dir}})
	if u.Err != nil || len(u.Windows) == 0 {
		t.Fatalf("usage: %+v", u)
	}
	w := u.Windows[0]
	if w.Window != "monthly_premium" || w.UsedPercent != 60 || w.ResetsAt == nil || w.Label == "" {
		t.Fatalf("premium window: %+v", w)
	}
	if empty := copilotUsage(t.Context(), Account{ID: "copilot", Env: map[string]string{"COPILOT_CACHE_HOME": t.TempDir()}}); len(empty.Windows) != 0 || empty.Note == "" {
		t.Fatalf("no cache: %+v", empty)
	}
}
