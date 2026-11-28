package observe

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/chat"
	"github.com/BryanSant/syre/internal/worker/shells"
)

// CopilotDir is one copilot account's home (~/.copilot, or a COPILOT_HOME). Account
// names it; "" for the default account.
type CopilotDir struct {
	Account string
	Dir     string
}

// CopilotSource observes GitHub Copilot CLI sessions (SYR-EFQSX). copilot keeps each
// session in <home>/session-state/<session id>/:
//
//   - events.jsonl, one event per line ({type, data, id, timestamp, parentId}):
//     session.start (cwd), user.message, assistant.turn_start/turn_end (one per model
//     call), assistant.message (text, the tool requests, phase), tool.execution_start
//     and tool.execution_complete, session.usage_record, session.shutdown, hooks;
//   - inuse.<pid>.hold, held by the running copilot: the session's process, as claude's
//     sessions/<pid>.json (cross-platform, no open-file listing needed).
//
// Status comes from the records; the hold file's live process keeps the session alive,
// and session.shutdown or the process's exit ends it.
type CopilotSource struct {
	Dirs  func() []CopilotDir
	Procs ProcTable
	// ProcTTL overrides procTableTTL (negative: snapshot every discovery).
	ProcTTL time.Duration

	mu      sync.Mutex
	alive   map[string]string // sessions alive at the last discovery → events.jsonl
	cwds    map[string]string
	table   map[int]shells.Proc
	tableAt time.Time
}

// Agent implements Source.
func (c *CopilotSource) Agent() string { return "copilot" }

// Discover implements Source.
func (c *CopilotSource) Discover(_ context.Context, since time.Time) ([]Session, error) {
	if c.Dirs == nil {
		return nil, nil
	}
	c.mu.Lock()
	prevAlive := c.alive
	c.mu.Unlock()
	nowAlive := map[string]string{}
	var out []Session
	seen := map[string]bool{}
	for _, d := range c.Dirs() {
		if d.Dir == "" || seen[d.Dir] {
			continue
		}
		seen[d.Dir] = true
		holds := c.liveHolds(d.Dir)
		files, _ := filepath.Glob(filepath.Join(d.Dir, "session-state", "*", "events.jsonl"))
		for _, path := range files {
			dir := filepath.Dir(path)
			id := filepath.Base(dir)
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			key := "copilot/" + d.Account + "/" + id
			p, live := holds[dir]
			_, wasAlive := prevAlive[key]
			if info.ModTime().Before(since) && !live && !wasAlive {
				continue
			}
			s := Session{Agent: "copilot", Account: d.Account, ID: id, Path: path, Cwd: c.cwdOf(path),
				Size: info.Size(), ModTime: info.ModTime()}
			switch {
			case live:
				nowAlive[key] = path
				s.Alive, s.PID, s.Ancestors = true, p.PID, ancestors(c.table, p.PID)
			case wasAlive:
				s.Status = protocol.ChatStatusDone
			}
			out = append(out, s)
		}
	}
	c.mu.Lock()
	c.alive = nowAlive
	c.mu.Unlock()
	return out, nil
}

// liveHolds maps each session dir with an inuse.<pid>.hold of a running process to it.
func (c *CopilotSource) liveHolds(home string) map[string]shells.Proc {
	if c.Procs == nil {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(home, "session-state", "*", "inuse.*.hold"))
	if len(files) == 0 {
		return nil
	}
	ttl := c.ProcTTL
	if ttl == 0 {
		ttl = procTableTTL
	}
	if c.table == nil || ttl < 0 || time.Since(c.tableAt) >= ttl {
		c.snapshot()
	}
	out := map[string]shells.Proc{}
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "inuse."), ".hold")
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		p, ok := c.table[pid]
		if !ok {
			c.snapshot() // a process newer than the snapshot
			p, ok = c.table[pid]
		}
		if !ok || p.Zombie {
			continue // left by a copilot that died
		}
		if p.Start.IsZero() {
			c.Procs.Details(&p)
		}
		if info, err := os.Stat(f); err == nil && !p.Start.IsZero() && p.Start.After(info.ModTime().Add(pidReuseSlack)) {
			continue // the PID now belongs to a newer process
		}
		out[filepath.Dir(f)] = p
	}
	return out
}

