// Package mcprelay carries agent chats' MCP HTTP requests over the hub link
// (KON-DTN92), so a machine needs no network path to the hub's port for its agents to
// reach their per-chat tools.
//
// konsertd runs a Local: a loopback HTTP server (127.0.0.1, ephemeral port) that the
// agent CLIs are pointed at. Each request becomes an mcp.request frame on the current
// hub link; the hub's Remote runs it through the chat's MCP handler and answers with an
// mcp.head, mcp.body chunks (flushed as the handler flushes, so SSE streams) and an
// mcp.end. Requests are multiplexed by an id konsertd picks. The relay changes nothing
// about authorisation: the agent's per-chat bearer token travels with the request and
// the hub checks it exactly as before (and also that the chat runs on the machine whose
// link carried the request).
package mcprelay

import "net/http"

// Link is one hub link as the relay sees it.
type Link struct {
	// Send queues one envelope on the link without blocking for long. It must encode
	// payload before returning. False means the link is gone.
	Send func(msgType string, payload any) bool
	// Done is closed when the link ends.
	Done <-chan struct{}
}

// relayedRequestHeaders are the request headers an MCP Streamable HTTP client needs;
// nothing else crosses the link (no Host, cookies, forwarding or identity headers).
var relayedRequestHeaders = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"Last-Event-Id",
	"Mcp-Protocol-Version",
	"Mcp-Session-Id",
	"User-Agent",
}

// requestHeader keeps only the relayed request headers of h.
func requestHeader(h http.Header) http.Header {
	out := http.Header{}
	for _, k := range relayedRequestHeaders {
		if v := h.Values(k); len(v) > 0 {
			out[k] = append([]string(nil), v...)
		}
	}
	return out
}

// hopByHop are response headers that describe one connection, not the response.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// copyResponseHeader copies src into dst without hop-by-hop headers.
func copyResponseHeader(dst, src http.Header) {
	for k, v := range src {
		k = http.CanonicalHeaderKey(k)
		if hopByHop[k] {
			continue
		}
		dst[k] = append([]string(nil), v...)
	}
}

// validID reports whether id is a usable request or chat id: 1..64 characters of
// [A-Za-z0-9_-] (konsertd's request ids are 32 hex characters, chat ids UUIDs).
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// allowedMethod reports whether m is a method of the MCP Streamable HTTP transport.
func allowedMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodPost || m == http.MethodDelete
}
