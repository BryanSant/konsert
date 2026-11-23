package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/orchestrator"
	"github.com/BryanSant/konsert/internal/store/sqlite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func connectMCPHTTP(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	client := mcp.NewClient(&mcp.Implementation{Name: "settings-test", Version: "1.0"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             url + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{token: token, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func callUpdateSettings(t *testing.T, sess *mcp.ClientSession, updates map[string]string) (*mcp.CallToolResult, error) {
	t.Helper()
	return sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "update_settings",
		Arguments: map[string]any{"updates": updates},
	})
}

func getSettings(t *testing.T, baseURL, adminToken string) (settingsResponse, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/settings", nil)
	if adminToken != "" {
		req.Header.Set("Authorization", "Bearer "+adminToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/settings: %d %s", resp.StatusCode, raw)
	}
	var out settingsResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out, string(raw)
}

func TestMCPSettingsShareRESTPathAndScope(t *testing.T) {
	_, ts := newSecurityTestServer(t, func(c *config.ControllerConfig) {
		c.NoAuth = false
		c.AdminToken = "admin-tok"
		c.MCPToken = "mcp-tok"
		c.FleetToken = "fleet-tok"
		c.EnableMCPHTTP = true
		c.Orchestrator.APIKey = "sk-original-secret"
	})

	scoped := connectMCPHTTP(t, ts.URL, "mcp-tok")

	// A model change through MCP takes effect immediately (no restart).
	res, err := callUpdateSettings(t, scoped, map[string]string{"llm_model": "gpt-4.1"})
	if err != nil || res.IsError {
		t.Fatalf("mcp llm_model update failed: %v %+v", err, res)
	}
	got, _ := getSettings(t, ts.URL, "admin-tok")
	if got.LLMModel != "gpt-4.1" {
		t.Fatalf("live settings not updated by MCP: %+v", got)
	}

	// An MCP-token-only caller may not redirect the key or replace it.
	for _, upd := range []map[string]string{
		{"llm_base_url": "https://attacker.example/v1"},
		{"llm_api_key": "sk-attacker"},
		{"clear_api_key": "true"},
	} {
		res, err = callUpdateSettings(t, scoped, upd)
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("expected %v to be refused for MCP-token caller", upd)
		}
	}
	got, _ = getSettings(t, ts.URL, "admin-tok")
	if got.LLMBaseURL != "" || !got.LLMAPIKeySet {
		t.Fatalf("scoped caller changed key routing: %+v", got)
	}

	// Validation applies to MCP too.
	res, err = callUpdateSettings(t, scoped, map[string]string{"llm_provider": "antropic"})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatal("expected unknown provider to be rejected over MCP")
	}
	res, err = callUpdateSettings(t, scoped, map[string]string{"bogus_key": "x"})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatal("expected unknown settings key to be rejected over MCP")
	}

	// The admin token over MCP holds admin scope.
	admin := connectMCPHTTP(t, ts.URL, "admin-tok")
	res, err = callUpdateSettings(t, admin, map[string]string{"llm_base_url": "https://proxy.example.com/v1"})
	if err != nil || res.IsError {
		t.Fatalf("admin llm_base_url update failed: %v %+v", err, res)
	}
	got, raw := getSettings(t, ts.URL, "admin-tok")
	if got.LLMBaseURL != "https://proxy.example.com/v1" {
		t.Fatalf("admin base URL change not live: %+v", got)
	}
	if strings.Contains(raw, "sk-original-secret") {
		t.Fatalf("GET /api/settings echoed the API key: %s", raw)
	}

	// get_settings over MCP reflects live config and never includes the key.
	gs, err := scoped.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_settings"})
	if err != nil || gs.IsError {
		t.Fatalf("get_settings: %v %+v", err, gs)
	}
	text := gs.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "gpt-4.1") || strings.Contains(text, "sk-original-secret") {
		t.Fatalf("unexpected get_settings output: %s", text)
	}
}

func TestRESTSettingsValidation(t *testing.T) {
	_, ts := newSecurityTestServer(t, nil)
	for _, body := range []string{
		`{"llm_provider":"antropic"}`,
		`{"llm_base_url":"not a url"}`,
		`{"llm_base_url":"file:///etc/passwd"}`,
	} {
		resp := doReq(t, http.MethodPut, ts.URL+"/api/settings", strings.NewReader(body),
			map[string]string{"Content-Type": "application/json"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", body, resp.StatusCode)
		}
	}
	got, _ := getSettings(t, ts.URL, "")
	if got.LLMProvider != "openai" || got.LLMBaseURL != "" {
		t.Fatalf("rejected update must not change settings: %+v", got)
	}

	resp := doReq(t, http.MethodPut, ts.URL+"/api/settings", strings.NewReader(`{"llm_provider":"gemini","llm_api_key":"g-secret"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid update: got %d", resp.StatusCode)
	}
	got, raw := getSettings(t, ts.URL, "")
	if got.LLMProvider != "gemini" || !got.LLMAPIKeySet || strings.Contains(raw, "g-secret") {
		t.Fatalf("unexpected settings after valid update: %s", raw)
	}
}

func TestInvalidSavedProviderIgnoredAtStartup(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "konsert.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSettings(ctx, map[string]string{"llm_provider": "antropic", "llm_model": "m"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	defer srv.Hub().Close()
	if got := srv.buildSettingsResponse(ctx); got.LLMProvider != "openai" {
		t.Fatalf("invalid saved provider must be ignored, got %+v", got)
	}
}

// TestCreateChatSessionDuringSettingsUpdate exercises the cfgMu read in
// handleCreateChatSession against concurrent writers (run with -race).
func TestCreateChatSessionDuringSettingsUpdate(t *testing.T) {
	_, ts := newSecurityTestServer(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", strings.NewReader(`{"llm_model":"m"}`))
			req.Header.Set("Content-Type", "application/json")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}()
		go func() {
			defer wg.Done()
			if resp, err := http.Post(ts.URL+"/api/chat/sessions", "application/json", strings.NewReader(`{}`)); err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}

func TestConfigProvidersMatchOrchestrator(t *testing.T) {
	if !slices.Equal(config.SupportedProviders, orchestrator.SupportedProviders) {
		t.Fatalf("config.SupportedProviders %v != orchestrator.SupportedProviders %v", config.SupportedProviders, orchestrator.SupportedProviders)
	}
	for _, p := range append(slices.Clone(orchestrator.SupportedProviders), "", "OpenAI", "antropic") {
		if (config.ValidateProvider(p) == nil) != (orchestrator.ValidateProvider(p) == nil) {
			t.Errorf("provider %q: config and orchestrator validation disagree", p)
		}
	}
}

func TestEffectiveModelHasNoBuiltInDefault(t *testing.T) {
	if got := effectiveModel(config.OrchestratorConfig{Provider: "anthropic"}); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := effectiveModel(config.OrchestratorConfig{Provider: "openai", Model: "x"}); got != "x" {
		t.Fatalf("got %q", got)
	}
}
