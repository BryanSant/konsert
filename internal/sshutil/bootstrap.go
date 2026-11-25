package sshutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/install"
	"gopkg.in/yaml.v3"
)

// BootstrapOptions controls 1-click SSH syred installation/startup on a remote host.
type BootstrapOptions struct {
	Target string // SSH alias or [user@]host[:port]
	// HubKey is the hub's own SSH key, allowed in the remote authorized_keys with a
	// forced command (`syred attach`) so the hub can attach to the daemon afterwards
	// (SYR-K1B60). The daemon is installed without a controller URL: it serves the hub
	// on its local socket and needs neither a machine credential nor the hub's address.
	HubKey     *HubKeyGrant
	Sandbox    bool
	EnableMock bool
	// AcceptHostKey is the SHA256 fingerprint ("SHA256:...") of the remote host key the
	// operator confirmed. It is only consulted when the host is not yet in known_hosts:
	// without it, an unknown host stops at the "host_key_confirmation" stage and the
	// result lists the offered keys. A changed host key is never accepted.
	AcceptHostKey string
	OnStage       func(stage, label, detail string)
}

// HubKeyGrant is the hub key to allow: the hub's id (its authorized_keys marker) and
// its public key ("ssh-ed25519 AAAA...").
type HubKeyGrant struct {
	HubID     string
	PublicKey string
}

