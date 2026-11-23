package sshutil

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// SSHHost represents a host entry discovered from ~/.ssh/config.
type SSHHost struct {
	Alias    string `json:"alias"`
	HostName string `json:"hostname"`
	User     string `json:"user,omitempty"`
	Port     string `json:"port,omitempty"`
	Display  string `json:"display"` // e.g. "bsant@bocabox" or "daybreak"
}

// DiscoverHosts parses ~/.ssh/config on the controller host and returns non-wildcard host entries.
func DiscoverHosts() ([]SSHHost, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return []SSHHost{}, nil
	}
	return ParseSSHConfigFile(filepath.Join(home, ".ssh", "config"))
}

// ParseSSHConfigFile parses an OpenSSH client config file into SSHHost entries.
func ParseSSHConfigFile(path string) ([]SSHHost, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []SSHHost{}, nil
		}
		return nil, err
	}
	return ParseSSHConfig(bytes.NewReader(raw)), nil
}

// ParseSSHConfig parses OpenSSH client config lines from r.
func ParseSSHConfig(r *bytes.Reader) []SSHHost {
	var hosts []SSHHost
	seen := make(map[string]bool)
	var current []*SSHHost

	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip inline comments outside quotes (simple split)
		if idx := strings.Index(line, " #"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.ToLower(fields[0])
		val := strings.Join(fields[1:], " ")

		if key == "host" {
			current = nil
			for _, pattern := range fields[1:] {
				if strings.ContainsAny(pattern, "*?!") {
					continue
				}
				if seen[pattern] {
					continue
				}
				seen[pattern] = true
				hosts = append(hosts, SSHHost{Alias: pattern})
				current = append(current, &hosts[len(hosts)-1])
			}
			continue
		}

		for _, h := range current {
			switch key {
			case "hostname":
				if h.HostName == "" {
					h.HostName = val
				}
			case "user":
				if h.User == "" {
					h.User = val
				}
			case "port":
				if h.Port == "" {
					h.Port = val
				}
			}
		}
	}

	for i := range hosts {
		target := hosts[i].HostName
		if target == "" {
			target = hosts[i].Alias
		}
		if hosts[i].User != "" {
			hosts[i].Display = hosts[i].User + "@" + target
		} else {
			hosts[i].Display = target
		}
	}

	sort.SliceStable(hosts, func(i, j int) bool {
		return strings.ToLower(hosts[i].Alias) < strings.ToLower(hosts[j].Alias)
	})
	return hosts
}

// BootstrapOptions controls 1-click SSH konsertd installation/startup on a remote host.
type BootstrapOptions struct {
	Target        string // SSH alias or user@host
	ControllerURL string // ws:// or wss:// URL reachable from the remote host
	FleetToken    string
	Sandbox       bool
	EnableMock    bool
}

