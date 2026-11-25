// Package mcpbridge serves a Streamable HTTP MCP server's tools over stdio. Agents that
// only accept stdio MCP servers (Antigravity over ACP) launch `syred mcp-bridge`, which
// forwards tool calls to the hub's per-chat endpoint (ask_user, fleet tools).
//
// The target URL and headers come from the environment (EnvURL, EnvHeaders) rather than
// argv, so the per-chat bearer token is not visible in process listings.
package mcpbridge

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// EnvURL is the Streamable HTTP MCP endpoint to bridge to.
	EnvURL = "SYRE_MCP_BRIDGE_URL"
	// EnvHeaders is a JSON object of extra HTTP headers (e.g. Authorization).
	EnvHeaders = "SYRE_MCP_BRIDGE_HEADERS"
)

// Config is where to forward.
type Config struct {
	URL     string
	Headers map[string]string
}

// ConfigFromEnv reads EnvURL / EnvHeaders.
func ConfigFromEnv() (Config, error) {
	cfg := Config{URL: os.Getenv(EnvURL)}
	if cfg.URL == "" {
		return cfg, fmt.Errorf("%s is not set", EnvURL)
	}
	if raw := os.Getenv(EnvHeaders); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Headers); err != nil {
			return cfg, fmt.Errorf("%s: %w", EnvHeaders, err)
		}
	}
	return cfg, nil
}

// headerTransport adds fixed headers to every request.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	return t.base.RoundTrip(r)
}

// Run connects to cfg.URL, mirrors its tools, and serves them on transport (stdio in
// production) until ctx ends or the client disconnects.
func Run(ctx context.Context, cfg Config, transport mcp.Transport) error {
	if cfg.URL == "" {
		return errors.New("no MCP URL to bridge to")
	}
	// No overall timeout: ask_user blocks until the user answers (up to the hub's limit).
	hc := &http.Client{Transport: headerTransport{base: http.DefaultTransport, headers: cfg.Headers}}
	client := mcp.NewClient(&mcp.Implementation{Name: "syred-mcp-bridge", Version: "1"}, nil)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	upstream, err := client.Connect(cctx, &mcp.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: hc}, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("connect to %s: %w", cfg.URL, err)
	}
	defer func() { _ = upstream.Close() }()

	lctx, lcancel := context.WithTimeout(ctx, 30*time.Second)
	tools, err := upstream.ListTools(lctx, nil)
	lcancel()
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "syre", Version: "1"}, nil)
	for _, t := range tools.Tools {
		name := t.Name
		server.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args any
			if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return nil, fmt.Errorf("invalid arguments for %s: %w", name, err)
				}
			}
			return upstream.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		})
	}
	return server.Run(ctx, transport)
}
