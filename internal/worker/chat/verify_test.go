package chat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

func TestVerifyShellCommand(t *testing.T) {
	t.Run("unix uses sh -c", func(t *testing.T) {
		bin, args := verifyShellCommand(verifyShellEnv{goos: "linux"}, "go test ./...")
		if bin != "sh" || !slices.Equal(args, []string{"-c", "go test ./..."}) {
			t.Fatalf("got (%q, %v), want (sh, [-c go test ./...])", bin, args)
		}
	})

	t.Run("windows prefers Git Bash", func(t *testing.T) {
		bin, args := verifyShellCommand(verifyShellEnv{
			goos:          "windows",
			detectGitBash: func() (string, bool) { return `C:\Program Files\Git\bin\bash.exe`, true },
			lookPath:      func(string) (string, error) { return "", errors.New("not found") },
		}, "go test ./...")
		if bin != `C:\Program Files\Git\bin\bash.exe` || !slices.Equal(args, []string{"-c", "go test ./..."}) {
			t.Fatalf("got (%q, %v)", bin, args)
		}
	})

	t.Run("windows falls back to pwsh then powershell", func(t *testing.T) {
		bin, args := verifyShellCommand(verifyShellEnv{
			goos:          "windows",
			detectGitBash: func() (string, bool) { return "", false },
			lookPath: func(name string) (string, error) {
				if name == "pwsh.exe" {
					return `C:\Program Files\PowerShell\7\pwsh.exe`, nil
				}
				return "", errors.New("not found")
			},
		}, "go test ./...")
		if bin != `C:\Program Files\PowerShell\7\pwsh.exe` || !slices.Equal(args, []string{"-NoProfile", "-NonInteractive", "-Command", "go test ./..."}) {
			t.Fatalf("got (%q, %v)", bin, args)
		}

		bin2, args2 := verifyShellCommand(verifyShellEnv{
			goos:          "windows",
			detectGitBash: func() (string, bool) { return "", false },
			lookPath:      func(string) (string, error) { return "", errors.New("not found") },
		}, "go test ./...")
		if bin2 != "powershell.exe" || !slices.Equal(args2, []string{"-NoProfile", "-NonInteractive", "-Command", "go test ./..."}) {
			t.Fatalf("got (%q, %v)", bin2, args2)
		}
	})
}

func TestRunVerifyMissingCommandSkipped(t *testing.T) {
	dir := t.TempDir()
	for _, cmd := range []string{"", "   \t\n  "} {
		res := RunVerify(context.Background(), dir, cmd, time.Minute)
		if !res.Skipped {
			t.Fatalf("cmd %q: expected Skipped=true", cmd)
		}
		if res.Verify != nil {
			t.Fatalf("cmd %q: expected Verify=nil, got %+v", cmd, res.Verify)
		}
	}
}

func TestRunVerifyPass(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh syntax")
	}
	dir := t.TempDir()
	cmd := "printf '%s\\n' '--- PASS: TestAlpha (0.00s)' '--- PASS: TestBeta (0.01s)' 'PASS'"
	res := RunVerify(context.Background(), dir, cmd, 10*time.Second)
	if res.Skipped || res.TimedOut {
		t.Fatalf("unexpected skipped=%v timedOut=%v", res.Skipped, res.TimedOut)
	}
	if res.Verify == nil {
		t.Fatalf("expected Verify populated")
	}
	if res.Verify.Cmd != cmd || res.Verify.Exit != 0 || res.Verify.Passed != 2 || res.Verify.Failed != 0 {
		t.Fatalf("unexpected Verify: %+v", res.Verify)
	}
}

