package drivers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

func init() {
	Register(&GrokDriver{})
}

// GrokDriver runs xAI's Grok Build CLI (`grok`), verified against grok 1.0.92.
//
// Flags (from `grok --help` and live runs):
//   - full_access: `--always-approve` (grok rejects `--yes`).
//   - sandboxed:   `--sandbox workspace` (Seatbelt/Landlock: writes limited to the workdir,
//     /tmp and ~/.grok), without auto-approval.
//   - model:       `--model <id>`.
//   - oneshot:     `--single=<prompt>` (headless; prints the reply to stdout and exits).
//   - interactive: the TUI, with the initial prompt after `--`.
//
// Every launch gets a fresh `--session-id <uuid>` (honoured by both headless and TUI
// modes), and telemetry tails only that session's
// $GROK_HOME/sessions/<url-encoded cwd>/<uuid>/updates.jsonl (the ACP session/update
// stream), so concurrent tasks or the user's own grok sessions never mix.
type GrokDriver struct {
	mu    sync.Mutex
	tasks map[string]*grokTask
}

// grokTask is the per-task state shared by BuildCommand, WatchTelemetry and ExtractSummary.
type grokTask struct {
	sessionID   string
	interactive bool
	grokHome    string
	lastMessage string // last complete assistant message segment
	pending     strings.Builder
}

func (d *GrokDriver) Name() string { return "grok" }

func (d *GrokDriver) Detect() (string, bool) {
	return FindBinary("grok")
}

func (d *GrokDriver) BuildCommand(opts LaunchOptions) LaunchSpec {
	bin, ok := d.Detect()
	if !ok {
		bin = "grok"
	}
	args := []string{}
	if IsFullAccess(opts.PermissionMode) {
		args = append(args, "--always-approve")
	} else {
		args = append(args, "--sandbox", "workspace")
	}
	if m := strings.TrimSpace(opts.Model); m != "" {
		args = append(args, "--model", m)
	}
	args = append(args, "--session-id", d.newTask(opts))
	// The prompt is either "="-joined to its flag or placed after "--", so a prompt that
	// begins with '-' can never be parsed as a CLI flag.
	if opts.Mode == "interactive" {
		if strings.TrimSpace(opts.Prompt) != "" {
			args = append(args, "--", opts.Prompt)
		}
	} else {
		args = append(args, "--single="+opts.Prompt)
	}
	return LaunchSpec{
		Binary: bin,
		Args:   args,
		Env:    opts.Env,
	}
}

func (d *GrokDriver) newTask(opts LaunchOptions) string {
	t := &grokTask{
		sessionID:   uuid.NewString(),
		interactive: opts.Mode == "interactive",
		grokHome:    grokHomeDir(opts.Env),
	}
	if opts.TaskID != "" {
		d.mu.Lock()
		if d.tasks == nil {
			d.tasks = make(map[string]*grokTask)
		}
		d.tasks[opts.TaskID] = t
		d.mu.Unlock()
	}
	return t.sessionID
}

func (d *GrokDriver) task(taskID string) *grokTask {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tasks[taskID]
}

// sessionFor returns the grok session UUID assigned to taskID ("" if none).
func (d *GrokDriver) sessionFor(taskID string) string {
	if t := d.task(taskID); t != nil {
		return t.sessionID
	}
	return ""
}

// grokHomeDir resolves grok's home: GROK_HOME from the task env, then the daemon env,
// then ~/.grok.
func grokHomeDir(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], "GROK_HOME="); ok && v != "" {
			return v
		}
	}
	if v := os.Getenv("GROK_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".grok")
}

// findSessionLog locates <grokHome>/sessions/*/<sessionID>/updates.jsonl. Grok names the
// parent directory after the URL-encoded, symlink-resolved cwd; matching on the unique
// session UUID avoids re-implementing that encoding.
func findSessionLog(grokHome, sessionID string) string {
	if grokHome == "" || sessionID == "" {
		return ""
	}
	base := filepath.Join(grokHome, "sessions")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(base, e.Name(), sessionID, "updates.jsonl")
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func (d *GrokDriver) WatchTelemetry(ctx context.Context, opts WatchOptions, emit func(TelemetryEvent)) {
	t := d.task(opts.TaskID)
	var (
		logPath       string
		offset        int64
		trustPrompted bool
	)
	poll := func() {
		if t == nil {
			return // no session of our own to tail; never read other sessions' logs
		}
		if logPath == "" {
			if logPath = findSessionLog(t.grokHome, t.sessionID); logPath == "" {
				if t.interactive && !trustPrompted && grokTrustPromptShown(opts.RingSnapshot) {
					trustPrompted = true
					emit(TelemetryEvent{Kind: "waiting_input", Message: grokTrustPrompt + " (answer y to proceed)"})
				}
				return
			}
		}
		offset = d.pollUpdates(t, logPath, offset, emit)
	}

	ticker := time.NewTicker(600 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			poll()
			return
		case line, ok := <-opts.OutputLines:
			if !ok {
				poll()
				return
			}
			if ev, ok := grokScanOutput(line); ok {
				emit(ev)
			}
		case <-ticker.C:
			poll()
		}
	}
}

