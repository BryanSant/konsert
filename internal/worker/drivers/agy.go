package drivers

import (
	"context"
	"fmt"
	"os/exec"
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
	p, err := exec.LookPath("agy")
	return p, err == nil
}

func (d *AgyDriver) BuildCommand(opts LaunchOptions) LaunchSpec {
	args := []string{}
	if IsFullAccess(opts.PermissionMode) {
		args = append(args, "--dangerously-skip-permissions")
	} else {
		args = append(args, "--sandbox")
	}
	if opts.Mode == "interactive" {
		if strings.TrimSpace(opts.Prompt) != "" {
			args = append(args, "--prompt-interactive", opts.Prompt)
		}
	} else {
		args = append(args, "--print", opts.Prompt)
	}
	return LaunchSpec{
		Binary: "agy",
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
			lower := strings.ToLower(clean)
			switch {
			case strings.Contains(lower, "approve") && (strings.Contains(lower, "[y/n]") || strings.Contains(lower, "confirm")):
				emit(TelemetryEvent{Kind: "waiting_input", Message: clean})
			case strings.HasPrefix(clean, "Edited ") || strings.HasPrefix(clean, "Created ") || strings.HasPrefix(clean, "Updated "):
				emit(TelemetryEvent{Kind: "file_edit", Message: clean})
			case strings.HasPrefix(clean, "Running ") || strings.HasPrefix(clean, "Executing ") || strings.HasPrefix(clean, "Tool: "):
				emit(TelemetryEvent{Kind: "tool_call", Message: clean})
			case strings.HasPrefix(lower, "error:") || strings.HasPrefix(lower, "fatal:"):
				emit(TelemetryEvent{Kind: "error", Message: clean})
			}
		}
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
