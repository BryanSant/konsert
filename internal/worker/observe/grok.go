package observe

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/chat"
)

// GrokDir is one grok account's home (~/.grok, or a GROK_HOME). Account names it; ""
// for the default account.
type GrokDir struct {
	Account string
	Dir     string
}

// GrokSource observes grok sessions (SYR-EE2KX). grok keeps each session in
// <home>/sessions/<url-encoded cwd>/<session id>/:
//
//   - updates.jsonl is the session's ACP update stream, one notification per line
//     ({timestamp, timestamp_us, method, params: {sessionId, update}}): prompts, message
//     and thought chunks, tool calls and their updates, plans (session/update), and
//     grok's own (_x.ai/session/update): turn_completed with its stop reason and usage,
//     hook_execution (session_start, session_end, ...), retries, sub-agents.
//   - summary.json holds the cwd, the generated title and the current model.
//
// grok publishes no live status: status comes from the records, and the grok process
// holding the session's files (or the only interactive grok in its cwd) marks it
// alive, as for codex. A session_end hook record ends it. Permission prompts are not
// recorded, so needs_you is not visible.
type GrokSource struct {
	Dirs      func() []GrokDir
	Procs     CodexProcs
	Cwd       func(pid int) string
	OpenFiles func(pid int) []string
	// ListEvery overrides grokListEvery and ProcTTL procTableTTL (negative: every time).
	ListEvery, ProcTTL time.Duration

	mu      sync.Mutex
	listed  map[string]*grokListing
	alive   map[string]string // sessions alive at the last discovery → updates.jsonl
	meta    map[string]grokMeta
	replays map[string]*grokReplay
	finder  *procFinder
}

type grokListing struct {
	at     time.Time
	recent []string
	groups map[string]time.Time // cwd group dir → its mtime at the listing
}

type grokMeta struct {
	mod               time.Time // summary.json's
	cwd, title, model string
}

// grokReplay carries a session's replay across reads, while they continue each other.
type grokReplay struct {
	next   int64
	r      *chat.ACPReplay
	status string
	title  string
}

const grokListEvery = 15 * time.Second

// Agent implements Source.
func (g *GrokSource) Agent() string { return "grok" }

// Discover implements Source.
func (g *GrokSource) Discover(_ context.Context, since time.Time) ([]Session, error) {
	if g.Dirs == nil {
		return nil, nil
	}
	g.mu.Lock()
	prevAlive := g.alive
	g.mu.Unlock()
	finder := g.procFinder()
	procs := finder.running()
	nowAlive := map[string]string{}
	var out []Session
	seen := map[string]bool{}
	for _, d := range g.Dirs() {
		if d.Dir == "" || seen[d.Dir] {
			continue
		}
		seen[d.Dir] = true
		for _, path := range g.candidates(d, prevAlive, procs) {
			dir := filepath.Dir(path)
			id := filepath.Base(dir)
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			key := "grok/" + d.Account + "/" + id
			m := g.metaOf(dir)
			p := finder.holder(procs, func(f string) bool { return strings.HasPrefix(f, dir+string(filepath.Separator)) }, m.cwd)
			_, wasAlive := prevAlive[key]
			if info.ModTime().Before(since) && p == nil && !wasAlive {
				continue
			}
			s := Session{Agent: "grok", Account: d.Account, ID: id, Path: path, Cwd: m.cwd, Model: m.model,
				Size: info.Size(), ModTime: info.ModTime()}
			switch {
			case p != nil:
				nowAlive[key] = path
				s.Alive, s.PID, s.Ancestors = true, p.pid, p.ancestors
			case wasAlive:
				s.Status = protocol.ChatStatusDone
			}
			out = append(out, s)
		}
	}
	g.mu.Lock()
	g.alive = nowAlive
	g.mu.Unlock()
	return out, nil
}

