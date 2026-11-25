package forge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/secrets"
)

type fakeCmdCall struct {
	name  string
	args  []string
	env   []string
	stdin string
}

type fakeCmdRunner struct {
	calls []fakeCmdCall
	fn    func(name string, args []string, env []string, stdin string) (string, error)
}

func (f *fakeCmdRunner) Run(_ context.Context, name string, args []string, env []string, stdin string) (string, error) {
	f.calls = append(f.calls, fakeCmdCall{name: name, args: args, env: env, stdin: stdin})
	if f.fn != nil {
		return f.fn(name, args, env, stdin)
	}
	return "", errors.New("not found")
}

type memSettings struct {
	data map[string]string
}

func (m *memSettings) GetAllSettings(_ context.Context) (map[string]string, error) {
	out := make(map[string]string, len(m.data))
	for k, v := range m.data {
		out[k] = v
	}
	return out, nil
}

func (m *memSettings) SetSettings(_ context.Context, kv map[string]string) error {
	if m.data == nil {
		m.data = make(map[string]string)
	}
	for k, v := range kv {
		m.data[k] = v
	}
	return nil
}

func newTestBox(t *testing.T) *secrets.Box {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func TestEncryptedSettingsStore(t *testing.T) {
	ctx := context.Background()
	rawSettings := &memSettings{}
	st := NewEncryptedSettingsStore(rawSettings, newTestBox(t))

	secretTok := "ghp_super_secret_12345"
	if err := st.PutToken(ctx, StoredToken{
		Host:   "GitHub.com",
		Token:  secretTok,
		Source: "pasted",
		Kind:   GitHub,
	}); err != nil {
		t.Fatalf("PutToken: %v", err)
	}

	// Verify encrypted at rest and never plaintext.
	raw := rawSettings.data["forge_token:github.com"]
	if !secrets.IsSealed(raw) {
		t.Fatalf("stored value is not sealed: %q", raw)
	}
	if strings.Contains(raw, secretTok) {
		t.Fatalf("stored value leaks plaintext token")
	}

	got, ok, err := st.GetToken(ctx, "github.com")
	if err != nil || !ok {
		t.Fatalf("GetToken: ok=%v err=%v", ok, err)
	}
	if got.Token != secretTok || got.Source != "pasted" || got.Kind != GitHub {
		t.Fatalf("got %+v", got)
	}

	list, err := st.ListTokens(ctx)
	if err != nil || len(list) != 1 || list[0].Host != "github.com" {
		t.Fatalf("ListTokens: %+v err=%v", list, err)
	}

	if err := st.DeleteToken(ctx, "github.com"); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	if _, ok, err := st.GetToken(ctx, "github.com"); err != nil || ok {
		t.Fatalf("expected deleted token to be absent: ok=%v err=%v", ok, err)
	}
	if list, err := st.ListTokens(ctx); err != nil || len(list) != 0 {
		t.Fatalf("expected empty list after delete, got %+v err=%v", list, err)
	}
}

func TestCredDiscoveryOrder(t *testing.T) {
	ctx := context.Background()
	rawSettings := &memSettings{}
	st := NewEncryptedSettingsStore(rawSettings, newTestBox(t))
	_ = st.PutToken(ctx, StoredToken{Host: "github.com", Token: "tok-stored", Source: "pasted", Kind: GitHub})
	_ = st.PutToken(ctx, StoredToken{Host: "gitlab.com", Token: "tok-gl-stored", Source: "device", Kind: GitLab})

	t.Run("git credential wins first and sets GIT_TERMINAL_PROMPT=0", func(t *testing.T) {
		runner := &fakeCmdRunner{
			fn: func(name string, args []string, env []string, stdin string) (string, error) {
				if name == "git" {
					if !slices.Contains(env, "GIT_TERMINAL_PROMPT=0") {
						t.Errorf("missing GIT_TERMINAL_PROMPT=0 in env: %v", env)
					}
					if !strings.Contains(stdin, "protocol=https\nhost=github.com\n") {
						t.Errorf("unexpected stdin: %q", stdin)
					}
					return "protocol=https\nhost=github.com\nusername=octocat\npassword=tok-git\n", nil
				}
				return "", errors.New("unexpected call")
			},
		}
		disc := &CredDiscovery{Runner: runner, Store: st}
		cred, err := disc.Discover(ctx, "github.com", GitHub)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if cred.Token != "tok-git" || cred.Source != "git-credential" {
			t.Fatalf("got %+v", cred)
		}
		if len(runner.calls) != 1 {
			t.Fatalf("expected 1 command call, got %d", len(runner.calls))
		}
	})

	t.Run("falls back to gh for GitHub", func(t *testing.T) {
		runner := &fakeCmdRunner{
			fn: func(name string, args []string, _ []string, _ string) (string, error) {
				if name == "gh" && len(args) == 4 && args[3] == "github.com" {
					return "tok-gh\n", nil
				}
				return "", errors.New("no cred")
			},
		}
		disc := &CredDiscovery{Runner: runner, Store: st}
		cred, err := disc.Discover(ctx, "github.com", GitHub)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if cred.Token != "tok-gh" || cred.Source != "gh" {
			t.Fatalf("got %+v", cred)
		}
	})

	t.Run("falls back to glab CLI for GitLab", func(t *testing.T) {
		runner := &fakeCmdRunner{
			fn: func(name string, args []string, _ []string, _ string) (string, error) {
				if name == "glab" && slices.Contains(args, "gitlab.com") {
					return "tok-glab-cli\n", nil
				}
				return "", errors.New("no cred")
			},
		}
		disc := &CredDiscovery{Runner: runner, Store: st}
		cred, err := disc.Discover(ctx, "gitlab.com", GitLab)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if cred.Token != "tok-glab-cli" || cred.Source != "glab" {
			t.Fatalf("got %+v", cred)
		}
	})

	t.Run("falls back to glab config.yml when glab CLI missing", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "config.yml")
		if err := os.WriteFile(cfgPath, []byte("hosts:\n  gitlab.com:\n    token: tok-glab-file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runner := &fakeCmdRunner{}
		disc := &CredDiscovery{Runner: runner, Store: st, GlabConfigPath: cfgPath}
		cred, err := disc.Discover(ctx, "gitlab.com", GitLab)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if cred.Token != "tok-glab-file" || cred.Source != "glab" {
			t.Fatalf("got %+v", cred)
		}
	})

	t.Run("falls back to encrypted store", func(t *testing.T) {
		runner := &fakeCmdRunner{}
		disc := &CredDiscovery{Runner: runner, Store: st, GlabConfigPath: filepath.Join(t.TempDir(), "missing.yml")}
		cred, err := disc.Discover(ctx, "gitlab.com", GitLab)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if cred.Token != "tok-gl-stored" || cred.Source != "device" {
			t.Fatalf("got %+v", cred)
		}
	})

	t.Run("returns ErrNoCredential when nothing matches", func(t *testing.T) {
		runner := &fakeCmdRunner{}
		disc := &CredDiscovery{Runner: runner, Store: st}
		_, err := disc.Discover(ctx, "codeberg.org", Forgejo)
		if !errors.Is(err, ErrNoCredential) {
			t.Fatalf("err = %v, want ErrNoCredential", err)
		}
	})
}
