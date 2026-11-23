package sshutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// BootstrapOptions controls 1-click SSH konsertd installation/startup on a remote host.
type BootstrapOptions struct {
	Target        string // SSH alias or [user@]host[:port]
	ControllerURL string // ws:// or wss:// URL reachable from the remote host
	FleetToken    string
	Sandbox       bool
	EnableMock    bool
	// AcceptHostKey is the SHA256 fingerprint ("SHA256:...") of the remote host key the
	// operator confirmed. It is only consulted when the host is not yet in known_hosts:
	// without it, an unknown host stops at the "host_key_confirmation" stage and the
	// result lists the offered keys. A changed host key is never accepted.
	AcceptHostKey string
	OnStage       func(stage, label, detail string)
}

// BootstrapStage records one discrete step in the zero-touch SSH onboarding pipeline.
type BootstrapStage struct {
	Stage  string `json:"stage"` // "connecting" | "host_key_confirmation" | "installing_daemon" | "configuring_service" | "installing_hooks" | "inspecting_facts" | "ready"
	Label  string `json:"label"`
	Status string `json:"status"` // "ok" | "skipped" | "error"
	Detail string `json:"detail,omitempty"`
}

// StageHostKeyConfirmation is the stage reported when an unknown host key needs the
// operator's confirmation before bootstrap can continue.
const StageHostKeyConfirmation = "host_key_confirmation"

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
	// HostKeyConfirmationRequired is set (with HostKeys) when the remote host key is
	// unknown. Re-submit with accept_host_key set to the confirmed fingerprint.
	HostKeyConfirmationRequired bool      `json:"host_key_confirmation_required,omitempty"`
	HostKeys                    []HostKey `json:"host_keys,omitempty"`
}

// session runs ssh/scp against one validated target with shared extra options.
type session struct {
	t         Target
	extraOpts []string
}

func (s *session) ssh(ctx context.Context, remoteCmd string, stdin []byte) (string, error) {
	return run(ctx, "ssh", sshArgs(s.t, s.extraOpts, remoteCmd), stdin)
}

func (s *session) scp(ctx context.Context, src, remotePath string) error {
	_, err := run(ctx, "scp", scpArgs(s.t, s.extraOpts, src, remotePath), nil)
	return err
}

// workerConfigFile is the YAML written to ~/.konsert/worker.yaml on the remote host.
type workerConfigFile struct {
	ControllerURL    string `yaml:"controller_url"`
	FleetToken       string `yaml:"fleet_token"`
	Name             string `yaml:"name"`
	HostGroup        string `yaml:"host_group"`
	Sandbox          bool   `yaml:"sandbox"`
	EnableMockDriver bool   `yaml:"enable_mock_driver"`
}

// writeWorkerConfigCmd reads the config from stdin, so the fleet token never appears
// in any process's argv on either host.
const writeWorkerConfigCmd = `umask 077 && mkdir -p "$HOME/.konsert" && chmod 700 "$HOME/.konsert" && ` +
	`cat > "$HOME/.konsert/worker.yaml.tmp" && chmod 600 "$HOME/.konsert/worker.yaml.tmp" && ` +
	`mv -f "$HOME/.konsert/worker.yaml.tmp" "$HOME/.konsert/worker.yaml"`

