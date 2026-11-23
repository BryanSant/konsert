package drivers

import (
	"slices"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
)

func TestDriverPermissionFlags(t *testing.T) {
	claude := &ClaudeDriver{}
	specFull := claude.BuildCommand(LaunchOptions{Prompt: "fix tests", Mode: "oneshot", PermissionMode: protocol.PermissionFullAccess})
	if !slices.Contains(specFull.Args, "--dangerously-skip-permissions") {
		t.Fatalf("expected claude full_access to include --dangerously-skip-permissions, got %v", specFull.Args)
	}
	specSand := claude.BuildCommand(LaunchOptions{Prompt: "fix tests", Mode: "oneshot", PermissionMode: protocol.PermissionSandboxed})
	if slices.Contains(specSand.Args, "--dangerously-skip-permissions") {
		t.Fatalf("expected claude sandboxed to omit --dangerously-skip-permissions, got %v", specSand.Args)
	}

	agy := &AgyDriver{}
	agyFull := agy.BuildCommand(LaunchOptions{Prompt: "fix tests", Mode: "oneshot", PermissionMode: protocol.PermissionFullAccess})
	if !slices.Contains(agyFull.Args, "--dangerously-skip-permissions") || !slices.Contains(agyFull.Args, "--print=fix tests") {
		t.Fatalf("expected agy full_access with --print=..., got %v", agyFull.Args)
	}
	agySand := agy.BuildCommand(LaunchOptions{Prompt: "fix tests", Mode: "oneshot", PermissionMode: protocol.PermissionSandboxed})
	if !slices.Contains(agySand.Args, "--sandbox") {
		t.Fatalf("expected agy sandboxed to include --sandbox, got %v", agySand.Args)
	}
	agyInter := agy.BuildCommand(LaunchOptions{Prompt: "", Mode: "interactive", PermissionMode: protocol.PermissionFullAccess})
	for _, a := range agyInter.Args {
		if strings.HasPrefix(a, "--print") || strings.HasPrefix(a, "--prompt-interactive") {
			t.Fatalf("expected empty interactive prompt to omit prompt flags, got %v", agyInter.Args)
		}
	}

	grok := &GrokDriver{}
	grokFull := grok.BuildCommand(LaunchOptions{Prompt: "fix tests", Mode: "oneshot", PermissionMode: protocol.PermissionFullAccess})
	if !slices.Contains(grokFull.Args, "--always-approve") {
		t.Fatalf("expected grok full_access to include --always-approve, got %v", grokFull.Args)
	}
	grokSand := grok.BuildCommand(LaunchOptions{Prompt: "fix tests", Mode: "oneshot", PermissionMode: protocol.PermissionSandboxed})
	if slices.Contains(grokSand.Args, "--always-approve") {
		t.Fatalf("expected grok sandboxed to omit --always-approve, got %v", grokSand.Args)
	}
}

func TestClaudeJSONLParserAndUsageDeltas(t *testing.T) {
	d := &ClaudeDriver{}
	var events []TelemetryEvent
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"internal/hub.go"}},{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}},{"type":"text","text":"All tests now pass."}]}}`
	d.parseJSONLLine("t1", []byte(line), func(ev TelemetryEvent) {
		events = append(events, ev)
	})
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	if events[0].Kind != "file_edit" || events[0].Message != "Edit: internal/hub.go" {
		t.Fatalf("unexpected first event: %+v", events[0])
	}
	if events[1].Kind != "tool_call" || events[1].Message != "Bash: go test ./..." {
		t.Fatalf("unexpected second event: %+v", events[1])
	}

	// Real Claude Code cost-state usage line (twice to verify delta non-duplication)
	cost1 := `{"type":"cost-state","modelUsage":{"claude-sonnet-5-5":{"inputTokens":10,"outputTokens":20,"cacheReadInputTokens":100,"cacheCreationInputTokens":50}}}`
	cost2 := `{"type":"cost-state","modelUsage":{"claude-sonnet-5-5":{"inputTokens":15,"outputTokens":35,"cacheReadInputTokens":100,"cacheCreationInputTokens":50}}}`
	d.parseJSONLLine("t1", []byte(cost1), func(ev TelemetryEvent) { events = append(events, ev) })
	d.parseJSONLLine("t1", []byte(cost2), func(ev TelemetryEvent) { events = append(events, ev) })

	var usageEvs []TelemetryEvent
	for _, ev := range events {
		if ev.Kind == "usage" {
			usageEvs = append(usageEvs, ev)
		}
	}
	if len(usageEvs) != 2 {
		t.Fatalf("expected 2 usage delta events, got %d", len(usageEvs))
	}
	if usageEvs[0].Meta["input_tokens"].(int64) != 160 || usageEvs[0].Meta["output_tokens"].(int64) != 20 {
		t.Fatalf("unexpected first usage delta: %+v", usageEvs[0].Meta)
	}
	if usageEvs[1].Meta["input_tokens"].(int64) != 5 || usageEvs[1].Meta["output_tokens"].(int64) != 15 {
		t.Fatalf("unexpected second usage delta: %+v", usageEvs[1].Meta)
	}

	summary := d.ExtractSummary(t.Context(), SummaryOptions{TaskID: "t1"})
	if summary != "All tests now pass." {
		t.Fatalf("unexpected summary: %q", summary)
	}
}

func TestAgyLineClassifier(t *testing.T) {
	ev, ok := classifyAgyLine("Edited internal/controller/api.go")
	if !ok || ev.Kind != "file_edit" {
		t.Fatalf("expected file_edit, got %+v ok=%v", ev, ok)
	}
	ev, ok = classifyAgyLine("Tool: run_command")
	if !ok || ev.Kind != "tool_call" {
		t.Fatalf("expected tool_call, got %+v ok=%v", ev, ok)
	}
	ev, ok = classifyAgyLine("Allow this command to proceed? [y/n]")
	if !ok || ev.Kind != "waiting_input" {
		t.Fatalf("expected waiting_input, got %+v ok=%v", ev, ok)
	}
}