func TestRunVerifyFailGoTestParsed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh syntax")
	}
	dir := t.TempDir()
	cmd := "printf '%s\\n' '=== RUN   TestOK' '--- PASS: TestOK (0.00s)' '=== RUN   TestBroken' '    foo_test.go:12: expected 1, got 2' '--- FAIL: TestBroken (0.01s)' '=== RUN   TestAlsoBroken' '--- FAIL: TestAlsoBroken (0.00s)' 'FAIL\texample.com/pkg\t0.02s' && exit 1"
	res := RunVerify(context.Background(), dir, cmd, 10*time.Second)
	if res.Skipped || res.TimedOut {
		t.Fatalf("unexpected skipped=%v timedOut=%v", res.Skipped, res.TimedOut)
	}
	if res.Verify == nil {
		t.Fatalf("expected Verify populated")
	}
	if res.Verify.Exit != 1 || res.Verify.Passed != 1 || res.Verify.Failed != 2 {
		t.Fatalf("unexpected Verify counts: %+v", res.Verify)
	}
	if !slices.Equal(res.Tests, []string{"TestBroken", "TestAlsoBroken"}) {
		t.Fatalf("failing tests = %v, want [TestBroken TestAlsoBroken]", res.Tests)
	}
	if !strings.Contains(string(res.Output), "expected 1, got 2") {
		t.Fatalf("expected failure detail in output, got %q", string(res.Output))
	}
}

func TestRunVerifyTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh sleep")
	}
	dir := t.TempDir()
	start := time.Now()
	res := RunVerify(context.Background(), dir, "echo starting && sleep 10", 150*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("RunVerify took too long on timeout: %v", elapsed)
	}
	if !res.TimedOut {
		t.Fatalf("expected TimedOut=true, got %+v", res)
	}
	if res.Verify == nil || res.Verify.Exit != -1 {
		t.Fatalf("expected Verify.Exit=-1 on timeout, got %+v", res.Verify)
	}
	if !strings.Contains(string(res.Output), "timed out") {
		t.Fatalf("expected timeout note in output, got %q", string(res.Output))
	}
}

func TestManagerTurnEndVerifyDigest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh syntax")
	}
	dir := t.TempDir()

	var (
		mu     sync.Mutex
		events []protocol.AgentChatEvent
		doneCh = make(chan protocol.AgentChatEvent, 8)
	)
	reg := NewAccountRegistry(AccountOptions{
		EnableMock: true,
		LookPath:   func(string) (string, bool) { return "", false },
	})
	mgr := NewManager(ManagerOptions{
		Engines:  map[string]Engine{MockAgent: NewMockEngine()},
		Accounts: reg,
		Sink: func(ev protocol.AgentChatEvent) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
			if ev.Kind == protocol.EventTurnEnd {
				doneCh <- ev
			}
		},
		MachineName: "test-box",
	})
	defer mgr.Shutdown(context.Background())

	waitTurnEnd := func(chatID string) protocol.AgentChatEvent {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case ev := <-doneCh:
				if ev.ChatID == chatID {
					return ev
				}
			case <-timeout:
				t.Fatalf("timed out waiting for turn_end on %s", chatID)
			}
		}
	}

	// 1. No verify command -> skipped, Verify nil, status review.
	mgr.Open(protocol.AgentChatOpen{
		ChatID:    "c-skip",
		Agent:     MockAgent,
		Cwd:       dir,
		TicketSID: "SYR-SKIP1",
	})
	mgr.Prompt(protocol.AgentChatPrompt{ChatID: "c-skip", TurnID: "t1", Text: "hello"})
	evSkip := waitTurnEnd("c-skip")
	if evSkip.Digest == nil || evSkip.Digest.Verify != nil || evSkip.Digest.Status != protocol.DigestStatusReview {
		t.Fatalf("skip digest = %+v", evSkip.Digest)
	}

	// 2. Failing verify command -> status failed, Failure.Category=verify, tests + tail populated.
	failCmd := "printf '%s\\n' '--- FAIL: TestGate (0.00s)' '    gate_test.go:9: boom' 'FAIL\texample.com/gate\t0.01s' && exit 1"
	mgr.Open(protocol.AgentChatOpen{
		ChatID:    "c-fail",
		Agent:     MockAgent,
		Cwd:       dir,
		TicketSID: "SYR-FAIL1",
		Verifies:  []protocol.AgentChatVerify{{Cmd: failCmd}},
	})
	mgr.Prompt(protocol.AgentChatPrompt{ChatID: "c-fail", TurnID: "t1", Text: "hello"})
	evFail := waitTurnEnd("c-fail")
	if evFail.Digest == nil {
		t.Fatalf("expected digest on failing verify")
	}
	d := evFail.Digest
	if d.Status != protocol.DigestStatusFailed || d.Attempt != 1 {
		t.Fatalf("expected failed attempt 1, got status=%q attempt=%d", d.Status, d.Attempt)
	}
	if d.Verify == nil || d.Verify.Cmd != failCmd || d.Verify.Exit != 1 || d.Verify.Failed != 1 {
		t.Fatalf("unexpected Verify: %+v", d.Verify)
	}
	if d.Failure == nil || d.Failure.Category != protocol.ErrKindVerify {
		t.Fatalf("expected Failure.Category=verify, got %+v", d.Failure)
	}
	if !slices.Equal(d.Failure.Tests, []string{"TestGate"}) {
		t.Fatalf("expected failing test [TestGate], got %v", d.Failure.Tests)
	}
	if len(d.Failure.Tail) == 0 {
		t.Fatalf("expected non-empty failure tail")
	}

	// 3. Passing verify command -> status review, Verify exit 0.
	passCmd := "printf '%s\\n' '--- PASS: TestGate (0.00s)' 'PASS'"
	mgr.Open(protocol.AgentChatOpen{
		ChatID:    "c-pass",
		Agent:     MockAgent,
		Cwd:       dir,
		TicketSID: "SYR-PASS1",
		Verifies:  []protocol.AgentChatVerify{{Cmd: passCmd}},
	})
	mgr.Prompt(protocol.AgentChatPrompt{ChatID: "c-pass", TurnID: "t1", Text: "hello"})
	evPass := waitTurnEnd("c-pass")
	if evPass.Digest == nil || evPass.Digest.Status != protocol.DigestStatusReview {
		t.Fatalf("expected review digest, got %+v", evPass.Digest)
	}
	if evPass.Digest.Verify == nil || evPass.Digest.Verify.Exit != 0 || evPass.Digest.Verify.Passed != 1 {
		t.Fatalf("unexpected passing Verify: %+v", evPass.Digest.Verify)
	}
}