// candidates lists the sessions of d worth a stat on this poll: all at each full
// listing; between them those written recently, those in a cwd group whose dir changed
// (a new session), those alive at the last poll and those a running grok holds open.
func (g *GrokSource) candidates(d GrokDir, prevAlive map[string]string, procs []agentProc) []string {
	now := time.Now()
	sessions := filepath.Join(d.Dir, "sessions")
	g.mu.Lock()
	ls := g.listed[d.Dir]
	g.mu.Unlock()
	every := g.ListEvery
	if every == 0 {
		every = grokListEvery
	}
	if ls == nil || now.Sub(ls.at) >= every {
		files, _ := filepath.Glob(filepath.Join(sessions, "*", "*", "updates.jsonl"))
		ls = &grokListing{at: now, groups: map[string]time.Time{}}
		for _, f := range files {
			if info, err := os.Stat(f); err == nil && now.Sub(info.ModTime()) < claudeRecentKeep {
				ls.recent = append(ls.recent, f)
			}
		}
		groups, _ := filepath.Glob(filepath.Join(sessions, "*"))
		for _, gd := range groups {
			if info, err := os.Stat(gd); err == nil && info.IsDir() {
				ls.groups[gd] = info.ModTime()
			}
		}
		g.mu.Lock()
		if g.listed == nil {
			g.listed = map[string]*grokListing{}
		}
		g.listed[d.Dir] = ls
		g.mu.Unlock()
		return files
	}
	out := append([]string(nil), ls.recent...)
	has := map[string]bool{}
	for _, f := range out {
		has[f] = true
	}
	addPath := func(f string) {
		if !has[f] {
			has[f] = true
			out = append(out, f)
		}
	}
	groups, _ := filepath.Glob(filepath.Join(sessions, "*"))
	for _, gd := range groups {
		info, err := os.Stat(gd)
		if err != nil || !info.IsDir() || info.ModTime().Equal(ls.groups[gd]) {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(gd, "*", "updates.jsonl"))
		for _, f := range files {
			addPath(f)
		}
	}
	prefix := "grok/" + d.Account + "/"
	for key, path := range prevAlive {
		if strings.HasPrefix(key, prefix) {
			addPath(path)
		}
	}
	for _, p := range procs {
		for _, f := range p.open {
			rel, err := filepath.Rel(sessions, f)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			if parts := strings.Split(filepath.ToSlash(rel), "/"); len(parts) >= 3 {
				addPath(filepath.Join(sessions, parts[0], parts[1], "updates.jsonl"))
			}
		}
	}
	return out
}

func (g *GrokSource) procFinder() *procFinder {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.finder == nil {
		g.finder = &procFinder{procs: g.Procs, cwd: g.Cwd, openFiles: g.OpenFiles, ttl: g.ProcTTL, isAgent: isGrokName,
			isServer: func(argv []string) bool {
				// Syre's grok chats: grok agent [flags] stdio.
				for i, a := range argv {
					if a == "agent" && i+1 < len(argv) {
						return true
					}
				}
				return false
			}}
	}
	return g.finder
}

// isGrokName reports a grok executable.
func isGrokName(name string) bool {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(name)), ".exe") == "grok"
}

// metaOf reads a session's summary.json (again when it changed): cwd, title, model.
// Without one, the cwd comes from the group dir's name (or its .cwd file).
func (g *GrokSource) metaOf(dir string) grokMeta {
	sum := filepath.Join(dir, "summary.json")
	info, err := os.Stat(sum)
	g.mu.Lock()
	m, ok := g.meta[dir]
	g.mu.Unlock()
	if ok && (err != nil || info.ModTime().Equal(m.mod)) {
		return m
	}
	m = grokMeta{}
	if err == nil {
		m.mod = info.ModTime()
		if raw, err := os.ReadFile(sum); err == nil {
			var s struct {
				Info struct {
					Cwd string `json:"cwd"`
				} `json:"info"`
				Title string `json:"session_summary"`
				Model string `json:"current_model_id"`
			}
			if json.Unmarshal(raw, &s) == nil {
				m.cwd, m.title, m.model = s.Info.Cwd, s.Title, s.Model
			}
		}
	}
	if m.cwd == "" {
		group := filepath.Dir(dir)
		if raw, err := os.ReadFile(filepath.Join(group, ".cwd")); err == nil {
			m.cwd = strings.TrimSpace(string(raw))
		} else if dec, err := url.PathUnescape(filepath.Base(group)); err == nil && filepath.IsAbs(dec) {
			m.cwd = dec
		}
	}
	g.mu.Lock()
	if g.meta == nil || len(g.meta) > 4096 {
		g.meta = map[string]grokMeta{}
	}
	g.meta[dir] = m
	g.mu.Unlock()
	return m
}

