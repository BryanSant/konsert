package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/worker/drivers"
)

// MockAgent is the agent name of the in-process mock engine (--enable-mock-driver).
const MockAgent = "mock"

// agentInfo describes how one agent CLI separates accounts.
type agentInfo struct {
	Agent  string // driver name (account Agent)
	Binary string // executable name
	Label  string // default account label
	// ConfigEnv are the variables that select a per-account state directory. The first
	// one is required for a wrapper script to be imported.
	ConfigEnv []string
	// TokenEnv receives a token_file's contents ("" = token files are not supported).
	TokenEnv string
	// ClearEnv are inherited auth variables cleared for non-default accounts unless the
	// account sets them, so a second account never silently runs on the daemon's login.
	ClearEnv []string
	// KeychainBound means the login lives in a global OS keychain item on macOS, so a
	// config directory alone does not separate accounts.
	KeychainBound bool
	// Relogin is the interactive re-login command.
	Relogin string
}

// knownAgents lists the agent CLIs syred discovers accounts for.
var knownAgents = []agentInfo{
	{Agent: "claude", Binary: "claude", Label: "Claude",
		ConfigEnv: []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR"},
		TokenEnv:  "CLAUDE_CODE_OAUTH_TOKEN", ClearEnv: []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"},
		Relogin: "claude auth login"},
	{Agent: "codex", Binary: "codex", Label: "Codex", ConfigEnv: []string{"CODEX_HOME"},
		Relogin: "codex login --device-auth"},
	{Agent: "grok", Binary: "grok", Label: "Grok", ConfigEnv: []string{"GROK_HOME"},
		Relogin: "grok login"},
	{Agent: "agy", Binary: "agy", Label: "Antigravity", TokenEnv: "GEMINI_API_KEY",
		ClearEnv: []string{"GEMINI_API_KEY"}, KeychainBound: true, Relogin: "agy"},
	{Agent: "cursor", Binary: "cursor-agent", Label: "Cursor", ConfigEnv: []string{"CURSOR_CONFIG_DIR"},
		TokenEnv: "CURSOR_API_KEY", ClearEnv: []string{"CURSOR_API_KEY"}, KeychainBound: true,
		Relogin: "cursor-agent login"},
	{Agent: "copilot", Binary: "copilot", Label: "Copilot", ConfigEnv: []string{"COPILOT_HOME", "GH_CONFIG_DIR"},
		TokenEnv: "COPILOT_GITHUB_TOKEN", ClearEnv: []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"},
		KeychainBound: true, Relogin: "copilot login"},
}

func lookupAgent(agent string) (agentInfo, bool) {
	for _, a := range knownAgents {
		if a.Agent == agent {
			return a, true
		}
	}
	return agentInfo{}, false
}

// KnownAgents returns the driver names syred discovers accounts for.
func KnownAgents() []string {
	out := make([]string, 0, len(knownAgents))
	for _, a := range knownAgents {
		out = append(out, a.Agent)
	}
	return out
}

// ErrAccountNotFound is returned by Resolve for an unknown account.
var ErrAccountNotFound = errors.New("agent account not found")

// AccountEntry is a registered account as listed for capabilities: no secrets (the token
// is only read by Resolve), but Env may still hold paths and must not be sent to the hub.
type AccountEntry struct {
	Account
	// Source says where the account came from: default, config, wrapper, mock.
	Source string
	// Problem explains why the account cannot be used (bad token file permissions,
	// CLI not installed, ...). Resolve fails while it is set.
	Problem string
	// Note is a user-facing caveat (e.g. keychain-bound accounts on macOS).
	Note string

	tokenFile string
	tokenEnv  string
}