func TestRunVerifyStepsAggregates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh syntax")
	}
	dir := t.TempDir()
	pass := VerifyStep{
		RepoName: "api",
		Dir:      dir,
		Cmd:      "printf '%s\\n' '--- PASS: TestOK (0.00s)' 'PASS'",
	}
	failCmd := "printf '%s\\n' '--- FAIL: TestBad (0.00s)' 'FAIL\texample.com/web\t0.01s' && exit 1"
	fail := VerifyStep{RepoName: "web", Dir: dir, Cmd: failCmd}
	res := RunVerifySteps(context.Background(), []VerifyStep{pass, fail}, time.Minute)
	if res.Canceled || res.TimedOut || res.Skipped || res.Verify == nil {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Verify.Cmd != failCmd || res.Verify.Exit != 1 {
		t.Fatalf("Verify cmd/exit = %q/%d, want failing step", res.Verify.Cmd, res.Verify.Exit)
	}
	if res.Verify.Passed != 1 || res.Verify.Failed != 1 {
		t.Fatalf("counts = passed %d failed %d", res.Verify.Passed, res.Verify.Failed)
	}
	out := string(res.Output)
	if !strings.Contains(out, "api: --- PASS: TestOK") || !strings.Contains(out, "web: --- FAIL: TestBad") {
		t.Fatalf("output not labelled:\n%s", out)
	}
	if !slices.Equal(res.Tests, []string{"TestBad"}) {
		t.Fatalf("tests = %v", res.Tests)
	}
}

