package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// account/rateLimits/read as a codex Plus account answered it (2026-11-27), plus a
// per-model limit and credits.
const codexRateLimitsByID = `{"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":1,"windowDurationMins":300,"resetsAt":1795853738},"secondary":{"usedPercent":34,"windowDurationMins":10080,"resetsAt":1795978282},"credits":{"hasCredits":true,"unlimited":false,"balance":"12.50"},"planType":"plus","rateLimitReachedType":null},
"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":1,"windowDurationMins":300,"resetsAt":1795853738}},
"gpt-5.1-codex-max":{"limitId":"gpt-5.1-codex-max","limitName":"GPT-5.1 Codex Max","primary":{"usedPercent":60,"windowDurationMins":300,"resetsAt":1795853738},"secondary":{"usedPercent":10,"windowDurationMins":10080}}}}`

func TestCodexUsageWithExtraLimitsAndCredits(t *testing.T) {
	var rr codexRateRead
	if err := unmarshal([]byte(codexRateLimitsByID), &rr); err != nil {
		t.Fatal(err)
	}
	u := rr.usage()
	got := map[string]float64{}
	for _, w := range u {
		got[w.Window] = w.UsedPercent
	}
	want := map[string]float64{"five_hour": 1, "seven_day": 34, "five_hour_gpt_5.1_codex_max": 60, "seven_day_gpt_5.1_codex_max": 10}
	if len(got) != len(want) {
		t.Fatalf("windows %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("window %s = %v (all: %v)", k, got[k], got)
		}
	}
	if c := rr.credits(); c == nil || c.Balance != "12.50" {
		t.Fatalf("credits %+v", c)
	}
	// A plan without credits shows none.
	_ = unmarshal([]byte(codexRateLimitsOK), &rr)
	if c := rr.credits(); c != nil {
		t.Fatalf("no credits expected: %+v", c)
	}
}

// A `codex login status` that fails (timed out on a busy machine) no longer hides usage
// when the app-server answers for a signed-in account.
func TestCodexProbeUsageDespiteLoginStatusFailure(t *testing.T) {
	e, _, _ := newTestCodex(t, true)
	inner := e.run
	e.run = func(ctx context.Context, spec ccSpec) ([]byte, int, error) {
		if len(spec.Args) > 0 && spec.Args[len(spec.Args)-1] == "status" {
			return nil, 0, errors.New("signal: killed")
		}
		return inner(ctx, spec)
	}
	a := e.Probe(context.Background(), Account{ID: "codex", Agent: "codex", IsDefault: true})
	if a.Auth.State != protocol.AuthOK || len(a.Usage) != 2 || a.UsageNote != "" {
		t.Fatalf("probe: auth=%+v usage=%v note=%q", a.Auth, a.Usage, a.UsageNote)
	}
}

func TestKeepLastUsageWhenAProbeComesBackEmpty(t *testing.T) {
	now := time.Now()
	good := protocol.AgentAccount{ID: "codex", Plan: "plus", Auth: protocol.AgentAuthStatus{State: protocol.AuthOK},
		Usage: []protocol.AgentRateLimit{{Window: "five_hour", UsedPercent: 12}}}
	prev := capEntry{acct: good, at: now.Add(-20 * time.Minute)}
	degraded := protocol.AgentAccount{ID: "codex", Auth: protocol.AgentAuthStatus{State: protocol.AuthUnknown, Detail: "capability check timed out"}}
	got := keepLastUsage(degraded, prev, now)
	if len(got.Usage) != 1 || got.Plan != "plus" || got.UsageNote == "" {
		t.Fatalf("kept: %+v", got)
	}
	// Signed out: the old reading is not shown.
	out := degraded
	out.Auth.State = protocol.AuthLoggedOut
	if got := keepLastUsage(out, prev, now); len(got.Usage) != 0 {
		t.Fatalf("signed out kept usage: %+v", got)
	}
	// Too old: dropped.
	if got := keepLastUsage(degraded, capEntry{acct: good, at: now.Add(-3 * time.Hour)}, now); len(got.Usage) != 0 {
		t.Fatalf("stale kept: %+v", got)
	}
	// A fresh reading always wins.
	fresh := good
	fresh.Usage = []protocol.AgentRateLimit{{Window: "five_hour", UsedPercent: 40}}
	if got := keepLastUsage(fresh, prev, now); got.Usage[0].UsedPercent != 40 || got.UsageNote != "" {
		t.Fatalf("fresh: %+v", got)
	}
}
