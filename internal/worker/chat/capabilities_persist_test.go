package chat

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// A restarted konsertd must still know an account is out of usage (seen live: claude2 at
// 100% weekly showed as fine after a restart until the user sent a message).
func TestExhaustedUsageSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-cache.json")
	now := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	reset := now.Add(71 * time.Hour)

	c1 := NewCapabilities(CapabilitiesOptions{PersistPath: path, Now: clock})
	c1.NoteRateLimit("claude2", protocol.AgentRateLimit{Window: "seven_day", State: "exhausted", UsedPercent: 100, ResetsAt: &reset})
	expired := now.Add(-time.Minute)
	c1.NoteRateLimit("other", protocol.AgentRateLimit{Window: "five_hour", State: "exhausted", ResetsAt: &expired})

	if runtime.GOOS != "windows" {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("usage cache not written privately: %v %v", st, err)
		}
	}

	c2 := NewCapabilities(CapabilitiesOptions{PersistPath: path, Now: clock})
	if until, ok := c2.Exhausted("claude2"); !ok || !until.Equal(reset) {
		t.Fatalf("restarted capabilities lost claude2 exhaustion: %v %v", until, ok)
	}
	if _, ok := c2.Exhausted("other"); ok {
		t.Fatal("an already-reset window must not be restored")
	}

	later := func() time.Time { return reset.Add(time.Second) }
	c3 := NewCapabilities(CapabilitiesOptions{PersistPath: path, Now: later})
	if _, ok := c3.Exhausted("claude2"); ok {
		t.Fatal("window restored after its reset time")
	}
}

func TestSandboxedWorkerDisablesFullAccessModes(t *testing.T) {
	a := protocol.AgentAccount{
		DefaultApproval: "bypassPermissions",
		Approvals: []protocol.AgentApprovalMode{
			{ID: "plan", Risk: 0}, {ID: "default", Risk: 1}, {ID: "auto", Risk: 3}, {ID: "bypassPermissions", Risk: 4},
		},
	}
	got := markSandboxedModes(a)
	for _, m := range got.Approvals {
		if (m.Risk > protocol.SandboxMaxRisk) != (m.Disabled != "") {
			t.Fatalf("mode %s risk %d disabled=%q", m.ID, m.Risk, m.Disabled)
		}
	}
	if got.DefaultApproval != "auto" {
		t.Fatalf("default should move to the highest allowed mode, got %q", got.DefaultApproval)
	}
	if a.Approvals[3].Disabled != "" {
		t.Fatal("input account modified")
	}
}
