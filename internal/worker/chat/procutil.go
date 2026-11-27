package chat

// Shared process helpers for chat engines: launching an agent CLI in its own process
// group with the account's environment, capturing a bounded stderr tail, killing and
// reaping the whole group, and redacting secrets from anything that leaves the worker.

import (
	"context"
	"errors"
	"fmt"
	"github.com/BryanSant/syre/internal/worker/guard"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProcSpec describes one engine process launch.
type ProcSpec struct {
	Binary string
	Args   []string
	Dir    string
	// Env overrides are applied on top of the daemon's environment (os.Environ).
	Env map[string]string
	// StderrTail is the number of stderr bytes kept for error reports (default 16 KiB).
	StderrTail int
	// Guard makes the launch a managed session (engines set it; probes don't).
	Guard *GuardSpec
}

// Proc is a running engine process with piped stdin/stdout and a captured stderr tail.
// The process leads its own process group (a job-like group on Windows), so Kill stops
// every helper it spawned.
type Proc struct {
	Cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr *TailBuffer

	secrets  []string
	guard    *guard.Session
	done     chan struct{}
	waitErr  error
	exitCode int
	killOnce sync.Once
}

// StartProc launches spec. A missing binary yields an error for which IsNotInstalled is true.
func StartProc(spec ProcSpec) (*Proc, error) {
	if spec.Binary == "" {
		return nil, &NotInstalledError{Binary: "(empty)"}
	}
	cmd, launch := guardedCommand(spec.Guard, spec.Binary, spec.Args)
	cmd.Dir = spec.Dir
	cmd.Env = MergeEnv(os.Environ(), spec.Env)
	setProcGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	n := spec.StderrTail
	if n <= 0 {
		n = 16 << 10
	}
	tail := NewTailBuffer(n)
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		if isNotFound(err) {
			return nil, &NotInstalledError{Binary: spec.Binary, Err: err}
		}
		return nil, err
	}
	p := &Proc{Cmd: cmd, Stdin: stdin, Stdout: stdout, Stderr: tail, secrets: EnvSecrets(spec.Env), guard: attachGuard(launch, cmd), done: make(chan struct{}), exitCode: -1}
	go func() {
		p.waitErr = cmd.Wait()
		if cmd.ProcessState != nil {
			p.exitCode = cmd.ProcessState.ExitCode()
		}
		p.guard.Release(cmd.ProcessState)
		close(p.done)
	}()
	return p, nil
}

// Done is closed once the process has exited and been reaped.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Exited reports whether the process has exited.
func (p *Proc) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// ExitCode is the exit code once Done is closed (-1 when killed by a signal or unknown).
func (p *Proc) ExitCode() int {
	<-p.done
	return p.exitCode
}

// WaitErr is cmd.Wait's error once Done is closed.
func (p *Proc) WaitErr() error {
	<-p.done
	return p.waitErr
}

// StderrTail returns the redacted tail of stderr (at most max bytes, from the end).
func (p *Proc) StderrTail(max int) string {
	s := p.Stderr.String()
	if max > 0 && len(s) > max {
		s = s[len(s)-max:]
		if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
			s = s[i+1:]
		}
	}
	return RedactSecrets(strings.TrimSpace(s), p.secrets...)
}

// Secrets lists the secret values from the launch environment (for redaction).
func (p *Proc) Secrets() []string { return p.secrets }

// Kill closes stdin, asks the process group to terminate, escalates to a hard kill after
// grace, and waits until the leader is reaped. Safe to call more than once.
func (p *Proc) Kill(grace time.Duration) {
	p.killOnce.Do(func() {
		_ = p.Stdin.Close()
		if p.Exited() {
			// The leader is gone; stragglers in its group still get a hard kill.
			killGroup(p.Cmd, true)
			return
		}
		killGroup(p.Cmd, false)
		select {
		case <-p.done:
		case <-time.After(grace):
		}
		killGroup(p.Cmd, true)
		p.guard.Kill()
	})
	<-p.done
}

// NotInstalledError reports a missing agent binary.
type NotInstalledError struct {
	Binary string
	Err    error
}

