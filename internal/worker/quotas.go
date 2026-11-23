package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// CollectSubscriptionQuotas reads lightweight local quota state files (never spawning interactive CLIs
// or reading credentials) to report rolling window & weekly subscription usage percentages.
func CollectSubscriptionQuotas(_ context.Context, homeDir, workerID string) []protocol.SubscriptionQuota {
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	if homeDir == "" {
		return nil
	}

	now := time.Now().UTC()
	var out []protocol.SubscriptionQuota
	seen := make(map[string]bool)

	add := func(q protocol.SubscriptionQuota) {
		if q.Provider == "" {
			return
		}
		key := strings.ToLower(q.Provider) + ":" + q.WindowLabel
		if seen[key] {
			return
		}
		seen[key] = true
		if q.Label == "" {
			q.Label = defaultQuotaLabel(q.Provider)
		}
		if q.UsedPercent < 0 {
			q.UsedPercent = 0
		}
		if q.UsedPercent > 100 {
			q.UsedPercent = 100
		}
		if q.Source == "" {
			q.Source = "probe"
		}
		q.WorkerID = workerID
		q.UpdatedAt = now
		out = append(out, q)
	}

	// 1. Konsert unified local quota snapshot (~/.konsert/quotas.json) — written by hooks or probes.
	if raw, err := os.ReadFile(filepath.Join(homeDir, ".konsert", "quotas.json")); err == nil {
		var envelope struct {
			Quotas []protocol.SubscriptionQuota `json:"quotas"`
		}
		if json.Unmarshal(raw, &envelope) == nil && len(envelope.Quotas) > 0 {
			for _, q := range envelope.Quotas {
				add(q)
			}
		} else {
			var list []protocol.SubscriptionQuota
			if json.Unmarshal(raw, &list) == nil {
				for _, q := range list {
					add(q)
				}
			}
		}
	}

	// 2. Best-effort lightweight vendor cache probes (read-only local JSON caches if present).
	vendorFiles := []struct {
		provider    string
		windowLabel string
		relPath     string
	}{
		{provider: "claude", windowLabel: "5h", relPath: filepath.Join(".claude", "rate-limit-cache.json")},
		{provider: "codex", windowLabel: "5h", relPath: filepath.Join(".codex", "quota.json")},
		{provider: "agy", windowLabel: "8h", relPath: filepath.Join(".gemini", "quota.json")},
		// grok 1.0.92 keeps no local plan-usage cache (`grok usage` without a session ID
		// says to run /usage inside the TUI; per-session token usage.json files carry no
		// plan limit), so this only picks up a quota.json written by a hook or probe.
		{provider: "grok", windowLabel: "", relPath: filepath.Join(".grok", "quota.json")},
		// cursor-agent keeps no local plan-usage cache either: its TUI /usage command calls
		// Cursor's DashboardService (GetCurrentPeriodUsage) with the keychain login, which
		// this probe never reads. A quota.json written by a hook or probe is accepted in the
		// generic shape or as the GetCurrentPeriodUsage JSON response
		// ({"billingCycleEnd":"<ms>","planUsage":{"totalPercentUsed":..}}).
		{provider: "cursor", windowLabel: "", relPath: filepath.Join(".cursor", "quota.json")},
		// copilot's plan quotas come from its own account cache, which the copilot driver
		// reads (and reports as quota telemetry) while a copilot task runs, right after the
		// CLI refreshes it; this entry only picks up a quota.json written by a hook or probe.
		{provider: "copilot", windowLabel: "", relPath: filepath.Join(".copilot", "quota.json")},
	}
	for _, vf := range vendorFiles {
		raw, err := os.ReadFile(filepath.Join(homeDir, vf.relPath))
		if err != nil {
			continue
		}
		var obj struct {
			UsedPercent *float64 `json:"used_percent"`
			Utilization *float64 `json:"utilization"`
			Window      string   `json:"window_label"`
			ResetAt     string   `json:"reset_at"`
			// Cursor GetCurrentPeriodUsage response shape.
			BillingCycleEnd json.Number `json:"billingCycleEnd"`
			PlanUsage       *struct {
				TotalPercentUsed *float64 `json:"totalPercentUsed"`
				IncludedSpend    float64  `json:"includedSpend"`
				Limit            float64  `json:"limit"`
			} `json:"planUsage"`
		}
		if json.Unmarshal(raw, &obj) != nil {
			continue
		}
		pct := -1.0
		if obj.UsedPercent != nil {
			pct = *obj.UsedPercent
		} else if obj.Utilization != nil {
			pct = *obj.Utilization
			if pct <= 1.0 && pct > 0 {
				pct *= 100
			}
		} else if pu := obj.PlanUsage; pu != nil {
			if pu.TotalPercentUsed != nil {
				pct = *pu.TotalPercentUsed
			} else if pu.Limit > 0 {
				pct = pu.IncludedSpend / pu.Limit * 100
			}
		}
		if pct < 0 {
			continue
		}
		win := obj.Window
		if win == "" {
			win = vf.windowLabel
		}
		var resetPtr *time.Time
		if obj.ResetAt != "" {
			if rt, err := time.Parse(time.RFC3339, obj.ResetAt); err == nil {
				resetPtr = &rt
			}
		} else if ms, err := obj.BillingCycleEnd.Int64(); err == nil && ms > 0 {
			rt := time.UnixMilli(ms).UTC()
			resetPtr = &rt
		}
		add(protocol.SubscriptionQuota{
			Provider:    vf.provider,
			Label:       defaultQuotaLabel(vf.provider),
			WindowLabel: win,
			UsedPercent: pct,
			ResetAt:     resetPtr,
			Source:      "probe",
		})
	}

	return out
}

func defaultQuotaLabel(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "agy", "antigravity", "gemini":
		return "Antigravity"
	case "claude", "anthropic":
		return "Claude"
	case "codex", "openai":
		return "Codex"
	case "grok", "xai":
		return "Grok"
	case "cursor":
		return "Cursor Models"
	case "copilot", "github":
		return "Copilot"
	default:
		return provider
	}
}
