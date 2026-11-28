package chat

import (
	"os"
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
