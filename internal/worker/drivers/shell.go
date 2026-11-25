package drivers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

func init() {
	Register(&ShellDriver{})
}

// ShellDriver launches an interactive native shell (zsh/bash/fish/pwsh/powershell/Git Bash)
// on the worker for Direct Mode sessions and the Floating Scratch Workspace.
type ShellDriver struct{}

// ShellDriverName is the registry name of the unrestricted shell driver. Sandboxed
// workers refuse it unless explicitly opted in, because a shell ignores permission mode.
const ShellDriverName = "shell"

func (d *ShellDriver) Name() string { return ShellDriverName }

// shellLookup abstracts OS/filesystem probes so shell detection and selection can be
// unit-tested on any host platform.
type shellLookup struct {
	goos        string
	getenv      func(string) string
	lookPath    func(string) (string, error)
	statFile    func(string) bool
	gitExecPath func() string
	gitRegDir   func() string
}

func defaultShellLookup() shellLookup {
	return shellLookup{
		goos:     runtime.GOOS,
		getenv:   os.Getenv,
		lookPath: exec.LookPath,
		statFile: func(p string) bool {
			info, err := os.Stat(p)
			return err == nil && !info.IsDir()
		},
		gitExecPath: func() string {
			gitBin, err := exec.LookPath("git")
			if err != nil {
				return ""
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, gitBin, "--exec-path").Output()
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(out))
		},
		gitRegDir: gitForWindowsRegistryPath,
	}
}

// DetectShells returns the supported terminal shells installed on this machine.
func DetectShells() []protocol.TerminalShell {
	return detectShells(defaultShellLookup())
}

// DetectGitBash locates Git for Windows's bash.exe on Windows.
func DetectGitBash() (string, bool) {
	return detectGitBash(defaultShellLookup())
}

func detectShells(l shellLookup) []protocol.TerminalShell {
	var out []protocol.TerminalShell
	add := func(id, label, p string) {
		if p != "" {
			out = append(out, protocol.TerminalShell{ID: id, Label: label, Path: p})
		}
	}
	switch l.goos {
	case "windows":
		if p, ok := detectGitBash(l); ok {
			add(protocol.ShellGitBash, "Git Bash", p)
		}
		if p, err := l.lookPath("pwsh.exe"); err == nil {
			add(protocol.ShellPwsh, "PowerShell 7", p)
		}
		if p, err := l.lookPath("powershell.exe"); err == nil {
			add(protocol.ShellPowerShell, "PowerShell 5.1", p)
		}
	case "darwin":
		if p, err := l.lookPath("zsh"); err == nil {
			add(protocol.ShellZsh, "zsh", p)
		}
		if p, err := l.lookPath("bash"); err == nil {
			add(protocol.ShellBash, "bash", p)
		}
	default:
		if p, err := l.lookPath("bash"); err == nil {
			add(protocol.ShellBash, "bash", p)
		}
		if p, err := l.lookPath("zsh"); err == nil {
			add(protocol.ShellZsh, "zsh", p)
		}
		if p, err := l.lookPath("fish"); err == nil {
			add(protocol.ShellFish, "fish", p)
		}
	}
	return out
}

