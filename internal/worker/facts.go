package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

var semverRegex = regexp.MustCompile(`\d+(?:\.\d+){1,2}`)

// CollectMachineFacts gathers rich, non-intrusive hardware, OS, virtualization,
// filesystem, and safe capability/auth-readiness facts for the local host.
// It never reads or transmits credentials, tokens, or account identifiers.
func CollectMachineFacts(parentCtx context.Context) protocol.MachineFacts {
	ctx, cancel := context.WithTimeout(parentCtx, 4*time.Second)
	defer cancel()

	f := protocol.MachineFacts{
		OSName:         runtime.GOOS,
		CPUCores:       runtime.NumCPU(),
		StorageType:    "unknown",
		Virtualization: "bare_metal",
		Tools:          map[string]string{},
		AuthReady:      map[string]bool{},
		CollectedAt:    time.Now().UTC(),
	}

	collectPlatformFacts(ctx, &f)
	f.Virtualization = classifyVirtualization(&f)
	collectToolAndAuthCapabilities(ctx, &f)
	return f
}

func classifyVirtualization(f *protocol.MachineFacts) string {
	switch {
	case f.IsWSL:
		return "wsl"
	case f.IsMacContainer:
		return "mac_container"
	case f.IsContainer:
		return "container"
	case f.IsVM:
		return "vm"
	default:
		return "bare_metal"
	}
}

type toolProbe struct {
	name    string
	bin     string
	args    []string
	timeout time.Duration // 0 means defaultToolProbeTimeout
}

const defaultToolProbeTimeout = 700 * time.Millisecond

var defaultToolProbes = []toolProbe{
	{name: "git", bin: "git", args: []string{"--version"}},
	{name: "go", bin: "go", args: []string{"version"}},
	{name: "node", bin: "node", args: []string{"--version"}},
	{name: "python3", bin: "python3", args: []string{"--version"}},
	{name: "rustc", bin: "rustc", args: []string{"--version"}},
	{name: "docker", bin: "docker", args: []string{"--version"}},
	{name: "podman", bin: "podman", args: []string{"--version"}},
	{name: "postgres", bin: "postgres", args: []string{"--version"}},
	{name: "psql", bin: "psql", args: []string{"--version"}},
	{name: "gcloud", bin: "gcloud", args: []string{"--version"}},
	{name: "aws", bin: "aws", args: []string{"--version"}},
	{name: "kubectl", bin: "kubectl", args: []string{"version", "--client", "--short"}},
	{name: "gh", bin: "gh", args: []string{"--version"}},
	{name: "claude", bin: "claude", args: []string{"--version"}},
	{name: "agy", bin: "agy", args: []string{"--version"}},
	{name: "codex", bin: "codex", args: []string{"--version"}},
	{name: "grok", bin: "grok", args: []string{"--version"}},
	{name: "copilot", bin: "copilot", args: []string{"--version"}},
	// cursor-agent is a Node bundle: `--version` takes over a second to start.
	{name: "cursor", bin: "cursor-agent", args: []string{"--version"}, timeout: 3 * time.Second},
}

func collectToolAndAuthCapabilities(parentCtx context.Context, f *protocol.MachineFacts) {
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, probe := range defaultToolProbes {
		binPath, ok := drivers.FindBinary(probe.bin)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(p toolProbe, bin string) {
			defer wg.Done()
			timeout := p.timeout
			if timeout == 0 {
				timeout = defaultToolProbeTimeout
			}
			tCtx, cancel := context.WithTimeout(parentCtx, timeout)
			defer cancel()
			out, err := exec.CommandContext(tCtx, bin, p.args...).CombinedOutput()
			ver := extractVersion(string(out))
			if ver == "" && err == nil {
				ver = "installed"
			}
			if ver == "" {
				ver = "installed"
			}
			mu.Lock()
			f.Tools[p.name] = ver
			// Normalize psql -> postgres capability when server binary isn't in PATH
			if p.name == "psql" {
				if _, hasPG := f.Tools["postgres"]; !hasPG {
					f.Tools["postgres"] = ver
				}
			}
			mu.Unlock()
		}(probe, binPath)
	}

	// Safe auth/daemon readiness probes (booleans ONLY — never inspect or transmit secrets/emails).
	authChecks := []struct {
		name string
		fn   func(context.Context) bool
	}{
		{
			name: "gcloud",
			fn: func(ctx context.Context) bool {
				bin, ok := drivers.FindBinary("gcloud")
				if !ok {
					return false
				}
				c, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
				defer cancel()
				out, err := exec.CommandContext(c, bin, "auth", "list", "--filter=status:ACTIVE", "--format=value(account)").Output()
				return err == nil && strings.TrimSpace(string(out)) != ""
			},
		},
		{
			name: "gh",
			fn: func(ctx context.Context) bool {
				bin, ok := drivers.FindBinary("gh")
				if !ok {
					return false
				}
				c, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
				defer cancel()
				return exec.CommandContext(c, bin, "auth", "status").Run() == nil
			},
		},
		{
			name: "kubectl",
			fn: func(ctx context.Context) bool {
				bin, ok := drivers.FindBinary("kubectl")
				if !ok {
					return false
				}
				c, cancel := context.WithTimeout(ctx, 600*time.Millisecond)
				defer cancel()
				out, err := exec.CommandContext(c, bin, "config", "current-context").Output()
				return err == nil && strings.TrimSpace(string(out)) != ""
			},
		},
		{
			name: "aws",
			fn: func(_ context.Context) bool {
				if os.Getenv("AWS_PROFILE") != "" || os.Getenv("AWS_ACCESS_KEY_ID") != "" {
					return true
				}
				home, _ := os.UserHomeDir()
				if home == "" {
					return false
				}
				if info, err := os.Stat(filepath.Join(home, ".aws", "credentials")); err == nil && info.Size() > 0 {
					return true
				}
				if info, err := os.Stat(filepath.Join(home, ".aws", "config")); err == nil && info.Size() > 0 {
					return true
				}
				return false
			},
		},
		{
			name: "podman",
			fn: func(ctx context.Context) bool {
				bin, ok := drivers.FindBinary("podman")
				if !ok {
					return false
				}
				c, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
				defer cancel()
				return exec.CommandContext(c, bin, "info", "--format", "{{.Host.Arch}}").Run() == nil
			},
		},
		{
			name: "docker",
			fn: func(ctx context.Context) bool {
				bin, ok := drivers.FindBinary("docker")
				if !ok {
					return false
				}
				c, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
				defer cancel()
				return exec.CommandContext(c, bin, "info", "--format", "{{.OSType}}").Run() == nil
			},
		},
	}

	for _, ac := range authChecks {
		wg.Add(1)
		go func(name string, fn func(context.Context) bool) {
			defer wg.Done()
			if fn(parentCtx) {
				mu.Lock()
				f.AuthReady[name] = true
				mu.Unlock()
			}
		}(ac.name, ac.fn)
	}

	wg.Wait()
}

func extractVersion(out string) string {
	firstLine := strings.Split(strings.TrimSpace(out), "\n")[0]
	if m := semverRegex.FindString(firstLine); m != "" {
		return m
	}
	return ""
}
