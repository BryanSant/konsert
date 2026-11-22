package drivers

import (
	"slices"
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
	if !slices.Contains(agyFull.Args, "--dangerously-skip-permissions") {
		t.Fatalf("expected agy full_access to include --dangerously-skip-permissions, got %v", agyFull.Args)
	}
	agySand := agy.BuildCommand(LaunchOptions{Prompt: "fix tests", Mode: "oneshot", PermissionMode: protocol.PermissionSandboxed})
	if !slices.Contains(agySand.Args, "--sandbox") {
		t.Fatalf("expected agy sandboxed to include --sandbox, got %v", agySand.Args)
	}

	grok := &GrokDriver{}
	grokFull := grok.BuildCommand(LaunchOptions{Prompt: "fix tests", Mode: "oneshot", PermissionMode: protocol.PermissionFullAccess})
	if !slices.Contains(grokFull.Args, "--yes") {
		t.Fatalf("expected grok full_access to include --yes, got %v", grokFull.Args)
	}
	grokSand := grok.BuildCommand(LaunchOptions{Prompt: "fix tests", Mode: "oneshot", PermissionMode: protocol.PermissionSandboxed})
	if slices.Contains(grokSand.Args, "--yes") {
		t.Fatalf("expected grok sandboxed to omit --yes, got %v", grokSand.Args)
	}
}

func TestClaudeJSONLParser(t *testing.T) {
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
	summary := d.ExtractSummary(t.Context(), SummaryOptions{TaskID: "t1"})
	if summary != "All tests now pass." {
		t.Fatalf("unexpected summary: %q", summary)
	}
}
