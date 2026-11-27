package observe

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/chat"
	"github.com/BryanSant/syre/internal/worker/shells"
)

// ClaudeDir is one claude account's config dir (~/.claude, or a CLAUDE_CONFIG_DIR such
// as ~/.claude2). Account names it ("claude2"); "" for the default account.
type ClaudeDir struct {
	Account string
	Dir     string
}

// ProcTable reads the process table (shells.OSSource() satisfies it).
type ProcTable interface {
	Snapshot() ([]shells.Proc, error)
	Details(p *shells.Proc)
}

// ClaudeSource observes claude sessions from every account's config dir:
//
//   - projects/<cwd-slug>/<session-id>.jsonl is the transcript: one JSON record per line
//     (user prompts and tool results, assistant content blocks, system markers).
//   - sessions/<pid>.json is written by each running interactive claude: its session id,
//     cwd and live status (busy, idle, or waiting with what it waits for). It gives the
//     status the transcript can't (a permission prompt writes no record) and the PID.
//
// Transcripts of sub-agents (projects/<slug>/<id>/subagents/...) are not sessions of
// their own and are not listed.
type ClaudeSource struct {
	// Dirs lists the config dirs to watch; it is called on every discovery, so accounts
	// added later are picked up.
	Dirs func() []ClaudeDir
	// Procs checks that a sessions/<pid>.json still belongs to a running process and
	// gives its ancestry; nil ignores those files (status then comes from the records).
	Procs ProcTable
	// ProcTTL overrides procTableTTL and ListEvery claudeListEvery (negative: snapshot
	// and list on every discovery).
	ProcTTL, ListEvery time.Duration

	mu   sync.Mutex
	cwds map[string]string // transcript path → cwd from its records
	live map[string]string // sessions a live pid file reported at the last discovery → transcript

	listed map[string]*claudeListing // by config dir

	table   map[int]shells.Proc // the last process snapshot
	tableAt time.Time
}

// claudeListing is the last full listing of a config dir's transcripts.
type claudeListing struct {
	at     time.Time
	recent []string // written within claudeRecentKeep then
}

// procTableTTL is how long a process snapshot is reused. A PID not in it forces a new
// one, so a new session's process is seen at once; an exit (when claude doesn't remove
// its file) is seen within the TTL.
const procTableTTL = 10 * time.Second

// Agent implements Source.
func (c *ClaudeSource) Agent() string { return "claude" }

// claudePidFile is claude's sessions/<pid>.json (the fields used here).
type claudePidFile struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	Cwd        string `json:"cwd"`
	StartedAt  int64  `json:"startedAt"` // unix ms
	Status     string `json:"status"`    // busy, shell, idle, waiting
	WaitingFor string `json:"waitingFor"`

	ancestors []int
}

// pidReuseSlack is how much later than the session's start its process may have
// started; a process started later is a reused PID, not the session's.
const pidReuseSlack = 30 * time.Second

// Discover implements Source.
func (c *ClaudeSource) Discover(_ context.Context, since time.Time) ([]Session, error) {
	if c.Dirs == nil {
		return nil, nil
	}
	c.mu.Lock()
	prevLive := c.live
	c.mu.Unlock()
	nowLive := map[string]string{}
	var out []Session
	seen := map[string]bool{}
	for _, d := range c.Dirs() {
		if d.Dir == "" || seen[d.Dir] {
			continue
		}
		seen[d.Dir] = true
		live := c.livePidFiles(d.Dir)
		// A claude that publishes live status writes sessions/ at start: there, a session
		// no running process publishes is over, whatever its records say.
		publishes := c.Procs != nil && isDir(filepath.Join(d.Dir, "sessions"))
		for _, path := range c.candidates(d, live, prevLive) {
			id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
			key := "claude/" + d.Account + "/" + id
			pf, isLive := live[id]
			_, wasLive := prevLive[key]
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if info.ModTime().Before(since) && !isLive && !wasLive {
				continue
			}
			s := Session{Agent: "claude", Account: d.Account, ID: id, Path: path,
				Size: info.Size(), ModTime: info.ModTime()}
			switch {
			case isLive:
				nowLive[key] = path
				s.PID, s.Cwd = pf.PID, pf.Cwd
				s.Status, s.Waiting = claudeLiveStatus(pf.Status), pf.WaitingFor
				s.Ancestors = pf.ancestors
			case wasLive, publishes:
				s.Status = protocol.ChatStatusDone // its process exited
			}
			if s.Cwd == "" {
				s.Cwd = c.cwdOf(path)
			}
			out = append(out, s)
		}
	}
	c.mu.Lock()
	c.live = nowLive
	c.mu.Unlock()
	return out, nil
}

