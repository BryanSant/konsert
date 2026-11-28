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
	"github.com/BryanSant/syre/internal/worker/shells"
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
	cpuSeen        map[int]cpuMark                            // pid -> CPU time at the last scan
	// procs is the process table the passive scan reads (nil: this machine's).
	procs shells.Source

	// observed reads agents' own session stores (claude's transcripts and live status
	// files); nil when not set up.
	observed *observedSessions
	// sendObserved sends a watched observed session's new events to the hub.
	sendObserved func(protocol.ObservedEvents)

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
		cpuSeen:        make(map[int]cpuMark),
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

// observedPIDs maps every observed session with a known agent process to it: passive
// ones (pid-<n>) and transcript ones whose agent publishes its PID.
func (s *SessionDiscovery) observedPIDs() map[string]int {
	out := map[string]int{}
	if s.observed != nil {
		out = s.observed.pids()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.activeExternal {
		if p.Source != "passive" || p.Status == "done" {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimPrefix(id, "pid-")); err == nil && pid > 0 {
			out[id] = pid
		}
	}
	return out
}

// PollObserved reads the agents' session stores once (see observedSessions).
func (s *SessionDiscovery) PollObserved(ctx context.Context) {
	if s.observed == nil || agentDiscoveryDisabled() {
		return
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.observed.Poll(ctx)
	s.observed.pollWatches(ctx, s.sendObserved)
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

// passiveAgentBinaries maps an agent CLI's executable name to the agent.
var passiveAgentBinaries = map[string]string{
	"claude":       "claude",
	"agy":          "agy",
	"codex":        "codex",
	"grok":         "grok",
	"copilot":      "copilot",
	"cursor-agent": "cursor",
}

// passiveScriptAgents finds agents that run as a script of a JavaScript runtime, by a
// marker in its first arguments: cursor-agent's launcher execs node with argv[0] set to
// its own path and versions/<v>/index.js; an npm-installed copilot runs
// node .../@github/copilot/index.js (not @github/copilot-language-server, an editor's).
var passiveScriptAgents = []struct{ marker, agent string }{
	{"/cursor-agent", "cursor"},
	{"@github/copilot/", "copilot"},
}

// passiveAgent names the agent CLI a process runs, or "". argv is read only for a
// JavaScript runtime. Executable names match exactly outside Windows, so a desktop app
// ("Claude", "Cursor") is not taken for its CLI; on Windows they match case-insensitively
// without .exe, and an installed desktop app's claude.exe is told apart by its path.
func passiveAgent(name string, argv func() []string) string {
	base := filepath.Base(filepath.ToSlash(name))
	if i := strings.LastIndexAny(base, `\`); i >= 0 {
		base = base[i+1:]
	}
	if runtime.GOOS == "windows" {
		base = strings.TrimSuffix(strings.ToLower(base), ".exe")
	}
	if agent, ok := passiveAgentBinaries[base]; ok {
		if runtime.GOOS == "windows" && agent == "claude" {
			if a := argv(); len(a) > 0 && strings.Contains(strings.ToLower(a[0]), "anthropicclaude") {
				return "" // the Claude desktop app
			}
		}
		return agent
	}
	rt := strings.TrimSuffix(strings.ToLower(base), ".exe")
	if versionName(rt) {
		// A versioned executable behind a symlink (~/.local/bin/claude →
		// ~/.local/share/claude/versions/2.1.332): the OS names the process by the file,
		// argv[0] by the command the user typed.
		if a := argv(); len(a) > 0 {
			if agent, ok := passiveAgentBinaries[argv0Name(a)]; ok {
				return agent
			}
		}
		return ""
	}
	// Recent node names its main thread, and so the process on Linux: "node-MainThread";
	// a single-executable app (copilot) shows just "MainThread", its argv[0] the command.
	if rt != "node" && rt != "bun" && rt != "mainthread" && !strings.HasPrefix(rt, "node-") {
		return ""
	}
	a := argv()
	if len(a) > 0 {
		if agent, ok := passiveAgentBinaries[argv0Name(a)]; ok {
			return agent
		}
	}
	for i := 0; i < len(a) && i < 3; i++ {
		arg := strings.ToLower(strings.ReplaceAll(a[i], `\`, "/"))
		for _, m := range passiveScriptAgents {
			if strings.Contains(arg, m.marker) {
				return m.agent
			}
		}
	}
	return ""
}

// argv0Name is the command a process was started as: argv[0]'s base name, lower case,
// without .exe.
func argv0Name(a []string) string {
	if len(a) == 0 {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(a[0], `\`, "/"))), ".exe")
}

// versionName reports a process name that is a version ("2.1.332"): digits and dots.
func versionName(n string) bool {
	if n == "" || n[0] < '0' || n[0] > '9' {
		return false
	}
	return strings.Trim(n, "0123456789.") == "" && strings.Contains(n, ".")
}

// passiveActiveCPUThreshold is the %CPU threshold above which a passively discovered
// coding agent process is considered "working" rather than "idle". Only for agents with
// no structured source (no session records Syre reads): the fallback.
const passiveActiveCPUThreshold = 5.0

// defaultCwdTimeout bounds one process's cwd lookup, so one slow lsof (common on macOS)
// cannot starve the rest of the scan.
const defaultCwdTimeout = 1500 * time.Millisecond

type psEntry struct {
	pid, ppid int
	pcpu      float64
	comm      string
	// agent is the agent the process runs, when the scan already matched it (by argv).
	agent string
}

// cpuMark is a process's CPU time at a scan, to turn the next one into a percentage.
type cpuMark struct {
	start time.Time
	cpu   time.Duration
	at    time.Time
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
	if agentDiscoveryDisabled() {
		return
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	src := s.procs
	if src == nil {
		src = shells.OSSource()
	}
	list, err := src.Snapshot()
	if err != nil {
		// Without a process list we cannot tell who exited: keep the current roster.
		return
	}
	now := time.Now()
	var entries []psEntry
	for _, p := range list {
		if p.Zombie || p.PID <= 1 {
			continue
		}
		e := psEntry{pid: p.PID, ppid: p.PPID, comm: p.Name}
		if agent := passiveAgent(p.Name, func() []string { return src.Argv(p.PID) }); agent != "" {
			// Only agent processes need their start and CPU (costly per process on Windows).
			src.Details(&p)
			e.agent = agent
			e.pcpu = s.cpuPercent(p, now)
		}
		entries = append(entries, e)
	}
	s.scanProcessList(ctx, entries)
}

// cpuPercent is p's CPU use since the previous scan (0 on first sight or when unknown).
func (s *SessionDiscovery) cpuPercent(p shells.Proc, now time.Time) float64 {
	if !p.CPUKnown {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.cpuSeen[p.PID]
	s.cpuSeen[p.PID] = cpuMark{start: p.Start, cpu: p.CPU, at: now}
	if !ok || !prev.start.Equal(p.Start) || p.CPU < prev.cpu || !now.After(prev.at) {
		return 0
	}
	return 100 * float64(p.CPU-prev.cpu) / float64(now.Sub(prev.at))
}

func (s *SessionDiscovery) scanProcessList(ctx context.Context, procs []psEntry) {
	if agentDiscoveryDisabled() {
		return
	}
	// Agents with a structured source (their own session records) are left to it: their
	// status comes from the records, never from CPU (SYR-HBQ5G).
	sourced := map[string]bool{}
	if s.observed != nil {
		sourced = s.observed.sourcedAgents()
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
		agent := p.agent
		if agent == "" {
			agent = passiveAgent(p.comm, func() []string { return nil })
		}
		if agent == "" || sourced[agent] {
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
	for pid := range s.cpuSeen {
		if !livePIDs[pid] {
			delete(s.cpuSeen, pid)
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
	if runtime.GOOS == "windows" {
		return processCwd(pid)
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
