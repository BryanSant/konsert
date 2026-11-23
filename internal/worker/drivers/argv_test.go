package drivers

import (
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
)

// promptIsInert reports whether a prompt can only be seen by the CLI as a prompt:
// either it follows a "--" terminator, or it is "="-joined to a long flag.
func promptIsInert(args []string, prompt string) bool {
	for i, a := range args {
		if a == prompt {
			return slices.Contains(args[:i], "--")
		}
		if strings.HasPrefix(a, "--") && strings.HasSuffix(a, "="+prompt) {
			return true
		}
	}
	return false
}

func TestDriverArgvHostilePrompt(t *testing.T) {
	const hostile = "--dangerously-skip-permissions"
	escalation := []string{hostile, "--yes", "--always-approve", "--dangerously-bypass-approvals-and-sandbox"}
	drvs := []AgentDriver{&ClaudeDriver{}, &GrokDriver{}, &CodexDriver{}, &AgyDriver{}}
	for _, d := range drvs {
		for _, mode := range []string{protocol.TaskModeOneshot, protocol.TaskModeInteractive} {
			spec := d.BuildCommand(LaunchOptions{Prompt: hostile, Mode: mode, PermissionMode: protocol.PermissionSandboxed})
			if !promptIsInert(spec.Args, hostile) {
				t.Errorf("%s/%s: hostile prompt is not separated from flags: %q", d.Name(), mode, spec.Args)
			}
			// No escalation flag may appear in option position on a sandboxed task.
			for i, a := range spec.Args {
				if a == "--" {
					break
				}
				if slices.Contains(escalation, a) {
					t.Errorf("%s/%s: escalation flag %q at argv[%d]: %q", d.Name(), mode, a, i, spec.Args)
				}
			}
		}
	}
}

func TestDriverArgvPromptStartingWithDash(t *testing.T) {
	const prompt = "-h please explain"
	cases := map[string]AgentDriver{"claude": &ClaudeDriver{}, "grok": &GrokDriver{}, "codex": &CodexDriver{}, "agy": &AgyDriver{}}
	for name, d := range cases {
		for _, mode := range []string{protocol.TaskModeOneshot, protocol.TaskModeInteractive} {
			spec := d.BuildCommand(LaunchOptions{Prompt: prompt, Mode: mode, PermissionMode: protocol.PermissionFullAccess})
			if !promptIsInert(spec.Args, prompt) {
				t.Errorf("%s/%s: dash prompt not protected: %q", name, mode, spec.Args)
			}
		}
	}
	if runtime.GOOS != "windows" {
		const script = "-x; echo hi"
		spec := (&ShellDriver{}).BuildCommand(LaunchOptions{Prompt: script, Mode: protocol.TaskModeOneshot})
		if !promptIsInert(spec.Args, script) {
			t.Errorf("shell: dash prompt not protected: %q", spec.Args)
		}
	}
}

func TestClaudeAndCodexArgvExact(t *testing.T) {
	claude := (&ClaudeDriver{}).BuildCommand(LaunchOptions{Prompt: "-p", Mode: protocol.TaskModeOneshot, PermissionMode: protocol.PermissionSandboxed})
	if want := []string{"-p", "--", "-p"}; !slices.Equal(claude.Args, want) {
		t.Fatalf("claude argv = %q, want %q", claude.Args, want)
	}
	codex := (&CodexDriver{}).BuildCommand(LaunchOptions{Prompt: "--help", Mode: protocol.TaskModeOneshot, PermissionMode: protocol.PermissionSandboxed})
	if want := []string{"exec", "--sandbox", "workspace-write", "--", "--help"}; !slices.Equal(codex.Args, want) {
		t.Fatalf("codex argv = %q, want %q", codex.Args, want)
	}
	grok := (&GrokDriver{}).BuildCommand(LaunchOptions{Prompt: "--yes", Mode: protocol.TaskModeOneshot, PermissionMode: protocol.PermissionSandboxed})
	if want := []string{"--single=--yes"}; !slices.Equal(grok.Args, want) {
		t.Fatalf("grok argv = %q, want %q", grok.Args, want)
	}
}
