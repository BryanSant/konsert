// Package activity derives a session's live activity (thinking, working, speaking, asking,
// blocked, queued, done, error) from what konsertd already sees of it, and publishes it to
// the hub at a bounded rate. See docs/design/activity.md.
package activity

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BryanSant/konsert/internal/protocol"
)

// maxLabel caps Label (runes).
const maxLabel = 120

// rateWindow is the span over which the speaking output rate is measured.
const rateWindow = 2 * time.Second

// charsPerToken estimates tokens from streamed text (no engine reports per-delta tokens).
const charsPerToken = 4.0

// userToolRe matches tools whose call means the agent is waiting for the user: Konsert's
// own MCP tools, however the agent names them (mcp__konsert__ask_user, konsert.ask_user,
// "konsert: ask_user").
var userToolRe = regexp.MustCompile(`(^|[^a-z0-9])(ask_user|request_user_action)($|[^a-z0-9_])`)

// IsUserTool reports whether a tool call (by name or title) waits for the user.
func IsUserTool(name string) bool {
	return name != "" && userToolRe.MatchString(strings.ToLower(name))
}

type runningTool struct {
	id, name, label string
	asks            bool
}

type rateSample struct {
	at    time.Time
	chars int
}

// Chat derives the activity of one agent chat from its normalized event stream
// (protocol.AgentChatEvent), the same for every engine. It is not safe for concurrent use;
// the chat manager calls it under the chat's lock.
type Chat struct {
	chatID string

	state, tool, label string
	since              time.Time

	tools     []runningTool // running tool calls, in start order
	pending   map[string]string
	suspended bool
	samples   []rateSample
	tps       float64
	seen      bool
}

// NewChat returns a deriver for chatID.
func NewChat(chatID string) *Chat {
	return &Chat{chatID: chatID, pending: map[string]string{}}
}

// Observe folds one event in and reports whether the activity changed (state, tool,
// label or output rate).
func (c *Chat) Observe(ev protocol.AgentChatEvent, now time.Time) bool {
	before := c.view()
	beforeTPS := c.tps
	switch ev.Kind {
	case protocol.EventStatus:
		c.onStatus(ev)
	case protocol.EventThoughtDelta:
		c.tools = nil // the model generates again: earlier tool calls are over
		c.set(protocol.ActivityThinking, "", "")
	case protocol.EventMessageDelta:
		c.tools = nil
		c.set(protocol.ActivitySpeaking, "", "")
		c.addChars(utf8.RuneCountInString(ev.Text), now)
	case protocol.EventMessage:
		// The reply (or this part of it) is complete; the model may go on with tools.
		if c.state == protocol.ActivitySpeaking {
			c.set(protocol.ActivityThinking, "", "")
		}
	case protocol.EventToolCall:
		c.onTool(ev.ToolCall)
	case protocol.EventElicitation:
		if el := ev.Elicitation; el != nil {
			c.pending[el.ID] = firstNonEmpty(el.Title, el.Detail, el.Kind)
		}
		c.settle()
	case protocol.EventElicitationOK:
		if el := ev.Elicitation; el != nil {
			delete(c.pending, el.ID)
		}
		c.settle()
	case protocol.EventTurnEnd:
		c.endTurn()
		switch ev.StopReason {
		case "error":
			c.set(protocol.ActivityError, "", "The turn failed")
		case "cancelled":
			c.set(protocol.ActivityDone, "", "Cancelled")
		case "max_tokens":
			c.set(protocol.ActivityDone, "", "Stopped at the output limit")
		case "refusal":
			c.set(protocol.ActivityDone, "", "The model declined")
		default:
			c.set(protocol.ActivityDone, "", "")
		}
	case protocol.EventError:
		label := "Failed"
		if ev.Error != nil && ev.Error.Message != "" {
			label = ev.Error.Message
		}
		if ev.Error != nil && ev.Error.Kind == protocol.ErrKindTimeout {
			// The watchdog: the turn keeps running, nothing arrived for a long time.
			c.set(protocol.ActivityError, "", label)
			break
		}
		c.endTurn()
		c.set(protocol.ActivityError, "", label)
	default:
		return false
	}
	if c.state == "" {
		return false
	}
	if c.state == protocol.ActivitySpeaking {
		c.tps = c.rate(now)
	} else {
		c.tps, c.samples = 0, nil
	}
	if !c.seen || c.view() != before {
		c.seen = true
		c.since = now
		return true
	}
	return c.tps != beforeTPS
}

