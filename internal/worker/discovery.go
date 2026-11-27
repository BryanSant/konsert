package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
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

	"github.com/BryanSant/syre/internal/gitutil"
	"github.com/BryanSant/syre/internal/protocol"
)

// SessionDiscovery watches ~/.syre/hook-events.jsonl incrementally and performs
// gentle passive process inspection so externally SSH-launched coding agents appear live in Syre.
type SessionDiscovery struct {
	homeDir   string
	emit      func(protocol.ExternalSessionPayload)
	isManaged func(pid int) bool

	// opMu serializes polls and scans (a Run from a just-closed connection may overlap the next).
	opMu sync.Mutex

	mu             sync.Mutex
	hookOffset     int64
	activeExternal map[string]protocol.ExternalSessionPayload // externalID -> last emitted state
	cwdCache       map[int]cachedCwd                          // pid -> resolved cwd (passive scan)

	// observed reads agents' own session stores (claude's transcripts and live status
	// files); nil when not set up.
	observed *observedSessions

	// resolveCwd and cwdTimeout are per-instance so tests can simulate a slow lsof.
	resolveCwd func(ctx context.Context, pid int) string
	cwdTimeout time.Duration
}

// cachedCwd remembers a resolved process cwd; comm guards against PID reuse.
type cachedCwd struct {
	comm string
	cwd  string
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
		cwdCache:       make(map[int]cachedCwd),
		resolveCwd:     resolveProcessCwd,
		cwdTimeout:     defaultCwdTimeout,
	}
	// Start tailing from current EOF so historical hook lines from previous days aren't replayed as active.
	if !agentDiscoveryDisabled() {
		if info, err := os.Stat(HookEventsPath(homeDir)); err == nil {
			sd.hookOffset = info.Size()
		}
	}
	return sd
}

// Run polls the local hook inbox every 2s (tiny stat/read) and passive processes every 12s.
// A SessionDiscovery is meant to outlive individual hub connections: run it once per
// connection; the hook-file offset is kept, so events written while disconnected are
// delivered on the next connection, and Resync re-announces known sessions.
func (s *SessionDiscovery) Run(ctx context.Context) {
	if agentDiscoveryDisabled() {
		return
	}
	s.Resync()
	// Pick up hook events written while disconnected, then scan processes right away so
	// already-open local sessions appear as idle/working.
	s.PollHookEvents(ctx)
	s.PollObserved(ctx)
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
			s.PollObserved(ctx)
		case <-procTicker.C:
			s.ScanPassiveProcesses(ctx)
		}
	}
}

// Resync re-emits every session that is not done, so a new hub connection (possibly to a
// restarted hub) learns the current external roster.
func (s *SessionDiscovery) Resync() {
	if agentDiscoveryDisabled() {
		return
	}
	s.mu.Lock()
	var live []protocol.ExternalSessionPayload
	for _, p := range s.activeExternal {
		if p.Status != "done" {
			live = append(live, p)
		}
	}
	s.mu.Unlock()
	if s.emit == nil {
		return
	}
	for _, p := range live {
		s.emit(p)
	}
}

