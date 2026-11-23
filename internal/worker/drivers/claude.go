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

	"github.com/BryanSant/konsert/internal/ids"
)

func init() {
	Register(&ClaudeDriver{})
}

type usageCounter struct {
	in  int64
	out int64
}

// ClaudeDriver runs Anthropic's Claude Code CLI (`claude`).
// In default full_access mode, it passes `--dangerously-skip-permissions`.
//
// Each launch gets a fresh `--session-id <uuid>`, and telemetry tails only that session's
// ~/.claude/projects/<dir>/<uuid>.jsonl, so concurrent tasks (or the user's own session) in
// the same directory never mix usage or summaries.
type ClaudeDriver struct {
	mu          sync.Mutex
	lastSummary map[string]string
	seenUsage   map[string]map[string]usageCounter // taskID -> model -> last reported cumulative totals
	sessions    map[string]string                  // taskID -> claude session UUID
}

// sessionFor returns the Claude session UUID assigned to taskID ("" if none).
func (d *ClaudeDriver) sessionFor(taskID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessions[taskID]
}

func (d *ClaudeDriver) newSession(taskID string) string {
	id := ids.UUID()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sessions == nil {
		d.sessions = make(map[string]string)
	}
	if taskID != "" {
		d.sessions[taskID] = id
	}
	return id
}

func (d *ClaudeDriver) Name() string { return "claude" }

func (d *ClaudeDriver) Detect() (string, bool) {
	return FindBinary("claude")
}

func (d *ClaudeDriver) BuildCommand(opts LaunchOptions) LaunchSpec {
	bin, ok := d.Detect()
	if !ok {
		bin = "claude"
	}
	args := []string{}
	if mode := permissionModeFlag(opts.ApprovalMode); mode != "" {
		args = append(args, "--permission-mode", mode)
	} else if IsFullAccess(opts.PermissionMode) {
		args = append(args, "--dangerously-skip-permissions")
	}
	if m := strings.TrimSpace(opts.Model); m != "" {
		args = append(args, "--model", m)
	}
	if e := flagValue(opts.Effort); e != "" {
		args = append(args, "--effort", e)
	}
	args = append(args, "--session-id", d.newSession(opts.TaskID))
	// "--" terminates option parsing so a prompt that begins with '-' (for example
	// "--dangerously-skip-permissions") can never be interpreted as a CLI flag.
	if opts.Mode == "interactive" {
		if strings.TrimSpace(opts.Prompt) != "" {
			args = append(args, "--", opts.Prompt)
		}
	} else {
		args = append(args, "-p", "--", opts.Prompt)
	}
	return LaunchSpec{
		Binary: bin,
		Args:   args,
		Env:    opts.Env,
	}
}

// encodeClaudeProjectDir converts a filesystem path to Claude Code's ~/.claude/projects/<encoded> directory name.
func encodeClaudeProjectDir(workdir string) string {
	cleaned := filepath.Clean(workdir)
	var b strings.Builder
	for _, r := range cleaned {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func claudeCandidateProjectDirs(workdir string) []string {
	home, _ := os.UserHomeDir()
	if home == "" || workdir == "" {
		return nil
	}
	base := filepath.Join(home, ".claude", "projects")
	seen := make(map[string]bool)
	var dirs []string
	add := func(p string) {
		if p == "" {
			return
		}
		enc := filepath.Join(base, encodeClaudeProjectDir(p))
		if !seen[enc] {
			seen[enc] = true
			dirs = append(dirs, enc)
		}
	}
	add(workdir)
	if resolved, err := filepath.EvalSymlinks(workdir); err == nil {
		add(resolved)
	}
	return dirs
}

func (d *ClaudeDriver) setLastSummary(taskID, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastSummary == nil {
		d.lastSummary = make(map[string]string)
	}
	d.lastSummary[taskID] = text
}

func (d *ClaudeDriver) getAndClearState(taskID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var s string
	if d.lastSummary != nil {
		s = d.lastSummary[taskID]
		delete(d.lastSummary, taskID)
	}
	if d.seenUsage != nil {
		delete(d.seenUsage, taskID)
	}
	delete(d.sessions, taskID)
	return s
}

func (d *ClaudeDriver) computeUsageDelta(taskID, model string, cumIn, cumOut int64) (int64, int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seenUsage == nil {
		d.seenUsage = make(map[string]map[string]usageCounter)
	}
	byModel := d.seenUsage[taskID]
	if byModel == nil {
		byModel = make(map[string]usageCounter)
		d.seenUsage[taskID] = byModel
	}
	prev := byModel[model]
	dIn := cumIn - prev.in
	dOut := cumOut - prev.out
	if dIn < 0 {
		dIn = 0
	}
	if dOut < 0 {
		dOut = 0
	}
	if cumIn > prev.in {
		prev.in = cumIn
	}
	if cumOut > prev.out {
		prev.out = cumOut
	}
	byModel[model] = prev
	return dIn, dOut
}

func (d *ClaudeDriver) WatchTelemetry(ctx context.Context, opts WatchOptions, emit func(TelemetryEvent)) {
	projectDirs := claudeCandidateProjectDirs(opts.Workdir)
	sessionID := d.sessionFor(opts.TaskID)

	offsets := make(map[string]int64)
	ticker := time.NewTicker(600 * time.Millisecond)
	defer ticker.Stop()

	pollAll := func() {
		if sessionID == "" {
			return // no session of our own to tail; never read other sessions' logs
		}
		for _, dir := range projectDirs {
			path := filepath.Join(dir, sessionID+".jsonl")
			offsets[path] = d.pollJSONLFile(opts.TaskID, path, offsets[path], emit)
		}
	}

	for {
		select {
		case <-ctx.Done():
			pollAll()
			return
		case line, ok := <-opts.OutputLines:
			if !ok {
				pollAll()
				return
			}
			clean := strings.TrimSpace(StripANSI(line))
			if clean == "" {
				continue
			}
			lower := strings.ToLower(clean)
			if strings.Contains(lower, "do you want to") ||
				strings.Contains(lower, "allow this") ||
				strings.Contains(lower, "yes, allow") ||
				strings.Contains(lower, "allow once") ||
				strings.Contains(lower, "[y/n]") {
				emit(TelemetryEvent{
					Kind:    "waiting_input",
					Message: clean,
				})
			}
		case <-ticker.C:
			pollAll()
		}
	}
}

type claudeJSONLLine struct {
	Type    string `json:"type"`
	Message *struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ModelUsage map[string]struct {
		InputTokens              int64 `json:"inputTokens"`
		OutputTokens             int64 `json:"outputTokens"`
		CacheReadInputTokens     int64 `json:"cacheReadInputTokens"`
		CacheCreationInputTokens int64 `json:"cacheCreationInputTokens"`
	} `json:"modelUsage"`
}

type claudeContentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// pollJSONLFile parses complete lines appended to path since offset and returns the new
// offset. The offset only advances past a trailing '\n': a record that is still being
// written is re-read in full on the next poll instead of being lost or split.
func (d *ClaudeDriver) pollJSONLFile(taskID, path string, offset int64, emit func(TelemetryEvent)) int64 {
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
			d.parseJSONLLine(taskID, line, emit)
		}
		if err != nil {
			return offset
		}
	}
}

