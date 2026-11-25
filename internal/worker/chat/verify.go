package chat

import (
	"context"
	"errors"
	"fmt"
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
	cmd := exec.Command(bin, args...)
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
	}
}