func (c *CopilotSource) snapshot() {
	list, err := c.Procs.Snapshot()
	if err != nil {
		return
	}
	m := make(map[int]shells.Proc, len(list))
	for _, p := range list {
		m[p.PID] = p
	}
	c.table, c.tableAt = m, time.Now()
}

// cwdOf reads the cwd from the session's session.start event (cached once found).
func (c *CopilotSource) cwdOf(path string) string {
	c.mu.Lock()
	cwd, ok := c.cwds[path]
	c.mu.Unlock()
	if ok {
		return cwd
	}
	chunk, _, _ := readRecords(path, 0, cwdHeadMax)
	for line := range bytes.Lines(chunk) {
		var r copilotEvent
		if json.Unmarshal(line, &r) != nil || r.Type != "session.start" {
			continue
		}
		var d struct {
			Context struct {
				Cwd string `json:"cwd"`
			} `json:"context"`
		}
		if json.Unmarshal(r.Data, &d) == nil {
			cwd = d.Context.Cwd
		}
		break
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

// Read implements Source.
func (c *CopilotSource) Read(_ context.Context, s Session, offset int64) ([]protocol.AgentChatEvent, int64, error) {
	chunk, next, err := readRecords(s.Path, offset, claudeReadMax)
	if err != nil || len(chunk) == 0 {
		return nil, next, err
	}
	return CopilotEvents(s.ID, chunk), next, nil
}

type copilotEvent struct {
	Type      string         `json:"type"`
	Data      jsontext.Value `json:"data"`
	ID        string         `json:"id"`
	Timestamp time.Time      `json:"timestamp"`
}

type copilotData struct {
	Content      jsontext.Value `json:"content"`
	MessageID    string         `json:"messageId"`
	Model        string         `json:"model"`
	NewModel     string         `json:"newModel"`
	ChosenModel  string         `json:"chosenModel"`
	Phase        string         `json:"phase"`
	ToolRequests []struct {
		ToolCallID string `json:"toolCallId"`
	} `json:"toolRequests"`
	ToolCallID string         `json:"toolCallId"`
	ToolName   string         `json:"toolName"`
	ToolTitle  string         `json:"toolTitle"`
	Arguments  jsontext.Value `json:"arguments"`
	Success    *bool          `json:"success"`
	Result     *struct {
		Content string `json:"content"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage *struct {
		Model           string `json:"model"`
		InputTokens     int64  `json:"inputTokens"`
		OutputTokens    int64  `json:"outputTokens"`
		CacheReadTokens int64  `json:"cacheReadTokens"`
		CacheWrite      int64  `json:"cacheWriteTokens"`
	} `json:"usage"`
}

// copilotToolCall describes a tool call from its name and arguments.
func copilotToolCall(id, name, title string, args jsontext.Value) *protocol.AgentToolCall {
	tc := &protocol.AgentToolCall{ID: id, Name: name, Status: "running", Kind: "other", Title: cmpOr(title, name),
		Input: chat.Redact(chat.Truncate(string(args), 4000))}
	var a struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
	_ = json.Unmarshal(args, &a)
	switch name {
	case "bash", "powershell", "shell", "write_bash", "write_powershell":
		tc.Kind = "execute"
		if a.Command != "" {
			tc.Title = "Run: " + chat.Redact(chat.Truncate(a.Command, 200))
		}
	case "edit", "create", "str_replace_editor", "str_replace", "write", "apply_patch":
		tc.Kind = "edit"
		if a.Path != "" {
			tc.Paths = []string{a.Path}
			tc.Title = "Edit: " + a.Path
		}
	case "view", "read", "read_file":
		tc.Kind = "read"
		if a.Path != "" {
			tc.Paths = []string{a.Path}
			tc.Title = "Read: " + a.Path
		}
	case "grep", "glob", "search":
		tc.Kind = "search"
	case "web_fetch", "fetch":
		tc.Kind = "fetch"
	}
	return tc
}

// CopilotEvents turns events.jsonl lines into normalized events:
//
//   - user.message: user_message, status working;
//   - assistant.message: message (its text), with status working;
//   - tool.execution_start/complete: tool_call running, then completed or failed with
//     the result;
//   - assistant.turn_end after a final answer (no tool requests): turn_end, status idle
//     (copilot ends a "turn" after every model call);
//   - abort: turn_end cancelled, status idle; session.shutdown: status done;
//   - session.usage_record: usage.
func CopilotEvents(sessionID string, lines []byte) []protocol.AgentChatEvent {
	var out []protocol.AgentChatEvent
	status, model := "", ""
	final := false
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
	set := func(s string, at time.Time) { add(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: s}, at) }
	turnEnd := func(reason string, at time.Time) {
		if status == protocol.ChatStatusIdle || status == protocol.ChatStatusDone {
			return
		}
		add(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, StopReason: reason}, at)
		set(protocol.ChatStatusIdle, at)
	}
	for line := range bytes.Lines(lines) {
		var e copilotEvent
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		var d copilotData
		if json.Unmarshal(e.Data, &d) != nil {
			continue
		}
		at := e.Timestamp
		switch e.Type {
		case "session.model_change":
			if d.NewModel != "" && d.NewModel != "auto" {
				model = d.NewModel
			}
		case "session.auto_mode_resolved":
			model = cmpOr(d.ChosenModel, model)
		case "user.message":
			var text string
			if json.Unmarshal(d.Content, &text) == nil && strings.TrimSpace(text) != "" {
				add(protocol.AgentChatEvent{Kind: protocol.EventUserMessage, MessageID: e.ID, Text: redactCut(text, claudePromptMax)}, at)
				set(protocol.ChatStatusWorking, at)
				final = false
			}
		case "assistant.turn_start":
			set(protocol.ChatStatusWorking, at)
			final = false
		case "assistant.message":
			model = cmpOr(d.Model, model)
			var text string
			if json.Unmarshal(d.Content, &text) == nil && strings.TrimSpace(text) != "" {
				set(protocol.ChatStatusWorking, at)
				add(protocol.AgentChatEvent{Kind: protocol.EventMessage, MessageID: cmpOr(d.MessageID, e.ID), Text: redactCut(text, claudeMessageMax)}, at)
			}
			final = len(d.ToolRequests) == 0
		case "tool.execution_start":
			set(protocol.ChatStatusWorking, at)
			tc := copilotToolCall(d.ToolCallID, d.ToolName, d.ToolTitle, d.Arguments)
			tools[d.ToolCallID] = tc
			cp := *tc
			add(protocol.AgentChatEvent{Kind: protocol.EventToolCall, ToolCall: &cp}, at)
		case "tool.execution_complete":
			tc := tools[d.ToolCallID]
			if tc == nil {
				tc = &protocol.AgentToolCall{ID: d.ToolCallID, Name: "tool"}
			}
			delete(tools, d.ToolCallID)
			tc.Status = "completed"
			if d.Success != nil && !*d.Success {
				tc.Status = "failed"
			}
			switch {
			case d.Result != nil:
				tc.Output = redactCut(d.Result.Content, chat.ClaudeOutputMax)
			case d.Error != nil:
				tc.Output = redactCut(d.Error.Message, chat.ClaudeOutputMax)
			}
			add(protocol.AgentChatEvent{Kind: protocol.EventToolCall, ToolCall: tc}, at)
		case "assistant.turn_end":
			if final {
				turnEnd("end_turn", at)
			}
		case "abort", "session.abort":
			turnEnd("cancelled", at)
		case "session.usage_record":
			if u := d.Usage; u != nil {
				add(protocol.AgentChatEvent{Kind: protocol.EventUsage, MessageID: e.ID, Model: u.Model,
					Usage: &protocol.AgentTokenUsage{InputTokens: max(0, u.InputTokens-u.CacheReadTokens), OutputTokens: u.OutputTokens,
						CacheReadTokens: u.CacheReadTokens, CacheCreateTokens: u.CacheWrite}}, at)
			}
		case "session.shutdown":
			turnEnd("end_turn", at)
			set(protocol.ChatStatusDone, at)
		}
	}
	return out
}
