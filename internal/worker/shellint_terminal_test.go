package worker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

// terminalHarness runs a real Konsert terminal (shell task) and records what the
// browser would see and the operator-command events.
type terminalHarness struct {
	t      *testing.T
	runner *Runner
	taskID string

	mu     sync.Mutex
	stream bytes.Buffer
	events []protocol.OperatorCommandPayload
	final  chan protocol.TaskStatusPayload
	done   chan struct{} // closed once the terminal has ended
	once   sync.Once
}

func startTerminal(t *testing.T, workdir, prompt string) *terminalHarness {
	t.Helper()
	h := &terminalHarness{t: t, taskID: ids.UUID(), final: make(chan protocol.TaskStatusPayload, 1), done: make(chan struct{})}
	h.runner = NewRunner(drivers.DefaultRegistry, false, nil, RunnerCallbacks{
		OnTaskStatus: func(p protocol.TaskStatusPayload) {
			if protocol.IsTerminalTaskStatus(p.Status) {
				select {
				case h.final <- p:
				default:
				}
				h.once.Do(func() { close(h.done) })
			}
		},
		OnPtyFrame: func(_ byte, _ string, data []byte) {
			h.mu.Lock()
			h.stream.Write(data)
			h.mu.Unlock()
		},
		OnOperatorCommand: func(p protocol.OperatorCommandPayload) {
			h.mu.Lock()
			h.events = append(h.events, p)
			h.mu.Unlock()
		},
	}, nil)
	if err := h.runner.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID:         h.taskID,
		Agent:          drivers.ShellDriverName,
		Workdir:        workdir,
		Prompt:         prompt,
		Mode:           protocol.TaskModeInteractive,
		PermissionMode: protocol.PermissionFullAccess,
		Cols:           200,
		Rows:           50,
	}); err != nil {
		t.Fatalf("start terminal: %v", err)
	}
	h.runner.SubscribePty(h.taskID)
	t.Cleanup(func() {
		select {
		case <-h.done:
			return
		default:
		}
		// Leave the shell politely; cancel if it does not go.
		_ = h.runner.WriteStdin(h.taskID, []byte("exit\r"))
		select {
		case <-h.done:
			return
		case <-time.After(5 * time.Second):
		}
		_ = h.runner.CancelTask(h.taskID)
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
		}
	})
	return h
}

func (h *terminalHarness) snapshot() (string, []protocol.OperatorCommandPayload) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stream.String(), append([]protocol.OperatorCommandPayload(nil), h.events...)
}

func (h *terminalHarness) waitFor(what string, timeout time.Duration, cond func(stream string, evs []protocol.OperatorCommandPayload) bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		s, evs := h.snapshot()
		if cond(s, evs) {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s\nstream tail: %q\nevents: %+v", what, tail(s, 1500), evs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// waitPrompt waits until the stream holds n prompt-start marks.
func (h *terminalHarness) waitPrompt(n int) {
	h.t.Helper()
	h.waitFor("prompt", 30*time.Second, func(s string, _ []protocol.OperatorCommandPayload) bool {
		return strings.Count(s, "\x1b]133;A") >= n
	})
}

// run types line, waits for its end event and returns the start and end events.
func (h *terminalHarness) run(line string) (start, end protocol.OperatorCommandPayload) {
	h.t.Helper()
	_, before := h.snapshot()
	if err := h.runner.WriteStdin(h.taskID, []byte(line+"\r")); err != nil {
		h.t.Fatalf("write %q: %v", line, err)
	}
	h.waitFor("end of "+line, 30*time.Second, func(_ string, evs []protocol.OperatorCommandPayload) bool {
		return len(evs) >= len(before)+2 && evs[len(evs)-1].EndedAt != nil
	})
	_, evs := h.snapshot()
	evs = evs[len(before):]
	if len(evs) != 2 {
		h.t.Fatalf("%q: want 2 events (start, end), got %+v", line, evs)
	}
	start, end = evs[0], evs[1]
	if start.EndedAt != nil || start.ExitCode != nil {
		h.t.Fatalf("%q: start event carries an end: %+v", line, start)
	}
	if start.CommandID == "" || start.CommandID != end.CommandID || start.TaskID != h.taskID {
		h.t.Fatalf("%q: ids differ: %+v / %+v", line, start, end)
	}
	if end.EndedAt.Before(start.StartedAt) {
		h.t.Fatalf("%q: ended before it started", line)
	}
	return start, end
}

func exitOf(t *testing.T, p protocol.OperatorCommandPayload) int {
	t.Helper()
	if p.ExitCode == nil {
		t.Fatalf("no exit code: %+v", p)
	}
	return *p.ExitCode
}

// sameDir compares paths after resolving symlinks (/var -> /private/var on macOS).
func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return strings.EqualFold(ra, rb)
}

// fakeHome points HOME at a fresh directory with the given startup files and hides the
// developer's ZDOTDIR, so the shell under test starts from a known configuration.
func fakeHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	if old, ok := os.LookupEnv("ZDOTDIR"); ok {
		os.Unsetenv("ZDOTDIR")
		t.Cleanup(func() { os.Setenv("ZDOTDIR", old) })
	}
	return home
}

