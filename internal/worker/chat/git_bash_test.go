package chat

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/protocol"
)

func withFakeGitBashEnv(t *testing.T, bashPath string) {
	t.Helper()
	orig := gitBashAgentEnv
	gitBashAgentEnv = func(agent string, enabled bool) map[string]string {
		if !enabled {
			return nil
		}
		switch agent {
		case "claude":
			return map[string]string{"CLAUDE_CODE_GIT_BASH_PATH": bashPath}
		case "grok":
			return map[string]string{"GROK_SHELL": "bash"}
		default:
			return nil
		}
	}
	t.Cleanup(func() { gitBashAgentEnv = orig })
}

func envHas(env []string, key, want string) bool {
	return slices.Contains(env, key+"="+want)
}

func envHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

func TestClaudeChatAppliesGitBashEnv(t *testing.T) {
	const gitBash = `C:\Program Files\Git\bin\bash.exe`
	withFakeGitBashEnv(t, gitBash)

	t.Run("enabled sets CLAUDE_CODE_GIT_BASH_PATH", func(t *testing.T) {
		e, l := newTestClaude(t, fakeClaudeOpts{})
		rec := newCCRecorder()
		s, err := e.Open(context.Background(), Account{ID: "claude", Agent: "claude"}, protocol.AgentChatOpen{
			ChatID:     "c-gb-on",
			Agent:      "claude",
			Cwd:        t.TempDir(),
			UseGitBash: true,
		}, rec.emit)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if l.count() != 1 {
			t.Fatalf("launched %d procs, want 1", l.count())
		}
		if !envHas(l.last().spec.Env, "CLAUDE_CODE_GIT_BASH_PATH", gitBash) {
			t.Fatalf("missing CLAUDE_CODE_GIT_BASH_PATH=%q in %v", gitBash, l.last().spec.Env)
		}
	})

	t.Run("disabled omits CLAUDE_CODE_GIT_BASH_PATH", func(t *testing.T) {
		e, l := newTestClaude(t, fakeClaudeOpts{})
		rec := newCCRecorder()
		s, err := e.Open(context.Background(), Account{ID: "claude", Agent: "claude"}, protocol.AgentChatOpen{
			ChatID:     "c-gb-off",
			Agent:      "claude",
			Cwd:        t.TempDir(),
			UseGitBash: false,
		}, rec.emit)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if l.count() != 1 {
			t.Fatalf("launched %d procs, want 1", l.count())
		}
		if envHasKey(l.last().spec.Env, "CLAUDE_CODE_GIT_BASH_PATH") {
			t.Fatalf("unexpected CLAUDE_CODE_GIT_BASH_PATH in %v", l.last().spec.Env)
		}
	})
}

func TestACPChatAppliesGitBashEnv(t *testing.T) {
	const gitBash = `C:\Program Files\Git\bin\bash.exe`
	withFakeGitBashEnv(t, gitBash)

	t.Run("grok sets GROK_SHELL=bash when enabled", func(t *testing.T) {
		s, _, _ := openFake(t, fakeOpts{agent: "grok", fixture: "grok", scenario: "basic"}, protocol.AgentChatOpen{
			UseGitBash: true,
		})
		if !envHas(s.conn.proc.Cmd.Env, "GROK_SHELL", "bash") {
			t.Fatalf("missing GROK_SHELL=bash in spawned env")
		}
	})

	t.Run("grok omits GROK_SHELL when disabled", func(t *testing.T) {
		s, _, _ := openFake(t, fakeOpts{agent: "grok", fixture: "grok", scenario: "basic"}, protocol.AgentChatOpen{
			UseGitBash: false,
		})
		if envHasKey(s.conn.proc.Cmd.Env, "GROK_SHELL") {
			t.Fatalf("unexpected GROK_SHELL in spawned env")
		}
	})

	t.Run("copilot gets no Git Bash env", func(t *testing.T) {
		s, _, _ := openFake(t, fakeOpts{agent: "copilot", fixture: "copilot", scenario: "basic"}, protocol.AgentChatOpen{
			UseGitBash: true,
		})
		for _, k := range []string{"CLAUDE_CODE_GIT_BASH_PATH", "GROK_SHELL", "COPILOT_BASH_PATH"} {
			if envHasKey(s.conn.proc.Cmd.Env, k) {
				t.Fatalf("unexpected %s in copilot spawned env", k)
			}
		}
	})

	t.Run("unsupported ACP agent (agy) gets no Git Bash env", func(t *testing.T) {
		s, _, _ := openFake(t, fakeOpts{agent: "agy", fixture: "agy", scenario: "basic"}, protocol.AgentChatOpen{
			UseGitBash: true,
		})
		for _, k := range []string{"CLAUDE_CODE_GIT_BASH_PATH", "GROK_SHELL", "COPILOT_BASH_PATH"} {
			if envHasKey(s.conn.proc.Cmd.Env, k) {
				t.Fatalf("unexpected %s in agy spawned env", k)
			}
		}
	})
}
