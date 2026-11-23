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
		{provider: "cursor", windowLabel: "", relPath: filepath.Join(".cursor", "quota.json")},
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
	default:
		return provider
	}
}