// AccountOptions configures account discovery. Zero values use the real environment.
type AccountOptions struct {
	// Configured are the YAML `accounts:` entries.
	Configured []config.AgentAccountConfig
	// EnableMock adds the mock engine's "mock" and "mock2" accounts.
	EnableMock bool
	// LookPath finds an agent binary (default drivers.FindBinary).
	LookPath func(name string) (string, bool)
	// HomeDir is used for "~" expansion and wrapper search (default os.UserHomeDir).
	HomeDir string
	// WrapperDirs are searched for <agent><N> wrapper scripts (default: PATH plus
	// ~/.local/bin and ~/bin). Set DisableWrappers to skip the importer.
	WrapperDirs     []string
	DisableWrappers bool
	Logger          *slog.Logger
}

// AccountRegistry holds every agent account on this worker: each installed CLI's default
// account, YAML extras and imported wrapper scripts.
type AccountRegistry struct {
	opts AccountOptions
	log  *slog.Logger

	mu       sync.RWMutex
	accounts []AccountEntry
	// logged remembers what was already logged, so periodic reloads stay quiet.
	logged map[string]bool
}

// NewAccountRegistry creates a registry and runs discovery once.
func NewAccountRegistry(opts AccountOptions) *AccountRegistry {
	if opts.LookPath == nil {
		opts.LookPath = drivers.FindBinary
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	r := &AccountRegistry{opts: opts, log: opts.Logger, logged: map[string]bool{}}
	r.Load()
	return r
}

func (r *AccountRegistry) homeDir() string {
	if r.opts.HomeDir != "" {
		return r.opts.HomeDir
	}
	h, _ := os.UserHomeDir()
	return h
}

// logOnce logs msg at most once per key (reloads run every few minutes).
func (r *AccountRegistry) logOnce(key string, level slog.Level, msg string, args ...any) {
	r.mu.Lock()
	seen := r.logged[key]
	r.logged[key] = true
	r.mu.Unlock()
	if !seen {
		r.log.Log(context.Background(), level, msg, args...)
	}
}

// Load (re)discovers accounts. It never spawns processes: binaries are found on PATH and
// wrapper scripts are parsed as text.
func (r *AccountRegistry) Load() {
	home := r.homeDir()
	var out []AccountEntry
	byID := map[string]int{}
	add := func(e AccountEntry) {
		if i, ok := byID[e.ID]; ok {
			out[i] = e
			return
		}
		byID[e.ID] = len(out)
		out = append(out, e)
	}

	// 1. Default accounts of installed CLIs.
	for _, a := range knownAgents {
		bin, ok := r.opts.LookPath(a.Binary)
		if !ok {
			continue
		}
		e := AccountEntry{Account: Account{ID: a.Agent, Agent: a.Agent, Label: a.Label, Binary: bin, IsDefault: true}, Source: "default"}
		add(e)
	}
	if r.opts.EnableMock {
		add(AccountEntry{Account: Account{ID: "mock", Agent: MockAgent, Label: "Mock", IsDefault: true}, Source: "mock"})
		add(AccountEntry{Account: Account{ID: "mock2", Agent: MockAgent, Label: "Mock 2",
			Env: map[string]string{"SYRE_MOCK_ACCOUNT": "mock2"}}, Source: "mock"})
	}

	// 2. YAML extras (they win over imported wrappers with the same ID).
	configured := map[string]bool{}
	for _, c := range r.opts.Configured {
		e, err := r.fromConfig(c, home)
		if err != nil {
			r.logOnce("config-invalid:"+c.ID+err.Error(), slog.LevelError, "ignoring agent account from config", "id", c.ID, "agent", c.Agent, "err", err)
			continue
		}
		configured[e.ID] = true
		add(e)
	}

	// 3. Wrapper scripts such as ~/.local/bin/claude2.
	if !r.opts.DisableWrappers {
		for _, e := range r.importWrappers(home) {
			if configured[e.ID] {
				r.logOnce("wrapper-shadowed:"+e.ID, slog.LevelInfo, "wrapper script ignored: a configured account has the same id", "id", e.ID)
				continue
			}
			if _, dup := byID[e.ID]; dup {
				continue
			}
			add(e)
		}
	}

	for i := range out {
		out[i].Note = accountNote(out[i])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return agentOrder(out[i].Agent) < agentOrder(out[j].Agent)
		}
		if out[i].IsDefault != out[j].IsDefault {
			return out[i].IsDefault
		}
		return out[i].ID < out[j].ID
	})
	r.mu.Lock()
	r.accounts = out
	r.mu.Unlock()
}

