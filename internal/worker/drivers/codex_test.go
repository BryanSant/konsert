package drivers

import (
	"slices"
	"testing"
)

func TestCodexDriverCommandAndTelemetry(t *testing.T) {
	drv := &CodexDriver{}
	if drv.Name() != "codex" {
		t.Fatalf("expected name codex, got %s", drv.Name())
	}

	// Oneshot + full_access + model
	spec := drv.BuildCommand(LaunchOptions{
		Prompt:         "fix tests",
		Mode:           "oneshot",
		PermissionMode: "full_access",
		Model:          "o4-mini",
	})
	if len(spec.Args) == 0 || spec.Args[0] != "exec" {
		t.Fatalf("expected exec subcommand for oneshot, got %v", spec.Args)
	}
	if !slices.Contains(spec.Args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("expected full_access bypass flag, got %v", spec.Args)
	}
	if !slices.Contains(spec.Args, "--model") || !slices.Contains(spec.Args, "o4-mini") {
		t.Fatalf("expected --model o4-mini, got %v", spec.Args)
	}

	// Interactive + sandboxed
	specInt := drv.BuildCommand(LaunchOptions{
		Prompt:         "",
		Mode:           "interactive",
		PermissionMode: "sandboxed",
	})
	if slices.Contains(specInt.Args, "exec") {
		t.Fatalf("interactive mode must not use exec subcommand, got %v", specInt.Args)
	}
	if !slices.Contains(specInt.Args, "--sandbox") || !slices.Contains(specInt.Args, "workspace-write") {
		t.Fatalf("expected sandbox flags, got %v", specInt.Args)
	}

	// Telemetry classification including 5h quota extraction
	ev, ok := classifyCodexLine("Codex 21% used of 5h window")
	if !ok || ev.Kind != "quota" || ev.Meta["provider"] != "codex" || ev.Meta["used_percent"] != float64(21) {
		t.Fatalf("expected quota telemetry event, got %+v ok=%v", ev, ok)
	}
	evEdit, ok := classifyCodexLine("Updated internal/store/store.go")
	if !ok || evEdit.Kind != "file_edit" {
		t.Fatalf("expected file_edit event, got %+v ok=%v", evEdit, ok)
	}
}
