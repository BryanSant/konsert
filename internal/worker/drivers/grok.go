package drivers

import (
	"context"
	"fmt"
	"strings"
)

func init() {
	Register(&GrokDriver{})
}

// GrokDriver runs the Grok CLI (`grok`).
// In default full_access mode, it runs with auto-approval enabled (`--always-approve`).
type GrokDriver struct{}

func (d *GrokDriver) Name() string { return "grok" }

func (d *GrokDriver) Detect() (string, bool) {
	return FindBinary("grok")
}

func (d *GrokDriver) BuildCommand(opts LaunchOptions) LaunchSpec {
	bin, ok := d.Detect()
	if !ok {
		bin = "grok"
	}
	args := []string{}
	if IsFullAccess(opts.PermissionMode) {
		args = append(args, "--always-approve")
	}
	if m := strings.TrimSpace(opts.Model); m != "" {
		args = append(args, "--model", m)
	}
	// The prompt is either "="-joined to its flag or placed after "--", so a prompt that
	// begins with '-' can never be parsed as a CLI flag.
	if opts.Mode == "interactive" {
		if strings.TrimSpace(opts.Prompt) != "" {
			args = append(args, "--", opts.Prompt)
		}
	} else {
		args = append(args, "--single="+opts.Prompt)
	}
	return LaunchSpec{
		Binary: bin,
		Args:   args,
		Env:    opts.Env,
	}
}

func (d *GrokDriver) WatchTelemetry(ctx context.Context, opts WatchOptions, emit func(TelemetryEvent)) {
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
			case strings.Contains(lower, "[y/n]") || strings.Contains(lower, "waiting for input"):
				emit(TelemetryEvent{Kind: "waiting_input", Message: clean})
			case strings.HasPrefix(clean, "Edit ") || strings.HasPrefix(clean, "Write "):
				emit(TelemetryEvent{Kind: "file_edit", Message: clean})
			case strings.HasPrefix(clean, "Run ") || strings.HasPrefix(clean, "Tool "):
				emit(TelemetryEvent{Kind: "tool_call", Message: clean})
			case strings.HasPrefix(lower, "error:"):
				emit(TelemetryEvent{Kind: "error", Message: clean})
			}
		}
	}
}

func (d *GrokDriver) ExtractSummary(_ context.Context, opts SummaryOptions) string {
	if opts.RingSnapshot != nil {
		if tail := TailCleanSummary(opts.RingSnapshot(), 15, 1500); tail != "" {
			return tail
		}
	}
	return fmt.Sprintf("grok exited with code %d", opts.ExitCode)
}