// BootstrapStage records one discrete step in the zero-touch SSH onboarding pipeline.
type BootstrapStage struct {
	Stage  string `json:"stage"` // "connecting" | "host_key_confirmation" | "installing_daemon" | "installing_hooks" | "authorizing_hub" | "configuring_service" | "ready"
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
	HooksInstalled bool             `json:"hooks_installed,omitzero"`
	ServiceMode    string           `json:"service_mode,omitempty"` // "systemd" | "launchd" | "schtasks" | "nohup"
	Message        string           `json:"message,omitempty"`
	Error          string           `json:"error,omitempty"`
	// WorkerID is the remote daemon's identity (`syred id`): its workers.id. Name is
	// the --name it was installed with.
	WorkerID string `json:"worker_id,omitempty"`
	Name     string `json:"name,omitempty"`
	// SyredPath is the remote syred binary the service and the hub key run.
	SyredPath string `json:"syred_path,omitempty"`
	// StartDeferred is set when syred was installed on Windows but is not running yet:
	// the SSH session was not elevated, so `syred install` registered a logon task
	// (an S4U task needs an administrator), which only runs while the user is signed in.
	StartDeferred bool `json:"start_deferred,omitzero"`
	// HostKeyConfirmationRequired is set (with HostKeys) when the remote host key is
	// unknown. Re-submit with accept_host_key set to the confirmed fingerprint.
	HostKeyConfirmationRequired bool      `json:"host_key_confirmation_required,omitzero"`
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

// nohupConfigFile is the YAML a POSIX host without a systemd user manager (a container,
// WSL without systemd) runs syred from, under nohup.
type nohupConfigFile struct {
	Name             string `yaml:"name"`
	HostGroup        string `yaml:"host_group"`
	Sandbox          bool   `yaml:"sandbox"`
	EnableMockDriver bool   `yaml:"enable_mock_driver"`
}

// writeNohupConfigCmd reads the config from stdin and writes it, readable only by the
// user, to ~/.syre/<base>.yaml. The nohup files are keyed by the worker name
// (install.NohupBase): hosts sharing a home directory each have their own.
func writeNohupConfigCmd(base string) string {
	f := `"$HOME/.syre/` + base + `.yaml"`
	tmp := `"$HOME/.syre/` + base + `.yaml.tmp"`
	return `umask 077 && mkdir -p "$HOME/.syre" && chmod 700 "$HOME/.syre" && ` +
		`cat > ` + tmp + ` && chmod 600 ` + tmp + ` && mv -f ` + tmp + ` ` + f
}

// nohupStartCmd (re)starts the nohup daemon recorded in its pid file.
func nohupStartCmd(exe, base string) string {
	return `PIDF="$HOME/.syre/` + base + `.pid"
CFG="$HOME/.syre/` + base + `.yaml"
if [ -f "$PIDF" ]; then OLD=$(cat "$PIDF"); if ps -p "$OLD" -o args= 2>/dev/null | grep -q "syred.* run -c $CFG"; then kill "$OLD"; sleep 1; fi; fi
nohup ` + shQuote(exe) + ` run -c "$CFG" </dev/null >>"$HOME/.syre/` + base + `.log" 2>&1 &
echo $! > "$PIDF"`
}

// windowsStartWait is how long the bootstrap waits for syred to come up on Windows
// (`syred status --start --wait`) before reporting it starts at the next logon.
const windowsStartWait = 20 * time.Second

// noUserManager matches `syred install`'s error on Linux without a systemd user instance.
var noUserManager = regexp.MustCompile(`no systemd user instance is reachable`)

var uuidLine = regexp.MustCompile(`(?m)^\s*([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\s*$`)

// BootstrapRemoteWorker connects over SSH, makes sure syred is on the remote host
// (copying a matching build from the controller when it is not), learns its worker id
// (`syred id`), installs agent hooks (`syred install-hooks`) and registers and
// starts the daemon with `syred install`: a systemd user unit on Linux, a LaunchAgent
// on macOS, a hidden logon task on Windows. It is idempotent, so the hub also uses it to
// restart a configured host whose daemon is down.
func BootstrapRemoteWorker(parentCtx context.Context, opts BootstrapOptions) BootstrapResult {
	t, err := ParseTarget(opts.Target)
	if err != nil {
		return BootstrapResult{OK: false, Target: strings.TrimSpace(opts.Target), Stage: "connecting", Error: err.Error()}
	}
	target := t.String()
	ctx, cancel := context.WithTimeout(parentCtx, 90*time.Second)
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
		if classifyHostKeyError(err) == hostKeyOK {
			if perr := appendKnownHost(key.knownHosts); perr == nil {
				stages = append(stages, BootstrapStage{Stage: StageHostKeyConfirmation, Label: "Confirm host key", Status: "ok", Detail: "trusted " + key.Fingerprint})
			} else {
				stages = append(stages, BootstrapStage{Stage: StageHostKeyConfirmation, Label: "Confirm host key", Status: "ok", Detail: "trusted " + key.Fingerprint + " for this run (known_hosts not updated: " + perr.Error() + ")"})
			}
		}
	}
	rOS, rArch, ok := parseUname(unameOut)
	if err != nil || !ok || isWindowsUname(rOS) {
		// No POSIX uname (Windows OpenSSH lands in PowerShell or cmd.exe), or a Git
		// Bash/MSYS uname on PATH: ask PowerShell instead.
		probe, perr := sess.ssh(ctx, encodePS(windowsProbeScript), nil)
		arch, wok := parseWindowsProbe(probe)
		switch {
		case perr == nil && wok:
			rOS, rArch, err = "windows", arch, nil
		case err != nil:
			return fail("connecting", "Connecting", "", "", fmt.Sprintf("SSH connection failed: %v", err))
		default:
			return fail("connecting", "Connecting", "", "", "unable to detect remote OS/arch")
		}
	}
	stages = append(stages, BootstrapStage{Stage: "connecting", Label: "Connecting", Status: "ok", Detail: rOS + "/" + rArch})

	var sh remoteShell = posixShell{}
	if rOS == "windows" {
		sh = windowsShell{}
	}

	// Stage 2: Installing daemon
	notify("installing_daemon", "Installing daemon", "Checking or transferring syred binary")
	findOut, err := sess.ssh(ctx, sh.findSyred(), nil)
	if err != nil {
		return fail("installing_daemon", "Installing daemon", rOS, rArch, fmt.Sprintf("look for syred failed: %v", err))
	}
	exe, found, managed := parseFind(findOut)
	if exe == "" || managed == "" {
		return fail("installing_daemon", "Installing daemon", rOS, rArch, fmt.Sprintf("unexpected reply looking for syred: %q", strings.TrimSpace(findOut)))
	}
	localBin := FindSyred(rOS, rArch)
	copyReason := ""
	switch {
	case !found && localBin == "":
		return fail("installing_daemon", "Installing daemon", rOS, rArch,
			fmt.Sprintf("syred not found on %s (%s/%s) and no matching local binary (%s) next to syre or in bin/ or dist/. Run `make build-all` on the controller or install syred on the target host.",
				target, rOS, rArch, syredFileName(rOS, rArch)))
	case !found:
		copyReason = "transferred "
	case localBin != "":
		// Keep the remote syred the same build as the hub's: an older one may lack
		// what the hub needs (syred attach). An unknown hash leaves it alone.
		if want, err := fileSHA256(localBin); err == nil {
			out, herr := sess.ssh(ctx, sh.fileHash(exe), nil)
			if got := parseHash(out); herr == nil && got != "" && got != want {
				copyReason = "updated "
			}
		}
	}
	if copyReason != "" {
		if _, err := sess.ssh(ctx, sh.prepareInstall(managed), nil); err != nil {
			return fail("installing_daemon", "Installing daemon", rOS, rArch, fmt.Sprintf("create the syred directory failed: %v", err))
		}
		if err := sess.scp(ctx, localBin, sh.scpPath(managed)); err != nil {
			return fail("installing_daemon", "Installing daemon", rOS, rArch, fmt.Sprintf("scp syred failed: %v", err))
		}
		if _, err := sess.ssh(ctx, sh.finishCopy(managed), nil); err != nil {
			return fail("installing_daemon", "Installing daemon", rOS, rArch, fmt.Sprintf("install the copied syred failed: %v", err))
		}
		exe = managed
		stages = append(stages, BootstrapStage{Stage: "installing_daemon", Label: "Installing daemon", Status: "ok", Detail: copyReason + exe})
	} else {
		stages = append(stages, BootstrapStage{Stage: "installing_daemon", Label: "Installing daemon", Status: "ok", Detail: "already installed: " + exe})
	}

	// The daemon's identity, learned before it starts (so the two can never race to
	// create it). It is the same one `run --name <host>` uses.
	name := t.Host
	runFlags := []string{"--name", name, "--host-group", name}
	idOut, err := sess.ssh(ctx, sh.syred(exe, append([]string{"id"}, runFlags...), false), nil)
	if err != nil {
		return fail("installing_daemon", "Installing daemon", rOS, rArch, fmt.Sprintf("syred id failed (is the remote syred too old?): %v", err))
	}
	m := uuidLine.FindStringSubmatch(idOut)
	if m == nil {
		return fail("installing_daemon", "Installing daemon", rOS, rArch, fmt.Sprintf("syred id printed no worker id: %q", strings.TrimSpace(idOut)))
	}
	workerID := strings.ToLower(m[1])

	// Stage 3: Installing agent hooks & skills (`syred install-hooks`)
	notify("installing_hooks", "Installing hooks", "Installing Claude/Antigravity/Codex telemetry hooks")
	hooksInstalled := false
	if _, hErr := sess.ssh(ctx, sh.syred(exe, []string{"install-hooks"}, false), nil); hErr == nil {
		hooksInstalled = true
		stages = append(stages, BootstrapStage{Stage: "installing_hooks", Label: "Installing hooks", Status: "ok", Detail: "agent hooks active"})
	} else {
		stages = append(stages, BootstrapStage{Stage: "installing_hooks", Label: "Installing hooks", Status: "skipped", Detail: hErr.Error()})
	}

	// Stage 4: allow the hub key (forced command `syred attach`), so the hub can
	// attach to the daemon over SSH with its own key from now on.
	if opts.HubKey != nil {
		notify("authorizing_hub", "Authorizing the hub", "Allowing the hub's SSH key to attach to syred")
		args := []string{"hub-key", "add", "--hub", opts.HubKey.HubID, "--worker", workerID}
		out, err := sess.ssh(ctx, sh.syred(exe, args, true), []byte(opts.HubKey.PublicKey+"\n"))
		if err != nil {
			return fail("authorizing_hub", "Authorizing the hub", rOS, rArch, fmt.Sprintf("allow the hub key failed: %v", err))
		}
		stages = append(stages, BootstrapStage{Stage: "authorizing_hub", Label: "Authorizing the hub", Status: "ok", Detail: strings.TrimSpace(out)})
	}

	// Stage 5: Register and start the daemon (`syred install`), serving the hub on
	// its local socket (no controller URL, no credential).
	notify("configuring_service", "Starting syred", "Registering syred to start automatically and starting it")
	installArgs := append([]string{"install"}, runFlags...)
	if opts.Sandbox {
		installArgs = append(installArgs, "--sandbox")
	}
	if opts.EnableMock {
		installArgs = append(installArgs, "--enable-mock-driver")
	}
	serviceMode := map[string]string{"linux": "systemd", "darwin": "launchd", "windows": "schtasks"}[rOS]
	startDeferred := false
	if _, err := sess.ssh(ctx, sh.syred(exe, installArgs, false), nil); err != nil {
		if rOS == "windows" || !noUserManager.MatchString(err.Error()) {
			return fail("configuring_service", "Starting syred", rOS, rArch, fmt.Sprintf("syred install failed: %v", err))
		}
		// No systemd user manager (container, WSL without systemd): run it under nohup,
		// from a config written over stdin. The hub restarts it the same way. (install
		// failed before writing anything: on a home shared with the container's host,
		// the config and unit names belong to the host's own syred.)
		base := install.NohupBase(name)
		cfg, merr := yaml.Marshal(nohupConfigFile{
			Name: name, HostGroup: name,
			Sandbox: opts.Sandbox, EnableMockDriver: opts.EnableMock,
		})
		if merr != nil {
			return fail("configuring_service", "Starting syred", rOS, rArch, fmt.Sprintf("render worker config: %v", merr))
		}
		if _, err := sess.ssh(ctx, writeNohupConfigCmd(base), cfg); err != nil {
			return fail("configuring_service", "Starting syred", rOS, rArch, fmt.Sprintf("write ~/.syre/%s.yaml failed: %v", base, err))
		}
		if _, err := sess.ssh(ctx, nohupStartCmd(exe, base), nil); err != nil {
			return fail("configuring_service", "Starting syred", rOS, rArch, fmt.Sprintf("start syred failed: %v", err))
		}
		serviceMode = "nohup"
	}
	if rOS == "windows" {
		// An administrator's (elevated) SSH session registers an S4U task, which /Run starts
		// with nobody signed in; anyone else gets a logon task (InteractiveToken), which only
		// runs while the user is signed in. The first /Run can be ignored (the previous
		// version still exiting): start it now and wait for the daemon itself (its local
		// socket) before calling it deferred.
		args := []string{"status", "--name", name, "--start", "--wait", windowsStartWait.String()}
		if st, err := sess.ssh(ctx, sh.syred(exe, args, false), nil); err == nil && notRunning(st) {
			startDeferred = true
		}
	}
	detail := "started via " + serviceMode
	if rOS == "windows" && !startDeferred {
		detail = "running (started via schtasks)"
	}
	if startDeferred {
		detail = "installed as a logon task (the SSH user is not an administrator); starts at the next Windows login of this user"
	}
	stages = append(stages, BootstrapStage{Stage: "configuring_service", Label: "Starting syred", Status: "ok", Detail: detail})
	msg := fmt.Sprintf("Connected %s (%s/%s, %s, hooks=%t)", target, rOS, rArch, serviceMode, hooksInstalled)
	if startDeferred {
		msg = fmt.Sprintf("Installed syred on %s; it will start at the next Windows login of the SSH user (a logon task: no one is signed in now). %s", target, install.HeadlessHint)
		notify("ready", "Installed", msg)
	} else {
		notify("ready", "Ready", fmt.Sprintf("syred started on %s (%s/%s)", target, rOS, rArch))
	}

	return BootstrapResult{
		OK:             true,
		Target:         target,
		RemoteOS:       rOS,
		RemoteArch:     rArch,
		Stage:          "ready",
		Stages:         stages,
		HooksInstalled: hooksInstalled,
		ServiceMode:    serviceMode,
		Message:        msg,
		WorkerID:       workerID,
		Name:           name,
		SyredPath:      exe,
		StartDeferred:  startDeferred,
	}
}

