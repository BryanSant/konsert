package activity

import (
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

type step struct {
	ev    protocol.AgentChatEvent
	state string
	tool  string
	label string
}

func runSteps(t *testing.T, c *Chat, steps []step) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, s := range steps {
		now = now.Add(100 * time.Millisecond)
		c.Observe(s.ev, now)
		a := c.Activity(now)
		if a.State != s.state || (s.tool != "" && a.Tool != s.tool) || (s.label != "" && a.Label != s.label) {
			t.Fatalf("step %d (%s): got %s/%s/%q, want %s/%s/%q", i, s.ev.Kind, a.State, a.Tool, a.Label, s.state, s.tool, s.label)
		}
	}
}

func status(st string) protocol.AgentChatEvent {
	return protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: st}
}

func tool(id, name, title, st string) protocol.AgentChatEvent {
	return protocol.AgentChatEvent{Kind: protocol.EventToolCall, ToolCall: &protocol.AgentToolCall{ID: id, Name: name, Title: title, Status: st}}
}

func TestChatTurnWalksTheStates(t *testing.T) {
	runSteps(t, NewChat("c1"), []step{
		{ev: status(protocol.ChatStatusStarting), state: protocol.ActivityQueued},
		{ev: status(protocol.ChatStatusIdle), state: protocol.ActivityDone, label: "Ready"},
		{ev: status(protocol.ChatStatusWorking), state: protocol.ActivityThinking},
		{ev: protocol.AgentChatEvent{Kind: protocol.EventThoughtDelta, Text: "hmm"}, state: protocol.ActivityThinking},
		{ev: tool("t1", "Bash", "Run: go test ./...", "running"), state: protocol.ActivityWorking, tool: "Bash", label: "Run: go test ./..."},
		{ev: tool("t2", "Read", "Read main.go", "running"), state: protocol.ActivityWorking, tool: "Read"},
		{ev: tool("t2", "Read", "Read main.go", "completed"), state: protocol.ActivityWorking, tool: "Bash"},
		{ev: tool("t1", "Bash", "Run: go test ./...", "failed"), state: protocol.ActivityThinking},
		{ev: protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, Text: "All tests pass."}, state: protocol.ActivitySpeaking},
		{ev: protocol.AgentChatEvent{Kind: protocol.EventMessage, Text: "All tests pass."}, state: protocol.ActivityThinking},
		{ev: protocol.AgentChatEvent{Kind: protocol.EventUsage}, state: protocol.ActivityThinking},
		{ev: protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, StopReason: "end_turn"}, state: protocol.ActivityDone},
		{ev: status(protocol.ChatStatusDone), state: protocol.ActivityDone},
	})
}

func TestChatJoinsToolsRunningAtOnce(t *testing.T) {
	runSteps(t, NewChat("c1"), []step{
		{ev: status(protocol.ChatStatusWorking), state: protocol.ActivityThinking},
		{ev: tool("r1", "Read", "Read: a.go", "running"), state: protocol.ActivityWorking, tool: "Read", label: "Read: a.go"},
		{ev: tool("r2", "Read", "Read: b.go", "running"), state: protocol.ActivityWorking, tool: "Read", label: "Read: a.go · Read: b.go"},
		{ev: tool("r3", "Read", "Read: c.go", "running"), state: protocol.ActivityWorking, tool: "Read", label: "Read: a.go · Read: b.go · Read: c.go"},
		{ev: tool("r1", "Read", "Read: a.go", "completed"), state: protocol.ActivityWorking, tool: "Read", label: "Read: b.go · Read: c.go"},
		{ev: tool("r2", "Read", "Read: b.go", "completed"), state: protocol.ActivityWorking, tool: "Read", label: "Read: c.go"},
	})
	// At most maxJoined labels, the latest ones.
	c := NewChat("c2")
	now := time.Now()
	for i := range maxJoined + 2 {
		c.Observe(tool(string(rune('a'+i)), "Read", "Read: "+string(rune('a'+i)), "running"), now)
	}
	if got := strings.Count(c.Activity(now).Label, "Read: "); got != maxJoined {
		t.Fatalf("label %q lists %d calls, want %d", c.Activity(now).Label, got, maxJoined)
	}
}

