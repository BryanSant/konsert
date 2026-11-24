package chat

import (
	"context"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/activity"
)

// TestClaudeSubagentLines: lines with parent_tool_use_id belong to the sub-agent started
// by that Task call. Their stream deltas are reported (marked with the parent) instead of
// dropped, their tool calls and permission prompts carry the parent, and the activity
// deriver gives the sub-agent its own state until the Task's tool_result.
func TestClaudeSubagentLines(t *testing.T) {
	e, l := newTestClaude(t, fakeClaudeOpts{onUser: func(p *ccFakeProc, _ map[string]any, _ int) {
		p.send(`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"task1","name":"Task","input":{"description":"Docs for cache flags","prompt":"…","subagent_type":"general-purpose"}}]},"parent_tool_use_id":null}`)
		p.send(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"s1"}},"parent_tool_use_id":"task1"}`)
		p.send(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"Which flags?"}},"parent_tool_use_id":"task1"}`)
		p.send(`{"type":"assistant","message":{"id":"s1","content":[{"type":"tool_use","id":"sb1","name":"Bash","input":{"command":"go test ./internal/cache/..."}}]},"parent_tool_use_id":"task1"}`)
		p.send(`{"type":"control_request","request_id":"perm1","request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"sb1","input":{"command":"go test ./internal/cache/..."}}}`)
	}})
	rec := newCCRecorder()
	s, err := e.Open(context.Background(), Account{ID: "claude"}, protocol.AgentChatOpen{Cwd: t.TempDir()}, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_ = s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "t", Text: "document the flags"})
	el, seen := rec.wait(t, isKind(protocol.EventElicitation))
	if el.ParentToolCallID != "task1" {
		t.Fatalf("permission prompt parent = %q", el.ParentToolCallID)
	}
	var thought protocol.AgentChatEvent
	var subTool *protocol.AgentChatEvent
	for i, ev := range seen {
		switch {
		case ev.Kind == protocol.EventThoughtDelta:
			thought = ev
		case ev.Kind == protocol.EventToolCall && ev.ToolCall.ID == "sb1":
			subTool = &seen[i]
		case ev.Kind == protocol.EventToolCall && ev.ToolCall.ID == "task1" && ev.ParentToolCallID != "":
			t.Fatalf("the Task call itself is the parent's: %+v", ev)
		}
	}
	if thought.ParentToolCallID != "task1" || thought.Text != "Which flags?" || thought.MessageID != "" {
		t.Fatalf("sub-agent thought = %+v", thought)
	}
	if subTool == nil || subTool.ParentToolCallID != "task1" || subTool.ToolCall.Status != "running" {
		t.Fatalf("sub-agent tool = %+v", subTool)
	}

	// The tool result and the Task result.
	if err := s.Respond(context.Background(), protocol.AgentChatRespond{ElicitationID: "perm1", OptionID: "allow_once"}); err != nil {
		t.Fatal(err)
	}
	p := l.last()
	p.send(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"sb1","content":"ok"}]},"parent_tool_use_id":"task1"}`)
	p.send(`{"type":"assistant","message":{"id":"s2","content":[{"type":"text","text":"Flags documented."}]},"parent_tool_use_id":"task1"}`)
	p.send(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"task1","content":"Flags documented."}]},"parent_tool_use_id":null}`)
	done, seen2 := rec.wait(t, func(ev protocol.AgentChatEvent) bool {
		return ev.Kind == protocol.EventToolCall && ev.ToolCall.ID == "task1" && ev.ToolCall.Status == "completed"
	})
	if done.ParentToolCallID != "" {
		t.Fatalf("Task result parent = %q", done.ParentToolCallID)
	}
	var sawSubResult, sawSubMsg bool
	for _, ev := range seen2 {
		if ev.Kind == protocol.EventToolCall && ev.ToolCall.ID == "sb1" && ev.ToolCall.Status == "completed" && ev.ParentToolCallID == "task1" {
			sawSubResult = true
		}
		if ev.Kind == protocol.EventMessage && ev.ParentToolCallID == "task1" {
			sawSubMsg = true
		}
		if ev.Kind == protocol.EventMessage && ev.ParentToolCallID == "" {
			t.Fatalf("a sub-agent's text became the parent's message: %+v", ev)
		}
	}
	if !sawSubResult || !sawSubMsg {
		t.Fatalf("sub result %v, sub message %v", sawSubResult, sawSubMsg)
	}

	// Folded by the deriver: the sub-agent worked, asked, and is gone with the Task result.
	act := activity.NewChat("c")
	now := time.Now()
	act.Observe(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusWorking}, now)
	var maxSubs int
	var asked bool
	for _, ev := range append(seen, seen2...) {
		act.Observe(ev, now)
		a := act.Activity(now)
		maxSubs = max(maxSubs, len(a.Subagents))
		for _, sa := range a.Subagents {
			if sa.State == protocol.ActivityAsking && sa.Title == "Docs for cache flags" && a.State == protocol.ActivityAsking {
				asked = true
			}
		}
	}
	if final := act.Activity(now); maxSubs != 1 || !asked || len(final.Subagents) != 0 {
		t.Fatalf("max %d sub-agents, asked %v, final %+v", maxSubs, asked, final.Subagents)
	}
}