// parseUname parses `uname -s && uname -m`.
func parseUname(out string) (osName, arch string, ok bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(lines[0])), normalizeArch(strings.ToLower(strings.TrimSpace(lines[1]))), true
}

// isWindowsUname reports a Git Bash / MSYS / Cygwin uname, i.e. a Windows host.
func isWindowsUname(osName string) bool {
	for _, p := range []string{"mingw", "msys", "cygwin", "windows"} {
		if strings.HasPrefix(osName, p) {
			return true
		}
	}
	return false
}

// parseFind parses findSyred's reply: "found:<path>" or "missing:<path>", and
// "managed:<path>", where the hub installs its own copy.
func parseFind(out string) (path string, found bool, managed string) {
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		l = strings.TrimSpace(l)
		if p, ok := strings.CutPrefix(l, "found:"); ok && p != "" && path == "" {
			path, found = p, true
		}
		if p, ok := strings.CutPrefix(l, "missing:"); ok && p != "" && path == "" {
			path = p
		}
		if p, ok := strings.CutPrefix(l, "managed:"); ok && p != "" {
			managed = p
		}
	}
	return path, found, managed
}

// parseHash returns the first SHA-256 hex digest in a hash command's output, lower case.
func parseHash(out string) string {
	for _, f := range strings.Fields(out) {
		if len(f) == 64 && strings.Trim(strings.ToLower(f), "0123456789abcdef") == "" {
			return strings.ToLower(f)
		}
	}
	return ""
}

