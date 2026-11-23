package main

import (
	"github.com/BryanSant/konsert/internal/mcpbridge"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// newMCPBridgeCmd serves an HTTP MCP endpoint's tools over stdio, for agents that only
// accept stdio MCP servers. Configured by KONSERT_MCP_BRIDGE_URL / _HEADERS (not argv,
// so the bearer token stays out of process listings).
func newMCPBridgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "mcp-bridge",
		Short:  "Serve an HTTP MCP endpoint's tools over stdio (used by agents without HTTP MCP)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := mcpbridge.ConfigFromEnv()
			if err != nil {
				return err
			}
			return mcpbridge.Run(cmd.Context(), cfg, &mcp.StdioTransport{})
		},
	}
}
