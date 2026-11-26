package chat

// Small process helpers shared by the claude and codex engines (process-group launch,
// stderr tail, secret redaction, environment building, one-shot commands). They are
// private to these two engines on purpose; see procutil.go for the package-wide versions.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ccSpec describes one engine process launch.
type ccSpec struct {
	Binary string
	Args   []string
	Env    []string // full environment (KEY=VALUE)
	Dir    string
}

// ccProcess is a running engine process. Tests substitute an in-memory fake.
type ccProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// Wait blocks until the process exits and returns its exit code (-1 if killed or
	// unknown) and the redacted-on-read stderr tail.
	Wait() (exitCode int, stderrTail string)
	// Kill terminates the process group (TERM, then KILL after a grace period).
	Kill()
}

// ccLauncher starts an engine process.
type ccLauncher func(ctx context.Context, spec ccSpec) (ccProcess, error)

// ccRunner runs a short command to completion (auth status, version, ...) and returns
// stdout and the exit code. err is set when the command could not run at all.
type ccRunner func(ctx context.Context, spec ccSpec) (stdout []byte, exitCode int, err error)

// ccExecProcess is the real ccProcess backed by os/exec.
type ccExecProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	tail   *ccTail

	waitOnce sync.Once
	exitCode int
	done     chan struct{}
}

func ccExecLaunch(_ context.Context, spec ccSpec) (ccProcess, error) {
	cmd := exec.Command(spec.Binary, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	ccSetProcGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tail := &ccTail{max: 8 << 10}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &ccExecProcess{cmd: cmd, stdin: stdin, stdout: stdout, tail: tail, done: make(chan struct{})}
	go p.wait()
	return p, nil
}

func (p *ccExecProcess) wait() {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		p.exitCode = 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				p.exitCode = ee.ExitCode()
			} else {
				p.exitCode = -1
			}
		}
		close(p.done)
	})
}

func (p *ccExecProcess) Stdin() io.WriteCloser { return p.stdin }

// pid is the process (group leader) id, for suspend/resume.
func (p *ccExecProcess) pid() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}
func (p *ccExecProcess) Stdout() io.Reader { return p.stdout }

// StderrTail returns the retained stderr without waiting for the process.
// max <= 0 returns the whole tail (already bounded by the tail buffer).
func (p *ccExecProcess) StderrTail(max int) string {
	if p == nil || p.tail == nil {
		return ""
	}
	s := p.tail.String()
	if max > 0 && len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}

// ccProcessStderr reads a redacted stderr tail when the process keeps one.
// Fakes that only implement ccProcess return an empty string.
func ccProcessStderr(proc ccProcess, env map[string]string) string {
	if proc == nil {
		return ""
	}
	type tailer interface{ StderrTail(int) string }
	t, ok := proc.(tailer)
	if !ok {
		return ""
	}
	return strings.TrimSpace(ccRedact(t.StderrTail(0), env))
}

func (p *ccExecProcess) Wait() (int, string) {
	<-p.done
	return p.exitCode, p.tail.String()
}

func (p *ccExecProcess) Kill() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.stdin.Close()
	ccSignalGroup(p.cmd, false)
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		ccSignalGroup(p.cmd, true)
		<-p.done
	}
}

// ccExecRun is the real ccRunner.
func ccExecRun(ctx context.Context, spec ccSpec) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, spec.Binary, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	cmd.WaitDelay = 2 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.Bytes(), ee.ExitCode(), nil
		}
		return out.Bytes(), -1, err
	}
	return out.Bytes(), 0, nil
}

// ccTail keeps the last max bytes written to it.
type ccTail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *ccTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *ccTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// ccSecretKey reports whether an environment variable name likely holds a secret.
func ccSecretKey(k string) bool {
	u := strings.ToUpper(k)
	for _, s := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL", "AUTH"} {
		if strings.Contains(u, s) {
			return true
		}
	}
	return false
}

var ccSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`),
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-~+/=]{8,}`),
	regexp.MustCompile(`(?i)("?(?:access_?token|refresh_?token|id_?token|api_?key|authorization)"?\s*[:=]\s*"?)[^\s",}]{8,}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`),
}

// ccRedact removes secret values (the account's secret env values plus token-looking
// strings) from s.
func ccRedact(s string, env map[string]string) string {
	for k, v := range env {
		if len(v) >= 8 && ccSecretKey(k) {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	for _, re := range ccSecretPatterns {
		if re.NumSubexp() > 0 {
			s = re.ReplaceAllString(s, "${1}[redacted]")
		} else {
			s = re.ReplaceAllString(s, "[redacted]")
		}
	}
	return s
}

// ccTailLines returns the last n non-empty lines of s, at most max bytes.
func ccTailLines(s string, n, max int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.TrimSpace(strings.Join(lines, "\n"))
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// ccTruncate shortens s to at most max bytes (on a rune boundary), marking the cut.
func ccTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// ccBuildEnv merges the daemon environment with the account overrides. drop lists
// inherited variables to remove (unless the account sets them itself).
func ccBuildEnv(overrides map[string]string, drop []string) []string {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	for _, k := range drop {
		if _, ok := overrides[k]; !ok {
			delete(m, k)
		}
	}
	for k, v := range overrides {
		m[k] = v
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

// ccEnvValue returns the effective value of key for an account: its override, else the
// daemon's environment.
func ccEnvValue(overrides map[string]string, key string) string {
	if v, ok := overrides[key]; ok {
		return v
	}
	return os.Getenv(key)
}

// ccNonSecretEnv returns the account env entries that are safe to show (no secrets).
func ccNonSecretEnv(env map[string]string, keys ...string) map[string]string {
	var out map[string]string
	for _, k := range keys {
		if v, ok := env[k]; ok && v != "" && !ccSecretKey(k) {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

// ccLineReader returns a scanner for newline-delimited JSON with a large buffer.
// Lines are repaired to valid UTF-8 so one bad byte in agent output cannot make
// a whole protocol line undecodable.
func ccLineReader(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256<<10), 64<<20)
	sc.Split(scanValidUTF8Lines)
	return sc
}

// ccTitle turns "xhigh" into "Xhigh", "medium" into "Medium".
func ccTitle(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ccEffortLabel maps an effort id to a label and short label.
func ccEffortLabel(id string) (string, string) {
	switch id {
	case "none":
		return "None", "None"
	case "minimal":
		return "Minimal", "Min"
	case "low":
		return "Low", "Low"
	case "medium":
		return "Medium", "Med"
	case "high":
		return "High", "High"
	case "xhigh":
		return "Extra high", "XHigh"
	case "max":
		return "Max", "Max"
	case "ultra":
		return "Ultra", "Ultra"
	}
	return ccTitle(id), ""
}
