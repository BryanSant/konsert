package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ControllerConfig holds configuration for the central konsert hub.
type ControllerConfig struct {
	ListenAddr            string             `yaml:"listen_addr"`
	DBPath                string             `yaml:"db_path"`
	FleetToken            string             `yaml:"fleet_token"`
	AdminToken            string             `yaml:"admin_token"`
	MCPToken              string             `yaml:"mcp_token"`
	NoAuth                bool               `yaml:"no_auth"`
	TrustTailscaleHeaders bool               `yaml:"trust_tailscale_headers"`
	EnableMCPHTTP         bool               `yaml:"enable_mcp_http"`
	Orchestrator          OrchestratorConfig `yaml:"orchestrator"`
}

// OrchestratorConfig configures the built-in Orchestrator Chat LLM backend.
type OrchestratorConfig struct {
	Provider    string `yaml:"provider"`     // "openai", "anthropic", "gemini"
	BaseURL     string `yaml:"base_url"`     // Optional custom OpenAI-compatible / proxy URL
	APIKey      string `yaml:"api_key"`      // Or read from env
	Model       string `yaml:"model"`        // e.g. "gpt-4o", "claude-sonnet-4-20250514", "gemini-2.5-pro"
	SystemExtra string `yaml:"system_extra"` // Optional additional system instructions
}

// WorkerConfig holds configuration for the konsertd worker daemon.
type WorkerConfig struct {
	ControllerURL    string            `yaml:"controller_url"`
	FleetToken       string            `yaml:"fleet_token"`
	WorkerID         string            `yaml:"worker_id"`
	Name             string            `yaml:"name"`
	HostGroup        string            `yaml:"host_group"`
	Isolation        string            `yaml:"isolation"` // "host", "container", "vm"
	Sandbox          bool              `yaml:"sandbox"`   // Opt-in sandboxed mode (default false = full_access)
	AllowedRoots     []string          `yaml:"allowed_roots"`
	Tags             map[string]string `yaml:"tags"`
	MaxConcurrent    int               `yaml:"max_concurrent"`
	EnableMockDriver bool              `yaml:"enable_mock_driver"`
}

// DefaultControllerConfig returns sensible MVP defaults.
func DefaultControllerConfig() ControllerConfig {
	return ControllerConfig{
		ListenAddr:    ":8420",
		DBPath:        "konsert.db",
		EnableMCPHTTP: true,
		Orchestrator: OrchestratorConfig{
			Provider: "openai",
			Model:    "gpt-4o",
		},
	}
}

// DefaultWorkerConfig returns sensible MVP defaults (full_access by default).
func DefaultWorkerConfig() WorkerConfig {
	host, _ := os.Hostname()
	if host == "" {
		host = "worker"
	}
	return WorkerConfig{
		ControllerURL: "ws://127.0.0.1:8420/ws/worker",
		Name:          host,
		HostGroup:     host,
		Isolation:     detectIsolation(),
		Sandbox:       false, // Full-Access ("YOLO") by default
		MaxConcurrent: 2,
		Tags:          map[string]string{},
	}
}

func detectIsolation() string {
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return "container"
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "container"
	}
	return "host"
}

// LoadControllerConfig loads YAML (if path != "") and applies environment variable overrides.
func LoadControllerConfig(path string) (ControllerConfig, error) {
	cfg := DefaultControllerConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read controller config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse controller config %s: %w", path, err)
		}
	}
	applyControllerEnv(&cfg)
	return cfg, nil
}

func applyControllerEnv(cfg *ControllerConfig) {
	if v := os.Getenv("KONSERT_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("KONSERT_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("KONSERT_FLEET_TOKEN"); v != "" {
		cfg.FleetToken = v
	}
	if v := os.Getenv("KONSERT_ADMIN_TOKEN"); v != "" {
		cfg.AdminToken = v
	}
	if v := os.Getenv("KONSERT_MCP_TOKEN"); v != "" {
		cfg.MCPToken = v
	}
	if v := os.Getenv("KONSERT_LLM_PROVIDER"); v != "" {
		cfg.Orchestrator.Provider = v
	}
	if v := os.Getenv("KONSERT_LLM_BASE_URL"); v != "" {
		cfg.Orchestrator.BaseURL = v
	}
	if v := os.Getenv("KONSERT_LLM_API_KEY"); v != "" {
		cfg.Orchestrator.APIKey = v
	}
	if v := os.Getenv("KONSERT_LLM_MODEL"); v != "" {
		cfg.Orchestrator.Model = v
	}
	// Fallback provider-specific API keys if KONSERT_LLM_API_KEY is unset.
	if cfg.Orchestrator.APIKey == "" {
		switch strings.ToLower(cfg.Orchestrator.Provider) {
		case "anthropic":
			cfg.Orchestrator.APIKey = os.Getenv("ANTHROPIC_API_KEY")
		case "gemini":
			if v := os.Getenv("GEMINI_API_KEY"); v != "" {
				cfg.Orchestrator.APIKey = v
			} else {
				cfg.Orchestrator.APIKey = os.Getenv("GOOGLE_API_KEY")
			}
		default:
			cfg.Orchestrator.APIKey = os.Getenv("OPENAI_API_KEY")
		}
	}
}

// LoadWorkerConfig loads YAML (if path != "") and applies environment variable overrides.
func LoadWorkerConfig(path string) (WorkerConfig, error) {
	cfg := DefaultWorkerConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read worker config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse worker config %s: %w", path, err)
		}
	}
	applyWorkerEnv(&cfg)
	if cfg.Tags == nil {
		cfg.Tags = map[string]string{}
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 2
	}
	return cfg, nil
}

func applyWorkerEnv(cfg *WorkerConfig) {
	if v := os.Getenv("KONSERT_CONTROLLER_URL"); v != "" {
		cfg.ControllerURL = v
	}
	if v := os.Getenv("KONSERT_FLEET_TOKEN"); v != "" {
		cfg.FleetToken = v
	}
	if v := os.Getenv("KONSERT_WORKER_ID"); v != "" {
		cfg.WorkerID = v
	}
	if v := os.Getenv("KONSERT_WORKER_NAME"); v != "" {
		cfg.Name = v
	}
	if v := os.Getenv("KONSERT_HOST_GROUP"); v != "" {
		cfg.HostGroup = v
	}
	if v := os.Getenv("KONSERT_ISOLATION"); v != "" {
		cfg.Isolation = v
	}
	if v := os.Getenv("KONSERT_ALLOWED_ROOTS"); v != "" {
		parts := strings.Split(v, ",")
		var roots []string
		for _, p := range parts {
			if t := strings.TrimSpace(p); t != "" {
				roots = append(roots, t)
			}
		}
		cfg.AllowedRoots = roots
	}
	if v := strings.ToLower(os.Getenv("KONSERT_SANDBOX")); v == "1" || v == "true" {
		cfg.Sandbox = true
	}
	if v := strings.ToLower(os.Getenv("KONSERT_ENABLE_MOCK_DRIVER")); v == "1" || v == "true" {
		cfg.EnableMockDriver = true
	}
}