func TestManagerVerifyRunsInBindingWorktrees(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh syntax")
	}
	root := t.TempDir()
	ticketDir := filepath.Join(root, "ticket")
	apiDir := filepath.Join(ticketDir, "api")
	webDir := filepath.Join(ticketDir, "web")
	for _, d := range []string{apiDir, webDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	doneCh := make(chan protocol.AgentChatEvent, 4)
	reg := NewAccountRegistry(AccountOptions{
		EnableMock: true,
		LookPath:   func(string) (string, bool) { return "", false },
	})
	mgr := NewManager(ManagerOptions{
		Engines:  map[string]Engine{MockAgent: NewMockEngine()},
		Accounts: reg,
		Sink: func(ev protocol.AgentChatEvent) {
			if ev.Kind == protocol.EventTurnEnd {
				doneCh <- ev
			}
		},
		MachineName: "test-box",
		ProvisionBindings: func(context.Context, string, string, []protocol.TaskRepoBinding) (ProvisionedWorktrees, error) {
			return ProvisionedWorktrees{
				Workdir: ticketDir,
				Worktrees: []protocol.TaskWorktree{
					{RepoName: "api", WorktreePath: apiDir},
					{RepoName: "web", WorktreePath: webDir},
				},
			}, nil
		},
	})
	defer mgr.Shutdown(context.Background())

	mgr.Open(protocol.AgentChatOpen{
		ChatID:    "c-cwd",
		Agent:     MockAgent,
		TicketSID: "SYR-CWD",
		Bindings: []protocol.TaskRepoBinding{
			{RepoName: "api"},
			{RepoName: "web"},
		},
		Verifies: []protocol.AgentChatVerify{
			{RepoName: "api", Cmd: "pwd > cwd.txt"},
			{RepoName: "web", Cmd: "pwd > cwd.txt"},
			{Cmd: "pwd > root.txt"},
		},
	})
	mgr.Prompt(protocol.AgentChatPrompt{ChatID: "c-cwd", TurnID: "t1", Text: "hello"})
	var ev protocol.AgentChatEvent
	select {
	case ev = <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for turn_end")
	}
	if ev.Digest == nil || ev.Digest.Verify == nil || ev.Digest.Verify.Exit != 0 {
		t.Fatalf("verify digest = %+v", ev.Digest)
	}
	if !strings.Contains(ev.Digest.Verify.Cmd, "api: pwd > cwd.txt") || !strings.Contains(ev.Digest.Verify.Cmd, "web: pwd > cwd.txt") {
		t.Fatalf("combined cmd = %q", ev.Digest.Verify.Cmd)
	}
	sameDir(t, readTrim(t, filepath.Join(apiDir, "cwd.txt")), apiDir)
	sameDir(t, readTrim(t, filepath.Join(webDir, "cwd.txt")), webDir)
	sameDir(t, readTrim(t, filepath.Join(ticketDir, "root.txt")), ticketDir)

	failCmd := "printf '%s\\n' '--- FAIL: TestGate (0.00s)' '    gate_test.go:9: boom' 'FAIL\texample.com/gate\t0.01s' && exit 1"
	mgr.Open(protocol.AgentChatOpen{
		ChatID:    "c-label",
		Agent:     MockAgent,
		TicketSID: "SYR-LABEL",
		Bindings:  []protocol.TaskRepoBinding{{RepoName: "web"}},
		Verifies:  []protocol.AgentChatVerify{{RepoName: "web", Cmd: failCmd}},
	})
	mgr.Prompt(protocol.AgentChatPrompt{ChatID: "c-label", TurnID: "t1", Text: "hello"})
	select {
	case ev = <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for labelled failure")
	}
	d := ev.Digest
	if d == nil || d.Verify == nil || d.Verify.Cmd != failCmd || d.Verify.Exit != 1 {
		t.Fatalf("labelled verify = %+v", d)
	}
	if d.Failure == nil || !slices.Equal(d.Failure.Tests, []string{"TestGate"}) {
		t.Fatalf("failure = %+v", d.Failure)
	}
	labelled := false
	for _, line := range d.Failure.Tail {
		if strings.HasPrefix(line, "web: ") {
			labelled = true
			break
		}
	}
	if !labelled {
		t.Fatalf("failure tail not labelled with repo: %v", d.Failure.Tail)
	}
}

func readTrim(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func sameDir(t *testing.T, got, want string) {
	t.Helper()
	g, err1 := filepath.EvalSymlinks(got)
	w, err2 := filepath.EvalSymlinks(want)
	if err1 != nil || err2 != nil || g != w {
		t.Fatalf("dir %q (%v) != %q (%v)", got, err1, want, err2)
	}
}