func (c *Chat) onStatus(ev protocol.AgentChatEvent) {
	switch ev.Status {
	case protocol.ChatStatusStarting:
		c.set(protocol.ActivityQueued, "", firstNonEmpty(ev.Text, "Starting the agent"))
	case protocol.ChatStatusWorking:
		c.suspended = false
		switch c.state {
		case protocol.ActivityThinking, protocol.ActivityWorking, protocol.ActivitySpeaking, protocol.ActivityAsking:
			// Already in the turn: a status note ("compacting", a restart) changes nothing.
			if ev.Text != "" && c.state == protocol.ActivityThinking {
				c.label = oneLine(ev.Text)
			}
		default:
			c.tools = nil
			c.pending = map[string]string{}
			c.set(protocol.ActivityThinking, "", "")
		}
	case protocol.ChatStatusNeedsYou:
		c.suspended = false
		if len(c.pending) == 0 && !c.toolAsks() {
			c.set(protocol.ActivityAsking, "", firstNonEmpty(ev.Text, "Waiting for you"))
		} else {
			c.settle()
		}
	case protocol.ChatStatusSuspended:
		c.suspended = true
		c.set(protocol.ActivityBlocked, "", "Suspended")
	case protocol.ChatStatusDone, protocol.ChatStatusIdle:
		if c.state == protocol.ActivityQueued || c.state == "" {
			// The engine came up with no turn running (or was closed before one).
			c.endTurn()
			c.set(protocol.ActivityDone, "", "Ready")
		} else if !protocol.ActivityTerminal(c.state) {
			c.endTurn()
			c.set(protocol.ActivityDone, "", "")
		}
	case protocol.ChatStatusFailed:
		c.endTurn()
		c.set(protocol.ActivityError, "", firstNonEmpty(ev.Text, "Failed"))
	case protocol.ChatStatusCancelled:
		c.endTurn()
		c.set(protocol.ActivityDone, "", "Cancelled")
	}
}

func (c *Chat) onTool(tc *protocol.AgentToolCall) {
	if tc == nil {
		return
	}
	idx := -1
	for i, t := range c.tools {
		if t.id == tc.ID {
			idx = i
		}
	}
	switch tc.Status {
	case "completed", "failed":
		if idx >= 0 {
			c.tools = append(c.tools[:idx], c.tools[idx+1:]...)
		}
		if len(c.tools) == 0 && len(c.pending) == 0 {
			// The result goes back to the model.
			c.set(protocol.ActivityThinking, "", "")
			return
		}
	default: // pending, running, ""
		rt := runningTool{id: tc.ID, name: tc.Name, label: firstNonEmpty(tc.Title, tc.Name), asks: IsUserTool(tc.Name) || IsUserTool(tc.Title)}
		if idx >= 0 {
			c.tools[idx] = rt
		} else {
			c.tools = append(c.tools, rt)
		}
	}
	c.settle()
}

// settle picks the state implied by open elicitations and running tools.
func (c *Chat) settle() {
	if c.suspended {
		return
	}
	for _, t := range c.pending {
		c.set(protocol.ActivityAsking, "", t)
		return
	}
	for i := len(c.tools) - 1; i >= 0; i-- {
		if c.tools[i].asks {
			c.set(protocol.ActivityAsking, c.tools[i].name, "")
			return
		}
	}
	if n := len(c.tools); n > 0 {
		t := c.tools[n-1]
		c.set(protocol.ActivityWorking, t.name, t.label)
		return
	}
	if c.state == protocol.ActivityAsking || c.state == protocol.ActivityWorking {
		c.set(protocol.ActivityThinking, "", "")
	}
}

func (c *Chat) toolAsks() bool {
	for _, t := range c.tools {
		if t.asks {
			return true
		}
	}
	return false
}

func (c *Chat) endTurn() {
	c.tools = nil
	c.pending = map[string]string{}
	c.suspended = false
}

func (c *Chat) set(state, tool, label string) {
	c.state, c.tool, c.label = state, tool, oneLine(label)
}

func (c *Chat) addChars(n int, now time.Time) {
	if n <= 0 {
		return
	}
	c.samples = append(c.samples, rateSample{at: now, chars: n})
}

// rate is the output rate over the last rateWindow (tokens/s, one decimal).
func (c *Chat) rate(now time.Time) float64 {
	cut := now.Add(-rateWindow)
	i := 0
	for i < len(c.samples) && c.samples[i].at.Before(cut) {
		i++
	}
	c.samples = c.samples[i:]
	if len(c.samples) == 0 {
		return 0
	}
	chars := 0
	for _, s := range c.samples {
		chars += s.chars
	}
	span := now.Sub(c.samples[0].at)
	if span < 250*time.Millisecond {
		// Too little to measure: one chunk over a nominal quarter second.
		span = 250 * time.Millisecond
	}
	tps := float64(chars) / charsPerToken / span.Seconds()
	return float64(int(tps*10+0.5)) / 10
}

type view struct{ state, tool, label string }

func (c *Chat) view() view { return view{c.state, c.tool, c.label} }

// Activity returns the current activity ("" State before the first meaningful event).
func (c *Chat) Activity(now time.Time) protocol.SessionActivity {
	return protocol.SessionActivity{
		ChatID:       c.chatID,
		State:        c.state,
		Tool:         c.tool,
		Label:        c.label,
		TokensPerSec: c.tps,
		Source:       protocol.ActivitySourceStream,
		Since:        c.since.UTC(),
		At:           now.UTC(),
	}
}

// oneLine collapses whitespace and caps s at maxLabel runes.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= maxLabel {
		return s
	}
	r := []rune(s)
	return string(r[:maxLabel-1]) + "…"
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
