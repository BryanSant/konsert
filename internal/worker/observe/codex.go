package observe

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/chat"
)

// CodexDir is one codex account's home (~/.codex, or a CODEX_HOME such as ~/.codex2).
// Account names it ("codex2"); "" for the default account.
type CodexDir struct {
	Account string
	Dir     string
}

// CodexProcs reads the process table with command lines (shells.OSSource() satisfies it).
type CodexProcs interface {
	ProcTable
	Argv(pid int) []string
}

// CodexSource observes codex sessions (SYR-E3CTR). Each session's rollout is
// <home>/sessions/YYYY/MM/DD/rollout-<time>-<session id>.jsonl, one JSON record per
// line: session_meta (id, cwd), turn_context (model), event_msg (a turn started,
// completed or aborted; finished items such as the user's and the agent's messages),
// response_item (the model's tool calls and their outputs) and token_usage_record.
//
// codex publishes no live status, so status comes from the records. A running codex
// keeps its rollout open: the process holding it (found through its open files where
// the OS lists them cheaply, Linux; else as the only codex running in the session's
// cwd) marks the session alive, gives its PID and ancestry (a codex Syre runs is
// managed), and its exit ends the session. A permission prompt writes no record, so
// codex's needs_you is not visible here.
type CodexSource struct {
	// Dirs lists the codex homes to watch; called on every discovery.
	Dirs func() []CodexDir
	// Procs finds the codex processes; nil: no process matching (status and liveness
	// from the records alone).
	Procs CodexProcs
	// Cwd is a process's working directory ("" unknown).
	Cwd func(pid int) string
	// OpenFiles lists the files a process holds open; nil where that is costly (the
	// session is then matched by cwd).
	OpenFiles func(pid int) []string
	// ListEvery overrides codexListEvery and ProcTTL procTableTTL (negative: list and
	// snapshot on every discovery).
	ListEvery, ProcTTL time.Duration

	mu     sync.Mutex
	meta   map[string]codexMeta // rollout path → its session's cwd and model
	listed map[string]*claudeListing
	alive  map[string]string // sessions alive at the last discovery → rollout path
	finder *procFinder
}

type codexMeta struct {
	cwd, model string
}

// Agent implements Source.
func (c *CodexSource) Agent() string { return "codex" }

// Listing every day dir is the costly part of discovery: it runs every codexListEvery,
// and the polls between stat only rollouts written within claudeRecentKeep of it and
// those of live sessions.
const codexListEvery = 15 * time.Second

// codexSessionID is the session id in a rollout file name, or "".
func codexSessionID(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if !strings.HasPrefix(name, "rollout-") || len(name) < 36 {
		return ""
	}
	id := name[len(name)-36:]
	if strings.Count(id, "-") != 4 {
		return ""
	}
	return id
}

// Discover implements Source.
func (c *CodexSource) Discover(_ context.Context, since time.Time) ([]Session, error) {
	if c.Dirs == nil {
		return nil, nil
	}
	c.mu.Lock()
	prevAlive := c.alive
	c.mu.Unlock()
	finder := c.procFinder()
	procs := finder.running()
	nowAlive := map[string]string{}
	var out []Session
	seen := map[string]bool{}
	for _, d := range c.Dirs() {
		if d.Dir == "" || seen[d.Dir] {
			continue
		}
		seen[d.Dir] = true
		for _, path := range c.candidates(d, prevAlive, procs) {
			id := codexSessionID(path)
			if id == "" {
				continue
			}
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			key := "codex/" + d.Account + "/" + id
			m := c.metaOf(path)
			p := finder.holder(procs, func(f string) bool { return f == path }, m.cwd)
			_, wasAlive := prevAlive[key]
			if info.ModTime().Before(since) && p == nil && !wasAlive {
				continue
			}
			s := Session{Agent: "codex", Account: d.Account, ID: id, Path: path, Cwd: m.cwd, Model: m.model,
				Size: info.Size(), ModTime: info.ModTime()}
			switch {
			case p != nil:
				nowAlive[key] = path
				s.Alive, s.PID, s.Ancestors = true, p.pid, p.ancestors
			case wasAlive:
				s.Status = protocol.ChatStatusDone // its codex exited
			}
			out = append(out, s)
		}
	}
	c.mu.Lock()
	c.alive = nowAlive
	c.mu.Unlock()
	return out, nil
}