func TestShellIntegrationUnixShells(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shells")
	}
	type shellCase struct{ name, bin string }
	var cases []shellCase
	seen := map[string]bool{}
	for _, name := range []string{"zsh", "bash", "/bin/bash"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			if seen[real] {
				continue
			}
			seen[real] = true
		}
		cases = append(cases, shellCase{name, p})
	}
	if len(cases) == 0 {
		t.Skip("neither zsh nor bash is installed")
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.name, "/", "_"), func(t *testing.T) {
			// The user's startup files must still run (and keep running first).
			rc := "export KONSERT_TEST_RC=sourced\nPS1='$ '\nPROMPT_COMMAND='KONSERT_TEST_PC=1'\n"
			fakeHome(t, map[string]string{
				".zshrc":        "export KONSERT_TEST_RC=sourced\nPS1='$ '\n",
				".zshenv":       "export KONSERT_TEST_ENV=1\n",
				".bash_profile": rc,
			})
			t.Setenv("SHELL", c.bin)
			workdir := t.TempDir()
			sub := filepath.Join(workdir, "sub dir")
			if err := os.Mkdir(sub, 0o700); err != nil {
				t.Fatal(err)
			}

			h := startTerminal(t, workdir, "")
			h.waitPrompt(1)

			start, end := h.run("true")
			if start.Command != "true" || exitOf(t, end) != 0 || !sameDir(start.Cwd, workdir) {
				t.Fatalf("true: %+v / %+v", start, end)
			}
			_, end = h.run("false")
			if exitOf(t, end) != 1 {
				t.Fatalf("false: %+v", end)
			}
			start, end = h.run("sh -c 'exit 7'")
			if start.Command != "sh -c 'exit 7'" || exitOf(t, end) != 7 {
				t.Fatalf("exit 7: %+v / %+v", start, end)
			}
			start, end = h.run(`test "$KONSERT_TEST_RC" = sourced && test -z "$KONSERT_SHELL_NONCE" && test -z "${ZDOTDIR-}"`)
			if exitOf(t, end) != 0 {
				t.Fatalf("user startup file not sourced, nonce leaked to the environment, or ZDOTDIR not restored: %+v", end)
			}
			_ = start
			start, end = h.run(`cd "sub dir"`)
			if exitOf(t, end) != 0 || !sameDir(start.Cwd, workdir) {
				t.Fatalf("cd: %+v / %+v", start, end)
			}
			start, _ = h.run("mytool --password=hunter2 ; echo 'é ✓;\\'")
			if !sameDir(start.Cwd, sub) {
				t.Fatalf("cwd after cd = %q, want %q", start.Cwd, sub)
			}
			if start.Command != "mytool --password=… ; echo 'é ✓;\\'" {
				t.Fatalf("redacted command = %q", start.Command)
			}
			full, ok := h.runner.CommandHistory().Get(start.CommandID)
			if !ok || full.Command != "mytool --password=hunter2 ; echo 'é ✓;\\'" {
				t.Fatalf("full command in daemon memory = %+v (%v)", full, ok)
			}
			// A multi-line command is reported as typed.
			start, end = h.run("for i in 1 2; do\necho $i; done")
			if !strings.Contains(start.Command, "for i in 1 2; do") || exitOf(t, end) != 0 {
				t.Fatalf("multi-line: %+v / %+v", start, end)
			}

			// Empty lines produce no events.
			_, before := h.snapshot()
			_ = h.runner.WriteStdin(h.taskID, []byte("\r"))
			prompts := strings.Count(func() string { s, _ := h.snapshot(); return s }(), "\x1b]133;A")
			h.waitPrompt(prompts + 1)
			if _, evs := h.snapshot(); len(evs) != len(before) {
				t.Fatalf("empty line produced events: %+v", evs[len(before):])
			}

			// Konsert-private marks never reach the browser; standard OSC 133 does.
			s, _ := h.snapshot()
			if strings.Contains(s, "633;") {
				t.Fatalf("browser stream contains OSC 633: %q", tail(s, 800))
			}
			if !strings.Contains(s, "\x1b]133;D;7\a") {
				t.Fatalf("browser stream lacks OSC 133;D")
			}

			// `exit 3` ends the terminal; the command is closed with the shell's status.
			_, before = h.snapshot()
			_ = h.runner.WriteStdin(h.taskID, []byte("exit 3\r"))
			select {
			case final := <-h.final:
				if final.ExitCode == nil || *final.ExitCode != 3 {
					t.Fatalf("terminal exit: %+v", final)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("terminal did not exit")
			}
			_, evs := h.snapshot()
			evs = evs[len(before):]
			if len(evs) != 2 || evs[0].Command != "exit 3" || exitOf(t, evs[1]) != 3 {
				t.Fatalf("exit events: %+v", evs)
			}
		})
	}
}

