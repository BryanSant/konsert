package worker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/chat"
	"github.com/BryanSant/syre/internal/worker/observe"
	"github.com/BryanSant/syre/internal/worker/shells"
)

// Observed sessions without a live agent process (no sessions/<pid>.json: an older
// CLI, or one that died) get their status from the records alone, which can't tell a
// finished session from one waiting at its prompt: a turn that stopped writing for
// observedStaleWorking is shown idle, and an idle one is done after observedDoneAfter.
const (
	observedStaleWorking = 10 * time.Minute
	observedDoneAfter    = 30 * time.Minute
	// observedLookback is how far back discovery looks after a start: a session with no
	// live process last written earlier is history.
	observedLookback = time.Hour
	// observedRecentEdit bounds which edits count as changed files: a first read of a
	// long transcript doesn't report every file it ever edited.
	observedRecentEdit = 10 * time.Minute
	// maxPendingEdits bounds the edit calls remembered per session.
	maxPendingEdits = 256
)

// observedSessions reads agents' own session stores (internal/worker/observe) and
// reports each session found as an external session: status, tool, model, title and
// changed files, keyed by the agent's session id (the id claude's hooks report too).
type observedSessions struct {
	reg     *observe.Registry
	managed *observe.ManagedSet
	emit    func(protocol.ExternalSessionPayload)
	now     func() time.Time

	sources []observe.Source
	// claudeDirs lists claude's config dirs (sourcedAgents: claude has a source when any
	// exists).
	claudeDirs func() []observe.ClaudeDir
	codexDirs  func() []observe.CodexDir

	mu      sync.Mutex
	st      map[string]*observedState  // by Session.Key
	known   map[string]observe.Session // by session id, for transcripts
	watches map[string]*transcriptWatch
}

type observedState struct {
	p         protocol.ExternalSessionPayload
	recStatus string    // status from the records
	titled    bool      // Summary is the agent's title, not a prompt
	live      bool      // a live process publishes the status
	pid       int       // its process, when known
	lastWrite time.Time // the transcript's mtime
	emitted   bool
	// edits are the paths of edit tool calls still running, by tool call id: a result
	// read in a later pass than its call carries no input.
	edits map[string][]string
}

// observedConfig wires observedSessions into a daemon.
type observedConfig struct {
	ClaudeDirs func() []observe.ClaudeDir
	CodexDirs  func() []observe.CodexDir
	Procs      observe.CodexProcs
	// Fresh snapshots processes and lists transcripts on every poll (tests).
	Fresh bool
	// IsManaged reports a process Syre runs (a task's agent, syred itself).
	IsManaged   func(pid int) bool
	OffsetsPath string
	Logger      *slog.Logger
	// Grace overrides observe.DefaultGrace (tests).
	Grace time.Duration
	Now   func() time.Time
}

func newObservedSessions(cfg observedConfig, emit func(protocol.ExternalSessionPayload)) *observedSessions {
	o := &observedSessions{managed: &observe.ManagedSet{}, emit: emit, now: cfg.Now, st: map[string]*observedState{}}
	if o.now == nil {
		o.now = time.Now
	}
	isManaged := cfg.IsManaged
	if isManaged == nil {
		isManaged = func(int) bool { return false }
	}
	claude := &observe.ClaudeSource{Dirs: cfg.ClaudeDirs, Procs: cfg.Procs}
	if cfg.Fresh {
		claude.ProcTTL, claude.ListEvery = -1, -1
	}
	codex := &observe.CodexSource{Dirs: cfg.CodexDirs, Procs: cfg.Procs, OpenFiles: processOpenFiles,
		Cwd: func(pid int) string {
			ctx, cancel := context.WithTimeout(context.Background(), defaultCwdTimeout)
			defer cancel()
			return resolveProcessCwd(ctx, pid)
		}}
	if cfg.Fresh {
		codex.ProcTTL, codex.ListEvery = -1, -1
	}
	o.sources = []observe.Source{claude, codex}
	o.claudeDirs, o.codexDirs = cfg.ClaudeDirs, cfg.CodexDirs
	o.reg = observe.New(observe.Config{
		Sources: o.sources,
		Managed: func(s observe.Session) bool {
			if o.managed.Has(s) {
				return true
			}
			if s.PID > 0 && isManaged(s.PID) {
				return true
			}
			for _, a := range s.Ancestors {
				if isManaged(a) {
					return true
				}
			}
			return false
		},
		Emit:        o.handle,
		OffsetsPath: cfg.OffsetsPath,
		Grace:       cfg.Grace,
		Lookback:    observedLookback,
		Now:         cfg.Now,
		Logger:      cfg.Logger,
	})
	return o
}

