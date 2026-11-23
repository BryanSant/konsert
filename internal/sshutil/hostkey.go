package sshutil

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// HostKey is a remote SSH host key offered for operator confirmation.
type HostKey struct {
	Type        string `json:"type"`        // e.g. "ssh-ed25519"
	Fingerprint string `json:"fingerprint"` // OpenSSH SHA256 form, e.g. "SHA256:abc..."
	knownHosts  string // the known_hosts line to trust if confirmed
}

// runFunc executes name with args, feeding stdin (may be nil), and returns stdout.
// On failure the error message is the trimmed stderr when available.
type runFunc func(ctx context.Context, name string, args []string, stdin []byte) (string, error)

// run is replaced in tests to capture argv/stdin without invoking ssh.
var run runFunc = execRun

func execRun(ctx context.Context, name string, args []string, stdin []byte) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), errors.New(msg)
	}
	return stdout.String(), nil
}

// userKnownHostsPath returns the file confirmed host keys are appended to.
var userKnownHostsPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot locate home directory for known_hosts")
	}
	return filepath.Join(home, ".ssh", "known_hosts"), nil
}

// hostKeyProblem classifies an ssh failure caused by host key checking.
type hostKeyProblem int

const (
	hostKeyOK hostKeyProblem = iota
	hostKeyUnknown
	hostKeyChanged
)

func classifyHostKeyError(err error) hostKeyProblem {
	if err == nil {
		return hostKeyOK
	}
	msg := err.Error()
	if strings.Contains(msg, "REMOTE HOST IDENTIFICATION HAS CHANGED") || strings.Contains(msg, "Offending") {
		return hostKeyChanged
	}
	if strings.Contains(msg, "host key is known for") || strings.Contains(msg, "Host key verification failed") {
		return hostKeyUnknown
	}
	return hostKeyOK
}

// resolvedHost is what ssh would actually connect to after applying ~/.ssh/config.
type resolvedHost struct {
	hostname     string
	port         string
	hostKeyAlias string
}

func resolveTarget(ctx context.Context, t Target) (resolvedHost, error) {
	args := []string{"-G"}
	if t.Port != "" {
		args = append(args, "-p", t.Port)
	}
	args = append(args, "--", t.Destination())
	out, err := run(ctx, "ssh", args, nil)
	if err != nil {
		return resolvedHost{}, fmt.Errorf("resolve ssh config for %s: %w", t, err)
	}
	rh := resolvedHost{hostname: t.Host, port: "22"}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		key, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(key) {
		case "hostname":
			rh.hostname = val
		case "port":
			rh.port = val
		case "hostkeyalias":
			if !strings.EqualFold(val, "none") {
				rh.hostKeyAlias = val
			}
		}
	}
	if strings.HasPrefix(rh.hostname, "-") {
		return resolvedHost{}, fmt.Errorf("resolved hostname %q is invalid", rh.hostname)
	}
	return rh, nil
}

// knownHostsName is the host pattern ssh looks up in known_hosts for rh.
func (rh resolvedHost) knownHostsName() string {
	if rh.hostKeyAlias != "" {
		return rh.hostKeyAlias
	}
	if rh.port == "" || rh.port == "22" {
		return rh.hostname
	}
	return "[" + rh.hostname + "]:" + rh.port
}

// scanHostKeys fetches the remote host's public keys with ssh-keyscan.
func scanHostKeys(ctx context.Context, t Target) ([]HostKey, error) {
	rh, err := resolveTarget(ctx, t)
	if err != nil {
		return nil, err
	}
	args := []string{"-T", "5"}
	if rh.port != "" && rh.port != "22" {
		args = append(args, "-p", rh.port)
	}
	args = append(args, "--", rh.hostname)
	out, err := run(ctx, "ssh-keyscan", args, nil)
	keys := parseKeyscan(out, rh.knownHostsName())
	if len(keys) == 0 {
		if err != nil {
			return nil, fmt.Errorf("ssh-keyscan %s: %w", rh.hostname, err)
		}
		return nil, fmt.Errorf("ssh-keyscan returned no host keys for %s", rh.hostname)
	}
	return keys, nil
}

// parseKeyscan parses ssh-keyscan output into host keys whose known_hosts lines use
// name as the host pattern.
func parseKeyscan(out, name string) []HostKey {
	var keys []HostKey
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		fp, err := FingerprintSHA256(f[2])
		if err != nil || seen[fp] {
			continue
		}
		seen[fp] = true
		keys = append(keys, HostKey{Type: f[1], Fingerprint: fp, knownHosts: name + " " + f[1] + " " + f[2]})
	}
	return keys
}

// FingerprintSHA256 returns the OpenSSH SHA256 fingerprint of a base64 public key blob.
func FingerprintSHA256(b64Key string) (string, error) {
	blob, err := base64.StdEncoding.DecodeString(b64Key)
	if err != nil {
		return "", fmt.Errorf("invalid public key encoding: %w", err)
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

// normalizeFingerprint accepts "SHA256:xxx" or bare "xxx" (trailing '=' tolerated).
func normalizeFingerprint(fp string) string {
	fp = strings.TrimSpace(fp)
	fp = strings.TrimPrefix(fp, "SHA256:")
	return "SHA256:" + strings.TrimRight(fp, "=")
}

func matchHostKey(keys []HostKey, confirmed string) (HostKey, bool) {
	want := normalizeFingerprint(confirmed)
	for _, k := range keys {
		if k.Fingerprint == want {
			return k, true
		}
	}
	return HostKey{}, false
}

func describeHostKeys(keys []HostKey) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k.Fingerprint+" ("+k.Type+")")
	}
	return strings.Join(parts, ", ")
}

// writeTempKnownHosts writes line to a private temporary known_hosts file.
func writeTempKnownHosts(line string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "konsert-known-hosts-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

// appendKnownHost persists a confirmed host key to the user's known_hosts.
func appendKnownHost(line string) error {
	path, err := userKnownHostsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
