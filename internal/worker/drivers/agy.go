package drivers

import (
	"context"
	"fmt"
	"strings"
)

func init() {
	Register(&AgyDriver{})
}

// AgyDriver runs the Google Antigravity CLI (`agy`).
// In default full_access mode, it passes `--dangerously-skip-permissions`.
// In opt-in sandboxed mode, it passes `--sandbox`.
type AgyDriver struct{}

func (d *AgyDriver) Name() string { return "agy" }

func (d *AgyDriver) Detect() (string, bool) {
	return FindBinary("agy")
}

func (d *AgyDriver) BuildCommand(opts LaunchOptions) LaunchSpec {
	bin, ok := d.Detect()
	if !ok {
		bin = "agy"
	}
	args := []string{}
	if IsFullAccess(opts.PermissionMode) {
		args = append(args, "--dangerously-skip-permissions")
	} else {
		args = append(args, "--sandbox")
	}
	if opts.Mode == "interactive" {
		if p := strings.TrimSpace(opts.Prompt); p != "" {
			args = append(args, "--prompt-interactive="+p)
		}
	} else {
		args = append(args, "--print="+opts.Prompt)
	}
	return LaunchSpec{
		Binary: bin,
		Args:   args,
		Env:    opts.Env,
	}
}

func (d *AgyDriver) WatchTelemetry(ctx context.Context, opts WatchOptions, emit func(TelemetryEvent)) {
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-opts.OutputLines:
			if !ok {
				return
			}
			clean := strings.TrimSpace(StripANSI(line))
			if clean == "" {
				continue
			}
			if ev, matched := classifyAgyLine(clean); matched {
				emit(ev)
			}
		}
	}
}

func classifyAgyLine(clean string) (TelemetryEvent, bool) {
	lower := strings.ToLower(clean)
	switch {
	case (strings.Contains(lower, "approve") || strings.Contains(lower, "allow") || strings.Contains(lower, "permission")) &&
		(strings.Contains(lower, "[y/n]") || strings.Contains(lower, "confirm") || strings.Contains(lower, "proceed")):
		return TelemetryEvent{Kind: "waiting_input", Message: clean}, true
	case strings.HasPrefix(clean, "Edited ") || strings.HasPrefix(clean, "Created ") ||
		strings.HasPrefix(clean, "Updated ") || strings.HasPrefix(clean, "Wrote ") ||
		strings.HasPrefix(clean, "Edit ") || strings.HasPrefix(clean, "Write "):
		return TelemetryEvent{Kind: "file_edit", Message: clean}, true
	case strings.HasPrefix(clean, "Running ") || strings.HasPrefix(clean, "Executing ") ||
		strings.HasPrefix(clean, "Tool: ") || strings.HasPrefix(clean, "Read ") ||
		strings.HasPrefix(clean, "Bash: ") || strings.HasPrefix(clean, "Search "):
		return TelemetryEvent{Kind: "tool_call", Message: clean}, true
	case strings.HasPrefix(lower, "error:") || strings.HasPrefix(lower, "fatal:"):
		return TelemetryEvent{Kind: "error", Message: clean}, true
	default:
		return TelemetryEvent{}, false
	}
}

func (d *AgyDriver) ExtractSummary(_ context.Context, opts SummaryOptions) string {
	if opts.RingSnapshot != nil {
		if tail := TailCleanSummary(opts.RingSnapshot(), 15, 1500); tail != "" {
			return tail
		}
	}
	return fmt.Sprintf("agy exited with code %d", opts.ExitCode)
}