// BootstrapResult reports the outcome of an SSH worker bootstrap attempt.
type BootstrapResult struct {
	OK        bool   `json:"ok"`
	Target    string `json:"target"`
	RemoteOS  string `json:"remote_os,omitempty"`
	RemoteArch string `json:"remote_arch,omitempty"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
}

// BootstrapRemoteWorker connects over SSH, ensures konsertd exists on the remote host
// (copying a matching local binary if available), and starts it detached in the background.
func BootstrapRemoteWorker(parentCtx context.Context, opts BootstrapOptions) BootstrapResult {
	target := strings.TrimSpace(opts.Target)
	if target == "" {
		return BootstrapResult{OK: false, Error: "target host is required"}
	}
	ctx, cancel := context.WithTimeout(parentCtx, 45*time.Second)
	defer cancel()

	// 1. Detect remote OS & architecture via batch SSH
	unameOut, err := runSSH(ctx, target, "uname -s && uname -m")
	if err != nil {
		return BootstrapResult{OK: false, Target: target, Error: fmt.Sprintf("SSH connection failed: %v", err)}
	}
	lines := strings.Split(strings.TrimSpace(unameOut), "\n")
	if len(lines) < 2 {
		return BootstrapResult{OK: false, Target: target, Error: "unable to detect remote OS/arch"}
	}
	rOS := strings.ToLower(strings.TrimSpace(lines[0]))
	rArchRaw := strings.ToLower(strings.TrimSpace(lines[1]))
	rArch := normalizeArch(rArchRaw)

	// 2. Check if konsertd is already installed on remote host
	_, hasRemote := runSSH(ctx, target, "command -v konsertd || test -x \"$HOME/.local/bin/konsertd\"")
	if hasRemote != nil {
		// Look for a matching local binary (either sibling bin/konsertd or dist/konsertd-<os>-<arch>)
		localBin := findMatchingLocalKonsertd(rOS, rArch)
		if localBin == "" {
			return BootstrapResult{
				OK:         false,
				Target:     target,
				RemoteOS:   rOS,
				RemoteArch: rArch,
				Error:      fmt.Sprintf("konsertd not found on %s (%s/%s) and no matching local binary found in bin/ or dist/. Run `make build-all` on the controller or install konsertd on the target host.", target, rOS, rArch),
			}
		}
		if _, err := runSSH(ctx, target, "mkdir -p \"$HOME/.local/bin\""); err != nil {
			return BootstrapResult{OK: false, Target: target, RemoteOS: rOS, RemoteArch: rArch, Error: fmt.Sprintf("mkdir ~/.local/bin failed: %v", err)}
		}
		if err := runSCP(ctx, localBin, target+":~/.local/bin/konsertd"); err != nil {
			return BootstrapResult{OK: false, Target: target, RemoteOS: rOS, RemoteArch: rArch, Error: fmt.Sprintf("scp konsertd failed: %v", err)}
		}
		_, _ = runSSH(ctx, target, "chmod +x \"$HOME/.local/bin/konsertd\"")
	}

	// 3. Build detached launch command
	workerName := target
	if _, hostPart, ok := strings.Cut(target, "@"); ok && hostPart != "" {
		workerName = hostPart
	}
	var flags []string
	flags = append(flags, fmt.Sprintf("--controller %s", shellQuote(opts.ControllerURL)))
	flags = append(flags, fmt.Sprintf("--name %s", shellQuote(workerName)))
	flags = append(flags, fmt.Sprintf("--host-group %s", shellQuote(workerName)))
	if opts.FleetToken != "" {
		flags = append(flags, fmt.Sprintf("--token %s", shellQuote(opts.FleetToken)))
	}
	if opts.Sandbox {
		flags = append(flags, "--sandbox")
	}
	if opts.EnableMock {
		flags = append(flags, "--enable-mock-driver")
	}

	startScript := fmt.Sprintf(
		`BIN=$(command -v konsertd || echo "$HOME/.local/bin/konsertd"); nohup "$BIN" run %s </dev/null >/tmp/konsertd.log 2>&1 & echo $!`,
		strings.Join(flags, " "),
	)
	pidOut, err := runSSH(ctx, target, startScript)
	if err != nil {
		return BootstrapResult{OK: false, Target: target, RemoteOS: rOS, RemoteArch: rArch, Error: fmt.Sprintf("start konsertd failed: %v", err)}
	}

	return BootstrapResult{
		OK:         true,
		Target:     target,
		RemoteOS:   rOS,
		RemoteArch: rArch,
		Message:    fmt.Sprintf("Started konsertd on %s (%s/%s, pid %s)", target, rOS, rArch, strings.TrimSpace(pidOut)),
	}
}

func runSSH(ctx context.Context, target, remoteCmd string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new",
		target,
		remoteCmd,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	return stdout.String(), nil
}

func runSCP(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "scp",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=8",
		src, dst,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func normalizeArch(raw string) string {
	switch raw {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return raw
	}
}

func findMatchingLocalKonsertd(targetOS, targetArch string) string {
	candidates := []string{
		filepath.Join("dist", fmt.Sprintf("konsertd-%s-%s", targetOS, targetArch)),
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, fmt.Sprintf("konsertd-%s-%s", targetOS, targetArch)),
		)
		if targetOS == runtime.GOOS && targetArch == runtime.GOARCH {
			candidates = append(candidates, filepath.Join(dir, "konsertd"))
		}
	}
	if targetOS == runtime.GOOS && targetArch == runtime.GOARCH {
		candidates = append(candidates, filepath.Join("bin", "konsertd"))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
