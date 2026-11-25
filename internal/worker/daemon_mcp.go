package worker

import (
	"context"
	"strings"

	"github.com/BryanSant/syre/internal/mcprelay"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/coder/websocket"
)

// Per-chat MCP relay (SYR-DTN92): agents on this machine reach their chat's Syre
// tools at http://127.0.0.1:<port>/mcp/chat/<chat id>, served by syred and relayed
// to the hub over the current link, whatever its transport. The listener starts with
// the first chat that needs it and keeps its port for the daemon's lifetime, so engines
// that outlive a link keep a working URL; a request made while the link is down waits
// for the next one (mcprelay.DefaultWaitLink) and then fails with 503.

// linkMCPRelay makes conn's queues the relay's link until connCtx ends.
func (d *Daemon) linkMCPRelay(connCtx context.Context, q *connQueues) {
	d.mcp.SetLink(&mcprelay.Link{
		Send: func(msgType string, payload any) bool {
			if connCtx.Err() != nil {
				return false
			}
			raw, err := protocol.NewEnvelope(msgType, "", payload)
			if err != nil {
				return false
			}
			return q.push(outboundMsg{msgType: websocket.MessageText, data: raw})
		},
		Done: connCtx.Done(),
	})
}

// relayMCPServers points the hub's relayed MCP servers at the loopback relay. A server
// that cannot be relayed (the listener failed) is left out: the agent runs without
// Syre tools rather than with a URL that goes nowhere.
func (d *Daemon) relayMCPServers(chatID string, servers []protocol.AgentMCPServer) []protocol.AgentMCPServer {
	out := make([]protocol.AgentMCPServer, 0, len(servers))
	for _, s := range servers {
		if !s.Relay {
			out = append(out, s)
			continue
		}
		if !strings.HasPrefix(s.URL, protocol.MCPChatPathPrefix) {
			d.logger.Warn("ignoring relayed MCP server with an unexpected path", "chat_id", chatID, "server", s.Name)
			continue
		}
		base, err := d.mcp.Start()
		if err != nil {
			d.logger.Warn("MCP relay unavailable; the chat runs without Syre tools", "chat_id", chatID, "err", err)
			continue
		}
		s.URL = base + s.URL
		s.Relay = false
		out = append(out, s)
	}
	return out
}
