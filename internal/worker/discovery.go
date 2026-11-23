package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/gitutil"
	"github.com/BryanSant/konsert/internal/protocol"
)

// SessionDiscovery watches ~/.konsert/hook-events.jsonl incrementally and performs
// gentle passive process inspection so externally SSH-launched coding agents appear live in Konsert.
type SessionDiscovery struct {
	homeDir   string
	emit      func(protocol.ExternalSessionPayload)
	isManaged func(pid int) bool

	mu             sync.Mutex
	hookOffset     int64
	activeExternal map[string]protocol.ExternalSessionPayload // externalID -> last emitted state
}

func NewSessionDiscovery(homeDir string, isManaged func(pid int) bool, emit func(protocol.ExternalSessionPayload)) *SessionDiscovery {
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	sd := &SessionDiscovery{
		homeDir:        homeDir,
		emit:           emit,
		isManaged:      isManaged,
		activeExternal: make(map[string]protocol.ExternalSessionPayload),
	}
	// Start tailing from current EOF so historical hook lines from previous days aren't replayed as active.
	if info, err := os.Stat(HookEventsPath(homeDir)); err == nil {
		sd.hookOffset = info.Size()
	}
	return sd
}

// Run polls the local hook inbox every 2s (tiny stat/read) and passive processes every 12s.
func (s *SessionDiscovery) Run(ctx context.Context) {
	// Immediate initial scan so already-open local sessions appear right away as idle/working.
	s.ScanPassiveProcesses(ctx)

	hookTicker := time.NewTicker(2 * time.Second)
	procTicker := time.NewTicker(12 * time.Second)
	defer hookTicker.Stop()
	defer procTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-hookTicker.C:
			s.PollHookEvents(ctx)
		case <-procTicker.C:
			s.ScanPassiveProcesses(ctx)
		}
	}
}

// PollHookEvents reads any newly appended lines in ~/.konsert/hook-events.jsonl and emits external session updates.
func (s *SessionDiscovery) PollHookEvents(ctx context.Context) {
	path := HookEventsPath(s.homeDir)
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	s.mu.Lock()
	if info.Size() < s.hookOffset {
		s.hookOffset = 0
	}
	if info.Size() == s.hookOffset {
		s.mu.Unlock()
		return
	}
	offset := s.hookOffset
	s.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(f, 512*1024))
	if err != nil || len(raw) == 0 {
		return
	}

	s.mu.Lock()
	s.hookOffset = offset + int64(len(raw))
	s.mu.Unlock()

	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		payload, ok := ParseHookEventLine(sc.Text())
		if !ok {
			continue
		}
		if payload.Cwd != "" && payload.Branch == "" {
			if b := quickGitBranch(ctx, payload.Cwd); b != "" {
				payload.Branch = b
			}
		}
		s.emitIfChanged(payload)
	}
}

// ParseHookEventLine normalizes a JSONL HookEvent line into an ExternalSessionPayload.
func ParseHookEventLine(line string) (protocol.ExternalSessionPayload, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return protocol.ExternalSessionPayload{}, false
	}
	var ev HookEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Agent == "" {
		return protocol.ExternalSessionPayload{}, false
	}
	extID := ev.SessionID
	if extID == "" {
		extID = ev.Agent + "-hook"
	}
	status := normalizeHookStatus(ev.Event)
	var changed []string
	if ev.FilePath != "" {
		changed = []string{ev.FilePath}
	}
	return protocol.ExternalSessionPayload{
		ExternalID:   extID,
		Agent:        strings.ToLower(strings.TrimSpace(ev.Agent)),
		Model:        ev.Model,
		Cwd:          ev.Cwd,
		Status:       status,
		Summary:      ev.Message,
		ChangedFiles: changed,
		Source:       "hook",
	}, true
}

func normalizeHookStatus(ev string) string {
	switch strings.ToLower(strings.TrimSpace(ev)) {
	case "stop", "idle":
		return "idle"
	case "done", "completed", "sessionend":
		return "done"
	case "notification", "needs_you", "waiting_input", "permissionrequest":
		return "needs_you"
	case "fail", "failed", "error":
		return "fail"
	default:
		return "working"
	}
}

