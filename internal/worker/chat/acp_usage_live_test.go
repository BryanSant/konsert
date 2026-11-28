package chat

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestACPLiveUsage runs each ACP agent's usage hook against the real default login on
// this machine (SYR-PZBT6). It logs window names, percentages and resets only.
func TestACPLiveUsage(t *testing.T) {
	if os.Getenv("SYRE_USAGE_LIVE") != "1" {
		t.Skip("set SYRE_USAGE_LIVE=1 to fetch real plan usage for grok, cursor, agy and copilot")
	}
	for _, agent := range []string{"grok", "cursor", "agy", "copilot"} {
		t.Run(agent, func(t *testing.T) {
			ad := acpAdapters[agent]
			bin, err := exec.LookPath(ad.DefaultBinary)
			if err != nil {
				t.Skipf("%s not installed", ad.DefaultBinary)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			u := ad.usage(ctx, Account{ID: agent, Agent: agent, Label: ad.Label, Binary: bin, IsDefault: true})
			t.Logf("plan=%q note=%q err=%v", u.Plan, u.Note, u.Err)
			for _, w := range u.Windows {
				reset := "-"
				if w.ResetsAt != nil {
					reset = w.ResetsAt.Local().Format(time.DateTime)
				}
				t.Logf("  %-28s %-22q %5.1f%% %-9s resets %s", w.Window, w.Label, w.UsedPercent, w.State, reset)
			}
			if len(u.Windows) == 0 {
				t.Errorf("no usage windows")
			}
		})
	}
}
