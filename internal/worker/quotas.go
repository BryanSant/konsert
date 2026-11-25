package worker

import (
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// quotasFromCapabilities derives the legacy worker.quotas payload from the agent
// capabilities report. Usage comes only from real sources: engine probes (claude OAuth
// usage / rate_limit_event, codex account/rateLimits, ...), copilot's own plan cache, and
// window state seen in live chats. There are no guessed vendor cache files.
//
// Provider is the account ID ("claude", "claude2"), so two subscriptions of one CLI are
// stored separately by the hub (its quota key is provider + window).
func quotasFromCapabilities(rep protocol.AgentCapabilitiesReport, workerID string) []protocol.SubscriptionQuota {
	now := time.Now().UTC()
	var out []protocol.SubscriptionQuota
	for _, a := range rep.Accounts {
		if a.Agent == "mock" {
			continue // scripted usage: shown in agent.capabilities only, never stored as a quota
		}
		for _, u := range a.Usage {
			if u.Window == "" {
				continue
			}
			pct := min(max(u.UsedPercent, 0), 100)
			if u.State == "exhausted" && pct < 100 {
				pct = 100
			}
			out = append(out, protocol.SubscriptionQuota{
				Provider:    a.ID,
				Label:       a.Label,
				WindowLabel: windowLabel(u.Window),
				UsedPercent: pct,
				ResetAt:     u.ResetsAt,
				Source:      "probe",
				WorkerID:    workerID,
				UpdatedAt:   now,
			})
		}
	}
	return out
}

// windowLabel shortens capability window names to the labels the usage UI already shows.
func windowLabel(w string) string {
	switch w {
	case "five_hour":
		return "5h"
	case "seven_day":
		return "7d"
	case "daily":
		return "1d"
	case "monthly":
		return "30d"
	}
	if rest, ok := strings.CutPrefix(w, "seven_day_"); ok {
		return "7d " + rest
	}
	if rest, ok := strings.CutPrefix(w, "monthly_"); ok {
		return rest
	}
	return w
}