// PollObserved reads the agents' session stores once (see observedSessions).
func (s *SessionDiscovery) PollObserved(ctx context.Context) {
	if s.observed == nil || agentDiscoveryDisabled() {
		return
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.observed.Poll(ctx)
}

// maxHookRead bounds how much of the hook inbox is read per poll.
const maxHookRead = 512 * 1024

// PollHookEvents reads any newly appended lines in ~/.syre/hook-events.jsonl and emits external session updates.
func (s *SessionDiscovery) PollHookEvents(ctx context.Context) {
	if agentDiscoveryDisabled() {
		return
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
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
	raw, err := io.ReadAll(io.LimitReader(f, maxHookRead))
	if err != nil || len(raw) == 0 {
		return
	}
	// Consume only complete lines: a record still being written (or cut at the read
	// limit) is left for the next poll instead of being lost.
	consumed := completeLinesLen(raw)
	if consumed == 0 {
		if len(raw) < maxHookRead {
			return // partial record; wait for its newline
		}
		consumed = len(raw) // a single line longer than the read window: skip it
		raw = nil
	}
	raw = raw[:min(consumed, len(raw))]

	s.mu.Lock()
	s.hookOffset = offset + int64(consumed)
	s.mu.Unlock()

	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		if s.observed != nil {
			if agent, sid := managedHookSession(sc.Bytes()); sid != "" {
				s.observed.MarkManaged(agent, sid)
			}
		}
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

// completeLinesLen returns the length of b up to and including its last '\n' (0 if none).
func completeLinesLen(b []byte) int {
	return bytes.LastIndexByte(b, '\n') + 1
}

// managedHookSession returns the agent and session id of a hook event fired inside a
// syred-managed task ("" otherwise): that session's transcript is not observed either.
func managedHookSession(line []byte) (agent, sessionID string) {
	if !bytes.Contains(line, []byte(`"task_id"`)) {
		return "", ""
	}
	var ev HookEvent
	if json.Unmarshal(line, &ev, protocol.JSONOptions) != nil || ev.TaskID == "" {
		return "", ""
	}
	return strings.ToLower(strings.TrimSpace(ev.Agent)), ev.SessionID
}

// ParseHookEventLine normalizes a JSONL HookEvent line into an ExternalSessionPayload.
func ParseHookEventLine(line string) (protocol.ExternalSessionPayload, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return protocol.ExternalSessionPayload{}, false
	}
	var ev HookEvent
	if err := json.Unmarshal([]byte(line), &ev, protocol.JSONOptions); err != nil || ev.Agent == "" {
		return protocol.ExternalSessionPayload{}, false
	}
	if ev.TaskID != "" {
		// Fired inside a syred-managed task: that task is not an external session.
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
	tool := ""
	if strings.EqualFold(ev.Event, "PreToolUse") {
		tool = ev.Tool // PostToolUse: the tool finished
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
		Tool:         tool,
		Event:        ev.Event,
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

// Per-step timeouts for the passive scan. Each step gets its own deadline so one slow
// lsof (common on macOS) cannot starve the rest of the scan.
const (
	psTimeout         = 3 * time.Second
	defaultCwdTimeout = 1500 * time.Millisecond
)

type psEntry struct {
	pid, ppid int
	pcpu      float64
	comm      string
}

func parsePsOutput(out []byte) []psEntry {
	var entries []psEntry
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		pcpu, _ := strconv.ParseFloat(fields[2], 64)
		entries = append(entries, psEntry{pid: pid, ppid: ppid, pcpu: pcpu, comm: strings.Join(fields[3:], " ")})
	}
	return entries
}

// managedOrDescendant reports whether pid, or any of its ancestors, is a syred-managed
// task process (an agent's own helper processes are covered by the managed task).
func (s *SessionDiscovery) managedOrDescendant(pid int, parent map[int]int) bool {
	if s.isManaged == nil {
		return false
	}
	for i := 0; i < 64 && pid > 1; i++ {
		if s.isManaged(pid) {
			return true
		}
		next, ok := parent[pid]
		if !ok || next == pid {
			return false
		}
		pid = next
	}
	return false
}

// ScanPassiveProcesses gently inspects running user agent processes not managed by syred.
func (s *SessionDiscovery) ScanPassiveProcesses(ctx context.Context) {
	if runtime.GOOS == "windows" || agentDiscoveryDisabled() {
		return
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	psCtx, cancel := context.WithTimeout(ctx, psTimeout)
	out, err := exec.CommandContext(psCtx, "ps", "-eo", "pid=,ppid=,pcpu=,comm=").Output()
	cancel()
	if err != nil {
		// Without a process list we cannot tell who exited: keep the current roster.
		return
	}
	s.scanProcessList(ctx, parsePsOutput(out))
}

func (s *SessionDiscovery) scanProcessList(ctx context.Context, procs []psEntry) {
	if agentDiscoveryDisabled() {
		return
	}
	// Snapshot active hook- and transcript-tracked (agent, cwd) pairs so passive discovery
	// never duplicates them.
	s.mu.Lock()
	hookCovered := make(map[string]bool)
	for _, prev := range s.activeExternal {
		if (prev.Source == "hook" || prev.Source == "transcript") && prev.Status != "done" && prev.Cwd != "" {
			hookCovered[prev.Agent+"|"+filepath.Clean(prev.Cwd)] = true
		}
	}
	s.mu.Unlock()

	parent := make(map[int]int, len(procs))
	for _, p := range procs {
		parent[p.pid] = p.ppid
	}

	seenNow := make(map[string]bool)
	livePIDs := make(map[int]bool)
	selfPID := os.Getpid()
	for _, p := range procs {
		if p.pid <= 1 || p.pid == selfPID {
			continue
		}
		agent, ok := passiveAgentBinaries[strings.ToLower(filepath.Base(p.comm))]
		if !ok {
			continue
		}
		if s.managedOrDescendant(p.pid, parent) {
			continue
		}
		livePIDs[p.pid] = true
		extID := fmt.Sprintf("pid-%d", p.pid)

		cwd := s.cachedProcessCwd(ctx, p)
		if cwd == "" {
			// Unresolved (slow/failed lsof): the process is still running, so a session we
			// already know stays present instead of flickering to done.
			s.mu.Lock()
			_, known := s.activeExternal[extID]
			s.mu.Unlock()
			if known {
				seenNow[extID] = true
			}
			continue
		}
		if hookCovered[agent+"|"+filepath.Clean(cwd)] {
			continue
		}
		seenNow[extID] = true

		status := "idle"
		if p.pcpu >= passiveActiveCPUThreshold {
			status = "working"
		}

		branch := quickGitBranch(ctx, cwd)
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
	for pid := range s.cwdCache {
		if !livePIDs[pid] {
			delete(s.cwdCache, pid)
		}
	}
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

// cachedProcessCwd resolves a process cwd with its own timeout. A successful result is
// cached per PID (guarded by comm against PID reuse); a failed lookup falls back to the
// cached value.
func (s *SessionDiscovery) cachedProcessCwd(ctx context.Context, p psEntry) string {
	cctx, cancel := context.WithTimeout(ctx, s.cwdTimeout)
	cwd := s.resolveCwd(cctx, p.pid)
	cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if cwd != "" {
		s.cwdCache[p.pid] = cachedCwd{comm: p.comm, cwd: cwd}
		return cwd
	}
	if c, ok := s.cwdCache[p.pid]; ok && c.comm == p.comm {
		return c.cwd
	}
	return ""
}

func (s *SessionDiscovery) emitIfChanged(p protocol.ExternalSessionPayload) {
	s.mu.Lock()
	prev, exists := s.activeExternal[p.ExternalID]
	changed := !exists || prev.Status != p.Status || prev.Tool != p.Tool || prev.Event != p.Event || prev.Cwd != p.Cwd || prev.Branch != p.Branch || prev.Model != p.Model || prev.Waiting != p.Waiting || len(p.ChangedFiles) > 0 || (p.Summary != "" && p.Summary != prev.Summary)
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