// sourcedAgents lists the agents whose sessions this machine reads from their own
// records: the process scan leaves those agents alone.
func (o *observedSessions) sourcedAgents() map[string]bool {
	out := map[string]bool{}
	if o.claudeDirs != nil && len(o.claudeDirs()) > 0 {
		out["claude"] = true
	}
	if o.codexDirs != nil && len(o.codexDirs()) > 0 {
		out["codex"] = true
	}
	return out
}

// MarkManaged records an agent session id Syre runs (a chat's session_ready, a hook
// fired inside a managed task): it is never reported, and withdrawn if it already was.
func (o *observedSessions) MarkManaged(agent, sessionID string) {
	o.managed.Mark(agent, sessionID)
}

// Poll runs one discovery and read pass, then ages sessions without a live process.
func (o *observedSessions) Poll(ctx context.Context) {
	o.reg.Poll(ctx)
	o.sweep()
}

func (o *observedSessions) handle(u observe.Update) {
	s := u.Session
	if u.Kind == observe.UpdateEvents {
		o.remember(s)
	}
	key := s.Key()
	now := o.now()
	o.mu.Lock()
	st := o.st[key]
	if u.Kind == observe.UpdateRetract {
		delete(o.st, key)
		o.mu.Unlock()
		if st != nil && st.emitted {
			p := st.p
			p.Status, p.Tool, p.ChangedFiles = "done", "", nil
			o.emit(p)
		}
		return
	}
	if st == nil {
		account := s.Account
		if account == "" {
			account = s.Agent
		}
		st = &observedState{p: protocol.ExternalSessionPayload{ExternalID: s.ID, Agent: s.Agent, Account: account, Source: "transcript"}}
		o.st[key] = st
	}
	p := &st.p
	p.ChangedFiles = nil
	if s.Cwd != "" && s.Cwd != p.Cwd {
		p.Cwd = s.Cwd
		p.Branch = quickGitBranch(context.Background(), s.Cwd)
	}
	if s.Model != "" {
		p.Model = s.Model
	}
	for _, ev := range u.Events {
		if ev.Model != "" {
			p.Model = ev.Model
		}
		switch ev.Kind {
		case protocol.EventStatus:
			st.recStatus = ev.Status
		case protocol.EventTitle:
			p.Summary, st.titled = ev.Text, true
		case protocol.EventUserMessage:
			if !st.titled {
				p.Summary = firstLine(ev.Text, 160)
			}
		case protocol.EventToolCall:
			tc := ev.ToolCall
			if tc.Status == "running" {
				p.Tool = tc.Name
				if tc.Kind == "edit" && len(tc.Paths) > 0 && len(st.edits) < maxPendingEdits {
					if st.edits == nil {
						st.edits = map[string][]string{}
					}
					st.edits[tc.ID] = tc.Paths
				}
				continue
			}
			p.Tool = ""
			paths, ok := st.edits[tc.ID]
			delete(st.edits, tc.ID)
			if ok && tc.Status == "completed" && now.Sub(ev.At) < observedRecentEdit {
				p.ChangedFiles = append(p.ChangedFiles, paths...)
			}
		}
	}
	st.live = s.Live()
	st.pid = s.PID
	st.lastWrite = s.ModTime
	switch {
	case s.Status != "":
		p.Status = s.Status
	case st.recStatus != "":
		p.Status = st.recStatus
	default:
		p.Status = protocol.ChatStatusIdle
	}
	p.Waiting = ""
	if p.Status == protocol.ChatStatusNeedsYou {
		p.Waiting = s.Waiting
	}
	if p.Status != protocol.ChatStatusWorking {
		p.Tool = ""
	}
	if !st.live {
		p.Status = recordStatusAt(p.Status, st.lastWrite, now)
	}
	out := *p
	done := out.Status == protocol.ChatStatusDone
	if done {
		delete(o.st, key)
	}
	// A session first seen already finished is history: nothing to show.
	quiet := !st.emitted && done
	st.emitted = true
	o.mu.Unlock()
	if !quiet {
		o.emit(out)
	}
}

// pids maps each observed session with a known process to it (external id → PID).
func (o *observedSessions) pids() map[string]int {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := map[string]int{}
	for _, st := range o.st {
		if st.live && st.pid > 0 {
			out[st.p.ExternalID] = st.pid
		}
	}
	return out
}

