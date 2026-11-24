package drivers

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

func init() {
	Register(&MockDriver{})
}

// MockDriver is a deterministic scripted agent driver for CI, integration tests,
// and local UI development without requiring LLM API credentials.
//
// Supported directives inside Prompt:
//   - SLEEP=<secs>          : sleep before finishing (default 0)
//   - EXIT=<code>           : exit with code (default 0)
//   - WAIT_INPUT            : wait for a line of stdin before completing (or mode=interactive)
//   - TOUCH_FILE=<filename> : create/append a file in workdir to test git diff extraction
//   - STREAM_LINES=<n>      : emit n colored ANSI output lines
//   - BG_SHELL=<secs>       : start a background shell (nohup sh -c 'sleep <secs>') that
//     outlives the task (Unix), for shell counting and Fleet's cleanup
type MockDriver struct{}

func (d *MockDriver) Name() string { return "mock" }

func (d *MockDriver) Detect() (string, bool) {
	if runtime.GOOS == "windows" {
		p, err := exec.LookPath("cmd.exe")
		return p, err == nil
	}
	p, err := exec.LookPath("sh")
	return p, err == nil
}

func (d *MockDriver) BuildCommand(opts LaunchOptions) LaunchSpec {
	var sleepSecs, exitCode, touchFile, streamLines, bgShell string
	waitInput := opts.Mode == "interactive"

	for _, tok := range strings.Fields(opts.Prompt) {
		switch {
		case strings.HasPrefix(tok, "SLEEP="):
			sleepSecs = strings.TrimPrefix(tok, "SLEEP=")
		case strings.HasPrefix(tok, "EXIT="):
			exitCode = strings.TrimPrefix(tok, "EXIT=")
		case strings.HasPrefix(tok, "TOUCH_FILE="):
			touchFile = strings.TrimPrefix(tok, "TOUCH_FILE=")
		case strings.HasPrefix(tok, "STREAM_LINES="):
			streamLines = strings.TrimPrefix(tok, "STREAM_LINES=")
		case strings.HasPrefix(tok, "BG_SHELL="):
			bgShell = strings.TrimPrefix(tok, "BG_SHELL=")
		case tok == "WAIT_INPUT":
			waitInput = true
		}
	}

	if runtime.GOOS == "windows" {
		var lines []string
		lines = append(lines, "echo [MOCK:MILESTONE] Starting mock agent task")
		if touchFile != "" {
			lines = append(lines, fmt.Sprintf("echo updated>> %s", touchFile))
			lines = append(lines, fmt.Sprintf("echo [MOCK:FILE] Modified %s", touchFile))
		}
		if waitInput {
			lines = append(lines, "echo [MOCK:WAITING] Waiting for input...")
			lines = append(lines, "set /p USER_IN=")
			lines = append(lines, "echo [MOCK:MILESTONE] Received input: %USER_IN%")
		}
		lines = append(lines, "echo [MOCK:SUMMARY] Mock agent finished cleanly")
		if exitCode != "" {
			lines = append(lines, "exit /b "+exitCode)
		}
		return LaunchSpec{
			Binary: "cmd.exe",
			Args:   []string{"/C", strings.Join(lines, " & ")},
			Env:    opts.Env,
		}
	}

	var script strings.Builder
	script.WriteString("printf '\\033[1;36m[konsert-mock]\\033[0m Starting task (permission=" + opts.PermissionMode + ")\\n'\n")
	script.WriteString("echo '[MOCK:MILESTONE] Analyzing workspace'\n")
	script.WriteString("echo '[MOCK:TOOL] Running workspace checks'\n")

	if streamLines != "" {
		script.WriteString(fmt.Sprintf("i=1; while [ \"$i\" -le %s ]; do printf '\\033[32m[line %%d]\\033[0m synthetic PTY stream output\\n' \"$i\"; i=$((i+1)); done\n", streamLines))
	}

	if touchFile != "" {
		script.WriteString(fmt.Sprintf("printf 'mock update\\n' >> %q\n", touchFile))
		script.WriteString(fmt.Sprintf("echo '[MOCK:FILE] Edited %s'\n", touchFile))
	}

	if n, err := strconv.Atoi(bgShell); err == nil && n > 0 {
		// A background shell that outlives the task (nohup: the end of the PTY session
		// doesn't hang it up), for the shell count and Fleet's cleanup.
		script.WriteString(fmt.Sprintf("nohup sh -c 'sleep %d; true' >/dev/null 2>&1 &\n", n))
		script.WriteString("echo '[MOCK:TOOL] Started a background shell'\n")
	}

	if sleepSecs != "" {
		script.WriteString(fmt.Sprintf("sleep %s\n", sleepSecs))
	}

	if waitInput {
		script.WriteString("echo '[MOCK:WAITING] Ready for interactive input (type exit to finish)'\n")
		script.WriteString("while IFS= read -r line; do\n")
		script.WriteString("  line=$(printf '%s' \"$line\" | tr -d '\\r')\n")
		script.WriteString("  printf '\\033[33m[mock-echo]\\033[0m %s\\n' \"$line\"\n")
		script.WriteString("  echo \"[MOCK:MILESTONE] Processed steering input: $line\"\n")
		script.WriteString("  if [ \"$line\" = \"exit\" ] || [ \"$line\" = \"done\" ]; then break; fi\n")
		script.WriteString("done\n")
	}

	script.WriteString("echo '[MOCK:SUMMARY] Mock agent finished cleanly'\n")
	if exitCode != "" {
		script.WriteString(fmt.Sprintf("exit %s\n", exitCode))
	}

	return LaunchSpec{
		Binary: "sh",
		Args:   []string{"-c", script.String()},
		Env:    opts.Env,
	}
}

