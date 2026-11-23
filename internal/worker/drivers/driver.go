package drivers

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/BryanSant/konsert/internal/protocol"
)

// FindBinary locates an executable via PATH first, then falls back to common user install directories
// (~/.local/bin, /opt/homebrew/bin, /usr/local/bin) so daemons launched via launchd/systemd still find CLIs.
func FindBinary(name string) (string, bool) {
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin", name),
			filepath.Join(home, "bin", name),
		)
	}
	if runtime.GOOS != "windows" {
		candidates = append(candidates,
			filepath.Join("/opt/homebrew/bin", name),
			filepath.Join("/usr/local/bin", name),
		)
	}
	for _, c := range candidates {
		info, err := os.Stat(c)
		if err != nil || info.IsDir() {
			continue
		}
		if runtime.GOOS == "windows" || info.Mode()&0o111 != 0 {
			return c, true
		}
	}
	return "", false
}

// LaunchOptions controls how an AgentDriver constructs its process invocation.
type LaunchOptions struct {
	TaskID         string
	Prompt         string
	Workdir        string
	Model          string
	Mode           string // "oneshot" | "interactive"
	PermissionMode string // "full_access" | "sandboxed"
	Env            []string
}

// LaunchSpec describes the command to spawn inside the PTY.
type LaunchSpec struct {
	Binary string
	Args   []string
	Env    []string
}

// TelemetryEvent represents a structured agent milestone emitted while a task runs.
type TelemetryEvent struct {
	Kind    string // "milestone", "tool_call", "file_edit", "waiting_input", "summary", "error"
	Message string
	Meta    map[string]any
}

// WatchOptions supplies inputs for live telemetry scraping.
type WatchOptions struct {
	TaskID       string
	Workdir      string
	OutputLines  <-chan string
	RingSnapshot func() []byte
}

// SummaryOptions supplies context for extracting a concise final summary.
type SummaryOptions struct {
	TaskID       string
	Workdir      string
	ExitCode     int
	RingSnapshot func() []byte
	Events       []TelemetryEvent
}

// AgentDriver abstracts vendor-specific coding agent CLIs.
type AgentDriver interface {
	Name() string
	Detect() (path string, ok bool)
	BuildCommand(opts LaunchOptions) LaunchSpec
	WatchTelemetry(ctx context.Context, opts WatchOptions, emit func(TelemetryEvent))
	ExtractSummary(ctx context.Context, opts SummaryOptions) string
}

// Registry stores registered AgentDrivers by name.
type Registry struct {
	mu      sync.RWMutex
	drivers map[string]AgentDriver
}

// NewRegistry creates an empty driver registry.
func NewRegistry() *Registry {
	return &Registry{
		drivers: make(map[string]AgentDriver),
	}
}

// DefaultRegistry is the global registry containing built-in MVP drivers.
var DefaultRegistry = NewRegistry()

// Register adds a driver to the global DefaultRegistry.
func Register(d AgentDriver) {
	DefaultRegistry.Register(d)
}

func (r *Registry) Register(d AgentDriver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drivers[d.Name()] = d
}

func (r *Registry) Get(name string) (AgentDriver, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drivers[name]
	return d, ok
}

// All returns all registered drivers sorted by name.
func (r *Registry) All() []AgentDriver {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AgentDriver, 0, len(r.drivers))
	for _, d := range r.drivers {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// DetectInstalled returns sorted names of available drivers.
// The "mock" driver is included only when includeMock is true.
func (r *Registry) DetectInstalled(includeMock bool) []string {
	var names []string
	for _, d := range r.All() {
		if d.Name() == "mock" && !includeMock {
			continue
		}
		if _, ok := d.Detect(); ok {
			names = append(names, d.Name())
		}
	}
	return names
}

// IsFullAccess returns true unless sandboxed mode is explicitly requested.
func IsFullAccess(permissionMode string) bool {
	return permissionMode != protocol.PermissionSandboxed
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// StripANSI removes terminal ANSI escape codes and normalizes carriage returns.
func StripANSI(s string) string {
	clean := ansiRegex.ReplaceAllString(s, "")
	clean = strings.ReplaceAll(clean, "\r\n", "\n")
	clean = strings.ReplaceAll(clean, "\r", "\n")
	return clean
}

// TailCleanSummary returns the last up to maxLines non-empty ANSI-stripped lines from raw PTY bytes.
func TailCleanSummary(raw []byte, maxLines int, maxChars int) string {
	if len(raw) == 0 {
		return ""
	}
	if maxLines <= 0 {
		maxLines = 15
	}
	if maxChars <= 0 {
		maxChars = 1500
	}
	cleaned := StripANSI(string(raw))
	rawLines := strings.Split(cleaned, "\n")
	var lines []string
	for _, l := range rawLines {
		t := strings.TrimSpace(l)
		if t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxChars {
		out = out[len(out)-maxChars:]
	}
	return out
}
