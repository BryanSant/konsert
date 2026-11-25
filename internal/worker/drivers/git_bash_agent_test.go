package drivers

import (
	"slices"
	"strings"
	"testing"
)

func fakeWindowsWithGitBash(bashPath string) shellLookup {
	return shellLookup{
		goos: "windows",
		getenv: func(k string) string {
			if k == "ProgramFiles" {
				return `C:\Program Files`
			}
			return ""
		},
		statFile: func(p string) bool { return p == bashPath },
	}
}

func fakeWindowsWithoutGitBash() shellLookup {
	return shellLookup{
		goos:     "windows",
		getenv:   func(string) string { return "" },
		statFile: func(string) bool { return false },
	}
}

func fakeDarwinLookup() shellLookup {
	return shellLookup{
		goos:     "darwin",
		getenv:   func(string) string { return "" },
		statFile: func(string) bool { return true },
	}
}

func envLookup(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix), true
		}
	}
	return "", false
}

func TestGitBashAgentEnv(t *testing.T) {
	const gitBash = `C:\Program Files\Git\bin\bash.exe`
	winWith := fakeWindowsWithGitBash(gitBash)
	winWithout := fakeWindowsWithoutGitBash()
	mac := fakeDarwinLookup()

	if got := gitBashAgentEnv(winWith, "claude", true); got["CLAUDE_CODE_GIT_BASH_PATH"] != gitBash || len(got) != 1 {
		t.Fatalf("claude env = %v, want CLAUDE_CODE_GIT_BASH_PATH=%q", got, gitBash)
	}
	if got := gitBashAgentEnv(winWith, "grok", true); got["GROK_SHELL"] != "bash" || len(got) != 1 {
		t.Fatalf("grok env = %v, want GROK_SHELL=bash", got)
	}
	for _, unsupported := range []string{"copilot", "codex", "cursor", "agy", "shell", ""} {
		if got := gitBashAgentEnv(winWith, unsupported, true); len(got) != 0 {
			t.Fatalf("%q should return empty env, got %v", unsupported, got)
		}
	}
	if got := gitBashAgentEnv(winWith, "claude", false); len(got) != 0 {
		t.Fatalf("disabled should return empty env, got %v", got)
	}
	if got := gitBashAgentEnv(winWithout, "claude", true); len(got) != 0 {
		t.Fatalf("missing Git Bash should return empty env, got %v", got)
	}
	if got := gitBashAgentEnv(mac, "claude", true); len(got) != 0 {
		t.Fatalf("darwin should return empty env, got %v", got)
	}
}