func agentOrder(agent string) int {
	for i, a := range knownAgents {
		if a.Agent == agent {
			return i
		}
	}
	return len(knownAgents)
}

var accountIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func (r *AccountRegistry) fromConfig(c config.AgentAccountConfig, home string) (AccountEntry, error) {
	id := strings.TrimSpace(c.ID)
	agent := strings.TrimSpace(c.Agent)
	if !accountIDRe.MatchString(id) {
		return AccountEntry{}, fmt.Errorf("invalid id %q (use lowercase letters, digits, '-' and '_')", c.ID)
	}
	if agent == MockAgent && r.opts.EnableMock {
		label := strings.TrimSpace(c.Label)
		if label == "" {
			label = id
		}
		return AccountEntry{Account: Account{ID: id, Agent: MockAgent, Label: label, Env: expandEnv(c.Env, home)}, Source: "config"}, nil
	}
	info, ok := lookupAgent(agent)
	if !ok {
		return AccountEntry{}, fmt.Errorf("unknown agent %q (known: %s)", c.Agent, strings.Join(KnownAgents(), ", "))
	}
	label := strings.TrimSpace(c.Label)
	if label == "" {
		label = info.Label + " (" + id + ")"
	}
	e := AccountEntry{
		Account: Account{ID: id, Agent: agent, Label: label, Env: expandEnv(c.Env, home), IsDefault: id == agent},
		Source:  "config",
	}
	if bin, ok := r.opts.LookPath(info.Binary); ok {
		e.Binary = bin
	} else {
		e.Problem = info.Binary + " is not installed on this worker"
	}
	if tf := strings.TrimSpace(c.TokenFile); tf != "" {
		if info.TokenEnv == "" {
			return AccountEntry{}, fmt.Errorf("token_file is not supported for agent %q (set its login directory in env instead)", agent)
		}
		e.tokenFile = expandHome(tf, home)
		e.tokenEnv = info.TokenEnv
		if err := checkTokenFile(e.tokenFile); err != nil {
			e.Problem = err.Error()
			r.logOnce("token-file:"+id+err.Error(), slog.LevelError, "agent account token file refused", "id", id, "err", err)
		}
	}
	return e, nil
}

// expandEnv copies env with "~" expanded in values.
func expandEnv(env map[string]string, home string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[strings.TrimSpace(k)] = expandHome(v, home)
	}
	return out
}

