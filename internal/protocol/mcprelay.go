package protocol

import "net/http"

// Per-chat MCP relay (SYR-DTN92). Agent chats reach the hub's per-chat MCP tools
// (ask_user, the fleet tools) through syred: syred serves a loopback HTTP endpoint
// and relays each request to the hub as frames over the link it already has (outbound,
// SSH or local), so no machine needs a network path to the hub's port. Frames carry a
// request id chosen by syred; the hub answers with one head, any number of body
// chunks (an SSE stream is relayed as the hub flushes it) and one end.
const (
	// MsgMCPRequest (worker->hub) is one HTTP request for a chat's MCP endpoint.
	MsgMCPRequest = "mcp.request"
	// MsgMCPCancel (worker->hub) abandons a request (the agent went away).
	MsgMCPCancel = "mcp.cancel"
	// MsgMCPHead (hub->worker) carries the response status and headers.
	MsgMCPHead = "mcp.head"
	// MsgMCPBody (hub->worker) carries a chunk of the response body.
	MsgMCPBody = "mcp.body"
	// MsgMCPEnd (hub->worker) ends the response; Error is set when it failed.
	MsgMCPEnd = "mcp.end"

	// MCPChatPathPrefix is the path of a chat's MCP endpoint (MCPChatPathPrefix + chat
	// id) on syred's loopback relay.
	MCPChatPathPrefix = "/mcp/chat/"

	// MCPMaxBody bounds one relayed request body.
	MCPMaxBody = 4 << 20
)

// MCPRequest is one relayed HTTP request. Body is base64 on the wire.
type MCPRequest struct {
	ID     string      `json:"id"`
	ChatID string      `json:"chat_id"`
	Method string      `json:"method"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

// MCPCancel abandons the request with ID.
type MCPCancel struct {
	ID string `json:"id"`
}

// MCPHead is the response status and headers of request ID.
type MCPHead struct {
	ID     string      `json:"id"`
	Status int         `json:"status"`
	Header http.Header `json:"header,omitempty"`
}

// MCPBody is a chunk of request ID's response body.
type MCPBody struct {
	ID   string `json:"id"`
	Data []byte `json:"data"`
}

// MCPEnd ends request ID's response.
type MCPEnd struct {
	ID    string `json:"id"`
	Error string `json:"error,omitempty"`
}
