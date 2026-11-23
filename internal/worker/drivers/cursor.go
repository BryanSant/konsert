package drivers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

func init() {
	Register(&CursorDriver{})
}

// CursorDriver runs the Cursor Agent CLI (`cursor-agent`), verified against
// cursor-agent 2026.11.19-f005e9d.
//
// Flags (from `cursor-agent --help` and live runs):
//   - full_access: `--force` (run every command unless explicitly denied) and `--trust`.
//   - sandboxed:   `--sandbox enabled` without --force: commands outside the allowlist are
//     not auto-run, and those that run are sandboxed.
//   - trust:       print mode refuses an untrusted workspace ("Workspace Trust Required"), so
//     oneshot tasks always pass `--trust`; sandboxed interactive tasks leave the decision to
//     the user in the TUI dialog. Trusting writes
//     $CURSOR_DATA_DIR/projects/<slug>/.workspace-trusted (trustMethod "cli-flag").
//   - model:       `--model=<id>` (`cursor-agent models` lists them; `auto` is the default).
//   - oneshot:     `--print --output-format text -- <prompt>` (prints only the reply).
//   - interactive: the TUI, with the initial prompt after `--`.
//
// Every launch gets a fresh `--resume=<uuid>`: resuming an unknown chat ID starts a new chat
// with that ID (verified), so the task owns a known chat. Telemetry tails only that chat's
// transcript, $CURSOR_DATA_DIR/projects/<slug>/agent-transcripts/<uuid>/<uuid>.jsonl.
//
// The transcript records messages, tool calls and turn ends but no token usage, and the
// text output format prints none either (only `--output-format stream-json` carries a
// usage block), so this driver reports no usage events.
type CursorDriver struct {
	mu    sync.Mutex
	tasks map[string]*cursorTask
}

type cursorTask struct {
	chatID      string
	interactive bool
	dataDir     string
	lastMessage string
}

func (d *CursorDriver) Name() string { return "cursor" }

func (d *CursorDriver) Detect() (string, bool) {
	return FindBinary("cursor-agent")
}

func (d *CursorDriver) BuildCommand(opts LaunchOptions) LaunchSpec {
	bin, ok := d.Detect()
	if !ok {
		bin = "cursor-agent"
	}
	interactive := opts.Mode == "interactive"
	var args []string
	mode := flagValue(opts.ApprovalMode)
	switch {
	case ApprovalModeFullAccess("cursor", mode):
		args = append(args, "--force", "--trust")
	case mode == "plan" || mode == "ask" || mode == "default":
		// Effort is part of cursor's model id (e.g. "-high" variants); no effort flag.
		if mode != "default" {
			args = append(args, "--mode="+mode)
		}
		if !IsFullAccess(opts.PermissionMode) {
			args = append(args, "--sandbox", "enabled")
		}
		if !interactive || IsFullAccess(opts.PermissionMode) {
			args = append(args, "--trust")
		}
	case IsFullAccess(opts.PermissionMode):
		args = append(args, "--force", "--trust")
	default:
		args = append(args, "--sandbox", "enabled")
		if !interactive {
			args = append(args, "--trust")
		}
	}
	if m := strings.TrimSpace(opts.Model); m != "" {
		args = append(args, "--model="+m)
	}
	args = append(args, "--resume="+d.newTask(opts))
	// "--" ends option parsing, so a prompt that begins with '-' stays a prompt.
	if interactive {
		if strings.TrimSpace(opts.Prompt) != "" {
			args = append(args, "--", opts.Prompt)
		}
	} else {
		args = append(args, "--print", "--output-format", "text", "--", opts.Prompt)
	}
	return LaunchSpec{Binary: bin, Args: args, Env: opts.Env}
}

func (d *CursorDriver) newTask(opts LaunchOptions) string {
	t := &cursorTask{
		chatID:      uuid.NewString(),
		interactive: opts.Mode == "interactive",
		dataDir:     agentHomeDir(opts.Env, "CURSOR_DATA_DIR", ".cursor"),
	}
	if opts.TaskID != "" {
		d.mu.Lock()
		if d.tasks == nil {
			d.tasks = make(map[string]*cursorTask)
		}
		d.tasks[opts.TaskID] = t
		d.mu.Unlock()
	}
	return t.chatID
}

func (d *CursorDriver) task(taskID string) *cursorTask {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tasks[taskID]
}

// chatFor returns the chat ID assigned to taskID ("" if none).
func (d *CursorDriver) chatFor(taskID string) string {
	if t := d.task(taskID); t != nil {
		return t.chatID
	}
	return ""
}

