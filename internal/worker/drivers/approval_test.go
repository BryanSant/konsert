package drivers

import (
	"slices"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
)

// flagsBeforePrompt returns argv up to the prompt terminator, minus session-id flags
// (random per task).
func flagsBeforePrompt(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || a == "-p" || strings.HasPrefix(a, "--single=") || strings.HasPrefix(a, "--print") || strings.HasPrefix(a, "--prompt=") {
			break
		}
		if a == "--session-id" {
			i++
			continue
		}
		if strings.HasPrefix(a, "--session-id=") || strings.HasPrefix(a, "--resume=") {
			continue
		}
		if a == "exec" {
			continue
		}
		out = append(out, a)
	}
	return out
}

func TestLaunchOptionsApprovalAndEffortArgv(t *testing.T) {
	full, sandboxed := protocol.PermissionFullAccess, protocol.PermissionSandboxed
	cases := []struct {
		name string
		drv  AgentDriver
		opts LaunchOptions
		want []string
	}{
		// claude
		{"claude mode+effort", &ClaudeDriver{}, LaunchOptions{ApprovalMode: "acceptEdits", Effort: "high", PermissionMode: full}, []string{"--permission-mode", "acceptEdits", "--effort", "high"}},
		{"claude fallback full", &ClaudeDriver{}, LaunchOptions{PermissionMode: full}, []string{"--dangerously-skip-permissions"}},
		{"claude unknown mode falls back", &ClaudeDriver{}, LaunchOptions{ApprovalMode: "weird; rm", PermissionMode: full}, []string{"--dangerously-skip-permissions"}},
		// grok
		{"grok mode full", &GrokDriver{}, LaunchOptions{ApprovalMode: "plan", Effort: "low", PermissionMode: full}, []string{"--permission-mode", "plan", "--trust", "--reasoning-effort", "low"}},
		{"grok mode sandboxed", &GrokDriver{}, LaunchOptions{ApprovalMode: "default", PermissionMode: sandboxed}, []string{"--permission-mode", "default", "--sandbox", "workspace"}},
		{"grok fallback full", &GrokDriver{}, LaunchOptions{PermissionMode: full}, []string{"--always-approve", "--trust"}},
		// codex
		{"codex read-only exec", &CodexDriver{}, LaunchOptions{ApprovalMode: "read-only", Effort: "xhigh", PermissionMode: full}, []string{"-c", `approval_policy="on-request"`, "--sandbox", "read-only", "-c", `model_reasoning_effort="xhigh"`}},
		{"codex auto interactive", &CodexDriver{}, LaunchOptions{ApprovalMode: "auto", Mode: protocol.TaskModeInteractive, PermissionMode: sandboxed}, []string{"--ask-for-approval", "on-request", "--sandbox", "workspace-write"}},
		{"codex full-access", &CodexDriver{}, LaunchOptions{ApprovalMode: "full-access", PermissionMode: full}, []string{"--dangerously-bypass-approvals-and-sandbox"}},
		{"codex fallback sandboxed", &CodexDriver{}, LaunchOptions{PermissionMode: sandboxed}, []string{"--sandbox", "workspace-write"}},
		// agy
		{"agy mode sandboxed", &AgyDriver{}, LaunchOptions{ApprovalMode: "accept-edits", Effort: "max", PermissionMode: sandboxed}, []string{"--mode=accept-edits", "--sandbox", "--effort=max"}},
		{"agy default full", &AgyDriver{}, LaunchOptions{ApprovalMode: "default", PermissionMode: full}, nil},
		{"agy fallback full", &AgyDriver{}, LaunchOptions{PermissionMode: full}, []string{"--dangerously-skip-permissions"}},
		// cursor (effort lives in the model id)
		{"cursor plan", &CursorDriver{}, LaunchOptions{ApprovalMode: "plan", Effort: "high", PermissionMode: full}, []string{"--mode=plan", "--trust"}},
		{"cursor force", &CursorDriver{}, LaunchOptions{ApprovalMode: "force", PermissionMode: full}, []string{"--force", "--trust"}},
		{"cursor ask sandboxed", &CursorDriver{}, LaunchOptions{ApprovalMode: "ask", PermissionMode: sandboxed}, []string{"--mode=ask", "--sandbox", "enabled", "--trust"}},
		// copilot
		{"copilot plan", &CopilotDriver{}, LaunchOptions{ApprovalMode: "plan", Effort: "medium", PermissionMode: full}, []string{"--mode=plan", "--reasoning-effort=medium"}},
		{"copilot autopilot sandboxed", &CopilotDriver{}, LaunchOptions{ApprovalMode: "autopilot", PermissionMode: sandboxed}, []string{"--mode=autopilot", "--sandbox"}},
		{"copilot allow-all", &CopilotDriver{}, LaunchOptions{ApprovalMode: "allow-all", PermissionMode: full}, []string{"--allow-all"}},
	}
	for _, c := range cases {
		c.opts.TaskID = "t-" + strings.ReplaceAll(c.name, " ", "-")
		c.opts.Prompt = "hello"
		if c.opts.Mode == "" {
			c.opts.Mode = protocol.TaskModeOneshot
		}
		spec := c.drv.BuildCommand(c.opts)
		if got := flagsBeforePrompt(spec.Args); !slices.Equal(got, c.want) {
			t.Errorf("%s: flags = %q, want %q (argv %q)", c.name, got, c.want, spec.Args)
		}
	}
}

func TestLaunchOptionsAccountEnvPassedThrough(t *testing.T) {
	env := []string{"CLAUDE_CONFIG_DIR=/c2", "GROK_HOME=/g2"}
	for _, d := range []AgentDriver{&ClaudeDriver{}, &CodexDriver{}, &GrokDriver{}, &AgyDriver{}, &CursorDriver{}, &CopilotDriver{}} {
		spec := d.BuildCommand(LaunchOptions{TaskID: "t-env-" + d.Name(), Prompt: "x", Mode: protocol.TaskModeOneshot, Env: env, AccountID: "acct2"})
		for _, kv := range env {
			if !slices.Contains(spec.Env, kv) {
				t.Errorf("%s: account env %q missing from %q", d.Name(), kv, spec.Env)
			}
		}
	}
}

func TestApprovalModeFullAccess(t *testing.T) {
	yes := map[string]string{"claude": "bypassPermissions", "grok": "bypassPermissions", "codex": "danger-full-access", "agy": "yolo", "cursor": "force", "copilot": "allow-all", "x": "full-access"}
	for agent, mode := range yes {
		if !ApprovalModeFullAccess(agent, mode) {
			t.Errorf("%s/%s should be full access", agent, mode)
		}
	}
	for _, c := range [][2]string{{"claude", "acceptEdits"}, {"claude", ""}, {"codex", "auto"}, {"copilot", "autopilot"}, {"cursor", "plan"}} {
		if ApprovalModeFullAccess(c[0], c[1]) {
			t.Errorf("%s/%s should not be full access", c[0], c[1])
		}
	}
}
