package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ControllerConfig holds configuration for the central konsert hub.
type ControllerConfig struct {
	// ListenAddr is the HTTP listen address. Empty means DefaultListenAddr(NoAuth):
	// all interfaces when auth is on, loopback only with --no-auth.
	ListenAddr string `yaml:"listen_addr"`
	DBPath     string `yaml:"db_path"`
	FleetToken string `yaml:"fleet_token"`
	// RecoveryKey is the hidden emergency credential (Bearer token, or the login page's
	// "Use recovery key" fallback). Normally unset: the hub generates one into TokensFile
	// and never prints it. People sign in by pairing devices instead.
	RecoveryKey string `yaml:"recovery_key"`
	// TokensFile is where the generated recovery key and fleet token are persisted
	// (0600). Empty means "konsert-tokens.yaml" next to DBPath.
	TokensFile string `yaml:"tokens_file"`
	// RecoveryKeyInTokensFile is set when the recovery key came from (or was generated
	// into) TokensFile, so rotating it can rewrite the file.
	RecoveryKeyInTokensFile bool `yaml:"-"`
	// SecretKeyFile holds the master key that encrypts secrets (the LLM API key) in the
	// database. Empty means "konsert-secret.key" next to DBPath. KONSERT_SECRET_KEY
	// (base64 of 32 bytes) supplies the key directly instead.
	SecretKeyFile         string `yaml:"secret_key_file"`
	NoAuth                bool   `yaml:"no_auth"`
	TrustTailscaleHeaders bool   `yaml:"trust_tailscale_headers"`
	EnableMCPHTTP         bool   `yaml:"enable_mcp_http"`
	// PublicURL is the externally reachable base URL of this controller (e.g.
	// https://konsert.example.ts.net). It is used to derive the worker WebSocket URL
	// handed to SSH-bootstrapped workers and is always an allowed browser origin.
	PublicURL string `yaml:"public_url"`
	// AllowedOrigins lists extra browser origins (e.g. "http://localhost:4200" or a
	// host pattern such as "*.example.ts.net") permitted to call the REST API and open
	// /ws/client. The request's own Host is always allowed.
	AllowedOrigins []string           `yaml:"allowed_origins"`
	Orchestrator   OrchestratorConfig `yaml:"orchestrator"`
}

// DefaultPort is the controller's default TCP port.
const DefaultPort = "8420"

// DefaultListenAddr returns the listen address used when none is configured: loopback
// only when authentication is disabled, all interfaces otherwise.
func DefaultListenAddr(noAuth bool) string {
	if noAuth {
		return "127.0.0.1:" + DefaultPort
	}
	return ":" + DefaultPort
}

// OrchestratorConfig configures the built-in Orchestrator Chat LLM backend.
type OrchestratorConfig struct {
	Provider    string `yaml:"provider"`     // "openai", "anthropic", "gemini"
	BaseURL     string `yaml:"base_url"`     // Optional custom OpenAI-compatible / proxy URL
	APIKey      string `yaml:"api_key"`      // Or read from env
	Model       string `yaml:"model"`        // e.g. "gpt-4o", "claude-sonnet-4-20250514", "gemini-2.5-pro"
	SystemExtra string `yaml:"system_extra"` // Optional additional system instructions
	// MaxTokens caps output tokens per LLM call; 0 uses the provider default.
	MaxTokens int `yaml:"max_tokens"`
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
	// Accounts are extra agent accounts (e.g. a second subscription of the same CLI) on
	// top of each installed CLI's default account. See docs/design/agent-chat.md.
	Accounts []AgentAccountConfig `yaml:"accounts"`
}

// AgentAccountConfig is one extra agent account in konsertd's YAML config:
//
//	accounts:
//	  - id: claude2
//	    agent: claude
//	    label: Claude 2
//	    env:
//	      CLAUDE_CONFIG_DIR: ~/.claude2
//	    token_file: ~/.claude2-oauth-token  # read into the agent's token env var; must be 0600
//
// Env values and the token never leave the worker.
type AgentAccountConfig struct {
	ID        string            `yaml:"id"`
	Agent     string            `yaml:"agent"`
	Label     string            `yaml:"label"`
	Env       map[string]string `yaml:"env"`
	TokenFile string            `yaml:"token_file"`
}

// DefaultControllerConfig returns sensible MVP defaults.
func DefaultControllerConfig() ControllerConfig {
	return ControllerConfig{
		DBPath:        "konsert.db",
		EnableMCPHTTP: true,
		Orchestrator: OrchestratorConfig{
			Provider: "openai",
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
	if v := os.Getenv("KONSERT_RECOVERY_KEY"); v != "" {
		cfg.RecoveryKey = v
	}
	if v := os.Getenv("KONSERT_TOKENS_FILE"); v != "" {
		cfg.TokensFile = v
	}
	if v := os.Getenv("KONSERT_SECRET_KEY_FILE"); v != "" {
		cfg.SecretKeyFile = v
	}
	if v := os.Getenv("KONSERT_PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}
	if v := os.Getenv("KONSERT_ALLOWED_ORIGINS"); v != "" {
		cfg.AllowedOrigins = SplitList(v)
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
	if v := os.Getenv("KONSERT_LLM_MAX_TOKENS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cfg.Orchestrator.MaxTokens = n
		} else {
			cfg.Orchestrator.MaxTokens = -1 // rejected by Validate
		}
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
		cfg.AllowedRoots = SplitList(v)
	}
	if v := strings.ToLower(os.Getenv("KONSERT_SANDBOX")); v == "1" || v == "true" {
		cfg.Sandbox = true
	}
	if v := strings.ToLower(os.Getenv("KONSERT_ENABLE_MOCK_DRIVER")); v == "1" || v == "true" {
		cfg.EnableMockDriver = true
	}
}

// SplitList splits a comma-separated list, trimming whitespace and dropping empty items.
func SplitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
