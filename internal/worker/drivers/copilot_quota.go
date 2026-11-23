package drivers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// copilotQuota is one limited plan bucket from copilot's cached account response.
type copilotQuota struct {
	Label       string
	WindowLabel string
	UsedPercent float64
	ResetAt     string // RFC 3339, "" if unknown
}

// copilotCacheDir mirrors copilot's COPILOT_CACHE_HOME default (`copilot help environment`):
// ~/Library/Caches/copilot on macOS, %LOCALAPPDATA%\copilot on Windows, and
// $XDG_CACHE_HOME/copilot or ~/.cache/copilot elsewhere. Task env values win over the
// daemon's own environment.
func copilotCacheDir(env []string) string {
	if v := envValue(env, "COPILOT_CACHE_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		if home == "" {
			return ""
		}
		return filepath.Join(home, "Library", "Caches", "copilot")
	case "windows":
		if v := envValue(env, "LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "copilot")
		}
		if home == "" {
			return ""
		}
		return filepath.Join(home, "AppData", "Local", "copilot")
	default:
		if v := envValue(env, "XDG_CACHE_HOME"); v != "" {
			return filepath.Join(v, "copilot")
		}
		if home == "" {
			return ""
		}
		return filepath.Join(home, ".cache", "copilot")
	}
}

type copilotSnapshot struct {
	Entitlement      float64 `json:"entitlement"`
	PercentRemaining float64 `json:"percent_remaining"`
	Unlimited        bool    `json:"unlimited"`
	HasQuota         *bool   `json:"has_quota"`
	// TokenBasedBilling marks accounts billed in AI credits. Their request buckets are not
	// drawn down by CLI usage (verified: "chat" stayed 100% remaining across runs that used
	// AI credits), and the AI-credit plan budget ("Plan: N% used" in /usage) is not cached
	// locally, so those buckets are not reported.
	TokenBasedBilling bool `json:"token_based_billing"`
}

// readCopilotQuotas parses copilot-user-cache.json, the disposable cache copilot 1.0.114
// keeps of its copilot_internal/user response and refreshes as the CLI starts. Only the plan
// quota fields are decoded; the file holds no credentials. Each limited bucket becomes one
// quota with used = 100 - percent_remaining, resetting at quota_reset_date_utc. The
// "completions" bucket (IDE code completions) is skipped: the CLI never draws on it.
//
// This is the only plan-usage source available without an interactive session: `/usage`
// exists only inside the TUI, and per-session logs carry token and AI-credit counts but no
// plan limit.
func readCopilotQuotas(cacheDir string) []copilotQuota {
	if cacheDir == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(cacheDir, "copilot-user-cache.json"))
	if err != nil {
		return nil
	}
	// The file starts with "//" comment lines.
	var body strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	var cache struct {
		CopilotUserCache map[string]struct {
			RetrievedAt time.Time `json:"retrievedAt"`
			Response    struct {
				QuotaResetDateUTC string                     `json:"quota_reset_date_utc"`
				QuotaSnapshots    map[string]copilotSnapshot `json:"quota_snapshots"`
			} `json:"response"`
		} `json:"copilotUserCache"`
	}
	if json.Unmarshal([]byte(body.String()), &cache) != nil {
		return nil
	}
	// One entry per signed-in account; report the most recently refreshed one.
	var (
		latest    time.Time
		resetDate string
		snaps     map[string]copilotSnapshot
	)
	for _, e := range cache.CopilotUserCache {
		if snaps == nil || e.RetrievedAt.After(latest) {
			latest, resetDate, snaps = e.RetrievedAt, e.Response.QuotaResetDateUTC, e.Response.QuotaSnapshots
		}
	}
	names := make([]string, 0, len(snaps))
	for name := range snaps {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []copilotQuota
	for _, name := range names {
		s := snaps[name]
		if name == "completions" || s.Unlimited || s.TokenBasedBilling || s.Entitlement <= 0 || (s.HasQuota != nil && !*s.HasQuota) {
			continue
		}
		q := copilotQuota{Label: "Copilot", WindowLabel: name, UsedPercent: 100 - s.PercentRemaining, ResetAt: resetDate}
		switch name {
		case "premium_interactions":
			q.Label, q.WindowLabel = "Copilot Premium", "premium"
		case "chat":
			q.Label = "Copilot Chat"
		}
		q.UsedPercent = min(max(q.UsedPercent, 0), 100)
		out = append(out, q)
	}
	return out
}

// copilotQuotaEvents returns quota telemetry for buckets whose usage differs from what
// seen last recorded (seen is updated in place).
func copilotQuotaEvents(cacheDir string, seen map[string]float64) []TelemetryEvent {
	var out []TelemetryEvent
	for _, q := range readCopilotQuotas(cacheDir) {
		if prev, ok := seen[q.WindowLabel]; ok && prev == q.UsedPercent {
			continue
		}
		seen[q.WindowLabel] = q.UsedPercent
		meta := map[string]any{
			"provider":     "copilot",
			"label":        q.Label,
			"window_label": q.WindowLabel,
			"used_percent": q.UsedPercent,
		}
		if q.ResetAt != "" {
			meta["reset_at"] = q.ResetAt
		}
		out = append(out, TelemetryEvent{
			Kind:    "quota",
			Message: q.Label + ": " + formatPercent(q.UsedPercent) + " used",
			Meta:    meta,
		})
	}
	return out
}

// PlanQuota is one plan usage bucket read from a local, credential-free cache.
type PlanQuota struct {
	Label       string
	Window      string // copilot: "premium" or "chat" (both reset monthly)
	UsedPercent float64
	ResetsAt    *time.Time
}

// CopilotPlanUsage returns copilot's cached plan quotas for an account env (COPILOT_CACHE_HOME
// etc. in env win over the daemon's own environment). It never spawns copilot.
func CopilotPlanUsage(env []string) []PlanQuota {
	var out []PlanQuota
	for _, q := range readCopilotQuotas(copilotCacheDir(env)) {
		p := PlanQuota{Label: q.Label, Window: q.WindowLabel, UsedPercent: q.UsedPercent}
		if q.ResetAt != "" {
			if t, err := time.Parse(time.RFC3339, q.ResetAt); err == nil {
				p.ResetsAt = &t
			}
		}
		out = append(out, p)
	}
	return out
}

func formatPercent(p float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(p, 'f', 1, 64), ".0") + "%"
}