// A terminal that runs a command first (e.g. "Open terminal and sign in") still runs
// it, then becomes an integrated interactive shell.
func TestShellIntegrationRunsPromptThenIntegratedShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shells")
	}
	for _, name := range []string{"zsh", "bash"} {
		bin, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		t.Run(name, func(t *testing.T) {
			fakeHome(t, map[string]string{".zshrc": "PS1='$ '\n", ".bash_profile": "PS1='$ '\n"})
			t.Setenv("SHELL", bin)
			h := startTerminal(t, t.TempDir(), "echo relogin-ran")
			h.waitFor("prompt command output", 30*time.Second, func(s string, _ []protocol.OperatorCommandPayload) bool {
				return strings.Contains(s, "relogin-ran")
			})
			h.waitPrompt(1)
			start, end := h.run("sh -c 'exit 4'")
			if start.Command != "sh -c 'exit 4'" || exitOf(t, end) != 4 {
				t.Fatalf("%+v / %+v", start, end)
			}
		})
	}
}

func TestShellIntegrationPowerShell(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell terminals are tested on Windows")
	}
	// The shell driver prefers pwsh.exe; with PATH limited to the system directories it
	// falls back to Windows PowerShell 5.1 (powershell.exe).
	sysRoot := os.Getenv("SystemRoot")
	for _, c := range []struct{ name, path string }{
		{"pwsh", os.Getenv("PATH")},
		{"powershell", filepath.Join(sysRoot, "System32") + ";" + filepath.Join(sysRoot, "System32", "WindowsPowerShell", "v1.0")},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PATH", c.path)
			bin, ok := (&drivers.ShellDriver{}).Detect()
			if !ok || !strings.EqualFold(strings.TrimSuffix(filepath.Base(bin), ".exe"), c.name) {
				t.Skipf("%s not installed (detected %q)", c.name, bin)
			}
			testPowerShellTerminal(t)
		})
	}
}

func testPowerShellTerminal(t *testing.T) {
	workdir := t.TempDir()
	// The terminal's start-up command (not typed, so not reported) keeps the test's
	// commands out of the user's PSReadLine history file.
	h := startTerminal(t, workdir, "Set-PSReadLineOption -HistorySaveStyle SaveNothing")
	h.waitPrompt(1)

	start, end := h.run("Write-Output hi")
	if start.Command != "Write-Output hi" || exitOf(t, end) != 0 || !sameDir(start.Cwd, workdir) {
		t.Fatalf("Write-Output: %+v / %+v", start, end)
	}
	_, end = h.run("cmd /c exit 7")
	if exitOf(t, end) != 7 {
		t.Fatalf("cmd /c exit 7: %+v", end)
	}
	_, end = h.run("Get-Item C:\\definitely\\missing -ErrorAction Stop")
	if exitOf(t, end) != 1 {
		t.Fatalf("failing cmdlet after a failed native command: %+v", end)
	}
	_, end = h.run("cmd /c exit 7")
	if exitOf(t, end) != 7 {
		t.Fatalf("repeated native exit code: %+v", end)
	}
	start, end = h.run("$env:FOO_TOKEN = 'hunter2'; if ($env:KONSERT_SHELL_NONCE) { exit 9 }")
	if start.Command != "$env:FOO_TOKEN = …; if ($env:KONSERT_SHELL_NONCE) { exit 9 }" || exitOf(t, end) != 0 {
		t.Fatalf("redaction / nonce: %+v / %+v", start, end)
	}
	if err := os.Mkdir(filepath.Join(workdir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.run("Set-Location sub")
	start, _ = h.run("Write-Output 'é ✓;\\'")
	if !sameDir(start.Cwd, filepath.Join(workdir, "sub")) || start.Command != "Write-Output 'é ✓;\\'" {
		t.Fatalf("cwd/unicode: %+v", start)
	}
	s, _ := h.snapshot()
	if strings.Contains(s, "633;") {
		t.Fatalf("browser stream contains OSC 633: %q", tail(s, 800))
	}
}
