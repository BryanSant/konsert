package worker

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/observe"
)

// TestLivePassiveScan prints what the passive scan finds on this machine
// (SYRE_LIVE_SCAN=1; a manual check on each OS).
func TestLivePassiveScan(t *testing.T) {
	if os.Getenv("SYRE_LIVE_SCAN") == "" {
		t.Skip("set SYRE_LIVE_SCAN=1")
	}
	var log emitLog
	disc := NewSessionDiscovery(t.TempDir(), nil, log.emit)
	disc.ScanPassiveProcesses(context.Background())
	time.Sleep(3 * time.Second)
	disc.ScanPassiveProcesses(context.Background())
	for _, p := range log.all() {
		t.Logf("%s agent=%s status=%s cwd=%q branch=%q", p.ExternalID, p.Agent, p.Status, p.Cwd, p.Branch)
	}
}

// TestLiveObservedCodex prints what the observed codex source reports on this machine
// for SYRE_LIVE_SECONDS (a manual check: start a codex session alongside).
func TestLiveObservedCodex(t *testing.T) {
	if os.Getenv("SYRE_LIVE_SCAN") == "" {
		t.Skip("set SYRE_LIVE_SCAN=1")
	}
	o := newObservedSessions(observedConfig{
		CodexDirs:   func() []observe.CodexDir { return codexHomes(nil) },
		GrokDirs:    func() []observe.GrokDir { return grokHomes(nil) },
		CopilotDirs: func() []observe.CopilotDir { return copilotHomes(nil) },
		Procs:       osProcTable(),
		OffsetsPath: filepath.Join(t.TempDir(), "offsets.json"),
	}, func(p protocol.ExternalSessionPayload) {
		t.Logf("%s %s agent=%s status=%s tool=%q cwd=%q summary=%q", time.Now().Format("15:04:05"), p.ExternalID, p.Agent, p.Status, p.Tool, p.Cwd, p.Summary)
	})
	secs, _ := strconv.Atoi(os.Getenv("SYRE_LIVE_SECONDS"))
	end := time.Now().Add(time.Duration(max(secs, 5)) * time.Second)
	for time.Now().Before(end) {
		o.Poll(context.Background())
		time.Sleep(2 * time.Second)
	}
}

// TestLiveReplayAllSessions parses every grok and codex session on this machine and
// reports oddities: a turn left working, tool calls never finished, empty messages.
func TestLiveReplayAllSessions(t *testing.T) {
	if os.Getenv("SYRE_LIVE_SCAN") == "" {
		t.Skip("set SYRE_LIVE_SCAN=1")
	}
	check := func(agent, path string, evs []protocol.AgentChatEvent) (odd string) {
		running := map[string]bool{}
		last := ""
		for _, ev := range evs {
			switch ev.Kind {
			case protocol.EventStatus:
				last = ev.Status
			case protocol.EventToolCall:
				running[ev.ToolCall.ID] = ev.ToolCall.Status == "running"
			case protocol.EventMessage, protocol.EventUserMessage:
				if ev.Text == "" {
					odd += " empty-message"
				}
			}
		}
		n := 0
		for _, r := range running {
			if r {
				n++
			}
		}
		if n > 0 {
			odd += " unfinished-tools=" + strconv.Itoa(n)
		}
		if last == protocol.ChatStatusWorking {
			odd += " ends-working"
		}
		return odd
	}
	home, _ := os.UserHomeDir()
	var total, odd int
	grok, _ := filepath.Glob(filepath.Join(home, ".grok", "sessions", "*", "*", "updates.jsonl"))
	for _, f := range grok {
		raw, _ := os.ReadFile(f)
		evs := observe.GrokEvents("x", raw)
		total++
		if o := check("grok", f, evs); o != "" {
			odd++
			info, _ := os.Stat(f)
			t.Logf("grok %s (%d events, modified %s):%s", filepath.Base(filepath.Dir(f)), len(evs), info.ModTime().Format("01-02 15:04"), o)
		}
	}
	codex, _ := filepath.Glob(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "rollout-*.jsonl"))
	for _, f := range codex {
		raw, _ := os.ReadFile(f)
		evs := observe.CodexEvents("x", raw)
		total++
		if o := check("codex", f, evs); o != "" {
			odd++
			t.Logf("codex %s (%d events):%s", filepath.Base(f), len(evs), o)
		}
	}
	t.Logf("%d sessions, %d odd", total, odd)
}
