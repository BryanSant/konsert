package chat

import (
	"context"
	"errors"
	"fmt"
	"github.com/BryanSant/syre/internal/worker/guard"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/digest"
	"github.com/BryanSant/syre/internal/worker/drivers"
)

const (
	// DefaultVerifyTimeout is the default timeout for a ticket verification command (20m).
	DefaultVerifyTimeout = 20 * time.Minute
	// defaultVerifyOutputCap caps combined stdout+stderr captured from a verification run.
	defaultVerifyOutputCap = 256 << 10 // 256 KiB
	verifyKillGrace        = 2 * time.Second
)

// VerifyResult holds the outcome of running a ticket's verification command.
type VerifyResult struct {
	Skipped  bool
	TimedOut bool
	Canceled bool
	Verify   *protocol.DigestVerify
	Output   []byte
	Tests    []string
	// Shed is set when the guardrails shed the command under memory pressure; the
	// result says nothing about the code then.
	Shed *guard.Evidence
}

type verifyShellEnv struct {
	goos          string
	detectGitBash func() (string, bool)
	lookPath      func(string) (string, error)
}

func defaultVerifyShellEnv() verifyShellEnv {
	return verifyShellEnv{
		goos:          runtime.GOOS,
		detectGitBash: drivers.DetectGitBash,
		lookPath:      exec.LookPath,
	}
}

// verifyShellCommand chooses the shell executable and flags to execute cmd on the host OS:
//   - Unix: sh -c <cmd>
//   - Windows: Git Bash (-c <cmd>) when installed, else pwsh.exe / powershell.exe
//     (-NoProfile -NonInteractive -Command <cmd>).
func verifyShellCommand(env verifyShellEnv, cmd string) (string, []string) {
	if env.goos == "windows" {
		if bashPath, ok := env.detectGitBash(); ok && bashPath != "" {
			return bashPath, []string{"-c", cmd}
		}
		shell := "powershell.exe"
		if p, err := env.lookPath("pwsh.exe"); err == nil && p != "" {
			shell = p
		} else if p, err := env.lookPath("powershell.exe"); err == nil && p != "" {
			shell = p
		}
		return shell, []string{"-NoProfile", "-NonInteractive", "-Command", cmd}
	}
	return "sh", []string{"-c", cmd}
}

// RunVerify runs cmdStr in dir with timeout, capturing combined stdout/stderr into a capped
// buffer and parsing test results via digest.ParseTestOutput.
// An empty cmdStr is skipped (Skipped=true, Verify=nil).
func RunVerify(ctx context.Context, dir, cmdStr string, timeout time.Duration) VerifyResult {
	cmdStr = strings.TrimSpace(cmdStr)
	if cmdStr == "" {
		return VerifyResult{Skipped: true}
	}
	if timeout <= 0 {
		timeout = DefaultVerifyTimeout
	}

	vctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin, args := verifyShellCommand(defaultVerifyShellEnv(), cmdStr)
	cmd, launch := guardedCommand(guardFrom(ctx), bin, args)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	setProcGroup(cmd)

	buf := NewTailBuffer(defaultVerifyOutputCap)
	cmd.Stdout = buf
	cmd.Stderr = buf

	if err := cmd.Start(); err != nil {
		msg := fmt.Sprintf("verify command failed to start: %v\n", err)
		return VerifyResult{
			Verify: &protocol.DigestVerify{Cmd: cmdStr, Exit: -1},
			Output: []byte(msg),
		}
	}

	gsess := attachGuard(launch, cmd)
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var (
		waitErr  error
		timedOut bool
		canceled bool
	)
	select {
	case waitErr = <-done:
	case <-vctx.Done():
		timedOut = errors.Is(vctx.Err(), context.DeadlineExceeded)
		canceled = !timedOut
		killGroup(cmd, false)
		select {
		case waitErr = <-done:
		case <-time.After(verifyKillGrace):
			killGroup(cmd, true)
			waitErr = <-done
		}
	}

	usage := gsess.Release(cmd.ProcessState)
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if timedOut || canceled {
		exitCode = -1
	} else if waitErr != nil && exitCode == 0 {
		exitCode = 1
	}

	outStr := RedactSecrets(buf.String())
	if timedOut {
		if outStr != "" && !strings.HasSuffix(outStr, "\n") {
			outStr += "\n"
		}
		outStr += fmt.Sprintf("verification command timed out after %s\n", timeout)
	}
	if usage.OOMKilled && usage.Shed == nil {
		if outStr != "" && !strings.HasSuffix(outStr, "\n") {
			outStr += "\n"
		}
		outStr += "verification command was killed by the out-of-memory killer\n"
	}

	dv := &protocol.DigestVerify{
		Cmd:  cmdStr,
		Exit: exitCode,
	}
	var failingTests []string
	if parsed, ok := digest.ParseTestOutput(outStr); ok {
		dv.Passed = parsed.Passed
		dv.Failed = parsed.Failed
		failingTests = parsed.Names
	}

	return VerifyResult{
		TimedOut: timedOut,
		Canceled: canceled,
		Verify:   dv,
		Output:   []byte(outStr),
		Tests:    failingTests,
		Shed:     usage.Shed,
	}
}

// VerifyStep is one verification command and the directory it runs in.
type VerifyStep struct {
	// RepoName labels output and the packet line. Empty means the ticket workdir.
	RepoName string
	Dir      string
	Cmd      string
}

