package drivers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

func init() {
	Register(&ShellDriver{})
}

// ShellDriver launches an interactive native shell (zsh/bash/sh/pwsh) on the worker
// for Direct Mode sessions and the Floating Scratch Workspace.
type ShellDriver struct{}

// ShellDriverName is the registry name of the unrestricted shell driver. Sandboxed
// workers refuse it unless explicitly opted in, because a shell ignores permission mode.
const ShellDriverName = "shell"

func (d *ShellDriver) Name() string { return ShellDriverName }

func (d *ShellDriver) Detect() (string, bool) {
	if runtime.GOOS == "windows" {
		for _, bin := range []string{"pwsh.exe", "powershell.exe", "cmd.exe"} {
			if p, err := exec.LookPath(bin); err == nil {
				return p, true
			}
		}
		return "", false
	}
	if sh := strings.TrimSpace(os.Getenv("SHELL")); sh != "" {
		if p, err := exec.LookPath(sh); err == nil {
			return p, true
		}
	}
	for _, bin := range []string{"zsh", "bash", "sh"} {
		if p, err := exec.LookPath(bin); err == nil {
			return p, true
		}
	}
	return "", false
}

func (d *ShellDriver) BuildCommand(opts LaunchOptions) LaunchSpec {
	bin, ok := d.Detect()
	if !ok {
		if runtime.GOOS == "windows" {
			bin = "cmd.exe"
		} else {
			bin = "/bin/sh"
		}
	}

	if runtime.GOOS == "windows" {
		if strings.TrimSpace(opts.Prompt) != "" && opts.Mode == "oneshot" {
			if strings.HasSuffix(strings.ToLower(bin), "cmd.exe") {
				return LaunchSpec{Binary: bin, Args: []string{"/C", opts.Prompt}, Env: opts.Env}
			}
			return LaunchSpec{Binary: bin, Args: []string{"-NoLogo", "-Command", opts.Prompt}, Env: opts.Env}
		}
		if strings.TrimSpace(opts.Prompt) != "" {
			// Interactive with a command (e.g. a relogin): run it, then stay open.
			if strings.HasSuffix(strings.ToLower(bin), "cmd.exe") {
				return LaunchSpec{Binary: bin, Args: []string{"/K", opts.Prompt}, Env: opts.Env}
			}
			return LaunchSpec{Binary: bin, Args: []string{"-NoLogo", "-NoExit", "-Command", opts.Prompt}, Env: opts.Env}
		}
		return LaunchSpec{Binary: bin, Args: nil, Env: opts.Env}
	}

	if strings.TrimSpace(opts.Prompt) != "" && opts.Mode == "oneshot" {
		return LaunchSpec{
			Binary: bin,
			Args:   []string{"-c", "--", opts.Prompt},
			Env:    opts.Env,
		}
	}

	if strings.TrimSpace(opts.Prompt) != "" {
		// Interactive with a command (e.g. "Open terminal and sign in" running
		// `codex login --device-auth`): run it in a login shell, then hand the terminal
		// over to an interactive shell so the user sees the output and can continue.
		return LaunchSpec{
			Binary: bin,
			Args:   []string{"-l", "-c", opts.Prompt + "\nexec " + shellSingleQuote(bin) + " -l"},
			Env:    opts.Env,
		}
	}

	// Interactive login shell
	return LaunchSpec{
		Binary: bin,
		Args:   []string{"-l"},
		Env:    opts.Env,
	}
}

func (d *ShellDriver) WatchTelemetry(ctx context.Context, opts WatchOptions, _ func(TelemetryEvent)) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-opts.OutputLines:
			if !ok {
				return
			}
		}
	}
}

func (d *ShellDriver) ExtractSummary(_ context.Context, opts SummaryOptions) string {
	if opts.RingSnapshot != nil {
		if tail := TailCleanSummary(opts.RingSnapshot(), 8, 800); tail != "" {
			return tail
		}
	}
	return fmt.Sprintf("Shell session exited with code %d", opts.ExitCode)
}

// shellSingleQuote quotes s for a POSIX shell.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
