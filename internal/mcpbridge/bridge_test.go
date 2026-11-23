package mcpbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type askArgs struct {
	Question string `json:"question"`
}

// The bridge mirrors an HTTP MCP server's tools over another transport, forwarding calls
// with the configured headers (the per-chat bearer token).
func TestBridgeForwardsToolCallsWithHeaders(t *testing.T) {
	var gotAuth string
	up := mcp.NewServer(&mcp.Implementation{Name: "hub", Version: "1"}, nil)
	mcp.AddTool(up, &mcp.Tool{Name: "ask_user", Description: "ask"}, func(ctx context.Context, req *mcp.CallToolRequest, in askArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "answer to: " + in.Question}}}, nil, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { gotAuth = r.Header.Get("Authorization"); return up }, nil)
	ts := httptest.NewServer(h)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	serverT, clientT := mcp.NewInMemoryTransports()
	errc := make(chan error, 1)
	go func() {
		errc <- Run(ctx, Config{URL: ts.URL, Headers: map[string]string{"Authorization": "Bearer chat-token"}}, serverT)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil)
	sess, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	tools, err := sess.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "ask_user" {
		t.Fatalf("tools = %+v, %v", tools, err)
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "ask_user", Arguments: map[string]any{"question": "red or blue?"}})
	if err != nil {
		t.Fatal(err)
	}
	if txt, ok := res.Content[0].(*mcp.TextContent); !ok || txt.Text != "answer to: red or blue?" {
		t.Fatalf("result = %+v", res.Content)
	}
	if gotAuth != "Bearer chat-token" {
		t.Fatalf("upstream Authorization = %q", gotAuth)
	}
}