func (e *NotInstalledError) Error() string {
	return fmt.Sprintf("%s is not installed or not executable", e.Binary)
}
func (e *NotInstalledError) Unwrap() error { return e.Err }

// IsNotInstalled reports whether err means the agent binary is missing.
func IsNotInstalled(err error) bool {
	var ni *NotInstalledError
	return errors.As(err, &ni) || isNotFound(err)
}

func isNotFound(err error) bool {
	// A missing working directory also surfaces as fs.ErrNotExist (from chdir); that is
	// not a missing binary.
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Op == "chdir" {
		return false
	}
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

// MergeEnv applies overrides (KEY=VALUE) on top of base, replacing existing keys.
// On Windows keys compare case-insensitively.
func MergeEnv(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}
	norm := func(k string) string {
		if envCaseInsensitive {
			return strings.ToUpper(k)
		}
		return k
	}
	over := make(map[string]bool, len(overrides))
	for k := range overrides {
		over[norm(k)] = true
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if over[norm(k)] {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+overrides[k])
	}
	return out
}

// TailBuffer is an io.Writer keeping the last N bytes written. Safe for concurrent use.
type TailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

// NewTailBuffer keeps the last max bytes.
func NewTailBuffer(max int) *TailBuffer { return &TailBuffer{max: max} }

func (t *TailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(b), nil
}

func (t *TailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// secretPatterns match credentials that commonly leak into CLI stderr and error text.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)([?&](?:key|api_key|apikey|access_token|refresh_token|token|code)=)[^&\s"']+`),
	regexp.MustCompile(`(?i)(\b(?:api_?key|access_token|refresh_token|id_token|token|secret|password)=)[^&\s"']{4,}`),
	regexp.MustCompile(`(?i)((?:x-goog-api-key|x-api-key|authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret|password)["']?\s*[:=]\s*["']?)[^\s"',}]{4,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`\bxai-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}`),
	regexp.MustCompile(`\bya29\.[0-9A-Za-z_-]{20,}`),
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), // JWT
}

const redactedMark = "[REDACTED]"

// RedactSecrets removes the given secret values and anything that looks like a credential.
func RedactSecrets(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec = strings.TrimSpace(sec); len(sec) >= 6 {
			s = strings.ReplaceAll(s, sec, redactedMark)
		}
	}
	for _, re := range secretPatterns {
		if re.NumSubexp() > 0 {
			s = re.ReplaceAllString(s, "${1}"+redactedMark)
		} else {
			s = re.ReplaceAllString(s, redactedMark)
		}
	}
	return s
}

var secretEnvKey = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSWORD|PASSWD|API_?KEY|CREDENTIAL|AUTH)`)

// EnvSecrets returns the values of env entries whose names look secret.
func EnvSecrets(env map[string]string) []string {
	var out []string
	for k, v := range env {
		if v != "" && secretEnvKey.MatchString(k) {
			out = append(out, v)
		}
	}
	return out
}

// ScratchDir creates a private temporary directory for probes, preferring /var/tmp on
// Unix (disk-backed) over the RAM-backed /tmp.
func ScratchDir(prefix string) (string, error) {
	base := os.TempDir()
	if st, err := os.Stat("/var/tmp"); err == nil && st.IsDir() && !envCaseInsensitive {
		base = "/var/tmp"
	}
	return os.MkdirTemp(base, prefix)
}

// RunQuick runs a short CLI command (status checks) with the account env and returns
// combined stdout (stderr discarded). The process group is killed on timeout.
func RunQuick(ctx context.Context, binary string, args []string, env map[string]string, timeout time.Duration) (string, int, error) {
	p, err := StartProc(ProcSpec{Binary: binary, Args: args, Env: env, StderrTail: 4 << 10})
	if err != nil {
		return "", -1, err
	}
	_ = p.Stdin.Close()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(p.Stdout, 1<<20))
		ch <- res{b, err}
	}()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case r := <-ch:
		select {
		case <-p.Done():
		case <-ctx.Done():
		}
		p.Kill(0)
		return string(r.b), p.ExitCode(), r.err
	case <-ctx.Done():
		p.Kill(0)
		return "", -1, ctx.Err()
	}
}
