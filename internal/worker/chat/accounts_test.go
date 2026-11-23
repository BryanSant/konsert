package chat

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/config"
)

// fakeBins returns a LookPath that "finds" exactly the given binaries under dir.
func fakeBins(dir string, names ...string) func(string) (string, bool) {
	return func(n string) (string, bool) {
		if slices.Contains(names, n) {
			return filepath.Join(dir, n), true
		}
		return "", false
	}
}

func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func TestAccountsDefaults(t *testing.T) {
	home := isolateHome(t)
	r := NewAccountRegistry(AccountOptions{
		HomeDir: home, DisableWrappers: true,
		LookPath: fakeBins("/bin", "claude", "cursor-agent", "agy", "codex", "grok", "copilot"),
	})
	var ids []string
	for _, e := range r.List() {
		ids = append(ids, e.ID)
		if !e.IsDefault || e.Source != "default" || e.Binary == "" {
			t.Errorf("default account = %+v", e)
		}
	}
	if want := []string{"claude", "codex", "grok", "agy", "cursor", "copilot"}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	cur, _ := r.Entry("cursor")
	if cur.Agent != "cursor" || cur.Binary != "/bin/cursor-agent" || cur.Label != "Cursor" {
		t.Fatalf("cursor = %+v", cur)
	}
	acct, err := r.Resolve("claude", "")
	if err != nil || acct.ID != "claude" || len(acct.Env) != 0 {
		t.Fatalf("resolve default = %+v, %v", acct, err)
	}
	if _, err := r.Resolve("nope", ""); err == nil {
		t.Fatal("resolved an unknown agent")
	}
}

func TestAccountsYAMLExtrasAndTokenFile(t *testing.T) {
	home := isolateHome(t)
	tok := filepath.Join(home, ".claude2-oauth-token")
	if err := os.WriteFile(tok, []byte("sk-secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	cfg := []config.AgentAccountConfig{
		{ID: "claude2", Agent: "claude", Label: "Claude 2", TokenFile: "~/.claude2-oauth-token",
			Env: map[string]string{"CLAUDE_CONFIG_DIR": "~/.claude2", "CLAUDE_SECURESTORAGE_CONFIG_DIR": "~/.claude2"}},
		{ID: "Bad ID", Agent: "claude"},
		{ID: "x", Agent: "unknown"},
		{ID: "codex2", Agent: "codex", TokenFile: "~/t"}, // codex has no token env
	}
	r := NewAccountRegistry(AccountOptions{
		HomeDir: home, DisableWrappers: true, Configured: cfg,
		LookPath: fakeBins("/bin", "claude"),
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
	})
	ids := []string{}
	for _, e := range r.List() {
		ids = append(ids, e.ID)
	}
	if !slices.Equal(ids, []string{"claude", "claude2"}) {
		t.Fatalf("ids = %v", ids)
	}
	acct, err := r.Resolve("claude", "claude2")
	if err != nil {
		t.Fatal(err)
	}
	if acct.Env["CLAUDE_CONFIG_DIR"] != filepath.Join(home, ".claude2") || acct.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-secret-token" {
		t.Fatalf("env = %v", acct.Env)
	}
	if v, ok := acct.Env["ANTHROPIC_API_KEY"]; !ok || v != "" {
		t.Fatalf("inherited API key not cleared: %v", acct.Env)
	}
	if acct.IsDefault || acct.Binary != "/bin/claude" || acct.Label != "Claude 2" {
		t.Fatalf("account = %+v", acct)
	}
	// List never carries the token.
	e, _ := r.Entry("claude2")
	if _, ok := e.Env["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatal("token leaked into the account list")
	}
	if strings.Contains(logs.String(), "sk-secret-token") {
		t.Fatal("token logged")
	}
	if !strings.Contains(logs.String(), "Bad ID") || !strings.Contains(logs.String(), "unknown agent") || !strings.Contains(logs.String(), "token_file is not supported") {
		t.Fatalf("invalid entries not logged:\n%s", logs.String())
	}
	if _, err := r.Resolve("codex", "claude2"); err == nil {
		t.Fatal("resolved an account for the wrong agent")
	}
	env := EnvList(acct)
	if !slices.IsSorted(env) || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=sk-secret-token") {
		t.Fatalf("EnvList = %v", env)
	}
	merged := AccountEnviron([]string{"ANTHROPIC_API_KEY=parent", "PATH=/bin"}, acct)
	if slices.Contains(merged, "ANTHROPIC_API_KEY=parent") || !slices.Contains(merged, "PATH=/bin") {
		t.Fatalf("AccountEnviron = %v", merged)
	}
	if pe := ProfileEnv(acct); pe["CLAUDE_CONFIG_DIR"] == "" || pe["CLAUDE_CODE_OAUTH_TOKEN"] != "" {
		t.Fatalf("ProfileEnv = %v", pe)
	}
}

func TestAccountsTokenFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	home := isolateHome(t)
	tok := filepath.Join(home, "tok")
	if err := os.WriteFile(tok, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	r := NewAccountRegistry(AccountOptions{
		HomeDir: home, DisableWrappers: true, LookPath: fakeBins("/bin", "claude"),
		Configured: []config.AgentAccountConfig{{ID: "claude2", Agent: "claude", TokenFile: tok}},
		Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	})
	e, ok := r.Entry("claude2")
	if !ok || !strings.Contains(e.Problem, "chmod 600") {
		t.Fatalf("entry = %+v", e)
	}
	if _, err := r.Resolve("claude", "claude2"); err == nil || !strings.Contains(err.Error(), "0644") {
		t.Fatalf("resolve err = %v", err)
	}
	if !strings.Contains(logs.String(), "token file refused") {
		t.Fatalf("refusal not logged:\n%s", logs.String())
	}
	// Fixing the mode and reloading makes the account usable.
	if err := os.Chmod(tok, 0o600); err != nil {
		t.Fatal(err)
	}
	r.Load()
	if acct, err := r.Resolve("claude", "claude2"); err != nil || acct.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "secret" {
		t.Fatalf("after chmod: %+v, %v", acct, err)
	}
}

// claude2Script mirrors the structure of the real ~/.local/bin/claude2 wrapper.
const claude2Script = `#!/usr/bin/env bash
# Launch Claude Code against the second subscription (~/.claude2).
set -euo pipefail

MAIN_CONFIG="${CLAUDE_MAIN_CONFIG_DIR:-$HOME/.claude}"
CONFIG_DIR="${CLAUDE_CONFIG_DIR_OVERRIDE:-$HOME/.claude2}"
CLAUDE_BIN="${CLAUDE_BIN:-$HOME/.local/bin/claude}"
TMP_DIR="${CLAUDE2_CODE_TMPDIR:-/var/tmp/claude2-${UID:-$(id -u)}}"
TOKEN_FILE="${CLAUDE2_OAUTH_TOKEN_FILE:-$HOME/.claude2-oauth-token}"

mkdir -p "$CONFIG_DIR" "$TMP_DIR"
if [[ ! -x "$CLAUDE_BIN" ]]; then
  CLAUDE_BIN="$(command -v claude || true)"
fi
TOKEN="${CLAUDE2_CODE_OAUTH_TOKEN:-}"
if [[ -z "$TOKEN" && -f "$TOKEN_FILE" ]]; then
  TOKEN="$(tr -d '\r' <"$TOKEN_FILE")"
fi
env_args=(
  -u CLAUDE_CODE_OAUTH_TOKEN
  -u ANTHROPIC_API_KEY
  CLAUDE_CONFIG_DIR="$CONFIG_DIR"
  CLAUDE_SECURESTORAGE_CONFIG_DIR="$CONFIG_DIR"
  CLAUDE_CODE_TMPDIR="$TMP_DIR"
)
if [[ -n "$TOKEN" ]]; then
  env_args+=(CLAUDE_CODE_OAUTH_TOKEN="$TOKEN")
fi
exec env "${env_args[@]}" "$CLAUDE_BIN" "$@"
`