// recordStatusAt ages a status that comes from the records alone.
func recordStatusAt(status string, lastWrite, now time.Time) string {
	if lastWrite.IsZero() || status == protocol.ChatStatusDone {
		return status
	}
	idleFor := now.Sub(lastWrite)
	switch {
	case idleFor > observedDoneAfter:
		return protocol.ChatStatusDone
	case status == protocol.ChatStatusWorking && idleFor > observedStaleWorking:
		return protocol.ChatStatusIdle
	}
	return status
}

// sweep re-ages the sessions without a live process; nothing is read.
func (o *observedSessions) sweep() {
	now := o.now()
	var out []protocol.ExternalSessionPayload
	o.mu.Lock()
	for key, st := range o.st {
		if st.live {
			continue
		}
		status := recordStatusAt(st.p.Status, st.lastWrite, now)
		if status == st.p.Status {
			continue
		}
		st.p.Status, st.p.Tool, st.p.ChangedFiles = status, "", nil
		if status == protocol.ChatStatusDone {
			delete(o.st, key)
		}
		if st.emitted {
			out = append(out, st.p)
		}
	}
	o.mu.Unlock()
	for _, p := range out {
		o.emit(p)
	}
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return chat.Truncate(s, max)
}

// observedClaudeDirs is replaced by tests so daemons under test never read the
// developer's real claude sessions.
var observedClaudeDirs = claudeConfigDirs

// claudeConfigDirs lists the config dir of every claude account: each extra account's
// CLAUDE_CONFIG_DIR (~/.claude2), then the default account's (syred's own
// CLAUDE_CONFIG_DIR, if set), ~/.claude, where a claude started in a plain terminal
// keeps its sessions, and any other ~/.claude<N> with sessions in it.
func claudeConfigDirs(accounts *chat.AccountRegistry) []observe.ClaudeDir {
	home, _ := os.UserHomeDir()
	var out []observe.ClaudeDir
	seen := map[string]bool{}
	add := func(account, dir string) {
		if dir == "" {
			return
		}
		if strings.HasPrefix(dir, "~/") && home != "" {
			dir = filepath.Join(home, dir[2:])
		}
		dir = filepath.Clean(dir)
		if seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, observe.ClaudeDir{Account: account, Dir: dir})
	}
	if accounts != nil {
		for _, e := range accounts.List() {
			if e.Agent == "claude" && !e.IsDefault {
				add(e.ID, e.Env["CLAUDE_CONFIG_DIR"])
			}
		}
	}
	add("", os.Getenv("CLAUDE_CONFIG_DIR"))
	if home != "" {
		add("", filepath.Join(home, ".claude"))
		// ~/.claude2 and the like: accounts the registry may not know (Windows .cmd
		// wrappers aren't imported, a CLAUDE_CONFIG_DIR set by hand), named like the
		// wrapper ids.
		matches, _ := filepath.Glob(filepath.Join(home, ".claude[0-9]*"))
		for _, m := range matches {
			name := filepath.Base(m)[1:]
			if strings.Trim(name[len("claude"):], "0123456789") == "" && dirExists(filepath.Join(m, "projects")) {
				add(name, m)
			}
		}
	}
	return out
}

// observedCodexDirs is replaced by tests, as observedClaudeDirs.
var observedCodexDirs = codexHomes

// codexHomes lists the home of every codex account with sessions in it: each extra
// account's CODEX_HOME (~/.codex2), syred's own CODEX_HOME, ~/.codex, and any other
// ~/.codex<N>.
func codexHomes(accounts *chat.AccountRegistry) []observe.CodexDir {
	home, _ := os.UserHomeDir()
	var out []observe.CodexDir
	seen := map[string]bool{}
	add := func(account, dir string) {
		if dir == "" {
			return
		}
		if strings.HasPrefix(dir, "~/") && home != "" {
			dir = filepath.Join(home, dir[2:])
		}
		dir = filepath.Clean(dir)
		if seen[dir] || !dirExists(filepath.Join(dir, "sessions")) {
			return
		}
		seen[dir] = true
		out = append(out, observe.CodexDir{Account: account, Dir: dir})
	}
	if accounts != nil {
		for _, e := range accounts.List() {
			if e.Agent == "codex" && !e.IsDefault {
				add(e.ID, e.Env["CODEX_HOME"])
			}
		}
	}
	add("", os.Getenv("CODEX_HOME"))
	if home != "" {
		add("", filepath.Join(home, ".codex"))
		matches, _ := filepath.Glob(filepath.Join(home, ".codex[0-9]*"))
		for _, m := range matches {
			name := filepath.Base(m)[1:]
			if strings.Trim(name[len("codex"):], "0123456789") == "" {
				add(name, m)
			}
		}
	}
	return out
}

// osProcTable is the process table observed sources check PIDs against.
func osProcTable() observe.CodexProcs { return shells.OSSource() }
