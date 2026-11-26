package controller

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/commander"
	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/store/sqlite"
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

func TestMCPSettingsToolsRemovedAndRESTNeverLeaksKey(t *testing.T) {
	_, ts := newSecurityTestServer(t, func(c *config.ControllerConfig) {
		c.NoAuth = false
		c.RecoveryKey = "admin-tok"
		c.EnableMCPHTTP = true
		c.Commander.APIKey = "sk-original-secret"
	})

	scoped := connectMCPHTTP(t, ts.URL, mintMCPCredential(t, ts.URL, "admin-tok"))
	for _, removed := range []string{"get_settings", "update_settings"} {
		res, err := scoped.CallTool(context.Background(), &mcp.CallToolParams{Name: removed})
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("expected removed tool %q to fail on /mcp, got %+v", removed, res)
		}
	}

	// GET /api/settings never echoes the API key.
	got, raw := getSettings(t, ts.URL, "admin-tok")
	if !got.LLMAPIKeySet || strings.Contains(raw, "sk-original-secret") {
		t.Fatalf("GET /api/settings leaked or hid key state: %s", raw)
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
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "syre.db"))
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

func TestConfigProvidersMatchCommander(t *testing.T) {
	if !slices.Equal(config.SupportedProviders, commander.SupportedProviders) {
		t.Fatalf("config.SupportedProviders %v != commander.SupportedProviders %v", config.SupportedProviders, commander.SupportedProviders)
	}
	for _, p := range append(slices.Clone(commander.SupportedProviders), "", "OpenAI", "antropic") {
		if (config.ValidateProvider(p) == nil) != (commander.ValidateProvider(p) == nil) {
			t.Errorf("provider %q: config and commander validation disagree", p)
		}
	}
}

func TestEffectiveModelHasNoBuiltInDefault(t *testing.T) {
	if got := effectiveModel(config.CommanderConfig{Provider: "anthropic"}); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := effectiveModel(config.CommanderConfig{Provider: "openai", Model: "x"}); got != "x" {
		t.Fatalf("got %q", got)
	}
}
