package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A quota.json holding Cursor's GetCurrentPeriodUsage response (as a hook could save it)
// is reported as the Cursor Models plan percentage.
func TestCursorPlanUsageQuota(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"billingCycleStart":"1793491200000","billingCycleEnd":"1796083200000","planUsage":{"totalSpend":1234,"includedSpend":1000,"limit":2000,"autoPercentUsed":10,"apiPercentUsed":30,"totalPercentUsed":42.5}}`
	if err := os.WriteFile(filepath.Join(home, ".cursor", "quota.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	quotas := CollectSubscriptionQuotas(context.Background(), home, "w1")
	if len(quotas) != 1 || quotas[0].Provider != "cursor" || quotas[0].Label != "Cursor Models" || quotas[0].UsedPercent != 42.5 {
		t.Fatalf("unexpected cursor quota: %+v", quotas)
	}
	if quotas[0].ResetAt == nil || quotas[0].ResetAt.UnixMilli() != 1796083200000 {
		t.Fatalf("unexpected reset: %v", quotas[0].ResetAt)
	}

	// Without totalPercentUsed the percentage falls back to includedSpend / limit.
	body = `{"planUsage":{"includedSpend":500,"limit":2000}}`
	if err := os.WriteFile(filepath.Join(home, ".cursor", "quota.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	quotas = CollectSubscriptionQuotas(context.Background(), home, "w1")
	if len(quotas) != 1 || quotas[0].UsedPercent != 25 {
		t.Fatalf("unexpected spend-based cursor quota: %+v", quotas)
	}
}

func TestCopilotQuotaFileLabel(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".copilot"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".copilot", "quota.json"), []byte(`{"used_percent":30}`), 0o600); err != nil {
		t.Fatal(err)
	}
	quotas := CollectSubscriptionQuotas(context.Background(), home, "w1")
	if len(quotas) != 1 || quotas[0].Provider != "copilot" || quotas[0].Label != "Copilot" || quotas[0].UsedPercent != 30 {
		t.Fatalf("unexpected copilot quota: %+v", quotas)
	}
}