// grokTrustPrompt is the TUI's first-run folder trust dialog. It blocks before the session
// starts until the user answers y/n, and is drawn with cursor addressing (no newline), so it
// never reaches OutputLines: it is detected in the PTY ring instead.
const grokTrustPrompt = "Do you trust the contents of this directory?"

// grokTrustPromptShown reports whether the tail of the PTY output shows the trust dialog.
func grokTrustPromptShown(snapshot func() []byte) bool {
	if snapshot == nil {
		return false
	}
	raw := snapshot()
	if len(raw) > 64*1024 {
		raw = raw[len(raw)-64*1024:]
	}
	return strings.Contains(StripANSI(string(raw)), grokTrustPrompt)
}

// grokScanOutput inspects one PTY output line. Structured milestones come from the
// session log; the PTY is only scanned for things the log never records: CLI usage
// errors (printed before a session exists) and explicit y/n confirmation prompts.
func grokScanOutput(line string) (TelemetryEvent, bool) {
	clean := strings.TrimSpace(StripANSI(line))
	if clean == "" {
		return TelemetryEvent{}, false
	}
	lower := strings.ToLower(clean)
	switch {
	case strings.HasPrefix(lower, "error:"):
		return TelemetryEvent{Kind: "error", Message: truncateRunes(clean, 300)}, true
	case strings.Contains(lower, "[y/n]"):
		return TelemetryEvent{Kind: "waiting_input", Message: truncateRunes(clean, 300)}, true
	}
	return TelemetryEvent{}, false
}

// pollUpdates parses complete lines appended to path since offset and returns the new
// offset. The offset only advances past a trailing '\n': a record that is still being
// written is re-read in full on the next poll instead of being lost or split.
func (d *GrokDriver) pollUpdates(t *grokTask, path string, offset int64, emit func(TelemetryEvent)) int64 {
	f, err := os.Open(path)
	if err != nil {
		return offset
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() < offset {
		offset = 0 // truncated or replaced
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return offset
		}
	}
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			offset += int64(len(line))
			d.parseUpdate(t, line, emit)
		}
		if err != nil {
			return offset
		}
	}
}

// grokUpdateLine is one updates.jsonl envelope: {"timestamp":..,"method":"session/update"
// or "_x.ai/session/update","params":{"sessionId":..,"update":{"sessionUpdate":..}}}.
type grokUpdateLine struct {
	Params struct {
		Update grokUpdate `json:"update"`
	} `json:"params"`
}

type grokUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`
	Content       struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Title    string         `json:"title"`
	Kind     string         `json:"kind"`
	RawInput map[string]any `json:"rawInput"`
	Meta     struct {
		Tool struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"x.ai/tool"`
	} `json:"_meta"`
	StopReason string     `json:"stop_reason"`
	Usage      *grokUsage `json:"usage"`
	ElapsedMS  int64      `json:"elapsed_ms"`
	Entries    []struct {
		Content string `json:"content"`
		Status  string `json:"status"`
	} `json:"entries"`
}

// grokUsage is a turn_completed usage block. Values are per prompt, not cumulative
// (a session's usage.json totals are the sum of its turns), and inputTokens already
// includes cachedReadTokens.
type grokUsage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CachedReadTokens int64 `json:"cachedReadTokens"`
	ReasoningTokens  int64 `json:"reasoningTokens"`
	CostUsdTicks     int64 `json:"costUsdTicks"`
	ModelUsage       map[string]struct {
		InputTokens      int64 `json:"inputTokens"`
		OutputTokens     int64 `json:"outputTokens"`
		CachedReadTokens int64 `json:"cachedReadTokens"`
		ReasoningTokens  int64 `json:"reasoningTokens"`
		CostUsdTicks     int64 `json:"costUsdTicks"`
	} `json:"modelUsage"`
}