// Read implements Source.
func (g *GrokSource) Read(_ context.Context, s Session, offset int64) ([]protocol.AgentChatEvent, int64, error) {
	chunk, next, err := readRecords(s.Path, offset, claudeReadMax)
	if err != nil {
		return nil, next, err
	}
	g.mu.Lock()
	st := g.replays[s.Path]
	if st == nil || st.next != offset {
		// A read that doesn't continue the last (a transcript fetch from the start, or a
		// restart) gets its own replay; one from the start becomes the session's.
		st = &grokReplay{next: offset, r: chat.NewACPReplay(s.ID)}
		if offset == 0 || g.replays[s.Path] == nil {
			if g.replays == nil || len(g.replays) > 1024 {
				g.replays = map[string]*grokReplay{}
			}
			g.replays[s.Path] = st
		}
	}
	g.mu.Unlock()
	var evs []protocol.AgentChatEvent
	if m := g.metaOf(filepath.Dir(s.Path)); m.title != "" && m.title != st.title {
		st.title = m.title
		evs = append(evs, protocol.AgentChatEvent{Kind: protocol.EventTitle, Text: m.title, At: s.ModTime, AgentSessionID: s.ID})
	}
	evs = append(evs, grokEvents(s.ID, chunk, st)...)
	st.next = next
	return evs, next, nil
}

// grokRecord is one updates.jsonl line.
type grokRecord struct {
	Timestamp   int64  `json:"timestamp"`
	TimestampUS int64  `json:"timestamp_us"`
	Method      string `json:"method"`
	Params      struct {
		Update jsontext.Value `json:"update"`
	} `json:"params"`
}

type grokUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`
	EventName     string `json:"event_name"`
	PromptID      string `json:"prompt_id"`
	StopReason    string `json:"stop_reason"`
	Usage         *struct {
		InputTokens      int64 `json:"inputTokens"`
		OutputTokens     int64 `json:"outputTokens"`
		CachedReadTokens int64 `json:"cachedReadTokens"`
		CacheCreation    int64 `json:"cacheCreationTokens"`
	} `json:"usage"`
}

// GrokEvents turns updates.jsonl lines into normalized events (a fresh replay; see
// grokEvents).
func GrokEvents(sessionID string, lines []byte) []protocol.AgentChatEvent {
	return grokEvents(sessionID, lines, &grokReplay{r: chat.NewACPReplay(sessionID)})
}

// grokEvents turns updates.jsonl lines into normalized events:
//
//   - a prompt, the agent's messages, tool calls and plans (ACP updates, through the
//     replay), with status working while they come;
//   - turn_completed: what is pending, turn_end with its stop reason, usage, status idle;
//   - a session_end hook: status done.
//
// Thought chunks, hooks, retries and memory records produce nothing else.
func grokEvents(sessionID string, lines []byte, st *grokReplay) []protocol.AgentChatEvent {
	var out []protocol.AgentChatEvent
	add := func(at time.Time, evs ...protocol.AgentChatEvent) {
		for _, ev := range evs {
			ev.At, ev.AgentSessionID = at, sessionID
			switch ev.Kind {
			case protocol.EventStatus:
				if ev.Status == st.status {
					continue
				}
				st.status = ev.Status
			case protocol.EventUserMessage:
				ev.Text = redactCut(ev.Text, claudePromptMax)
			case protocol.EventMessage:
				ev.Text = redactCut(ev.Text, claudeMessageMax)
			}
			out = append(out, ev)
		}
	}
	status := func(s string) protocol.AgentChatEvent {
		return protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: s}
	}
	for line := range bytes.Lines(lines) {
		var r grokRecord
		if json.Unmarshal(line, &r) != nil || len(r.Params.Update) == 0 {
			continue
		}
		at := time.Unix(r.Timestamp, 0)
		if r.TimestampUS > 0 {
			at = time.UnixMicro(r.TimestampUS)
		}
		evs, kind := st.r.Update(r.Params.Update)
		switch kind {
		case "user_message_chunk", "agent_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update", "plan":
			add(at, status(protocol.ChatStatusWorking))
			add(at, evs...)
			continue
		}
		var u grokUpdate
		if json.Unmarshal(r.Params.Update, &u) != nil {
			continue
		}
		switch {
		case kind == "turn_completed":
			reason := cmpOr(u.StopReason, "end_turn")
			add(at, st.r.Flush(reason)...)
			if st.status != protocol.ChatStatusIdle {
				add(at, protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, StopReason: reason})
			}
			if us := u.Usage; us != nil && u.PromptID != "" {
				add(at, protocol.AgentChatEvent{Kind: protocol.EventUsage, MessageID: u.PromptID,
					Usage: &protocol.AgentTokenUsage{InputTokens: max(0, us.InputTokens-us.CachedReadTokens),
						OutputTokens: us.OutputTokens, CacheReadTokens: us.CachedReadTokens, CacheCreateTokens: us.CacheCreation}})
			}
			add(at, status(protocol.ChatStatusIdle))
		case kind == "hook_execution" && u.EventName == "session_end":
			add(at, st.r.Flush("cancelled")...)
			add(at, status(protocol.ChatStatusDone))
		}
	}
	return out
}