// candidates lists the rollouts of d worth a stat on this poll: every rollout at each
// full listing; between them those written recently, today's and yesterday's (a
// listing of one or two day dirs, so a new session is seen within a poll), those of
// sessions alive at the last poll and those a running codex holds open.
func (c *CodexSource) candidates(d CodexDir, prevAlive map[string]string, procs []agentProc) []string {
	now := time.Now()
	c.mu.Lock()
	ls := c.listed[d.Dir]
	c.mu.Unlock()
	every := c.ListEvery
	if every == 0 {
		every = codexListEvery
	}
	if ls == nil || now.Sub(ls.at) >= every {
		files, _ := filepath.Glob(filepath.Join(d.Dir, "sessions", "*", "*", "*", "rollout-*.jsonl"))
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
		if !has[f] {
			has[f] = true
			out = append(out, f)
		}
	}
	// codex names day dirs by local date; a session started before midnight is in
	// yesterday's.
	for _, day := range []time.Time{now, now.AddDate(0, 0, -1)} {
		files, _ := filepath.Glob(filepath.Join(d.Dir, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"), "rollout-*.jsonl"))
		for _, f := range files {
			addPath(f)
		}
	}
	prefix := "codex/" + d.Account + "/"
	for key, path := range prevAlive {
		if strings.HasPrefix(key, prefix) {
			addPath(path)
		}
	}
	sessions := filepath.Join(d.Dir, "sessions") + string(filepath.Separator)
	for _, p := range procs {
		for _, f := range p.open {
			if strings.HasPrefix(f, sessions) && codexSessionID(f) != "" {
				addPath(f)
			}
		}
	}
	return out
}

// procFinder finds the running codex processes (built once from the fields).
func (c *CodexSource) procFinder() *procFinder {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finder == nil {
		c.finder = &procFinder{procs: c.Procs, cwd: c.Cwd, openFiles: c.OpenFiles, ttl: c.ProcTTL, isAgent: isCodexName,
			isServer: func(argv []string) bool {
				for _, a := range argv {
					if a == "app-server" || a == "mcp-server" {
						return true
					}
				}
				return false
			}}
	}
	return c.finder
}

// isCodexName reports a codex executable: codex, codex.exe, or a platform build such as
// codex-x86_64-unknown-linux-musl (not its sandbox helpers).
func isCodexName(name string) bool {
	n := strings.TrimSuffix(strings.ToLower(filepath.Base(name)), ".exe")
	if n == "codex" {
		return true
	}
	return strings.HasPrefix(n, "codex-") && !strings.Contains(n, "sandbox") &&
		(strings.Contains(n, "linux") || strings.Contains(n, "apple") || strings.Contains(n, "windows") || strings.Contains(n, "darwin"))
}

// metaOf reads a rollout's head for its cwd and model (cached once the cwd is known).
func (c *CodexSource) metaOf(path string) codexMeta {
	c.mu.Lock()
	m, ok := c.meta[path]
	c.mu.Unlock()
	if ok {
		return m
	}
	f, err := os.Open(path)
	if err != nil {
		return codexMeta{}
	}
	defer f.Close()
	head, _ := io.ReadAll(io.LimitReader(f, cwdHeadMax))
	for line := range bytes.Lines(head) {
		var r codexRecord
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		var p struct {
			Cwd   string `json:"cwd"`
			Model string `json:"model"`
		}
		switch r.Type {
		case "session_meta", "turn_context":
			if json.Unmarshal(r.Payload, &p) == nil {
				m.cwd = cmpOr(m.cwd, p.Cwd)
				m.model = cmpOr(m.model, p.Model)
			}
		}
		if m.cwd != "" && m.model != "" {
			break
		}
	}
	if m.cwd == "" {
		return m
	}
	c.mu.Lock()
	if c.meta == nil || len(c.meta) > 4096 {
		c.meta = map[string]codexMeta{}
	}
	c.meta[path] = m
	c.mu.Unlock()
	return m
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Read implements Source.
func (c *CodexSource) Read(_ context.Context, s Session, offset int64) ([]protocol.AgentChatEvent, int64, error) {
	chunk, next, err := readRecords(s.Path, offset, claudeReadMax)
	if err != nil || len(chunk) == 0 {
		return nil, next, err
	}
	return CodexEvents(s.ID, chunk), next, nil
}

// codexRecord is one rollout line.
type codexRecord struct {
	Timestamp time.Time      `json:"timestamp"`
	Type      string         `json:"type"`
	Payload   jsontext.Value `json:"payload"`
}

type codexPayload struct {
	Type   string         `json:"type"`
	Model  string         `json:"model"`
	Item   jsontext.Value `json:"item"`
	Name   string         `json:"name"`
	CallID string         `json:"call_id"`
	// custom_tool_call carries input, function_call arguments.
	Input      string         `json:"input"`
	Arguments  string         `json:"arguments"`
	Output     jsontext.Value `json:"output"`
	ResponseID string         `json:"response_id"`
	Usage      *struct {
		InputTokens       int64 `json:"input_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
	} `json:"usage"`
}

type codexRecItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Phase   string `json:"phase"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (it codexRecItem) text() string {
	var parts []string
	for _, c := range it.Content {
		if strings.EqualFold(c.Type, "text") && strings.TrimSpace(c.Text) != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

var (
	// codexCmdRe finds the commands a JavaScript exec cell runs:
	// tools.exec_command({cmd:"…"}) (the cmd key quoted or not).
	codexCmdRe = regexp.MustCompile(`exec_command\(\{\s*"?cmd"?\s*:\s*"((?:[^"\\]|\\.)*)"`)
	// codexPatchRe finds the files an apply_patch touches.
	codexPatchRe = regexp.MustCompile(`(?m)^\*\*\* (?:Update|Add|Delete) File: (.+?)\s*$`)
)

// codexToolCall describes a model tool call from its name and input.
func codexToolCall(id, name, input string) *protocol.AgentToolCall {
	tc := &protocol.AgentToolCall{ID: id, Name: name, Status: "running", Kind: "other", Title: name,
		Input: chat.Redact(chat.Truncate(input, 4000))}
	if m := codexPatchRe.FindAllStringSubmatch(input, 20); len(m) > 0 {
		for _, p := range m {
			tc.Paths = append(tc.Paths, p[1])
		}
		tc.Kind, tc.Title = "edit", "Edit: "+strings.Join(tc.Paths, ", ")
		return tc
	}
	var cmds []string
	for _, m := range codexCmdRe.FindAllStringSubmatch(input, 5) {
		var s string
		if json.Unmarshal([]byte(`"`+m[1]+`"`), &s) != nil {
			s = m[1]
		}
		cmds = append(cmds, s)
	}
	if len(cmds) == 0 && (name == "shell" || name == "exec_command") {
		var a struct {
			Cmd     string   `json:"cmd"`
			Command []string `json:"command"`
		}
		if json.Unmarshal([]byte(input), &a) == nil {
			cmds = append(cmds, cmpOr(a.Cmd, codexCommandLine(a.Command)))
		}
	}
	if len(cmds) > 0 && cmds[0] != "" {
		tc.Kind = "execute"
		tc.Title = "Run: " + chat.Redact(chat.Truncate(strings.Join(cmds, "; "), 200))
	}
	return tc
}

// codexCommandLine is the command a shell call runs: the script of `sh -lc <script>`.
func codexCommandLine(argv []string) string {
	if n := len(argv); n >= 3 && (argv[n-2] == "-lc" || argv[n-2] == "-c") {
		return argv[n-1]
	}
	return strings.Join(argv, " ")
}

// codexOutputText is a tool output's text: a string, or input_text items.
func codexOutputText(raw jsontext.Value) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var items []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &items) == nil {
		var b strings.Builder
		for _, it := range items {
			b.WriteString(it.Text)
		}
		return b.String()
	}
	return ""
}

// CodexEvents turns rollout lines into normalized events:
//
//   - a turn starts: status working; it completes or is aborted: turn_end, status idle;
//   - the user's message (a finished UserMessage item): user_message;
//   - the agent's messages (AgentMessage items, commentary or final answer): message;
//   - the model's tool calls: tool_call running, completed with their output (the
//     cell polls of a long command, "wait", are not calls of their own);
//   - token usage per response: usage.
//
// The model comes from turn_context records. Events carry the record's time and the
// session id.
func CodexEvents(sessionID string, lines []byte) []protocol.AgentChatEvent {
	var out []protocol.AgentChatEvent
	status, model := "", ""
	tools := map[string]*protocol.AgentToolCall{}
	add := func(ev protocol.AgentChatEvent, at time.Time) {
		ev.At, ev.AgentSessionID = at, sessionID
		if ev.Model == "" {
			ev.Model = model
		}
		if ev.Kind == protocol.EventStatus {
			if ev.Status == status {
				return
			}
			status = ev.Status
		}
		out = append(out, ev)
	}
	working := func(at time.Time) {
		add(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusWorking}, at)
	}
	turnEnd := func(reason string, at time.Time) {
		for id, tc := range tools {
			delete(tools, id)
			tc.Status = "completed"
			if reason == "cancelled" {
				tc.Status = "failed"
			}
			add(protocol.AgentChatEvent{Kind: protocol.EventToolCall, ToolCall: tc}, at)
		}
		if status == protocol.ChatStatusIdle {
			return
		}
		add(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, StopReason: reason}, at)
		add(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusIdle}, at)
	}
	for line := range bytes.Lines(lines) {
		var r codexRecord
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		var p codexPayload
		if json.Unmarshal(r.Payload, &p) != nil {
			continue
		}
		at := r.Timestamp
		switch r.Type {
		case "turn_context":
			model = cmpOr(p.Model, model)
		case "event_msg":
			switch p.Type {
			case "task_started":
				working(at)
			case "task_complete":
				turnEnd("end_turn", at)
			case "turn_aborted":
				turnEnd("cancelled", at)
			case "item_completed":
				var it codexRecItem
				if json.Unmarshal(p.Item, &it) != nil {
					continue
				}
				switch it.Type {
				case "UserMessage":
					if text := it.text(); text != "" {
						add(protocol.AgentChatEvent{Kind: protocol.EventUserMessage, MessageID: it.ID,
							Text: redactCut(text, claudePromptMax)}, at)
						working(at)
					}
				case "AgentMessage":
					if text := it.text(); text != "" {
						working(at)
						add(protocol.AgentChatEvent{Kind: protocol.EventMessage, MessageID: it.ID,
							Text: redactCut(text, claudeMessageMax)}, at)
					}
				}
			}
		case "response_item":
			switch p.Type {
			case "custom_tool_call", "function_call":
				if p.CallID == "" || p.Name == "wait" {
					continue
				}
				working(at)
				tc := codexToolCall(p.CallID, p.Name, cmpOr(p.Input, p.Arguments))
				tools[p.CallID] = tc
				cp := *tc
				add(protocol.AgentChatEvent{Kind: protocol.EventToolCall, ToolCall: &cp}, at)
			case "custom_tool_call_output", "function_call_output":
				tc := tools[p.CallID]
				if tc == nil {
					continue // a "wait" poll, or a call read in an earlier pass
				}
				delete(tools, p.CallID)
				tc.Status = "completed"
				tc.Output = redactCut(codexOutputText(p.Output), chat.ClaudeOutputMax)
				add(protocol.AgentChatEvent{Kind: protocol.EventToolCall, ToolCall: tc}, at)
			}
		case "token_usage_record":
			if u := p.Usage; u != nil && p.ResponseID != "" {
				add(protocol.AgentChatEvent{Kind: protocol.EventUsage, MessageID: p.ResponseID,
					Usage: &protocol.AgentTokenUsage{InputTokens: max(0, u.InputTokens-u.CachedInputTokens),
						OutputTokens: u.OutputTokens, CacheReadTokens: u.CachedInputTokens}}, at)
			}
		}
	}
	return out
}
