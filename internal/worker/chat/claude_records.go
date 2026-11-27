package chat

import (
	"encoding/json/jsontext"

	"github.com/BryanSant/syre/internal/protocol"
)

// The helpers below turn claude's message content into the normalized events, for the
// observed-session source that reads claude's own transcripts (internal/worker/observe):
// those records carry the same content blocks as the stream-json the engine reads.

// ClaudeInputMax and ClaudeOutputMax bound a tool card's input and output text.
const (
	ClaudeInputMax  = claudeInputMax
	ClaudeOutputMax = claudeOutputMax
)

// ClaudeToolCall is the tool card for a tool_use block (status left empty).
func ClaudeToolCall(id, name string, input jsontext.Value) *protocol.AgentToolCall {
	return claudeToolCall(id, name, input)
}

// ClaudePlan is the plan a TodoWrite input sets (nil when it carries none).
func ClaudePlan(input jsontext.Value) []protocol.AgentPlanItem { return claudePlan(input) }

// ClaudeToolResultText flattens a tool_result content (a string or content blocks).
func ClaudeToolResultText(raw jsontext.Value) string { return claudeToolResultText(raw) }

// Redact removes token-looking strings from s.
func Redact(s string) string { return ccRedact(s, nil) }

// Truncate shortens s to at most max bytes on a rune boundary, marking the cut.
func Truncate(s string, max int) string { return ccTruncate(s, max) }