func (d *GrokDriver) parseUpdate(t *grokTask, raw []byte, emit func(TelemetryEvent)) {
	var rec grokUpdateLine
	if err := json.Unmarshal(raw, &rec); err != nil {
		return
	}
	u := rec.Params.Update
	switch u.SessionUpdate {
	case "user_message_chunk":
		d.mu.Lock()
		t.pending.Reset()
		d.mu.Unlock()
	case "agent_message_chunk":
		if u.Content.Type == "text" || u.Content.Type == "" {
			d.mu.Lock()
			t.pending.WriteString(u.Content.Text)
			if s := strings.TrimSpace(t.pending.String()); s != "" {
				t.lastMessage = s
			}
			d.mu.Unlock()
		}
	case "tool_call":
		// A tool call ends the current assistant message segment.
		d.mu.Lock()
		t.pending.Reset()
		d.mu.Unlock()
		emit(grokToolEvent(u))
	case "plan":
		done := 0
		var current string
		for _, e := range u.Entries {
			switch e.Status {
			case "completed":
				done++
			case "in_progress":
				if current == "" {
					current = e.Content
				}
			}
		}
		msg := fmt.Sprintf("Plan: %d/%d done", done, len(u.Entries))
		if current != "" {
			msg += " (now: " + truncateRunes(current, 120) + ")"
		}
		emit(TelemetryEvent{Kind: "milestone", Message: msg, Meta: map[string]any{"total": len(u.Entries), "done": done}})
	case "turn_completed":
		d.emitUsage(u.Usage, emit)
		msg := "Turn completed"
		if u.StopReason != "" && u.StopReason != "end_turn" {
			msg += " (" + u.StopReason + ")"
		}
		if u.ElapsedMS > 0 {
			msg += fmt.Sprintf(" in %.1fs", float64(u.ElapsedMS)/1000)
		}
		emit(TelemetryEvent{Kind: "milestone", Message: msg, Meta: map[string]any{"stop_reason": u.StopReason}})
		if t.interactive {
			// The TUI is now idle at its prompt, waiting for the next instruction.
			emit(TelemetryEvent{Kind: "waiting_input", Message: "grok is waiting for your next prompt"})
		}
	}
}

func (d *GrokDriver) emitUsage(u *grokUsage, emit func(TelemetryEvent)) {
	if u == nil {
		return
	}
	if len(u.ModelUsage) == 0 {
		if u.InputTokens > 0 || u.OutputTokens > 0 {
			emit(grokUsageEvent("", u.InputTokens, u.OutputTokens, u.CachedReadTokens, u.CostUsdTicks))
		}
		return
	}
	for model, mu := range u.ModelUsage {
		if mu.InputTokens > 0 || mu.OutputTokens > 0 {
			emit(grokUsageEvent(model, mu.InputTokens, mu.OutputTokens, mu.CachedReadTokens, mu.CostUsdTicks))
		}
	}
}

func grokUsageEvent(model string, in, out, cached, costTicks int64) TelemetryEvent {
	label := model
	if label == "" {
		label = "unknown model"
	}
	meta := map[string]any{
		"provider":      "grok",
		"model":         model,
		"input_tokens":  in,
		"output_tokens": out,
	}
	if cached > 0 {
		meta["cached_input_tokens"] = cached
	}
	if costTicks > 0 {
		meta["cost_usd_ticks"] = costTicks
	}
	return TelemetryEvent{
		Kind:    "usage",
		Message: fmt.Sprintf("Grok usage (%s): +%d in / +%d out", label, in, out),
		Meta:    meta,
	}
}

// grokToolEvent maps a tool_call update to a telemetry event. Tool names are grok's
// internal ids (write, search_replace, run_terminal_command, read_file, ...) with an
// ACP kind (edit, write, execute, read, search, plan, ask_user, ...).
func grokToolEvent(u grokUpdate) TelemetryEvent {
	name := u.Meta.Tool.Name
	if name == "" {
		name = strings.TrimSuffix(strings.TrimSpace(u.Title), ":")
	}
	kind := u.Meta.Tool.Kind
	if kind == "" {
		kind = u.Kind
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := u.RawInput[k].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
		return ""
	}
	ev := TelemetryEvent{Kind: "tool_call", Message: name, Meta: map[string]any{"tool": name}}
	if kind != "" {
		ev.Meta["tool_kind"] = kind
	}
	switch {
	case kind == "edit" || kind == "write" || name == "write" || name == "search_replace":
		ev.Kind = "file_edit"
		if fp := str("file_path", "target_file", "path"); fp != "" {
			ev.Message = name + ": " + fp
		}
	case kind == "execute" || name == "run_terminal_command":
		if cmd := str("command"); cmd != "" {
			ev.Message = "Run: " + truncateRunes(cmd, 120)
		}
	case kind == "read" || name == "read_file":
		if fp := str("target_file", "file_path", "path"); fp != "" {
			ev.Message = "Read: " + fp
		}
	case kind == "ask_user" || name == "ask_user_question":
		ev.Kind = "waiting_input"
		ev.Message = "grok is asking a question"
		if qs, ok := u.RawInput["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				if s, ok := q["question"].(string); ok && s != "" {
					ev.Message = truncateRunes(s, 300)
				}
			}
		}
	}
	return ev
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func (d *GrokDriver) ExtractSummary(_ context.Context, opts SummaryOptions) string {
	d.mu.Lock()
	t := d.tasks[opts.TaskID]
	delete(d.tasks, opts.TaskID)
	var s string
	if t != nil {
		s = t.lastMessage
	}
	d.mu.Unlock()
	if s != "" {
		return truncateRunes(s, 2000)
	}
	// Headless mode prints only the reply, so the PTY tail is a usable fallback. The TUI
	// repaints with cursor addressing, so its tail is not; skip it for interactive tasks.
	if opts.RingSnapshot != nil && (t == nil || !t.interactive) {
		if tail := TailCleanSummary(opts.RingSnapshot(), 15, 1500); tail != "" {
			return tail
		}
	}
	return fmt.Sprintf("grok exited with code %d", opts.ExitCode)
}