// expandHome expands a leading "~" or "~/" (also "~\" on Windows).
func expandHome(v, home string) string {
	if home == "" {
		return v
	}
	if v == "~" {
		return home
	}
	if strings.HasPrefix(v, "~/") || (runtime.GOOS == "windows" && strings.HasPrefix(v, `~\`)) {
		return filepath.Join(home, v[2:])
	}
	return v
}

// checkTokenFile refuses token files other users can read (or that are missing).
func checkTokenFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("token file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("token file %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("token file %s has mode %04o; it must be readable only by you (chmod 600 %s)", path, info.Mode().Perm(), path)
	}
	return nil
}

func readTokenFile(path string) (string, error) {
	if err := checkTokenFile(path); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	tok := strings.TrimSpace(strings.ReplaceAll(string(raw), "\r", ""))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return tok, nil
}

func accountNote(e AccountEntry) string {
	info, ok := lookupAgent(e.Agent)
	if !ok || !info.KeychainBound || e.IsDefault || runtime.GOOS != "darwin" {
		return ""
	}
	if e.tokenFile != "" || (info.TokenEnv != "" && e.Env[info.TokenEnv] != "") {
		return ""
	}
	return info.Label + " keeps its login in the macOS Keychain, so per-directory accounts are not supported on macOS: this account uses the same sign-in as the default one" +
		tokenHint(info)
}

func tokenHint(info agentInfo) string {
	if info.TokenEnv == "" {
		return "."
	}
	return " unless it sets a token_file (" + info.TokenEnv + ")."
}

// List returns every account (secrets are never included: tokens are read on Resolve).
func (r *AccountRegistry) List() []AccountEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AccountEntry, len(r.accounts))
	for i, e := range r.accounts {
		e.Env = copyEnv(e.Env)
		out[i] = e
	}
	return out
}

// Entry returns one account by ID.
func (r *AccountRegistry) Entry(id string) (AccountEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.accounts {
		if e.ID == id {
			e.Env = copyEnv(e.Env)
			return e, true
		}
	}
	return AccountEntry{}, false
}

// Resolve returns the runnable account for agent: accountID, or the agent's default
// account when accountID is empty. The token file (if any) is read now, so a rotated
// token is picked up by the next session. The result's Env holds secrets.
func (r *AccountRegistry) Resolve(agent, accountID string) (Account, error) {
	r.mu.RLock()
	var (
		e     AccountEntry
		found bool
	)
	for _, cand := range r.accounts {
		if (accountID != "" && cand.ID == accountID) || (accountID == "" && cand.Agent == agent && cand.IsDefault) {
			e, found = cand, true
			break
		}
	}
	r.mu.RUnlock()
	if !found {
		if accountID == "" {
			return Account{}, fmt.Errorf("%w: %s has no default account on this worker (not installed?)", ErrAccountNotFound, agent)
		}
		return Account{}, fmt.Errorf("%w: %q", ErrAccountNotFound, accountID)
	}
	if agent != "" && e.Agent != agent {
		return Account{}, fmt.Errorf("account %q belongs to %s, not %s", e.ID, e.Agent, agent)
	}
	if e.Problem != "" {
		return Account{}, errors.New(e.Problem)
	}
	acct := e.Account
	acct.Env = copyEnv(e.Env)
	if !acct.IsDefault {
		if info, ok := lookupAgent(acct.Agent); ok {
			for _, k := range info.ClearEnv {
				if _, set := acct.Env[k]; !set {
					if acct.Env == nil {
						acct.Env = map[string]string{}
					}
					acct.Env[k] = ""
				}
			}
		}
	}
	if e.tokenFile != "" {
		tok, err := readTokenFile(e.tokenFile)
		if err != nil {
			return Account{}, err
		}
		if acct.Env == nil {
			acct.Env = map[string]string{}
		}
		acct.Env[e.tokenEnv] = tok
	}
	return acct, nil
}

func copyEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

// EnvList renders an account's Env as sorted KEY=VALUE entries, to append after the
// daemon's environment (later entries win in os/exec).
func EnvList(acct Account) []string {
	keys := make([]string, 0, len(acct.Env))
	for k := range acct.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+acct.Env[k])
	}
	return out
}

// AccountEnviron returns base with the account's Env applied (overriding existing keys).
func AccountEnviron(base []string, acct Account) []string {
	if len(acct.Env) == 0 {
		return append([]string(nil), base...)
	}
	fold := runtime.GOOS == "windows"
	key := func(k string) string {
		if fold {
			return strings.ToUpper(k)
		}
		return k
	}
	override := map[string]bool{}
	for k := range acct.Env {
		override[key(k)] = true
	}
	out := make([]string, 0, len(base)+len(acct.Env))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !override[key(k)] {
			out = append(out, kv)
		}
	}
	return append(out, EnvList(acct)...)
}

// ProfileEnv returns the account's non-secret profile variables (config directories),
// suitable for AgentRelogin.Env.
func ProfileEnv(acct Account) map[string]string {
	info, ok := lookupAgent(acct.Agent)
	if !ok {
		return nil
	}
	var out map[string]string
	for _, k := range info.ConfigEnv {
		if v := acct.Env[k]; v != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}