// Listing every project dir is the costly part of discovery, so it runs every
// claudeListEvery; the polls between stat only the transcripts written within
// claudeRecentKeep of the last listing and those live pid files point at. A session
// with a pid file is seen at once; one without (an older claude) within claudeListEvery.
const (
	claudeListEvery  = 10 * time.Second
	claudeRecentKeep = 30 * time.Minute
)

// candidates lists the transcripts of d worth a stat on this poll.
func (c *ClaudeSource) candidates(d ClaudeDir, live map[string]claudePidFile, prevLive map[string]string) []string {
	now := time.Now()
	c.mu.Lock()
	ls := c.listed[d.Dir]
	c.mu.Unlock()
	every := c.ListEvery
	if every == 0 {
		every = claudeListEvery
	}
	if ls == nil || now.Sub(ls.at) >= every {
		files, _ := filepath.Glob(filepath.Join(d.Dir, "projects", "*", "*.jsonl"))
		ls = &claudeListing{at: now}
		for _, f := range files {
			if info, err := os.Stat(f); err == nil && now.Sub(info.ModTime()) < claudeRecentKeep {
				ls.recent = append(ls.recent, f)
			}
		}
		c.mu.Lock()
		if c.listed == nil {
			c.listed = map[string]*claudeListing{}
		}
		c.listed[d.Dir] = ls
		c.mu.Unlock()
		return files
	}
	out := append([]string(nil), ls.recent...)
	has := map[string]bool{}
	for _, f := range out {
		has[f] = true
	}
	addPath := func(f string) {
		if f != "" && !has[f] {
			has[f] = true
			out = append(out, f)
		}
	}
	for _, pf := range live {
		addPath(c.transcriptOf(d.Dir, pf))
	}
	prefix := "claude/" + d.Account + "/"
	for key, path := range prevLive {
		if strings.HasPrefix(key, prefix) {
			addPath(path)
		}
	}
	return out
}