// RunVerifySteps runs every step under one shared timeout. All steps run unless the
// timeout or a cancel stops the loop. Passed and failed counts are summed. On failure,
// Verify.Cmd and Exit are the first failing step's raw command and exit; a timeout
// replaces that with the timed-out step and stops. Cancel returns Canceled with no
// verify result. Output lines are prefixed with the repo name. A single successful
// step keeps its raw command; several successful steps are joined as "repo: cmd".
func RunVerifySteps(ctx context.Context, steps []VerifyStep, timeout time.Duration) VerifyResult {
	active := make([]VerifyStep, 0, len(steps))
	for _, s := range steps {
		s.Cmd = strings.TrimSpace(s.Cmd)
		s.RepoName = strings.TrimSpace(s.RepoName)
		if s.Cmd == "" {
			continue
		}
		active = append(active, s)
	}
	if len(active) == 0 {
		return VerifyResult{Skipped: true}
	}
	if timeout <= 0 {
		timeout = DefaultVerifyTimeout
	}
	vctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		passed, failed int
		tests          []string
		outputs        []string
		failCmd        string
		failExit       int
		haveFail       bool
		timedOut       bool
	)
	for _, step := range active {
		if err := vctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				timedOut = true
				failCmd = step.Cmd
				failExit = -1
				haveFail = true
				outputs = append(outputs, labelVerifyOutput(step.RepoName, fmt.Sprintf("verification command timed out after %s\n", timeout)))
				break
			}
			return VerifyResult{Canceled: true}
		}
		var res VerifyResult
		if strings.TrimSpace(step.Dir) == "" {
			msg := "verification directory is missing\n"
			if step.RepoName != "" {
				msg = fmt.Sprintf("worktree for %s is missing\n", step.RepoName)
			}
			res = VerifyResult{
				Verify: &protocol.DigestVerify{Cmd: step.Cmd, Exit: -1},
				Output: []byte(msg),
			}
		} else {
			res = RunVerify(vctx, step.Dir, step.Cmd, timeout)
		}
		if res.Canceled {
			return VerifyResult{Canceled: true}
		}
		if res.Shed != nil {
			return VerifyResult{Shed: res.Shed}
		}
		if res.Verify != nil {
			passed += res.Verify.Passed
			failed += res.Verify.Failed
		}
		tests = append(tests, res.Tests...)
		text := string(res.Output)
		if text == "" && res.Verify != nil && res.Verify.Exit != 0 {
			text = fmt.Sprintf("exit %d\n", res.Verify.Exit)
		}
		if text != "" {
			outputs = append(outputs, labelVerifyOutput(step.RepoName, text))
		}
		if res.TimedOut {
			timedOut = true
			failCmd = step.Cmd
			if res.Verify != nil {
				failExit = res.Verify.Exit
			} else {
				failExit = -1
			}
			haveFail = true
			break
		}
		if res.Verify != nil && res.Verify.Exit != 0 && !haveFail {
			failCmd = res.Verify.Cmd
			failExit = res.Verify.Exit
			haveFail = true
		}
	}

	cmd := successVerifyCmd(active)
	exit := 0
	if haveFail {
		cmd = failCmd
		exit = failExit
	}
	return VerifyResult{
		TimedOut: timedOut,
		Verify: &protocol.DigestVerify{
			Cmd:    cmd,
			Exit:   exit,
			Passed: passed,
			Failed: failed,
		},
		Output: []byte(strings.Join(outputs, "")),
		Tests:  tests,
	}
}

func successVerifyCmd(steps []VerifyStep) string {
	if len(steps) == 1 {
		return steps[0].Cmd
	}
	parts := make([]string, 0, len(steps))
	for _, s := range steps {
		if s.RepoName != "" {
			parts = append(parts, s.RepoName+": "+s.Cmd)
		} else {
			parts = append(parts, s.Cmd)
		}
	}
	return strings.Join(parts, "; ")
}

// labelVerifyOutput prefixes each non-empty line with "repo: " and keeps a trailing newline.
func labelVerifyOutput(repo, s string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || s == "" {
		return s
	}
	trailingNL := strings.HasSuffix(s, "\n")
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	var b strings.Builder
	for i, line := range lines {
		if line != "" {
			b.WriteString(repo)
			b.WriteString(": ")
			b.WriteString(line)
		}
		if i < len(lines)-1 || trailingNL {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// verifySteps pairs each verify command with its cwd: a named repo uses that
// worktree, and an empty name uses the ticket workdir.
func verifySteps(verifies []protocol.AgentChatVerify, worktrees []protocol.TaskWorktree, ticketDir string) []VerifyStep {
	byRepo := make(map[string]string, len(worktrees))
	for _, wt := range worktrees {
		if wt.RepoName != "" && wt.WorktreePath != "" {
			byRepo[wt.RepoName] = wt.WorktreePath
		}
	}
	steps := make([]VerifyStep, 0, len(verifies))
	for _, v := range verifies {
		cmd := strings.TrimSpace(v.Cmd)
		if cmd == "" {
			continue
		}
		name := strings.TrimSpace(v.RepoName)
		dir := ticketDir
		if name != "" {
			dir = byRepo[name]
		}
		steps = append(steps, VerifyStep{RepoName: name, Dir: dir, Cmd: cmd})
	}
	return steps
}
