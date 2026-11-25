package sshutil

import (
	"bytes"
	"context"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// The hub's SSH transport (SYR-K1B60). The hub keeps one long-lived ssh per machine,
// authenticated with its own key only, whose forced command on the machine is
// `syred attach`; the session's stdin/stdout carry the worker protocol.

// attachOptions harden the long-lived attach session: only the hub key (never the
// user's agent or personal keys, never a shared ControlMaster connection that a
// personal key opened), no forwarding of any kind, no TTY, keepalives so a dead link
// is noticed within ~45 s, and strict host key checking against the pinned keys.
var attachOptions = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=10",
	"-o", "StrictHostKeyChecking=yes",
	"-o", "ServerAliveInterval=15",
	"-o", "ServerAliveCountMax=3",
	"-o", "IdentitiesOnly=yes",
	"-o", "IdentityAgent=none",
	"-o", "ControlMaster=no",
	"-o", "ControlPath=none",
	"-o", "ForwardAgent=no",
	"-o", "ForwardX11=no",
	"-o", "ClearAllForwardings=yes",
	"-o", "PermitLocalCommand=no",
	"-o", "RemoteCommand=none",
	"-o", "RequestTTY=no",
	"-o", "LogLevel=ERROR",
	"-T",
}

// AttachArgs is the ssh argv that attaches to workerID's syred on target with the
// hub key at keyPath. The remote command is only a request (the forced command runs
// regardless and reads it from SSH_ORIGINAL_COMMAND).
func AttachArgs(t Target, keyPath, workerID string) []string {
	args := append([]string{}, attachOptions...)
	args = append(args, "-i", keyPath)
	if t.Port != "" {
		args = append(args, "-p", t.Port)
	}
	return append(args, "--", t.Destination(), "syred attach "+workerID)
}

// CommandConn is a net.Conn over a child process's stdin/stdout (ssh running the
// forced command). Close ends the process.
type CommandConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *io.PipeReader
	stderr *tailBuffer

	closeOnce sync.Once
	waitErr   error
	waited    chan struct{}
}

// StartCommandConn starts cmd and returns a connection over its stdio. Its stdout is
// read through a pipe that Wait does not close, so nothing it wrote before exiting is
// lost.
func StartCommandConn(cmd *exec.Cmd) (*CommandConn, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	c := &CommandConn{cmd: cmd, stdin: stdin, stdout: pr, stderr: &tailBuffer{max: 4096}, waited: make(chan struct{})}
	cmd.Stderr = c.stderr
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = 2 * time.Second
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		c.waitErr = cmd.Wait()
		_ = pw.Close()
		close(c.waited)
	}()
	return c, nil
}

// DialSSH starts the attach ssh for workerID on target.
func DialSSH(ctx context.Context, target, keyPath, workerID string) (*CommandConn, error) {
	t, err := ParseTarget(target)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "ssh", AttachArgs(t, keyPath, workerID)...)
	return StartCommandConn(cmd)
}

func (c *CommandConn) Read(p []byte) (int, error) { return c.stdout.Read(p) }

func (c *CommandConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Close stops the process and waits (bounded) for it to exit.
func (c *CommandConn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		_ = c.stdout.Close()
	})
	select {
	case <-c.waited:
	case <-time.After(5 * time.Second):
	}
	return nil
}

// Err describes why the process ended: its last stderr lines (ssh's own errors, such as
// "Permission denied (publickey)", or syred attach's), else its exit status. Empty
// while it runs.
func (c *CommandConn) Err() string {
	select {
	case <-c.waited:
	default:
		return ""
	}
	if s := strings.TrimSpace(c.stderr.String()); s != "" {
		return s
	}
	if c.waitErr != nil {
		return c.waitErr.Error()
	}
	return ""
}

// Done is closed when the process has exited.
func (c *CommandConn) Done() <-chan struct{} { return c.waited }

func (c *CommandConn) LocalAddr() net.Addr                { return pipeAddr{} }
func (c *CommandConn) RemoteAddr() net.Addr               { return pipeAddr{} }
func (c *CommandConn) SetDeadline(t time.Time) error      { return nil }
func (c *CommandConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *CommandConn) SetWriteDeadline(t time.Time) error { return nil }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "ssh" }
func (pipeAddr) String() string  { return "ssh" }

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p)
	if over := b.buf.Len() - b.max; over > 0 {
		b.buf.Next(over)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// AttachFailure is what a failed attach says about the machine (SYR-JAX1E).
type AttachFailure int

const (
	// AttachFailureOther: the link failed for another reason (a handshake that timed
	// out, an identity the hub refused); nothing says the daemon is down.
	AttachFailureOther AttachFailure = iota
	// AttachUnreachable: ssh could not reach or log in to the host (no route, refused,
	// timed out, DNS, host key, the hub key refused). Restarting syred cannot help.
	AttachUnreachable
	// AttachDaemonDown: the host answered, but its syred is not running (the forced
	// command found no socket) or its binary is gone.
	AttachDaemonDown
)

func (f AttachFailure) String() string {
	switch f {
	case AttachUnreachable:
		return "unreachable"
	case AttachDaemonDown:
		return "daemon_down"
	}
	return "other"
}

// DaemonDownMarker starts `syred attach`'s error when the daemon's socket is not
// there (worker.Attach).
const DaemonDownMarker = "syred is not running here"

// sshFailures are ssh's own messages for a host it could not reach or log in to.
var sshFailures = []string{
	"ssh: connect to host", "Could not resolve hostname", "Connection refused",
	"Connection timed out", "Operation timed out", "No route to host", "Network is unreachable",
	"Host is down", "Connection closed by", "Connection reset by", "kex_exchange_identification",
	"Permission denied", "Host key verification failed", "REMOTE HOST IDENTIFICATION HAS CHANGED",
	"host key is known for", "Too many authentication failures", "banner exchange",
	"Bad owner or permissions",
}

// forcedCommandMissing are the shells' messages when the forced command's binary is
// gone (the host answered and ran the command).
var forcedCommandMissing = []string{
	"No such file or directory", "command not found", ": not found", "cannot execute",
	"is not recognized as an internal or external command",
}

// ClassifyAttachFailure reads the error of a failed SSH attach (ssh's stderr or
// `syred attach`'s) and says whether the host was unreachable or its daemon down.
func ClassifyAttachFailure(msg string) AttachFailure {
	if strings.Contains(msg, DaemonDownMarker) {
		return AttachDaemonDown
	}
	for _, s := range sshFailures {
		if strings.Contains(msg, s) {
			return AttachUnreachable
		}
	}
	for _, s := range forcedCommandMissing {
		if strings.Contains(msg, s) {
			return AttachDaemonDown
		}
	}
	return AttachFailureOther
}
