package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

func TestPolicyRefusalOnTheMachineView(t *testing.T) {
	noon := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	m := &protocol.WorkerMetrics{MemTotalMB: 16384, MemUsedMB: 14000, LoadAvg: 1}
	f := &protocol.MachineFacts{CPUCores: 8, SurvivalReserveMB: 1024}
	for _, c := range []struct {
		name string
		p    *protocol.MachinePolicy
		rule string
		msg  string
	}{
		{"no policy", nil, "", ""},
		{"takes work", &protocol.MachinePolicy{MaxSessions: 4}, "", ""},
		{"control only", &protocol.MachinePolicy{Role: protocol.MachineRoleControlOnly}, "role", "control-only"},
		{"quiet hours", &protocol.MachinePolicy{QuietHours: []protocol.QuietWindow{{Start: "11:00", End: "13:00"}}, TimeZone: "UTC"}, "quiet_hours", "quiet hours (11:00 to 13:00 UTC)"},
		{"ram floor", &protocol.MachinePolicy{MinFreeRAMMB: 2048}, "min_free_ram", "2384 MB available, below the 3072 MB kept free"},
		{"sessions", &protocol.MachinePolicy{MaxSessions: 2}, "max_sessions", "at most 2 sessions"},
		{"interactive holds automatic work only", &protocol.MachinePolicy{Role: protocol.MachineRoleInteractive}, "", ""},
	} {
		hosted := 0
		if c.name == "sessions" {
			hosted = 2
		}
		got := policyRefusal(c.p, hosted, m, f, noon)
		if c.rule == "" {
			if got != nil {
				t.Errorf("%s: refused %+v", c.name, got)
			}
			continue
		}
		if got == nil || got.Rule != c.rule || !strings.Contains(got.Message, c.msg) || strings.HasPrefix(got.Message, "machine policy:") {
			t.Errorf("%s: got %+v, want rule %s with %q", c.name, got, c.rule, c.msg)
		}
	}
}
