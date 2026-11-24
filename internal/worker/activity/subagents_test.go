package activity

import (
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

func sub(parent string, ev protocol.AgentChatEvent) protocol.AgentChatEvent {
	ev.ParentToolCallID = parent
	return ev
}

func subStates(a protocol.SessionActivity) []string {
	var out []string
	for _, s := range a.Subagents {
		out = append(out, s.Title+"="+s.State+":"+s.Label)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestChatSubagentsHaveTheirOwnState(t *testing.T) {
	c := NewChat("c1")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tick := func() time.Time { now = now.Add(100 * time.Millisecond); return now }
	type want struct {
		state   string
		waiting bool
		subs    []string
		changed bool
	}
	check := func(i int, ev protocol.AgentChatEvent, w want) {
		t.Helper()
		at := tick()
		changed := c.Observe(ev, at)
		a := c.Activity(at)
		if a.State != w.state || a.Waiting != w.waiting || !eq(subStates(a), w.subs) || changed != w.changed {
			t.Fatalf("step %d: got %s waiting=%v subs=%v changed=%v; want %s waiting=%v subs=%v changed=%v",
				i, a.State, a.Waiting, subStates(a), changed, w.state, w.waiting, w.subs, w.changed)
		}
	}
	check(0, status(protocol.ChatStatusWorking), want{state: protocol.ActivityThinking, changed: true})
	// Two Task calls: the parent waits on them (grey ring), each sub-agent starts thinking.
	check(1, tool("A", "Task", "Agent: S3 client tests", "running"), want{state: protocol.ActivityWorking, waiting: true, subs: []string{"S3 client tests=thinking:"}, changed: true})
	check(2, tool("B", "Agent", "Agent: Docs for cache flags", "running"), want{state: protocol.ActivityWorking, waiting: true,
		subs: []string{"S3 client tests=thinking:", "Docs for cache flags=thinking:"}, changed: true})
	// A runs a tool: only A changes; the parent stays waiting.
	check(3, sub("A", tool("a1", "Bash", "Run: go test ./internal/cache/...", "running")), want{state: protocol.ActivityWorking, waiting: true,
		subs: []string{"S3 client tests=working:Run: go test ./internal/cache/...", "Docs for cache flags=thinking:"}, changed: true})
	// B streams its reply; the same state again is no change.
	check(4, sub("B", protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, Text: "The flags"}), want{state: protocol.ActivityWorking, waiting: true,
		subs: []string{"S3 client tests=working:Run: go test ./internal/cache/...", "Docs for cache flags=speaking:"}, changed: true})
	check(5, sub("B", protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, Text: " are"}), want{state: protocol.ActivityWorking, waiting: true,
		subs: []string{"S3 client tests=working:Run: go test ./internal/cache/...", "Docs for cache flags=speaking:"}, changed: false})
	// B asks for permission: the whole session asks, naming the sub-agent.
	el := &protocol.AgentElicitation{ID: "p1", Kind: protocol.ElicitPermission, Title: "Allow Write: docs/flags.md?"}
	check(6, sub("B", protocol.AgentChatEvent{Kind: protocol.EventElicitation, Elicitation: el}), want{state: protocol.ActivityAsking,
		subs: []string{"S3 client tests=working:Run: go test ./internal/cache/...", "Docs for cache flags=asking:Allow Write: docs/flags.md?"}, changed: true})
	if l := c.Activity(now).Label; l != "Sub-agent “Docs for cache flags” asks: Allow Write: docs/flags.md?" {
		t.Fatalf("asking label = %q", l)
	}
	check(7, protocol.AgentChatEvent{Kind: protocol.EventElicitationOK, Elicitation: &protocol.AgentElicitation{ID: "p1"}}, want{state: protocol.ActivityWorking, waiting: true,
		subs: []string{"S3 client tests=working:Run: go test ./internal/cache/...", "Docs for cache flags=thinking:"}, changed: true})
	// A's tool ends; then A's Task returns: A is gone.
	check(8, sub("A", tool("a1", "Bash", "", "completed")), want{state: protocol.ActivityWorking, waiting: true,
		subs: []string{"S3 client tests=thinking:", "Docs for cache flags=thinking:"}, changed: true})
	check(9, tool("A", "Task", "", "completed"), want{state: protocol.ActivityWorking, waiting: true, subs: []string{"Docs for cache flags=thinking:"}, changed: true})
	// The parent runs its own tool next to B: no longer only waiting.
	check(10, tool("r1", "Read", "Read: a.go", "running"), want{state: protocol.ActivityWorking, subs: []string{"Docs for cache flags=thinking:"}, changed: true})
	// The turn ends: no sub-agents left.
	check(11, protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, StopReason: "end_turn"}, want{state: protocol.ActivityDone, changed: true})
}

func TestChatSubagentWithoutKnownParentTool(t *testing.T) {
	// A sub-agent line can come before (or without) the parent's tool call.
	c := NewChat("c1")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.Observe(status(protocol.ChatStatusWorking), now)
	if !c.Observe(sub("X", protocol.AgentChatEvent{Kind: protocol.EventThoughtDelta, Text: "hm"}), now) {
		t.Fatal("a new sub-agent is a change")
	}
	a := c.Activity(now)
	if len(a.Subagents) != 1 || a.Subagents[0].ID != "X" || a.Subagents[0].State != protocol.ActivityThinking || a.State != protocol.ActivityThinking {
		t.Fatalf("got %+v", a)
	}
}

func TestChatCapsSubagents(t *testing.T) {
	c := NewChat("c1")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.Observe(status(protocol.ChatStatusWorking), now)
	for i := range 40 {
		c.Observe(tool(string(rune('a'+i)), "Task", "Agent: x", "running"), now)
	}
	if n := len(c.Activity(now).Subagents); n != protocol.MaxSubagents {
		t.Fatalf("%d sub-agents listed", n)
	}
}
