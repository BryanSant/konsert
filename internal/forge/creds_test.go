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

	t.Run("encrypted store wins over git credential and CLI login", func(t *testing.T) {
		runner := &fakeCmdRunner{
			fn: func(name string, _ []string, _ []string, _ string) (string, error) {
				t.Errorf("runner should not be called when stored token exists, got %s", name)
				return "password=tok-git\n", nil
			},
		}
		disc := &CredDiscovery{Runner: runner, Store: st}

		cred, err := disc.Discover(ctx, "github.com", GitHub)
		if err != nil {
			t.Fatalf("Discover github: %v", err)
		}
		if cred.Token != "tok-stored" || cred.Source != "pasted" {
			t.Fatalf("github got %+v", cred)
		}

		cred, err = disc.Discover(ctx, "gitlab.com", GitLab)
		if err != nil {
			t.Fatalf("Discover gitlab: %v", err)
		}
		if cred.Token != "tok-gl-stored" || cred.Source != "device" {
			t.Fatalf("gitlab got %+v", cred)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("expected 0 runner calls, got %d", len(runner.calls))
		}
	})

	emptyStore := NewEncryptedSettingsStore(&memSettings{}, newTestBox(t))

	t.Run("git credential wins when no stored token exists", func(t *testing.T) {
		runner := &fakeCmdRunner{
			fn: func(name string, _ []string, _ []string, _ string) (string, error) {
				if name == "git" {
					return "protocol=https\nhost=github.com\nusername=octocat\npassword=tok-git\n", nil
				}
				return "", errors.New("unexpected call")
			},
		}
		disc := &CredDiscovery{Runner: runner, Store: emptyStore}
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

	t.Run("falls back to gh for GitHub when store and git credential miss", func(t *testing.T) {
		runner := &fakeCmdRunner{
			fn: func(name string, args []string, _ []string, _ string) (string, error) {
				if name == "gh" && len(args) == 4 && args[3] == "github.com" {
					return "tok-gh\n", nil
				}
				return "", errors.New("no cred")
			},
		}
		disc := &CredDiscovery{Runner: runner, Store: emptyStore}
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
		disc := &CredDiscovery{Runner: runner, Store: emptyStore}
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
		disc := &CredDiscovery{Runner: runner, Store: emptyStore, GlabConfigPath: cfgPath}
		cred, err := disc.Discover(ctx, "gitlab.com", GitLab)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if cred.Token != "tok-glab-file" || cred.Source != "glab" {
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

func TestCredDiscoveryNonInteractiveRunnerArgsAndEnv(t *testing.T) {
	ctx := context.Background()

	t.Run("git and gh calls pass non-interactive args, env, and explicit stdin", func(t *testing.T) {
		runner := &fakeCmdRunner{}
		disc := &CredDiscovery{Runner: runner}
		_, err := disc.Discover(ctx, "github.com", GitHub)
		if !errors.Is(err, ErrNoCredential) {
			t.Fatalf("err = %v, want ErrNoCredential", err)
		}
		if len(runner.calls) != 2 {
			t.Fatalf("expected 2 runner calls (git, gh), got %d: %+v", len(runner.calls), runner.calls)
		}

		gitCall := runner.calls[0]
		if gitCall.name != "git" {
			t.Fatalf("call[0].name = %q, want git", gitCall.name)
		}
		wantGitArgs := []string{"-c", "credential.interactive=false", "credential", "fill"}
		if !slices.Equal(gitCall.args, wantGitArgs) {
			t.Errorf("git args = %v, want %v", gitCall.args, wantGitArgs)
		}
		for _, wantEnv := range []string{
			"GIT_TERMINAL_PROMPT=0",
			"GCM_INTERACTIVE=never",
			"GIT_ASKPASS=",
			"SSH_ASKPASS=",
		} {
			if !slices.Contains(gitCall.env, wantEnv) {
				t.Errorf("git env %v missing %q", gitCall.env, wantEnv)
			}
		}
		wantGitStdin := "protocol=https\nhost=github.com\n\n"
		if gitCall.stdin != wantGitStdin {
			t.Errorf("git stdin = %q, want %q", gitCall.stdin, wantGitStdin)
		}

		ghCall := runner.calls[1]
		if ghCall.name != "gh" {
			t.Fatalf("call[1].name = %q, want gh", ghCall.name)
		}
		wantGHArgs := []string{"auth", "token", "--hostname", "github.com"}
		if !slices.Equal(ghCall.args, wantGHArgs) {
			t.Errorf("gh args = %v, want %v", ghCall.args, wantGHArgs)
		}
		if !slices.Contains(ghCall.env, "GH_PROMPT_DISABLED=1") {
			t.Errorf("gh env %v missing GH_PROMPT_DISABLED=1", ghCall.env)
		}
		if ghCall.stdin != "" {
			t.Errorf("gh stdin = %q, want empty", ghCall.stdin)
		}
	})

	t.Run("glab call passes NO_PROMPT=1 and empty stdin", func(t *testing.T) {
		runner := &fakeCmdRunner{}
		disc := &CredDiscovery{Runner: runner, GlabConfigPath: filepath.Join(t.TempDir(), "missing.yml")}
		_, err := disc.Discover(ctx, "gitlab.com", GitLab)
		if !errors.Is(err, ErrNoCredential) {
			t.Fatalf("err = %v, want ErrNoCredential", err)
		}
		if len(runner.calls) != 2 {
			t.Fatalf("expected 2 runner calls (git, glab), got %d: %+v", len(runner.calls), runner.calls)
		}

		glabCall := runner.calls[1]
		if glabCall.name != "glab" {
			t.Fatalf("call[1].name = %q, want glab", glabCall.name)
		}
		wantGlabArgs := []string{"config", "get", "token", "--host", "gitlab.com"}
		if !slices.Equal(glabCall.args, wantGlabArgs) {
			t.Errorf("glab args = %v, want %v", glabCall.args, wantGlabArgs)
		}
		if !slices.Contains(glabCall.env, "NO_PROMPT=1") {
			t.Errorf("glab env %v missing NO_PROMPT=1", glabCall.env)
		}
		if glabCall.stdin != "" {
			t.Errorf("glab stdin = %q, want empty", glabCall.stdin)
		}
	})
}