// BootstrapRemoteWorker connects over SSH, ensures konsertd exists on the remote host,
// writes ~/.konsert/worker.yaml & persistent user service (systemd/launchd/nohup),
// installs agent hooks (`konsertd install-hooks`), and starts konsertd so initial MachineFacts arrive.
func BootstrapRemoteWorker(parentCtx context.Context, opts BootstrapOptions) BootstrapResult {
	t, err := ParseTarget(opts.Target)
	if err != nil {
		return BootstrapResult{OK: false, Target: strings.TrimSpace(opts.Target), Stage: "connecting", Error: err.Error()}
	}
	target := t.String()
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
	sess := &session{t: t}

	// Stage 1: Connecting & detecting remote OS/arch (strict host key checking)
	notify("connecting", "Connecting", "Detecting remote OS and architecture over SSH")
	const unameCmd = "uname -s && uname -m"
	unameOut, err := sess.ssh(ctx, unameCmd, nil)
	switch classifyHostKeyError(err) {
	case hostKeyChanged:
		return fail("connecting", "Connecting", "", "",
			fmt.Sprintf("SSH host key for %s has CHANGED (possible man-in-the-middle). Refusing to continue; verify the host and fix ~/.ssh/known_hosts on the controller. ssh said: %v", target, err))
	case hostKeyUnknown:
		keys, kerr := scanHostKeys(ctx, t)
		if kerr != nil {
			return fail("connecting", "Connecting", "", "", fmt.Sprintf("SSH host key for %s is unknown and could not be fetched: %v", target, kerr))
		}
		if strings.TrimSpace(opts.AcceptHostKey) == "" {
			notify(StageHostKeyConfirmation, "Confirm host key", describeHostKeys(keys))
			stages = append(stages, BootstrapStage{Stage: StageHostKeyConfirmation, Label: "Confirm host key", Status: "error", Detail: describeHostKeys(keys)})
			return BootstrapResult{
				OK:                          false,
				Target:                      target,
				Stage:                       StageHostKeyConfirmation,
				Stages:                      stages,
				HostKeyConfirmationRequired: true,
				HostKeys:                    keys,
				Error: fmt.Sprintf("Unknown SSH host key for %s. Verify and confirm the fingerprint to continue: %s",
					target, describeHostKeys(keys)),
			}
		}
		key, ok := matchHostKey(keys, opts.AcceptHostKey)
		if !ok {
			return fail(StageHostKeyConfirmation, "Confirm host key", "", "",
				fmt.Sprintf("the host key offered by %s (%s) does not match the confirmed fingerprint %s", target, describeHostKeys(keys), opts.AcceptHostKey))
		}
		khPath, cleanup, terr := writeTempKnownHosts(key.knownHosts)
		if terr != nil {
			return fail("connecting", "Connecting", "", "", fmt.Sprintf("prepare known_hosts: %v", terr))
		}
		defer cleanup()
		// Trust exactly the confirmed key for this run; StrictHostKeyChecking=yes still
		// rejects any other key the real connection presents.
		sess.extraOpts = []string{"-o", fmt.Sprintf("UserKnownHostsFile=%q", khPath)}
		unameOut, err = sess.ssh(ctx, unameCmd, nil)
		if err == nil {
			if perr := appendKnownHost(key.knownHosts); perr == nil {
				stages = append(stages, BootstrapStage{Stage: StageHostKeyConfirmation, Label: "Confirm host key", Status: "ok", Detail: "trusted " + key.Fingerprint})
			} else {
				stages = append(stages, BootstrapStage{Stage: StageHostKeyConfirmation, Label: "Confirm host key", Status: "ok", Detail: "trusted " + key.Fingerprint + " for this run (known_hosts not updated: " + perr.Error() + ")"})
			}
		}
	}
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
	_, hasRemote := sess.ssh(ctx, "command -v konsertd || test -x \"$HOME/.local/bin/konsertd\"", nil)
	if hasRemote != nil {
		localBin := findMatchingLocalKonsertd(rOS, rArch)
		if localBin == "" {
			return fail("installing_daemon", "Installing daemon", rOS, rArch,
				fmt.Sprintf("konsertd not found on %s (%s/%s) and no matching local binary found in bin/ or dist/. Run `make build-all` on the controller or install konsertd on the target host.", target, rOS, rArch))
		}
		if _, err := sess.ssh(ctx, "mkdir -p \"$HOME/.local/bin\"", nil); err != nil {
			return fail("installing_daemon", "Installing daemon", rOS, rArch, fmt.Sprintf("mkdir ~/.local/bin failed: %v", err))
		}
		if err := sess.scp(ctx, localBin, "~/.local/bin/konsertd"); err != nil {
			return fail("installing_daemon", "Installing daemon", rOS, rArch, fmt.Sprintf("scp konsertd failed: %v", err))
		}
		_, _ = sess.ssh(ctx, "chmod +x \"$HOME/.local/bin/konsertd\"", nil)
		stages = append(stages, BootstrapStage{Stage: "installing_daemon", Label: "Installing daemon", Status: "ok", Detail: "transferred ~/.local/bin/konsertd"})
	} else {
		stages = append(stages, BootstrapStage{Stage: "installing_daemon", Label: "Installing daemon", Status: "ok", Detail: "already installed"})
	}

	// Stage 3: Configuring ~/.konsert/worker.yaml (sent over stdin) + persistent service
	notify("configuring_service", "Configuring service", "Writing worker config and persistence")
	configYAML, err := yaml.Marshal(workerConfigFile{
		ControllerURL:    opts.ControllerURL,
		FleetToken:       opts.FleetToken,
		Name:             t.Host,
		HostGroup:        t.Host,
		Sandbox:          opts.Sandbox,
		EnableMockDriver: opts.EnableMock,
	})
	if err != nil {
		return fail("configuring_service", "Configuring service", rOS, rArch, fmt.Sprintf("render worker config: %v", err))
	}
	if _, err := sess.ssh(ctx, writeWorkerConfigCmd, configYAML); err != nil {
		return fail("configuring_service", "Configuring service", rOS, rArch, fmt.Sprintf("write ~/.konsert/worker.yaml failed: %v", err))
	}
	stages = append(stages, BootstrapStage{Stage: "configuring_service", Label: "Configuring service", Status: "ok", Detail: "~/.konsert/worker.yaml"})

	// Stage 4: Installing agent hooks & skills (`konsertd install-hooks`)
	notify("installing_hooks", "Installing hooks", "Installing Claude/Antigravity/Codex telemetry hooks")
	hooksInstalled := false
	if _, hErr := sess.ssh(ctx, `BIN=$(command -v konsertd || echo "$HOME/.local/bin/konsertd"); "$BIN" install-hooks`, nil); hErr == nil {
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
  systemctl --user daemon-reload && systemctl --user enable konsertd.service && systemctl --user restart konsertd.service && echo "systemd" && exit 0
fi
pkill -f "konsertd run" >/dev/null 2>&1 || true
nohup "$BIN" run -c "$CFG" </dev/null >/tmp/konsertd.log 2>&1 & echo "nohup:$!"`
	startOut, err := sess.ssh(ctx, startScript, nil)
	if err != nil {
		return fail("inspecting_facts", "Inspecting facts", rOS, rArch, fmt.Sprintf("start konsertd failed: %v", err))
	}
	if strings.HasPrefix(strings.TrimSpace(startOut), "systemd") {
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