func (d *MockDriver) WatchTelemetry(ctx context.Context, opts WatchOptions, emit func(TelemetryEvent)) {
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-opts.OutputLines:
			if !ok {
				return
			}
			clean := strings.TrimSpace(StripANSI(line))
			switch {
			case strings.HasPrefix(clean, "[MOCK:MILESTONE]"):
				emit(TelemetryEvent{
					Kind:    "milestone",
					Message: strings.TrimSpace(strings.TrimPrefix(clean, "[MOCK:MILESTONE]")),
				})
			case strings.HasPrefix(clean, "[MOCK:TOOL]"):
				emit(TelemetryEvent{
					Kind:    "tool_call",
					Message: strings.TrimSpace(strings.TrimPrefix(clean, "[MOCK:TOOL]")),
				})
			case strings.HasPrefix(clean, "[MOCK:FILE]"):
				emit(TelemetryEvent{
					Kind:    "file_edit",
					Message: strings.TrimSpace(strings.TrimPrefix(clean, "[MOCK:FILE]")),
				})
			case strings.HasPrefix(clean, "[MOCK:WAITING]"):
				emit(TelemetryEvent{
					Kind:    "waiting_input",
					Message: strings.TrimSpace(strings.TrimPrefix(clean, "[MOCK:WAITING]")),
				})
			case strings.HasPrefix(clean, "[MOCK:SUMMARY]"):
				emit(TelemetryEvent{
					Kind:    "summary",
					Message: strings.TrimSpace(strings.TrimPrefix(clean, "[MOCK:SUMMARY]")),
				})
			}
		}
	}
}

func (d *MockDriver) ExtractSummary(_ context.Context, opts SummaryOptions) string {
	for i := len(opts.Events) - 1; i >= 0; i-- {
		if opts.Events[i].Kind == "summary" && opts.Events[i].Message != "" {
			return opts.Events[i].Message
		}
	}
	if opts.RingSnapshot != nil {
		if tail := TailCleanSummary(opts.RingSnapshot(), 10, 1000); tail != "" {
			return tail
		}
	}
	return fmt.Sprintf("Mock task exited with code %d", opts.ExitCode)
}