// transcriptOf finds the transcript a live pid file's session writes: under its cwd's
// project dir, else under any (a session resumed from another directory).
func (c *ClaudeSource) transcriptOf(dir string, pf claudePidFile) string {
	if pf.Cwd != "" {
		p := filepath.Join(dir, "projects", claudeProjectDir(pf.Cwd), pf.SessionID+".jsonl")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	m, _ := filepath.Glob(filepath.Join(dir, "projects", "*", pf.SessionID+".jsonl"))
	if len(m) > 0 {
		return m[0]
	}
	return ""
}

// claudeProjectDir is the project dir name claude derives from a cwd: every character
// but ASCII letters and digits becomes '-'.
func claudeProjectDir(cwd string) string {
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r
	}
	var b strings.Builder
	for _, r := range cwd {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// claudeLiveStatus maps a sessions/<pid>.json status to a chat status.
func claudeLiveStatus(s string) string {
	switch s {
	case "busy", "shell": // shell: a shell command runs in the session
		return protocol.ChatStatusWorking
	case "waiting":
		return protocol.ChatStatusNeedsYou
	default:
		return protocol.ChatStatusIdle
	}
}

// procTable returns the process table by PID: the cached snapshot while it is fresh,
// else a new one (nil without Procs or on failure).
func (c *ClaudeSource) procTable(fresh bool) map[int]shells.Proc {
	if c.Procs == nil {
		return nil
	}
	c.mu.Lock()
	table, at := c.table, c.tableAt
	c.mu.Unlock()
	ttl := c.ProcTTL
	if ttl == 0 {
		ttl = procTableTTL
	}
	if !fresh && table != nil && time.Since(at) < ttl {
		return table
	}
	list, err := c.Procs.Snapshot()
	if err != nil {
		return nil
	}
	m := make(map[int]shells.Proc, len(list))
	for _, p := range list {
		m[p.PID] = p
	}
	c.mu.Lock()
	c.table, c.tableAt = m, time.Now()
	c.mu.Unlock()
	return m
}

// livePidFiles reads dir/sessions/*.json and keeps those whose process still runs, by
// session id.
func (c *ClaudeSource) livePidFiles(dir string) map[string]claudePidFile {
	if c.Procs == nil {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(dir, "sessions", "*.json"))
	if len(files) == 0 {
		return nil
	}
	var pfs []claudePidFile
	procs := c.procTable(false)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var pf claudePidFile
		if json.Unmarshal(raw, &pf) != nil || pf.PID <= 0 || pf.SessionID == "" {
			continue
		}
		if _, known := procs[pf.PID]; !known && procs != nil {
			procs = c.procTable(true) // a process newer than the snapshot
		}
		pfs = append(pfs, pf)
	}
	if procs == nil {
		return nil
	}
	out := map[string]claudePidFile{}
	for _, pf := range pfs {
		p, ok := procs[pf.PID]
		if !ok || p.Zombie {
			continue // a file left by a process that died without cleaning up
		}
		if p.Start.IsZero() {
			c.Procs.Details(&p)
		}
		if pf.StartedAt > 0 && !p.Start.IsZero() && p.Start.After(time.UnixMilli(pf.StartedAt).Add(pidReuseSlack)) {
			continue // the PID now belongs to a newer process
		}
		if prev, dup := out[pf.SessionID]; dup && prev.StartedAt > pf.StartedAt {
			continue
		}
		pf.ancestors = ancestors(procs, pf.PID)
		out[pf.SessionID] = pf
	}
	return out
}

// ancestors lists pid's parent chain, nearest first.
func ancestors(procs map[int]shells.Proc, pid int) []int {
	var out []int
	for range 64 {
		p, ok := procs[pid]
		if !ok || p.PPID <= 0 || p.PPID == pid {
			break
		}
		out = append(out, p.PPID)
		pid = p.PPID
	}
	return out
}

// cwdHeadMax bounds how much of a transcript's head is scanned for its cwd.
const cwdHeadMax = 256 << 10

// cwdOf returns the cwd the transcript's records carry (cached per path; "" if none
// yet: the next discovery looks again).
func (c *ClaudeSource) cwdOf(path string) string {
	c.mu.Lock()
	cwd, ok := c.cwds[path]
	c.mu.Unlock()
	if ok {
		return cwd
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head, _ := io.ReadAll(io.LimitReader(f, cwdHeadMax))
	for line := range bytes.Lines(head) {
		if !bytes.Contains(line, []byte(`"cwd"`)) {
			continue
		}
		var r struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(line, &r) == nil && r.Cwd != "" {
			cwd = r.Cwd
			break
		}
	}
	if cwd == "" {
		return ""
	}
	c.mu.Lock()
	if c.cwds == nil || len(c.cwds) > 4096 {
		c.cwds = map[string]string{}
	}
	c.cwds[path] = cwd
	c.mu.Unlock()
	return cwd
}

// claudeReadMax bounds one Read; a longer backlog is read over several polls.
const claudeReadMax = 4 << 20

// Read implements Source.
func (c *ClaudeSource) Read(_ context.Context, s Session, offset int64) ([]protocol.AgentChatEvent, int64, error) {
	chunk, next, err := readRecords(s.Path, offset, claudeReadMax)
	if err != nil || len(chunk) == 0 {
		return nil, next, err
	}
	return ClaudeEvents(s.ID, chunk), next, nil
}

// readRecords reads complete lines of path from offset, at most max bytes, and returns
// them with the offset after the last one. A line still being written is left for the
// next call; a single line longer than max is skipped.
func readRecords(path string, offset int64, max int) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	unread := info.Size() - offset
	if unread <= 0 {
		return nil, offset, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	buf := make([]byte, min(int64(max), unread))
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, offset, err
	}
	buf = buf[:n]
	if end := bytes.LastIndexByte(buf, '\n'); end >= 0 {
		return buf[:end+1], offset + int64(end+1), nil
	}
	if n < max {
		return nil, offset, nil // a partial record: wait for its newline
	}
	// One record longer than max: skip to the end of it.
	pos := offset + int64(n)
	buf = make([]byte, 64<<10)
	for {
		m, err := f.Read(buf)
		if i := bytes.IndexByte(buf[:m], '\n'); i >= 0 {
			return nil, pos + int64(i+1), nil
		}
		pos += int64(m)
		if err != nil {
			return nil, offset, nil // not finished yet; try again later
		}
	}
}

// claudeRecord is one transcript line (the fields used here).
type claudeRecord struct {
	Type             string         `json:"type"`
	Subtype          string         `json:"subtype"`
	UUID             string         `json:"uuid"`
	Timestamp        time.Time      `json:"timestamp"`
	IsMeta           bool           `json:"isMeta"`
	IsSidechain      bool           `json:"isSidechain"`
	IsCompactSummary bool           `json:"isCompactSummary"`
	IsAPIError       bool           `json:"isApiErrorMessage"`
	Message          jsontext.Value `json:"message"`
	AITitle          string         `json:"aiTitle"`
}

type claudeRecMessage struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Content    jsontext.Value `json:"content"`
	Usage      *struct {
		InputTokens       int64 `json:"input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
		CacheReadTokens   int64 `json:"cache_read_input_tokens"`
		CacheCreateTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

type claudeRecBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Input     jsontext.Value `json:"input"`
	ToolUseID string         `json:"tool_use_id"`
	Content   jsontext.Value `json:"content"`
	IsError   bool           `json:"is_error"`
}

func (m claudeRecMessage) blocks() []claudeRecBlock {
	var bs []claudeRecBlock
	if json.Unmarshal(m.Content, &bs) == nil {
		return bs
	}
	var text string
	if json.Unmarshal(m.Content, &text) == nil && text != "" {
		return []claudeRecBlock{{Type: "text", Text: text}}
	}
	return nil
}

// claudePromptMax and claudeMessageMax bound a prompt's and a reply's text in an event.
const (
	claudePromptMax  = 4000
	claudeMessageMax = 64 << 10
)

// redactCut truncates s to max and removes secrets from what is kept. It cuts a little
// past max first, so a secret straddling the cut is still recognized.
func redactCut(s string, max int) string {
	return chat.Truncate(chat.Redact(chat.Truncate(s, max+256)), max)
}

// claudeInterrupted starts the user record claude writes when a turn is interrupted.
const claudeInterrupted = "[Request interrupted by user"

// ClaudeEvents turns transcript lines into normalized events:
//
//   - a typed prompt: user_message, then status working;
//   - assistant content: message (text), tool_call running (tool_use), plan (TodoWrite),
//     usage per message id (repeated for each content block of a message; the latest
//     per MessageID counts);
//   - tool results: tool_call completed or failed;
//   - the end of a turn (a final assistant message, claude's turn markers, an
//     interruption): turn_end and status idle;
//   - aiTitle records: title.
//
// Meta records, slash commands, compaction summaries and side chains produce nothing.
// Events carry the record's time and the session id; ChatID and Seq are left to the
// consumer.
func ClaudeEvents(sessionID string, lines []byte) []protocol.AgentChatEvent {
	var out []protocol.AgentChatEvent
	tools := map[string]*protocol.AgentToolCall{}
	status := ""
	var lastAt time.Time
	add := func(ev protocol.AgentChatEvent, at time.Time) {
		if at.IsZero() {
			at = lastAt // records such as ai-title carry no time of their own
		}
		ev.At, ev.AgentSessionID = at, sessionID
		if ev.Kind == protocol.EventStatus {
			if ev.Status == status {
				return
			}
			status = ev.Status
		}
		out = append(out, ev)
	}
	turnEnd := func(reason string, at time.Time) {
		if status == protocol.ChatStatusIdle {
			return
		}
		add(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, StopReason: reason}, at)
		add(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusIdle}, at)
	}
	for line := range bytes.Lines(lines) {
		var r claudeRecord
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		if !r.Timestamp.IsZero() {
			lastAt = r.Timestamp
		}
		switch r.Type {
		case "ai-title":
			if r.AITitle != "" {
				add(protocol.AgentChatEvent{Kind: protocol.EventTitle, Text: r.AITitle}, r.Timestamp)
			}
		case "system":
			if r.Subtype == "turn_duration" || r.Subtype == "stop_hook_summary" {
				turnEnd("end_turn", r.Timestamp)
			}
		case "user":
			if r.IsMeta || r.IsSidechain || r.IsCompactSummary {
				continue
			}
			var m claudeRecMessage
			if json.Unmarshal(r.Message, &m) != nil {
				continue
			}
			var prompt []string
			for _, b := range m.blocks() {
				switch b.Type {
				case "text":
					prompt = append(prompt, b.Text)
				case "tool_result":
					tc := tools[b.ToolUseID]
					if tc == nil {
						tc = &protocol.AgentToolCall{ID: b.ToolUseID, Name: "tool"}
					}
					delete(tools, b.ToolUseID)
					tc.Status = "completed"
					if b.IsError {
						tc.Status = "failed"
					}
					tc.Output = redactCut(chat.ClaudeToolResultText(b.Content), chat.ClaudeOutputMax)
					add(protocol.AgentChatEvent{Kind: protocol.EventToolCall, ToolCall: tc}, r.Timestamp)
				}
			}
			text := strings.TrimSpace(strings.Join(prompt, "\n"))
			switch {
			case text == "":
			case strings.HasPrefix(text, claudeInterrupted):
				turnEnd("cancelled", r.Timestamp)
			case strings.HasPrefix(text, "<command-name>"), strings.HasPrefix(text, "<local-command"):
				// A slash command handled by the CLI itself, not a prompt.
			default:
				add(protocol.AgentChatEvent{Kind: protocol.EventUserMessage, MessageID: r.UUID,
					Text: redactCut(text, claudePromptMax)}, r.Timestamp)
				add(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusWorking}, r.Timestamp)
			}
		case "assistant":
			if r.IsSidechain {
				continue
			}
			var m claudeRecMessage
			if json.Unmarshal(r.Message, &m) != nil {
				continue
			}
			if r.IsAPIError {
				turnEnd("error", r.Timestamp)
				continue
			}
			usedTool, hadText := false, false
			for _, b := range m.blocks() {
				switch b.Type {
				case "text":
					if strings.TrimSpace(b.Text) == "" {
						continue
					}
					hadText = true
					add(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusWorking}, r.Timestamp)
					add(protocol.AgentChatEvent{Kind: protocol.EventMessage, MessageID: r.UUID, Model: m.Model,
						Text: redactCut(b.Text, claudeMessageMax)}, r.Timestamp)
				case "tool_use":
					usedTool = true
					add(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusWorking}, r.Timestamp)
					if b.Name == "TodoWrite" {
						if plan := chat.ClaudePlan(b.Input); plan != nil {
							add(protocol.AgentChatEvent{Kind: protocol.EventPlan, Plan: plan}, r.Timestamp)
						}
					}
					tc := chat.ClaudeToolCall(b.ID, b.Name, b.Input)
					tc.Input = chat.Redact(tc.Input)
					tc.Status = "running"
					tools[b.ID] = tc
					cp := *tc
					add(protocol.AgentChatEvent{Kind: protocol.EventToolCall, Model: m.Model, ToolCall: &cp}, r.Timestamp)
				}
			}
			if u := m.Usage; u != nil && m.ID != "" {
				add(protocol.AgentChatEvent{Kind: protocol.EventUsage, MessageID: m.ID, Model: m.Model,
					Usage: &protocol.AgentTokenUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
						CacheReadTokens: u.CacheReadTokens, CacheCreateTokens: u.CacheCreateTokens}}, r.Timestamp)
			}
			// A message is written one content block per record, each with the message's
			// stop reason: the turn ends with its text, not with the thinking before it.
			if !usedTool && hadText {
				switch m.StopReason {
				case "end_turn", "max_tokens", "stop_sequence", "refusal":
					turnEnd(m.StopReason, r.Timestamp)
				}
			}
		}
	}
	return out
}
