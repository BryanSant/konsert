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
	OnStage       func(stage, label, detail string)
}

// BootstrapStage records one discrete step in the zero-touch SSH onboarding pipeline.
type BootstrapStage struct {
	Stage  string `json:"stage"`  // "connecting" | "installing_daemon" | "configuring_service" | "installing_hooks" | "inspecting_facts" | "ready"
	Label  string `json:"label"`
	Status string `json:"status"` // "ok" | "skipped" | "error"
	Detail string `json:"detail,omitempty"`
}

// BootstrapResult reports the outcome of an SSH worker bootstrap attempt.
type BootstrapResult struct {
	OK             bool             `json:"ok"`
	Target         string           `json:"target"`
	RemoteOS       string           `json:"remote_os,omitempty"`
	RemoteArch     string           `json:"remote_arch,omitempty"`
	Stage          string           `json:"stage,omitempty"`
	Stages         []BootstrapStage `json:"stages,omitempty"`
	HooksInstalled bool             `json:"hooks_installed,omitempty"`
	ServiceMode    string           `json:"service_mode,omitempty"` // "systemd" | "launchd" | "nohup"
	Message        string           `json:"message,omitempty"`
	Error          string           `json:"error,omitempty"`
}

// BootstrapRemoteWorker connects over SSH, ensures konsertd exists on the remote host,
// writes ~/.konsert/worker.yaml & persistent user service (systemd/launchd/nohup),
// installs agent hooks (`konsertd install-hooks`), and starts konsertd so initial MachineFacts arrive.
func BootstrapRemoteWorker(parentCtx context.Context, opts BootstrapOptions) BootstrapResult {
	target := strings.TrimSpace(opts.Target)
	if target == "" {
		return BootstrapResult{OK: false, Stage: "connecting", Error: "target host is required"}
	}
	ctx, cancel := context.WithTimeout(parentCtx, 60*time.Second)
	defer cancel()

	var stages []BootstrapStage
	notify := func(stage, label, detail string) {
		if opts.OnStage != nil {
			opts.OnStage(stage, label, detail)
		}
	}
	fail := func(stage, label, rOS, rArch, msg string) BootstrapResult {
		stages = append(stages, BootstrapStage{Stage: stage, Label: label, Status: "error", Detail: msg})
		return BootstrapResult{
			OK:         false,
			Target:     target,
			RemoteOS:   rOS,
			RemoteArch: rArch,
			Stage:      stage,
			Stages:     stages,
			Error:      msg,
		}
	}

	// Stage 1: Connecting & detecting remote OS/arch
	notify("connecting", "Connecting", "Detecting remote OS and architecture over SSH")
	unameOut, err := runSSH(ctx, target, "uname -s && uname -m")
	if err != nil {
		return fail("connecting", "Connecting", "", "", fmt.Sprintf("SSH connection failed: %v", err))
	}
	lines := strings.Split(strings.TrimSpace(unameOut), "\n")
	if len(lines) < 2 {
		return fail("connecting", "Connecting", "", "", "unable to detect remote OS/arch")
	}
	rOS := strings.ToLower(strings.TrimSpace(lines[0]))
	rArch := normalizeArch(strings.ToLower(strings.TrimSpace(lines[1])))
	stages = append(stages, BootstrapStage{Stage: "connecting", Label: "Connecting", Status: "ok", Detail: rOS + "/" + rArch})

	// Stage 2: Installing daemon
	notify("installing_daemon", "Installing daemon", "Checking or transferring konsertd binary")
	_, hasRemote := runSSH(ctx, target, "command -v konsertd || test -x \"$HOME/.local/bin/konsertd\"")
	if hasRemote != nil {
		localBin := findMatchingLocalKonsertd(rOS, rArch)
		if localBin == "" {
			return fail("installing_daemon", "Installing daemon", rOS, rArch,
				fmt.Sprintf("konsertd not found on %s (%s/%s) and no matching local binary found in bin/ or dist/. Run `make build-all` on the controller or install konsertd on the target host.", target, rOS, rArch))
		}
		if _, err := runSSH(ctx, target, "mkdir -p \"$HOME/.local/bin\""); err != nil {
			return fail("installing_daemon", "Installing daemon", rOS, rArch, fmt.Sprintf("mkdir ~/.local/bin failed: %v", err))
		}
		if err := runSCP(ctx, localBin, target+":~/.local/bin/konsertd"); err != nil {
			return fail("installing_daemon", "Installing daemon", rOS, rArch, fmt.Sprintf("scp konsertd failed: %v", err))
		}
		_, _ = runSSH(ctx, target, "chmod +x \"$HOME/.local/bin/konsertd\"")
		stages = append(stages, BootstrapStage{Stage: "installing_daemon", Label: "Installing daemon", Status: "ok", Detail: "transferred ~/.local/bin/konsertd"})
	} else {
		stages = append(stages, BootstrapStage{Stage: "installing_daemon", Label: "Installing daemon", Status: "ok", Detail: "already installed"})
	}

	// Stage 3: Configuring ~/.konsert/worker.yaml + persistent service
	notify("configuring_service", "Configuring service", "Writing worker config and persistence")
	workerName := target
	if _, hostPart, ok := strings.Cut(target, "@"); ok && hostPart != "" {
		workerName = hostPart
	}
	configYAML := fmt.Sprintf("controller_url: %q\nfleet_token: %q\nname: %q\nhost_group: %q\nsandbox: %t\nenable_mock_driver: %t\n",
		opts.ControllerURL, opts.FleetToken, workerName, workerName, opts.Sandbox, opts.EnableMock)
	writeCfgCmd := fmt.Sprintf(`mkdir -p "$HOME/.konsert" && chmod 700 "$HOME/.konsert" && cat > "$HOME/.konsert/worker.yaml" <<'KONSERT_EOF'
%sKONSERT_EOF
chmod 600 "$HOME/.konsert/worker.yaml"`, configYAML)
	if _, err := runSSH(ctx, target, writeCfgCmd); err != nil {
		return fail("configuring_service", "Configuring service", rOS, rArch, fmt.Sprintf("write ~/.konsert/worker.yaml failed: %v", err))
	}
	stages = append(stages, BootstrapStage{Stage: "configuring_service", Label: "Configuring service", Status: "ok", Detail: "~/.konsert/worker.yaml"})

	// Stage 4: Installing agent hooks & skills (`konsertd install-hooks`)
	notify("installing_hooks", "Installing hooks", "Installing Claude/Antigravity/Codex telemetry hooks")
	hooksInstalled := false
	if _, hErr := runSSH(ctx, target, `BIN=$(command -v konsertd || echo "$HOME/.local/bin/konsertd"); "$BIN" install-hooks`); hErr == nil {
		hooksInstalled = true
		stages = append(stages, BootstrapStage{Stage: "installing_hooks", Label: "Installing hooks", Status: "ok", Detail: "agent hooks active"})
	} else {
		stages = append(stages, BootstrapStage{Stage: "installing_hooks", Label: "Installing hooks", Status: "skipped", Detail: hErr.Error()})
	}

	// Stage 5: Starting persistent daemon & triggering initial fact inspection
	notify("inspecting_facts", "Inspecting facts", "Starting konsertd and collecting machine facts")
	serviceMode := "nohup"
	startScript := `BIN=$(command -v konsertd || echo "$HOME/.local/bin/konsertd")
CFG="$HOME/.konsert/worker.yaml"
if command -v systemctl >/dev/null 2>&1 && systemctl --user status >/dev/null 2>&1; then
  mkdir -p "$HOME/.config/systemd/user"
  cat > "$HOME/.config/systemd/user/konsertd.service" <<SVC_EOF
[Unit]
Description=Konsert Worker Daemon
After=network-online.target

[Service]
ExecStart=$BIN run -c $CFG
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
SVC_EOF
  systemctl --user daemon-reload && systemctl --user enable --now konsertd.service && echo "systemd" && exit 0
fi
nohup "$BIN" run -c "$CFG" </dev/null >/tmp/konsertd.log 2>&1 & echo "nohup:$!"`
	startOut, err := runSSH(ctx, target, startScript)
	if err != nil {
		return fail("inspecting_facts", "Inspecting facts", rOS, rArch, fmt.Sprintf("start konsertd failed: %v", err))
	}
	trimmedStart := strings.TrimSpace(startOut)
	if strings.HasPrefix(trimmedStart, "systemd") {
		serviceMode = "systemd"
	}
	stages = append(stages, BootstrapStage{Stage: "inspecting_facts", Label: "Inspecting facts", Status: "ok", Detail: "started via " + serviceMode})
	notify("ready", "Ready", fmt.Sprintf("konsertd running on %s (%s/%s)", target, rOS, rArch))

	return BootstrapResult{
		OK:             true,
		Target:         target,
		RemoteOS:       rOS,
		RemoteArch:     rArch,
		Stage:          "ready",
		Stages:         stages,
		HooksInstalled: hooksInstalled,
		ServiceMode:    serviceMode,
		Message:        fmt.Sprintf("Connected %s (%s/%s, %s, hooks=%t)", target, rOS, rArch, serviceMode, hooksInstalled),
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