func TestChatAskingBlockedAndErrors(t *testing.T) {
	el := &protocol.AgentElicitation{ID: "e1", Kind: protocol.ElicitPermission, Title: "Allow Bash: rm -rf build/?"}
	runSteps(t, NewChat("c1"), []step{
		{ev: status(protocol.ChatStatusWorking), state: protocol.ActivityThinking},
		{ev: tool("t1", "Bash", "Run: rm -rf build/", "pending"), state: protocol.ActivityWorking},
		{ev: protocol.AgentChatEvent{Kind: protocol.EventElicitation, Elicitation: el}, state: protocol.ActivityAsking, label: "Allow Bash: rm -rf build/?"},
		{ev: status(protocol.ChatStatusNeedsYou), state: protocol.ActivityAsking},
		{ev: status(protocol.ChatStatusSuspended), state: protocol.ActivityBlocked, label: "Suspended"},
		{ev: status(protocol.ChatStatusNeedsYou), state: protocol.ActivityAsking},
		{ev: protocol.AgentChatEvent{Kind: protocol.EventElicitationOK, Elicitation: el}, state: protocol.ActivityWorking, tool: "Bash"},
		{ev: tool("t1", "Bash", "", "completed"), state: protocol.ActivityThinking},
		// Syre's ask_user MCP tool: the agent waits for the user.
		{ev: tool("t2", "mcp__syre__ask_user", "", "running"), state: protocol.ActivityAsking, tool: "mcp__syre__ask_user"},
		{ev: tool("t2", "mcp__syre__ask_user", "", "completed"), state: protocol.ActivityThinking},
		{ev: protocol.AgentChatEvent{Kind: protocol.EventError, Error: &protocol.AgentChatError{Kind: protocol.ErrKindUsageLimit, Message: "5-hour limit reached"}},
			state: protocol.ActivityError, label: "5-hour limit reached"},
		{ev: status(protocol.ChatStatusWorking), state: protocol.ActivityThinking},
		{ev: protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, StopReason: "cancelled"}, state: protocol.ActivityDone, label: "Cancelled"},
	})
}

func TestChatSpeakingRateAndChanges(t *testing.T) {
	c := NewChat("c1")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.Observe(status(protocol.ChatStatusWorking), t0)
	if !c.Observe(protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, Text: strings.Repeat("x", 40)}, t0.Add(100*time.Millisecond)) {
		t.Fatal("first delta should change the activity")
	}
	// 400 chars more over one second: ~110 tokens/s.
	for i := 1; i <= 10; i++ {
		c.Observe(protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, Text: strings.Repeat("x", 40)}, t0.Add(time.Duration(100+i*100)*time.Millisecond))
	}
	a := c.Activity(t0.Add(1100 * time.Millisecond))
	if a.State != protocol.ActivitySpeaking || a.TokensPerSec < 90 || a.TokensPerSec > 130 {
		t.Fatalf("speaking rate = %v (%s), want ~110 tokens/s", a.TokensPerSec, a.State)
	}
	if a.Since != t0.Add(100*time.Millisecond) {
		t.Errorf("since = %v, want the first delta", a.Since)
	}
	// Events that change nothing report no change.
	if c.Observe(protocol.AgentChatEvent{Kind: protocol.EventPlan}, t0.Add(2*time.Second)) {
		t.Error("a plan event should not change the activity")
	}
	c.Observe(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd}, t0.Add(3*time.Second))
	if a := c.Activity(t0.Add(3 * time.Second)); a.TokensPerSec != 0 {
		t.Errorf("rate after the turn = %v, want 0", a.TokensPerSec)
	}
}

func TestChatLabelIsOneShortLine(t *testing.T) {
	c := NewChat("c1")
	now := time.Now()
	c.Observe(tool("t1", "Bash", "Run:\n  "+strings.Repeat("long ", 60), "running"), now)
	a := c.Activity(now)
	if strings.Contains(a.Label, "\n") || len([]rune(a.Label)) > maxLabel {
		t.Fatalf("label %q is not one short line", a.Label)
	}
}

func TestIsUserTool(t *testing.T) {
	for _, n := range []string{"ask_user", "mcp__syre__ask_user", "syre.request_user_action", "syre/ask_user"} {
		if !IsUserTool(n) {
			t.Errorf("%s should wait for the user", n)
		}
	}
	for _, n := range []string{"Bash", "task_user_asked", "mcp__syre__fleet_status"} {
		if IsUserTool(n) {
			t.Errorf("%s should not wait for the user", n)
		}
	}
}