// detectGitBash finds Git for Windows's bash.exe without touching System32\bash.exe (WSL).
// Lookup order: git --exec-path, Windows registry, %ProgramFiles%, %ProgramFiles(x86)%,
// %LocalAppData%\Programs.
func detectGitBash(l shellLookup) (string, bool) {
	if l.goos != "windows" {
		return "", false
	}
	checkGitRoot := func(root string) (string, bool) {
		root = strings.TrimRight(strings.TrimSpace(root), `/\`)
		if root == "" {
			return "", false
		}
		for _, rel := range []string{`bin\bash.exe`, `usr\bin\bash.exe`} {
			cand := winJoin(root, rel)
			if l.statFile(cand) {
				return cand, true
			}
		}
		return "", false
	}

	// 1. Derive Git root from `git --exec-path` (e.g. C:/Program Files/Git/mingw64/libexec/git-core).
	if l.gitExecPath != nil {
		if ep := strings.TrimSpace(l.gitExecPath()); ep != "" {
			dir := normalizeWinDir(ep)
			for range 5 {
				if p, ok := checkGitRoot(dir); ok {
					return p, true
				}
				parent := winParent(dir)
				if parent == "" || parent == dir {
					break
				}
				dir = parent
			}
		}
	}

	// 2. Windows registry (HKLM/HKCU\SOFTWARE\GitForWindows InstallPath).
	if l.gitRegDir != nil {
		if reg := strings.TrimSpace(l.gitRegDir()); reg != "" {
			if p, ok := checkGitRoot(reg); ok {
				return p, true
			}
		}
	}

	// 3. Well-known install locations.
	var roots []string
	if pf := strings.TrimSpace(l.getenv("ProgramFiles")); pf != "" {
		roots = append(roots, winJoin(pf, "Git"))
	} else {
		roots = append(roots, `C:\Program Files\Git`)
	}
	if pf86 := strings.TrimSpace(l.getenv("ProgramFiles(x86)")); pf86 != "" {
		roots = append(roots, winJoin(pf86, "Git"))
	}
	if lad := strings.TrimSpace(l.getenv("LocalAppData")); lad != "" {
		roots = append(roots, winJoin(lad, `Programs\Git`))
	}
	for _, r := range roots {
		if p, ok := checkGitRoot(r); ok {
			return p, true
		}
	}
	return "", false
}

// winJoin joins a Windows base path and relative path using backslashes so Windows path
// construction behaves identically in cross-platform unit tests.
func winJoin(base, rel string) string {
	b := strings.TrimRight(strings.ReplaceAll(base, "/", `\`), `\`)
	r := strings.TrimLeft(strings.ReplaceAll(rel, "/", `\`), `\`)
	if b == "" {
		return r
	}
	if r == "" {
		return b
	}
	return b + `\` + r
}

func normalizeWinDir(p string) string {
	return strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(p), "/", `\`), `\`)
}

func winParent(p string) string {
	norm := strings.ReplaceAll(p, `\`, "/")
	parent := path.Dir(norm)
	if parent == "." || parent == "/" || parent == norm {
		return ""
	}
	// Preserve bare drive root "C:" as "C:\"
	if len(parent) == 2 && parent[1] == ':' {
		return parent + `\`
	}
	return strings.ReplaceAll(parent, "/", `\`)
}

func (d *ShellDriver) Detect() (string, bool) {
	return detectDefaultShell(defaultShellLookup())
}

func detectDefaultShell(l shellLookup) (string, bool) {
	if l.goos == "windows" {
		for _, bin := range []string{"pwsh.exe", "powershell.exe"} {
			if p, err := l.lookPath(bin); err == nil {
				return p, true
			}
		}
		if p, ok := detectGitBash(l); ok {
			return p, true
		}
		if p, err := l.lookPath("cmd.exe"); err == nil {
			return p, true
		}
		return "", false
	}
	if sh := strings.TrimSpace(l.getenv("SHELL")); sh != "" {
		if p, err := l.lookPath(sh); err == nil {
			return p, true
		}
	}
	for _, bin := range []string{"zsh", "bash", "sh"} {
		if p, err := l.lookPath(bin); err == nil {
			return p, true
		}
	}
	return "", false
}

func resolveShell(l shellLookup, shellID string) (string, bool) {
	if id := strings.TrimSpace(shellID); id != "" {
		for _, s := range detectShells(l) {
			if s.ID == id {
				return s.Path, true
			}
		}
	}
	return detectDefaultShell(l)
}

func (d *ShellDriver) BuildCommand(opts LaunchOptions) LaunchSpec {
	return buildShellCommand(defaultShellLookup(), opts)
}

func buildShellCommand(l shellLookup, opts LaunchOptions) LaunchSpec {
	bin, ok := resolveShell(l, opts.Shell)
	if !ok {
		if l.goos == "windows" {
			bin = "cmd.exe"
		} else {
			bin = "/bin/sh"
		}
	}

	// Interactive terminals get shell integration when the shell supports it (zsh, bash,
	// Git Bash, PowerShell); the launch shape is otherwise the same as below.
	if opts.ShellIntegration != nil && opts.Mode != "oneshot" {
		if args, env, ok := opts.ShellIntegration.Command(bin, opts.Prompt); ok {
			return LaunchSpec{Binary: bin, Args: args, Env: append(append([]string(nil), opts.Env...), env...), ShellIntegration: true}
		}
	}

	base := strings.ToLower(path.Base(strings.ReplaceAll(bin, `\`, "/")))
	baseNoExt := strings.TrimSuffix(base, ".exe")

	switch baseNoExt {
	case "cmd":
		if strings.TrimSpace(opts.Prompt) != "" && opts.Mode == "oneshot" {
			return LaunchSpec{Binary: bin, Args: []string{"/C", opts.Prompt}, Env: opts.Env}
		}
		if strings.TrimSpace(opts.Prompt) != "" {
			return LaunchSpec{Binary: bin, Args: []string{"/K", opts.Prompt}, Env: opts.Env}
		}
		return LaunchSpec{Binary: bin, Args: nil, Env: opts.Env}
	case "pwsh", "powershell":
		if strings.TrimSpace(opts.Prompt) != "" && opts.Mode == "oneshot" {
			return LaunchSpec{Binary: bin, Args: []string{"-NoLogo", "-Command", opts.Prompt}, Env: opts.Env}
		}
		if strings.TrimSpace(opts.Prompt) != "" {
			return LaunchSpec{Binary: bin, Args: []string{"-NoLogo", "-NoExit", "-Command", opts.Prompt}, Env: opts.Env}
		}
		return LaunchSpec{Binary: bin, Args: nil, Env: opts.Env}
	}

	// POSIX shells (bash, zsh, sh, fish, including Git Bash on Windows).
	env := opts.Env
	if l.goos == "windows" && baseNoExt == "bash" {
		env = append(append([]string(nil), opts.Env...), "CHERE_INVOKING=1")
	}
	if strings.TrimSpace(opts.Prompt) != "" && opts.Mode == "oneshot" {
		return LaunchSpec{
			Binary: bin,
			Args:   []string{"-c", "--", opts.Prompt},
			Env:    env,
		}
	}
	if strings.TrimSpace(opts.Prompt) != "" {
		// Interactive with a command (e.g. "Open terminal and sign in" running
		// `codex login --device-auth`): run it in a login shell, then hand the terminal
		// over to an interactive shell so the user sees the output and can continue.
		execBin := bin
		if l.goos == "windows" {
			execBin = strings.ReplaceAll(execBin, `\`, "/")
		}
		return LaunchSpec{
			Binary: bin,
			Args:   []string{"-l", "-c", opts.Prompt + "\nexec " + shellSingleQuote(execBin) + " -l"},
			Env:    env,
		}
	}
	return LaunchSpec{
		Binary: bin,
		Args:   []string{"-l"},
		Env:    env,
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