var passiveAgentBinaries = map[string]string{
	"claude": "claude",
	"agy":    "agy",
	"codex":  "codex",
	"grok":   "grok",
}

// passiveActiveCPUThreshold is the %CPU threshold above which a passively discovered
// coding agent process (without hook telemetry) is considered actively "working" vs "idle".
const passiveActiveCPUThreshold = 5.0

// ScanPassiveProcesses gently inspects running user agent processes not managed by konsertd.
func (s *SessionDiscovery) ScanPassiveProcesses(ctx context.Context) {
	if runtime.GOOS == "windows" {
		return
	}
	pCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	out, err := exec.CommandContext(pCtx, "ps", "-eo", "pid=,pcpu=,comm=").Output()
	if err != nil {
		return
	}

	// Snapshot active hook-tracked (agent, cwd) pairs so passive discovery never duplicates them.
	s.mu.Lock()
	hookCovered := make(map[string]bool)
	for _, prev := range s.activeExternal {
		if prev.Source == "hook" && prev.Status != "done" && prev.Cwd != "" {
			hookCovered[prev.Agent+"|"+filepath.Clean(prev.Cwd)] = true
		}
	}
	s.mu.Unlock()

	seenNow := make(map[string]bool)
	selfPID := os.Getpid()
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 1 || pid == selfPID {
			continue
		}
		if s.isManaged != nil && s.isManaged(pid) {
			continue
		}
		pcpu, _ := strconv.ParseFloat(fields[1], 64)
		comm := strings.Join(fields[2:], " ")
		base := filepath.Base(comm)
		agent, ok := passiveAgentBinaries[strings.ToLower(base)]
		if !ok {
			continue
		}
		cwd := resolveProcessCwd(pCtx, pid)
		if cwd == "" {
			continue
		}
		if hookCovered[agent+"|"+filepath.Clean(cwd)] {
			continue
		}
		extID := fmt.Sprintf("pid-%d", pid)
		seenNow[extID] = true

		status := "idle"
		if pcpu >= passiveActiveCPUThreshold {
			status = "working"
		}

		branch := quickGitBranch(pCtx, cwd)
		s.emitIfChanged(protocol.ExternalSessionPayload{
			ExternalID: extID,
			Agent:      agent,
			Cwd:        cwd,
			Branch:     branch,
			Status:     status,
			Source:     "passive",
		})
	}

	// Mark previously discovered passive PIDs that have exited as "done".
	s.mu.Lock()
	var finished []protocol.ExternalSessionPayload
	for id, prev := range s.activeExternal {
		if prev.Source == "passive" && prev.Status != "done" && !seenNow[id] {
			prev.Status = "done"
			s.activeExternal[id] = prev
			finished = append(finished, prev)
		}
	}
	s.mu.Unlock()
	for _, fin := range finished {
		if s.emit != nil {
			s.emit(fin)
		}
	}
}

func (s *SessionDiscovery) emitIfChanged(p protocol.ExternalSessionPayload) {
	s.mu.Lock()
	prev, exists := s.activeExternal[p.ExternalID]
	changed := !exists || prev.Status != p.Status || prev.Cwd != p.Cwd || prev.Branch != p.Branch || len(p.ChangedFiles) > 0 || (p.Summary != "" && p.Summary != prev.Summary)
	if changed {
		s.activeExternal[p.ExternalID] = p
	}
	s.mu.Unlock()
	if changed && s.emit != nil {
		s.emit(p)
	}
}

func resolveProcessCwd(ctx context.Context, pid int) string {
	if runtime.GOOS == "linux" {
		if p, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
			return p
		}
		return ""
	}
	if runtime.GOOS == "darwin" {
		out, err := exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
		if err != nil {
			return ""
		}
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "n/") {
				return strings.TrimPrefix(line, "n")
			}
		}
	}
	return ""
}

func quickGitBranch(ctx context.Context, dir string) string {
	if dir == "" {
		return ""
	}
	bCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	if _, isGit := gitutil.IsGitRepo(bCtx, dir); !isGit {
		return ""
	}
	out, err := exec.CommandContext(bCtx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