// fileSHA256 hashes a local file.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// notRunning reports `syred status` output saying the daemon is not running.
func notRunning(status string) bool {
	for _, l := range strings.Split(strings.ReplaceAll(status, "\r", ""), "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "Running" {
			return strings.HasPrefix(strings.TrimSpace(v), "no")
		}
	}
	return false
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

// syredFileName is the cross-built binary name `make build-all` writes to dist/.
func syredFileName(targetOS, targetArch string) string {
	name := fmt.Sprintf("syred-%s-%s", targetOS, targetArch)
	if targetOS == "windows" {
		name += ".exe"
	}
	return name
}

// FindSyred returns the absolute path of a syred binary for targetOS/targetArch
// on the controller machine, or "". It looks next to the running syre executable,
// then in dist/ and bin/ of the working directory, and, for this machine's own
// platform, on PATH.
func FindSyred(targetOS, targetArch string) string {
	cross := syredFileName(targetOS, targetArch)
	native := targetOS == runtime.GOOS && targetArch == runtime.GOARCH
	plain := "syred"
	if targetOS == "windows" {
		plain += ".exe"
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		dir := filepath.Dir(exe)
		if native {
			candidates = append(candidates, filepath.Join(dir, plain))
		}
		candidates = append(candidates, filepath.Join(dir, cross))
	}
	candidates = append(candidates, filepath.Join("dist", cross))
	if native {
		candidates = append(candidates, filepath.Join("bin", plain))
		if p, err := exec.LookPath(plain); err == nil {
			candidates = append(candidates, p)
		}
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			if abs, err := filepath.Abs(c); err == nil {
				return abs
			}
			return c
		}
	}
	return ""
}
