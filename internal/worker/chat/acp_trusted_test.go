package chat

import (
	"encoding/json"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
)

// Seen live: cursor asked the user for permission to call konsert's ask_user tool.
func TestACPTrustedKonsertToolsAutoApproved(t *testing.T) {
	s := &acpSession{opts: protocol.AgentChatOpen{MCPServers: []protocol.AgentMCPServer{{Name: "konsert", URL: "http://hub/mcp/chat/x"}}}}
	opts := []protocol.AgentElicitationOption{{ID: "allow-once", Kind: "allow_once"}, {ID: "reject", Kind: "reject_once"}}

	cursorRaw := json.RawMessage(`{"rawInput":{"providerIdentifier":"konsert","toolName":"ask_user"}}`)
	if got := s.trustedToolAllowOption(&protocol.AgentToolCall{Title: "ask_user"}, cursorRaw, opts); got != "allow-once" {
		t.Fatalf("cursor-style konsert tool: got %q", got)
	}
	// Real cursor shape: the permission request carries only id/title; the identifying
	// rawInput arrived earlier and is merged into the tool call's Input.
	merged := &protocol.AgentToolCall{Name: "other", Title: "konsert-ask_user: ask_user", Input: `{"args":{"question":"Choose"},"providerIdentifier":"konsert","toolName":"ask_user"}`}
	if got := s.trustedToolAllowOption(merged, json.RawMessage(`{"toolCallId":"call-1"}`), opts); got != "allow-once" {
		t.Fatalf("real cursor permission shape: got %q", got)
	}
	// Real agy shape (via the stdio mcp-bridge).
	if got := s.trustedToolAllowOption(&protocol.AgentToolCall{Name: "other", Title: "Run mcp_konsert_ask_user"}, nil, opts); got != "allow-once" {
		t.Fatalf("agy-style konsert tool: got %q", got)
	}
	if got := s.trustedToolAllowOption(&protocol.AgentToolCall{Name: "mcp__konsert__fleet_status"}, nil, opts); got != "allow-once" {
		t.Fatalf("mcp__konsert__ tool: got %q", got)
	}
	if got := s.trustedToolAllowOption(&protocol.AgentToolCall{Title: "Write hello.txt"}, json.RawMessage(`{"rawInput":{"providerIdentifier":"other"}}`), opts); got != "" {
		t.Fatalf("unrelated tool must still ask the user, got %q", got)
	}
	none := &acpSession{}
	if got := none.trustedToolAllowOption(&protocol.AgentToolCall{Name: "mcp__konsert__ask_user"}, nil, opts); got != "" {
		t.Fatal("without Konsert MCP servers nothing is trusted")
	}
}