func (d *ClaudeDriver) parseJSONLLine(taskID string, raw []byte, emit func(TelemetryEvent)) {
	var rec claudeJSONLLine
	if err := json.Unmarshal(raw, &rec); err != nil {
		return
	}

	// Capture cumulative modelUsage deltas from Claude Code cost-state entries.
	if rec.Type == "cost-state" && len(rec.ModelUsage) > 0 {
		for model, mu := range rec.ModelUsage {
			totalIn := mu.InputTokens + mu.CacheReadInputTokens + mu.CacheCreationInputTokens
			dIn, dOut := d.computeUsageDelta(taskID, model, totalIn, mu.OutputTokens)
			if dIn > 0 || dOut > 0 {
				emit(TelemetryEvent{
					Kind:    "usage",
					Message: fmt.Sprintf("Claude usage (%s): +%d in / +%d out", model, dIn, dOut),
					Meta: map[string]any{
						"provider":      "claude",
						"model":         model,
						"input_tokens":  dIn,
						"output_tokens": dOut,
					},
				})
			}
		}
		return
	}

	if rec.Message == nil || rec.Message.Role != "assistant" || len(rec.Message.Content) == 0 {
		return
	}

	// Content may be a JSON string or an array of content blocks.
	var textOnly string
	if err := json.Unmarshal(rec.Message.Content, &textOnly); err == nil {
		if t := strings.TrimSpace(textOnly); t != "" {
			d.setLastSummary(taskID, t)
		}
		return
	}

	var blocks []claudeContentBlock
	if err := json.Unmarshal(rec.Message.Content, &blocks); err != nil {
		return
	}
	var textParts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				textParts = append(textParts, t)
			}
		case "tool_use":
			kind := "tool_call"
			msg := b.Name
			switch strings.ToLower(b.Name) {
			case "edit", "write", "multiedit", "notebookedit":
				kind = "file_edit"
				if fp, ok := b.Input["file_path"].(string); ok {
					msg = fmt.Sprintf("%s: %s", b.Name, fp)
				}
			case "bash":
				if cmd, ok := b.Input["command"].(string); ok {
					if len(cmd) > 120 {
						cmd = cmd[:120] + "..."
					}
					msg = fmt.Sprintf("Bash: %s", cmd)
				}
			case "read":
				if fp, ok := b.Input["file_path"].(string); ok {
					msg = fmt.Sprintf("Read: %s", fp)
				}
			}
			emit(TelemetryEvent{
				Kind:    kind,
				Message: msg,
				Meta:    map[string]any{"tool": b.Name},
			})
		}
	}
	if len(textParts) > 0 {
		d.setLastSummary(taskID, strings.Join(textParts, "\n"))
	}
}

func (d *ClaudeDriver) ExtractSummary(_ context.Context, opts SummaryOptions) string {
	if s := d.getAndClearState(opts.TaskID); s != "" {
		if len(s) > 2000 {
			return s[:2000] + "..."
		}
		return s
	}
	if opts.RingSnapshot != nil {
		if tail := TailCleanSummary(opts.RingSnapshot(), 15, 1500); tail != "" {
			return tail
		}
	}
	return fmt.Sprintf("claude exited with code %d", opts.ExitCode)
}
