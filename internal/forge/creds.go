package forge

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/secrets"
	"gopkg.in/yaml.v3"
)

const (
	defaultCredTimeout = 5 * time.Second
	forgeTokenPrefix   = "forge_token:"
)

// CmdRunner executes external helper commands (git credential, gh, glab).
// Injectable for deterministic unit tests.
type CmdRunner interface {
	Run(ctx context.Context, name string, args []string, env []string, stdin string) (stdout string, err error)
}

// execRunner is the production CmdRunner backed by os/exec.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, env []string, stdin string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	// Discard stderr so no credential helper output is ever surfaced or logged.
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// StoredToken is a credential persisted on the hub (from device sign-in or paste).
type StoredToken struct {
	Host   string `json:"host"`
	Token  string `json:"token"`
	Source string `json:"source"` // "device" | "pasted"
	Kind   Kind   `json:"kind,omitempty"`
}

// TokenStore persists forge tokens encrypted at rest on the hub.
type TokenStore interface {
	GetToken(ctx context.Context, host string) (StoredToken, bool, error)
	PutToken(ctx context.Context, tok StoredToken) error
	DeleteToken(ctx context.Context, host string) error
	ListTokens(ctx context.Context) ([]StoredToken, error)
}

// SettingsBackend is the minimal subset of store.Store needed by EncryptedSettingsStore.
type SettingsBackend interface {
	GetAllSettings(ctx context.Context) (map[string]string, error)
	SetSettings(ctx context.Context, kv map[string]string) error
}

// EncryptedSettingsStore implements TokenStore on top of the hub's settings table,
// encrypting every token with internal/secrets.Box before writing.
type EncryptedSettingsStore struct {
	settings SettingsBackend
	box      *secrets.Box
}

// NewEncryptedSettingsStore returns a TokenStore backed by settings and encrypted with box.
func NewEncryptedSettingsStore(settings SettingsBackend, box *secrets.Box) *EncryptedSettingsStore {
	return &EncryptedSettingsStore{settings: settings, box: box}
}

type sealedTokenPayload struct {
	Token  string `json:"token"`
	Source string `json:"source"`
	Kind   Kind   `json:"kind,omitempty"`
}

func (s *EncryptedSettingsStore) GetToken(ctx context.Context, host string) (StoredToken, bool, error) {
	if s == nil || s.settings == nil || s.box == nil {
		return StoredToken{}, false, nil
	}
	host = NormalizeHost(host)
	if host == "" {
		return StoredToken{}, false, nil
	}
	all, err := s.settings.GetAllSettings(ctx)
	if err != nil {
		return StoredToken{}, false, err
	}
	raw := strings.TrimSpace(all[forgeTokenPrefix+host])
	if raw == "" {
		return StoredToken{}, false, nil
	}
	return s.openEntry(host, raw)
}

func (s *EncryptedSettingsStore) PutToken(ctx context.Context, tok StoredToken) error {
	if s == nil || s.settings == nil || s.box == nil {
		return errors.New("forge: token store not configured")
	}
	host := NormalizeHost(tok.Host)
	token := strings.TrimSpace(tok.Token)
	if host == "" || token == "" {
		return errors.New("forge: host and token are required")
	}
	source := strings.TrimSpace(tok.Source)
	if source == "" {
		source = "pasted"
	}
	plain, err := json.Marshal(sealedTokenPayload{
		Token:  token,
		Source: source,
		Kind:   tok.Kind,
	})
	if err != nil {
		return fmt.Errorf("forge: encode token: %w", err)
	}
	sealed, err := s.box.Seal(string(plain))
	if err != nil {
		return fmt.Errorf("forge: encrypt token: %w", err)
	}
	return s.settings.SetSettings(ctx, map[string]string{
		forgeTokenPrefix + host: sealed,
	})
}

func (s *EncryptedSettingsStore) DeleteToken(ctx context.Context, host string) error {
	if s == nil || s.settings == nil {
		return nil
	}
	host = NormalizeHost(host)
	if host == "" {
		return nil
	}
	return s.settings.SetSettings(ctx, map[string]string{
		forgeTokenPrefix + host: "",
	})
}

