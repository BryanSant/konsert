package drivers

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

func init() {
	Register(&CodexDriver{})
}

var codexQuotaPctRegex = regexp.MustCompile(`(?i)(\d{1,3})%\s*(?:used|of\s+5h|5h|quota)`)

// CodexDriver runs OpenAI's Codex CLI (`codex`).
// In default full_access mode, it passes `--dangerously-bypass-approvals-and-sandbox`.
// In opt-in sandboxed mode, it passes `--sandbox workspace-write`.
type CodexDriver struct{}

func (d *CodexDriver) Name() string { return "codex" }

func (d *CodexDriver) Detect() (string, bool) {
	return FindBinary("codex")
}

func (d *CodexDriver) BuildCommand(opts LaunchOptions) LaunchSpec {
	bin, ok := d.Detect()
	if !ok {
		bin = "codex"
	}

	var args []string
	if opts.Mode != "interactive" {
		args = append(args, "exec")
	}
	if IsFullAccess(opts.PermissionMode) {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	} else {
		args = append(args, "--sandbox", "workspace-write")
	}
	if m := strings.TrimSpace(opts.Model); m != "" {
		args = append(args, "--model", m)
	}
	if p := strings.TrimSpace(opts.Prompt); p != "" {
		// "--" stops option parsing so a prompt beginning with '-' stays a prompt.
		args = append(args, "--", p)
	}

	return LaunchSpec{
		Binary: bin,
		Args:   args,
		Env:    opts.Env,
	}
}

func (d *CodexDriver) WatchTelemetry(ctx context.Context, opts WatchOptions, emit func(TelemetryEvent)) {
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
			if ev, matched := classifyCodexLine(clean); matched {
				emit(ev)
			}
		}
	}
}

func classifyCodexLine(clean string) (TelemetryEvent, bool) {
	lower := strings.ToLower(clean)
	if m := codexQuotaPctRegex.FindStringSubmatch(clean); len(m) == 2 {
		if pct, err := strconv.ParseFloat(m[1], 64); err == nil && pct >= 0 && pct <= 100 {
			return TelemetryEvent{
				Kind:    "quota",
				Message: clean,
				Meta: map[string]any{
					"provider":     "codex",
					"label":        "Codex",
					"window_label": "5h",
					"used_percent": pct,
				},
			}, true
		}
	}
	switch {
	case (strings.Contains(lower, "approve") || strings.Contains(lower, "confirm") || strings.Contains(lower, "allow")) &&
		(strings.Contains(lower, "[y/n]") || strings.Contains(lower, "press enter") || strings.Contains(lower, "proceed")):
		return TelemetryEvent{Kind: "waiting_input", Message: clean}, true
	case strings.HasPrefix(clean, "M ") || strings.HasPrefix(clean, "A ") ||
		strings.HasPrefix(clean, "Updated ") || strings.HasPrefix(clean, "Edited ") ||
		strings.HasPrefix(clean, "Created ") || strings.HasPrefix(clean, "Patched "):
		return TelemetryEvent{Kind: "file_edit", Message: clean}, true
	case strings.HasPrefix(clean, "exec ") || strings.HasPrefix(clean, "Running ") ||
		strings.HasPrefix(clean, "Shell ") || strings.HasPrefix(clean, "Tool "):
		return TelemetryEvent{Kind: "tool_call", Message: clean}, true
	case strings.HasPrefix(lower, "error:") || strings.HasPrefix(lower, "fatal:"):
		return TelemetryEvent{Kind: "error", Message: clean}, true
	default:
		return TelemetryEvent{}, false
	}
}

func (d *CodexDriver) ExtractSummary(_ context.Context, opts SummaryOptions) string {
	tail := TailCleanSummary(opts.RingSnapshot(), 15, 1600)
	if tail != "" {
		return tail
	}
	return fmt.Sprintf("codex exited with status %d", opts.ExitCode)
}