func writeScript(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestAccountsWrapperImport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell wrappers are POSIX only")
	}
	home := isolateHome(t)
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	writeScript(t, bin, "claude2", claude2Script, 0o755)
	if err := os.WriteFile(filepath.Join(home, ".claude2-oauth-token"), []byte("tok2"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not imported: computed config dir, no exec, not a script, not executable, bad names.
	writeScript(t, bin, "claude3", "#!/bin/sh\nCLAUDE_CONFIG_DIR=\"$(mktemp -d)\" exec claude \"$@\"\n", 0o755)
	writeScript(t, bin, "claude4", "#!/bin/sh\nCLAUDE_CONFIG_DIR=$HOME/.c4\necho claude\n", 0o755)
	writeScript(t, bin, "claude5", "\x7fELF binary", 0o755)
	writeScript(t, bin, "claude6", "#!/bin/sh\nexport CLAUDE_CONFIG_DIR=~/.c6\nexec claude \"$@\"\n", 0o644)
	writeScript(t, bin, "claude01", "#!/bin/sh\nexport CLAUDE_CONFIG_DIR=~/.c1\nexec claude \"$@\"\n", 0o755)
	// Imported: simple codex wrapper with an inline assignment.
	writeScript(t, bin, "codex2", "#!/bin/zsh\nexec env CODEX_HOME=\"$HOME/.codex2\" /opt/codex/bin/codex \"$@\"\n", 0o755)

	var logs bytes.Buffer
	r := NewAccountRegistry(AccountOptions{
		HomeDir: home, LookPath: fakeBins("/bin", "claude", "codex"),
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	ids := []string{}
	for _, e := range r.List() {
		ids = append(ids, e.ID)
	}
	if !slices.Equal(ids, []string{"claude", "claude2", "codex", "codex2"}) {
		t.Fatalf("ids = %v\n%s", ids, logs.String())
	}
	e, _ := r.Entry("claude2")
	if e.Label != "Claude 2" || e.Source != "wrapper" || e.Binary != "/bin/claude" || e.Problem != "" {
		t.Fatalf("claude2 = %+v", e)
	}
	acct, err := r.Resolve("claude", "claude2")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".claude2")
	if acct.Env["CLAUDE_CONFIG_DIR"] != want || acct.Env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] != want || acct.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "tok2" {
		t.Fatalf("claude2 env = %v", acct.Env)
	}
	if _, ok := acct.Env["CLAUDE_CODE_TMPDIR"]; ok {
		t.Fatal("computed TMPDIR should not be imported")
	}
	c2, _ := r.Entry("codex2")
	if c2.Env["CODEX_HOME"] != filepath.Join(home, ".codex2") || c2.Label != "Codex 2" {
		t.Fatalf("codex2 = %+v", c2)
	}
	if !strings.Contains(logs.String(), "imported agent account from wrapper script") || strings.Contains(logs.String(), "tok2") {
		t.Fatalf("import log:\n%s", logs.String())
	}

	// A YAML account with the same id wins over the wrapper.
	r2 := NewAccountRegistry(AccountOptions{
		HomeDir: home, LookPath: fakeBins("/bin", "claude"),
		Configured: []config.AgentAccountConfig{{ID: "claude2", Agent: "claude", Label: "Mine", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/x"}}},
	})
	if e, _ := r2.Entry("claude2"); e.Label != "Mine" || e.Source != "config" {
		t.Fatalf("config did not win: %+v", e)
	}
}

func TestShellResolve(t *testing.T) {
	vars := map[string]string{"HOME": "/h", "A": "/a"}
	known := map[string]bool{"HOME": true, "A": true}
	cases := map[string]string{
		"$HOME/x":             "/h/x",
		"${HOME}/x":           "/h/x",
		"${OVERRIDE:-$A/y}":   "/a/y",
		"${OVERRIDE-~/z}":     "/h/z",
		"~/w":                 "/h/w",
		"${X:-${HOME}/.c2}":   "/h/.c2",
		"/literal/path":       "/literal/path",
		"${HOME:-/ignored}/q": "/h/q",
	}
	for in, want := range cases {
		if got, ok := resolveShell(in, vars, known, "/h"); !ok || got != want {
			t.Errorf("resolveShell(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"$UNKNOWN/x", "${UNKNOWN}", "~other/x", "${a[@]}"} {
		if got, ok := resolveShell(bad, vars, known, "/h"); ok {
			t.Errorf("resolveShell(%q) = %q, want failure", bad, got)
		}
	}
	for _, bad := range []string{`"$(id -u)"`, "`id`", `"unterminated`, `x y`} {
		if _, ok := shellWord(bad); ok {
			t.Errorf("shellWord(%q) accepted", bad)
		}
	}
}

func TestAccountsMock(t *testing.T) {
	r := mockAccounts(t)
	ids := []string{}
	for _, e := range r.List() {
		ids = append(ids, e.ID)
	}
	if !slices.Equal(ids, []string{"mock", "mock2"}) {
		t.Fatalf("ids = %v", ids)
	}
	if a, err := r.Resolve(MockAgent, ""); err != nil || a.ID != "mock" {
		t.Fatalf("default mock = %+v, %v", a, err)
	}
}

// TestLiveWrapperImport reports what the importer finds on this machine (ids and labels
// only). Run with KONSERT_LIVE_ACCOUNTS=1.
func TestLiveWrapperImport(t *testing.T) {
	if os.Getenv("KONSERT_LIVE_ACCOUNTS") != "1" {
		t.Skip("set KONSERT_LIVE_ACCOUNTS=1")
	}
	r := NewAccountRegistry(AccountOptions{})
	for _, e := range r.List() {
		t.Logf("account id=%s label=%q agent=%s source=%s default=%v problem=%q note=%q", e.ID, e.Label, e.Agent, e.Source, e.IsDefault, e.Problem, e.Note)
	}
}
