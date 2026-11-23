package worker

import (
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

func TestQuotasFromCapabilities(t *testing.T) {
	reset := time.Date(2026, 11, 23, 18, 0, 0, 0, time.UTC)
	rep := protocol.AgentCapabilitiesReport{Accounts: []protocol.AgentAccount{
		{ID: "claude", Label: "Claude", Usage: []protocol.AgentRateLimit{{Window: "five_hour", UsedPercent: 42, ResetsAt: &reset}}},
		{ID: "claude2", Label: "Claude 2", Usage: []protocol.AgentRateLimit{{Window: "seven_day", UsedPercent: 97, State: "exhausted", ResetsAt: &reset}}},
		{ID: "copilot", Label: "Copilot", Usage: []protocol.AgentRateLimit{{Window: "monthly_premium", UsedPercent: 130}}},
		{ID: "grok", Label: "Grok"},
		{ID: "mock", Agent: "mock", Label: "Mock", Usage: []protocol.AgentRateLimit{{Window: "five_hour", UsedPercent: 12}}},
	}}
	qs := quotasFromCapabilities(rep, "w1")
	if len(qs) != 3 {
		t.Fatalf("quotas = %+v", qs)
	}
	want := []struct {
		provider, window string
		pct              float64
	}{{"claude", "5h", 42}, {"claude2", "7d", 100}, {"copilot", "premium", 100}}
	for i, w := range want {
		q := qs[i]
		if q.Provider != w.provider || q.WindowLabel != w.window || q.UsedPercent != w.pct || q.WorkerID != "w1" {
			t.Errorf("quota %d = %+v, want %+v", i, q, w)
		}
	}
	if qs[0].ResetAt == nil || !qs[0].ResetAt.Equal(reset) || qs[1].Label != "Claude 2" {
		t.Errorf("unexpected quota details: %+v", qs)
	}
}