// findCursorTranscript locates <dataDir>/projects/*/agent-transcripts/<id>/<id>.jsonl.
// Cursor names the project directory after a slug of the resolved workspace path; matching
// on the unique chat ID avoids re-implementing that slug.
func findCursorTranscript(dataDir, chatID string) string {
	if dataDir == "" || chatID == "" {
		return ""
	}
	base := filepath.Join(dataDir, "projects")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(base, e.Name(), "agent-transcripts", chatID, chatID+".jsonl")
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// cursorTrustPrompt is the TUI's workspace trust dialog (sandboxed interactive tasks only).
const cursorTrustPrompt = "Do you trust the contents of this directory?"

func (d *CursorDriver) WatchTelemetry(ctx context.Context, opts WatchOptions, emit func(TelemetryEvent)) {
	t := d.task(opts.TaskID)
	var (
		path          string
		offset        int64
		trustPrompted bool
	)
	poll := func() {
		if t == nil {
			return // no chat of our own to tail; never read other chats' transcripts
		}
		if path == "" {
			if path = findCursorTranscript(t.dataDir, t.chatID); path == "" {
				if t.interactive && !trustPrompted && ringContains(opts.RingSnapshot, cursorTrustPrompt) {
					trustPrompted = true
					emit(TelemetryEvent{Kind: "waiting_input", Message: cursorTrustPrompt + " (choose to trust it to proceed)"})
				}
				return
			}
		}
		offset = tailJSONL(path, offset, func(line []byte) { d.parseLine(t, line, emit) })
	}
	watchLoop(ctx, opts.OutputLines, poll, func(line string) {
		if ev, ok := scanCLIOutput(line); ok {
			emit(ev)
		}
	})
}

// cursorLine is one transcript record: a message ({"role":"user"|"assistant","message":
// {"content":[{"type":"text"|"tool_use",..}]}}) or a marker ({"type":"turn_ended",..}).
type cursorLine struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Role    string `json:"role"`
	Message struct {
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

func (d *CursorDriver) parseLine(t *cursorTask, raw []byte, emit func(TelemetryEvent)) {
	var rec cursorLine
	if err := json.Unmarshal(raw, &rec); err != nil {
		return
	}
	if rec.Type == "turn_ended" {
		msg := "Turn completed"
		if rec.Status != "" && rec.Status != "success" {
			msg += " (" + rec.Status + ")"
		}
		emit(TelemetryEvent{Kind: "milestone", Message: msg, Meta: map[string]any{"status": rec.Status}})
		if t.interactive {
			emit(TelemetryEvent{Kind: "waiting_input", Message: "cursor-agent is waiting for your next prompt"})
		}
		return
	}
	if rec.Role != "assistant" {
		return
	}
	for _, c := range rec.Message.Content {
		switch c.Type {
		case "text":
			if s := strings.TrimSpace(c.Text); s != "" {
				d.mu.Lock()
				t.lastMessage = s
				d.mu.Unlock()
			}
		case "tool_use":
			if c.Name != "" {
				emit(cursorToolEvent(c.Name, c.Input))
			}
		}
	}
}

// cursorToolEvent maps a transcript tool_use block (Write, StrReplace, Delete, Shell, Read,
// Grep, Glob, ...) to a telemetry event.
func cursorToolEvent(name string, input map[string]any) TelemetryEvent {
	ev := TelemetryEvent{Kind: "tool_call", Message: name, Meta: map[string]any{"tool": name}}
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "write") || strings.Contains(lower, "edit") || strings.Contains(lower, "replace") ||
		strings.Contains(lower, "patch") || strings.Contains(lower, "delete"):
		ev.Kind = "file_edit"
		if fp := firstString(input, "path", "file_path", "target_file"); fp != "" {
			ev.Message = name + ": " + fp
		}
	case strings.Contains(lower, "shell") || strings.Contains(lower, "terminal") || strings.Contains(lower, "bash"):
		if cmd := firstString(input, "command"); cmd != "" {
			ev.Message = "Run: " + truncateRunes(cmd, 120)
		}
	case strings.Contains(lower, "read"):
		if fp := firstString(input, "path", "file_path", "target_file"); fp != "" {
			ev.Message = "Read: " + fp
		}
	case strings.Contains(lower, "ask") || strings.Contains(lower, "question"):
		ev.Kind = "waiting_input"
		ev.Message = "cursor-agent is asking a question"
		if q := firstString(input, "question", "prompt"); q != "" {
			ev.Message = truncateRunes(q, 300)
		}
	}
	return ev
}

func (d *CursorDriver) ExtractSummary(_ context.Context, opts SummaryOptions) string {
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
	// Print mode writes only the reply, so the PTY tail is a usable fallback; the TUI
	// repaints with cursor addressing, so skip it for interactive tasks.
	if opts.RingSnapshot != nil && (t == nil || !t.interactive) {
		if tail := TailCleanSummary(opts.RingSnapshot(), 15, 1500); tail != "" {
			return tail
		}
	}
	return fmt.Sprintf("cursor-agent exited with code %d", opts.ExitCode)
}
