package config

import (
	"strings"
	"testing"
)

func TestDefaultControllerConfigIsValid(t *testing.T) {
	if err := DefaultControllerConfig().Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestControllerConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ControllerConfig)
		wantErr string
	}{
		{"typo provider", func(c *ControllerConfig) { c.Orchestrator.Provider = "antropic" }, `"antropic" is not supported`},
		{"empty provider means openai", func(c *ControllerConfig) { c.Orchestrator.Provider = "" }, ""},
		{"negative max tokens", func(c *ControllerConfig) { c.Orchestrator.MaxTokens = -1 }, "max_tokens"},
		{"provider case-insensitive", func(c *ControllerConfig) { c.Orchestrator.Provider = "Anthropic" }, ""},
		{"base url no scheme", func(c *ControllerConfig) { c.Orchestrator.BaseURL = "api.example.com/v1" }, "must use scheme"},
		{"base url ftp", func(c *ControllerConfig) { c.Orchestrator.BaseURL = "ftp://example.com" }, "must use scheme"},
		{"base url ok", func(c *ControllerConfig) { c.Orchestrator.BaseURL = "https://proxy.example.com/v1" }, ""},
		{"listen bad", func(c *ControllerConfig) { c.ListenAddr = "8420" }, "listen_addr"},
		{"listen bad port", func(c *ControllerConfig) { c.ListenAddr = ":99999" }, "invalid port"},
		{"listen ok", func(c *ControllerConfig) { c.ListenAddr = "127.0.0.1:8420" }, ""},
		{"empty db", func(c *ControllerConfig) { c.DBPath = " " }, "db_path"},
		{"public url bad", func(c *ControllerConfig) { c.PublicURL = "konsert.local" }, "public_url"},
		{"public url ok", func(c *ControllerConfig) { c.PublicURL = "https://konsert.example.ts.net" }, ""},
		{"origin bad pattern", func(c *ControllerConfig) { c.AllowedOrigins = []string{"[bad"} }, "not a valid pattern"},
		{"origin empty", func(c *ControllerConfig) { c.AllowedOrigins = []string{" "} }, "empty entry"},
		{"origins ok", func(c *ControllerConfig) {
			c.AllowedOrigins = []string{"http://localhost:4200", "*.example.ts.net"}
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultControllerConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	cfg := DefaultControllerConfig()
	cfg.Orchestrator.Provider = "nope"
	cfg.Orchestrator.BaseURL = "nope"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider") || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("expected both problems reported, got %v", err)
	}
}

func TestWorkerConfigValidate(t *testing.T) {
	w := DefaultWorkerConfig()
	if err := w.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	w.Isolation = "docker"
	if err := w.Validate(); err == nil || !strings.Contains(err.Error(), "isolation") {
		t.Fatalf("expected isolation error, got %v", err)
	}
	w = DefaultWorkerConfig()
	w.ControllerURL = "http://127.0.0.1:8420/ws/worker"
	if err := w.Validate(); err == nil || !strings.Contains(err.Error(), "controller_url") {
		t.Fatalf("expected controller_url error, got %v", err)
	}
}
