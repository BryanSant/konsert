package chat

import (
	"encoding/json/jsontext"
	"fmt"
	"strings"

	"github.com/BryanSant/syre/internal/protocol"
)

// ACPReplay turns a recorded stream of ACP session updates (an agent's own session log,
// such as grok's updates.jsonl) into normalized events, as a live ACP chat emits them but
// message-level: an agent message's chunks coalesce into one message, closed by the next
// tool call, prompt or the end of the turn; a tool call's updates merge into one call,
// emitted on each change. Tool input and output are redacted.
type ACPReplay struct {
	turn   acpTurn
	n      int // messages so far, for message ids
	prompt strings.Builder
}

// NewACPReplay starts a replay; prefix keeps its message ids apart from other sessions'.
func NewACPReplay(prefix string) *ACPReplay {
	r := &ACPReplay{}
	r.turn.id = prefix
	return r
}

// Update applies one session/update's update object and returns its events and its
// sessionUpdate kind, so the caller handles the agent's own extensions.
func (r *ACPReplay) Update(raw jsontext.Value) ([]protocol.AgentChatEvent, string) {
	var u acpUpdate
	if unmarshal(raw, &u) != nil {
		return nil, ""
	}
	var evs []protocol.AgentChatEvent
	switch u.SessionUpdate {
	case "user_message_chunk":
		evs = append(evs, r.flushMessage()...)
		r.prompt.WriteString(blockText(u.Content))
		return evs, u.SessionUpdate
	case "agent_message_chunk":
		evs = append(evs, r.flushPrompt()...)
		text := blockText(u.Content)
		if text == "" {
			break
		}
		if r.turn.msgID == "" {
			r.n++
			r.turn.msgID = fmt.Sprintf("%s-m%d", r.turn.id, r.n)
		}
		r.turn.text.WriteString(text)
	case "tool_call", "tool_call_update":
		evs = append(evs, r.flushPrompt()...)
		evs = append(evs, r.flushMessage()...)
		tc := r.turn.toolCall(&u)
		cp := *tc
		cp.Input, cp.Output = Redact(cp.Input), Redact(cp.Output)
		if cp.Status == "pending" {
			cp.Status = "running" // recorded, so started
		}
		evs = append(evs, protocol.AgentChatEvent{Kind: protocol.EventToolCall, ToolCall: &cp})
		if cp.Status == "completed" || cp.Status == "failed" {
			delete(r.turn.tools, u.ToolCallID)
		}
	case "plan":
		evs = append(evs, r.flushPrompt()...)
		plan := make([]protocol.AgentPlanItem, 0, len(u.Entries))
		for _, e := range u.Entries {
			plan = append(plan, protocol.AgentPlanItem{Text: e.Content, Status: e.Status})
		}
		evs = append(evs, protocol.AgentChatEvent{Kind: protocol.EventPlan, Plan: plan})
	}
	return evs, u.SessionUpdate
}

// Flush closes what is pending at the end of a turn: the prompt, the message, and tool
// calls that never finished (reported failed when the turn was cancelled or failed,
// else completed).
func (r *ACPReplay) Flush(stopReason string) []protocol.AgentChatEvent {
	evs := append(r.flushPrompt(), r.flushMessage()...)
	for id, tc := range r.turn.tools {
		delete(r.turn.tools, id)
		cp := *tc
		cp.Status = "completed"
		if stopReason != "" && stopReason != "end_turn" {
			cp.Status = "failed"
		}
		cp.Input, cp.Output = Redact(cp.Input), Redact(cp.Output)
		evs = append(evs, protocol.AgentChatEvent{Kind: protocol.EventToolCall, ToolCall: &cp})
	}
	return evs
}

func (r *ACPReplay) flushPrompt() []protocol.AgentChatEvent {
	text := strings.TrimSpace(r.prompt.String())
	r.prompt.Reset()
	if text == "" {
		return nil
	}
	r.n++
	return []protocol.AgentChatEvent{{Kind: protocol.EventUserMessage, MessageID: fmt.Sprintf("%s-u%d", r.turn.id, r.n), Text: text}}
}

func (r *ACPReplay) flushMessage() []protocol.AgentChatEvent {
	if ev, ok := r.turn.flushMessage(); ok {
		ev.TurnID = ""
		return []protocol.AgentChatEvent{ev}
	}
	return nil
}