func TestAgentDriversGitBashOnWindows(t *testing.T) {
	const gitBash = `C:\Program Files\Git\bin\bash.exe`
	winWith := fakeWindowsWithGitBash(gitBash)
	winWithout := fakeWindowsWithoutGitBash()
	mac := fakeDarwinLookup()

	t.Run("claude sets CLAUDE_CODE_GIT_BASH_PATH on Windows when enabled", func(t *testing.T) {
		d := &ClaudeDriver{}
		baseEnv := []string{"CLAUDE_CONFIG_DIR=/tmp/c1", "CLAUDE_CODE_GIT_BASH_PATH=old"}
		spec := d.buildCommand(winWith, LaunchOptions{
			TaskID:     "t-claude-1",
			Prompt:     "hi",
			Mode:       "oneshot",
			Env:        baseEnv,
			UseGitBash: true,
		})
		got, ok := envLookup(spec.Env, "CLAUDE_CODE_GIT_BASH_PATH")
		if !ok || got != gitBash {
			t.Fatalf("CLAUDE_CODE_GIT_BASH_PATH = %q (ok=%v), want %q", got, ok, gitBash)
		}
		if cfg, ok := envLookup(spec.Env, "CLAUDE_CONFIG_DIR"); !ok || cfg != "/tmp/c1" {
			t.Fatalf("lost existing env: %v", spec.Env)
		}
		// Must not mutate caller's slice.
		if !slices.Equal(baseEnv, []string{"CLAUDE_CONFIG_DIR=/tmp/c1", "CLAUDE_CODE_GIT_BASH_PATH=old"}) {
			t.Fatalf("caller Env mutated: %v", baseEnv)
		}
		// Exactly one CLAUDE_CODE_GIT_BASH_PATH entry.
		count := 0
		for _, kv := range spec.Env {
			if strings.HasPrefix(kv, "CLAUDE_CODE_GIT_BASH_PATH=") {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("expected 1 CLAUDE_CODE_GIT_BASH_PATH entry, got %d in %v", count, spec.Env)
		}
	})

	t.Run("grok sets GROK_SHELL=bash on Windows when enabled", func(t *testing.T) {
		d := &GrokDriver{}
		spec := d.buildCommand(winWith, LaunchOptions{
			TaskID:     "t-grok-1",
			Prompt:     "hi",
			Mode:       "oneshot",
			Env:        []string{"GROK_HOME=/tmp/g1"},
			UseGitBash: true,
		})
		got, ok := envLookup(spec.Env, "GROK_SHELL")
		if !ok || got != "bash" {
			t.Fatalf("GROK_SHELL = %q (ok=%v), want bash", got, ok)
		}
		if home, ok := envLookup(spec.Env, "GROK_HOME"); !ok || home != "/tmp/g1" {
			t.Fatalf("lost existing env: %v", spec.Env)
		}
	})

	t.Run("copilot never sets Git Bash env", func(t *testing.T) {
		d := &CopilotDriver{}
		spec := d.BuildCommand(LaunchOptions{
			TaskID:     "t-copilot-1",
			Prompt:     "hi",
			Mode:       "oneshot",
			Env:        []string{"COPILOT_HOME=/tmp/cp1"},
			UseGitBash: true,
		})
		for _, k := range []string{"COPILOT_BASH_PATH", "CLAUDE_CODE_GIT_BASH_PATH", "GROK_SHELL"} {
			if _, ok := envLookup(spec.Env, k); ok {
				t.Fatalf("copilot should not set %s: %v", k, spec.Env)
			}
		}
	})

	t.Run("disabled option skips Git Bash env for all supported drivers", func(t *testing.T) {
		opts := LaunchOptions{TaskID: "t-off", Prompt: "hi", Mode: "oneshot", UseGitBash: false}
		if _, ok := envLookup((&ClaudeDriver{}).buildCommand(winWith, opts).Env, "CLAUDE_CODE_GIT_BASH_PATH"); ok {
			t.Fatal("claude should not set CLAUDE_CODE_GIT_BASH_PATH when UseGitBash=false")
		}
		if _, ok := envLookup((&GrokDriver{}).buildCommand(winWith, opts).Env, "GROK_SHELL"); ok {
			t.Fatal("grok should not set GROK_SHELL when UseGitBash=false")
		}
	})

	t.Run("missing Git Bash skips env on Windows", func(t *testing.T) {
		opts := LaunchOptions{TaskID: "t-missing", Prompt: "hi", Mode: "oneshot", UseGitBash: true}
		if _, ok := envLookup((&ClaudeDriver{}).buildCommand(winWithout, opts).Env, "CLAUDE_CODE_GIT_BASH_PATH"); ok {
			t.Fatal("claude should not set CLAUDE_CODE_GIT_BASH_PATH when Git Bash is absent")
		}
		if _, ok := envLookup((&GrokDriver{}).buildCommand(winWithout, opts).Env, "GROK_SHELL"); ok {
			t.Fatal("grok should not set GROK_SHELL when Git Bash is absent")
		}
	})

	t.Run("non-Windows skips Git Bash env even when UseGitBash=true", func(t *testing.T) {
		opts := LaunchOptions{TaskID: "t-mac", Prompt: "hi", Mode: "oneshot", UseGitBash: true}
		if _, ok := envLookup((&ClaudeDriver{}).buildCommand(mac, opts).Env, "CLAUDE_CODE_GIT_BASH_PATH"); ok {
			t.Fatal("claude should not set CLAUDE_CODE_GIT_BASH_PATH on darwin")
		}
		if _, ok := envLookup((&GrokDriver{}).buildCommand(mac, opts).Env, "GROK_SHELL"); ok {
			t.Fatal("grok should not set GROK_SHELL on darwin")
		}
	})
}