func (s *EncryptedSettingsStore) ListTokens(ctx context.Context) ([]StoredToken, error) {
	if s == nil || s.settings == nil || s.box == nil {
		return nil, nil
	}
	all, err := s.settings.GetAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	var out []StoredToken
	for k, v := range all {
		if !strings.HasPrefix(k, forgeTokenPrefix) {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		host := strings.TrimPrefix(k, forgeTokenPrefix)
		tok, ok, err := s.openEntry(host, v)
		if err != nil || !ok {
			continue
		}
		out = append(out, tok)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

func (s *EncryptedSettingsStore) openEntry(host, raw string) (StoredToken, bool, error) {
	plain, err := s.box.Open(raw)
	if err != nil {
		return StoredToken{}, false, fmt.Errorf("forge: decrypt token for %s: %w", host, err)
	}
	var payload sealedTokenPayload
	if err := json.Unmarshal([]byte(plain), &payload); err != nil {
		return StoredToken{}, false, fmt.Errorf("forge: decode token for %s: %w", host, err)
	}
	if strings.TrimSpace(payload.Token) == "" {
		return StoredToken{}, false, nil
	}
	src := payload.Source
	if src == "" {
		src = "pasted"
	}
	return StoredToken{
		Host:   host,
		Token:  payload.Token,
		Source: src,
		Kind:   payload.Kind,
	}, true, nil
}

// CredDiscovery resolves credentials for a forge host in order:
//  1. git credential fill (HTTPS, GIT_TERMINAL_PROMPT=0)
//  2. gh auth token (GitHub) / glab stored token (GitLab)
//  3. encrypted hub TokenStore (device sign-in or pasted token)
type CredDiscovery struct {
	Runner         CmdRunner
	Store          TokenStore
	Timeout        time.Duration
	GlabConfigPath string // optional override for glab config.yml (tests)
}

func (c *CredDiscovery) runner() CmdRunner {
	if c != nil && c.Runner != nil {
		return c.Runner
	}
	return execRunner{}
}

func (c *CredDiscovery) timeout() time.Duration {
	if c != nil && c.Timeout > 0 {
		return c.Timeout
	}
	return defaultCredTimeout
}

// Discover finds a credential for host (and forge kind) following the documented order.
// Returns ErrNoCredential if no source yields a token.
func (c *CredDiscovery) Discover(ctx context.Context, host string, kind Kind) (Credential, error) {
	host = NormalizeHost(host)
	if host == "" {
		return Credential{}, ErrNoCredential
	}

	// (a) git credential fill
	if tok := c.fromGitCredential(ctx, host); tok != "" {
		return Credential{Host: host, Token: tok, Source: "git-credential"}, nil
	}

	// (b) existing CLI login (gh for GitHub, glab for GitLab)
	switch kind {
	case GitHub:
		if tok := c.fromGH(ctx, host); tok != "" {
			return Credential{Host: host, Token: tok, Source: "gh"}, nil
		}
	case GitLab:
		if tok := c.fromGlab(ctx, host); tok != "" {
			return Credential{Host: host, Token: tok, Source: "glab"}, nil
		}
	}

	// (c) hub encrypted token store (device sign-in or pasted)
	if c != nil && c.Store != nil {
		if st, ok, err := c.Store.GetToken(ctx, host); err == nil && ok && st.Token != "" {
			src := st.Source
			if src == "" {
				src = "pasted"
			}
			return Credential{Host: host, Token: st.Token, Source: src}, nil
		}
	}

	return Credential{}, fmt.Errorf("%w: %s", ErrNoCredential, host)
}

func (c *CredDiscovery) fromGitCredential(ctx context.Context, host string) string {
	cmdCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	stdin := "protocol=https\nhost=" + host + "\n\n"
	out, err := c.runner().Run(cmdCtx, "git", []string{"credential", "fill"}, []string{"GIT_TERMINAL_PROMPT=0"}, stdin)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if v, ok := strings.CutPrefix(line, "password="); ok {
			if tok := strings.TrimSpace(v); tok != "" {
				return tok
			}
		}
	}
	return ""
}

func (c *CredDiscovery) fromGH(ctx context.Context, host string) string {
	cmdCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	out, err := c.runner().Run(cmdCtx, "gh", []string{"auth", "token", "--hostname", host}, []string{"GH_PROMPT_DISABLED=1"}, "")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (c *CredDiscovery) fromGlab(ctx context.Context, host string) string {
	cmdCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	out, err := c.runner().Run(cmdCtx, "glab", []string{"config", "get", "token", "--host", host}, []string{"NO_PROMPT=1"}, "")
	if err == nil {
		tok := strings.TrimSpace(out)
		if tok != "" && tok != "null" && tok != "<nil>" {
			return tok
		}
	}
	return c.fromGlabConfigFile(host)
}

func (c *CredDiscovery) fromGlabConfigFile(host string) string {
	path := ""
	if c != nil && c.GlabConfigPath != "" {
		path = c.GlabConfigPath
	} else {
		if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
			path = filepath.Join(xdg, "glab-cli", "config.yml")
		} else if home, err := os.UserHomeDir(); err == nil && home != "" {
			path = filepath.Join(home, ".config", "glab-cli", "config.yml")
		}
	}
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		Hosts map[string]struct {
			Token string `yaml:"token"`
		} `yaml:"hosts"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	for h, entry := range cfg.Hosts {
		if NormalizeHost(h) == host {
			if tok := strings.TrimSpace(entry.Token); tok != "" {
				return tok
			}
		}
	}
	return ""
}
