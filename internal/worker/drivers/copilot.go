package drivers

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BryanSant/syre/internal/ids"
)

func init() {
	Register(&CopilotDriver{})
}

// CopilotDriver runs GitHub Copilot CLI (`copilot`), verified against copilot 1.0.114.
//
// Flags (from `copilot --help`, `copilot help permissions|sandbox|sessions` and live runs):
//   - full_access: `--allow-all` (= --allow-all-tools --allow-all-paths --allow-all-urls),
//     plus COPILOT_ALLOW_ALL=true in the task env. That exact value also trusts the working
//     directory for this run, so the TUI's "Confirm folder trust" dialog is skipped; it
//     saves nothing to ~/.copilot/config.json (verified).
//   - sandboxed:   `--sandbox` (OS command sandbox: Seatbelt on macOS, bubblewrap on Linux)
//     without allow-all, and COPILOT_ALLOW_ALL=false so a value inherited from the daemon's
//     environment cannot re-enable it. File access stays limited to the workdir and the temp
//     directory (path verification). Headless runs cannot ask for approval and deny anything
//     no rule allows, so oneshot tasks also get `--allow-tool=write --allow-tool=shell`:
//     edits stay path-verified and shell commands run inside the sandbox. Interactive tasks
//     ask in the TUI instead.
//   - model:       `--model=<id>` (`auto` lets Copilot pick).
//   - oneshot:     `--prompt=<prompt>` (non-interactive; prints the reply and a usage summary).
//   - interactive: the TUI, with the initial prompt in `--interactive=<prompt>`. Copilot has
//     no positional prompt (its positional is a subcommand), so the prompt is always
//     "="-joined to its flag and can never be parsed as a flag or subcommand.
//
// Every launch gets a fresh `--session-id=<uuid>` (sets the UUID of a new session), and
// telemetry tails only $COPILOT_HOME/session-state/<uuid>/events.jsonl, so concurrent tasks
// and the user's own copilot sessions never mix. That log yields tool calls, per-call token
// usage (session.usage_record), turn ends and a shutdown accounting snapshot; plan quota
// percentages come from the CLI's account cache (see readCopilotQuotas) and are reported as
// "quota" events when the session starts.
type CopilotDriver struct {
	mu    sync.Mutex
	tasks map[string]*copilotTask
}

type copilotTask struct {
	sessionID    string
	interactive  bool
	home         string
	cacheDir     string             // COPILOT_CACHE_HOME: holds the account/quota cache
	quotaSeen    map[string]float64 // last reported used% per quota bucket
	lastMessage  string
	finalPending bool // the last assistant message requested no tools: the turn is ending
}

func (d *CopilotDriver) Name() string { return "copilot" }

func (d *CopilotDriver) Detect() (string, bool) {
	return FindBinary("copilot")
}

func (d *CopilotDriver) BuildCommand(opts LaunchOptions) LaunchSpec {
	bin, ok := d.Detect()
	if !ok {
		bin = "copilot"
	}
	interactive := opts.Mode == "interactive"
	env := append([]string(nil), opts.Env...)
	var args []string
	mode := flagValue(opts.ApprovalMode)
	switch {
	case ApprovalModeFullAccess("copilot", mode):
		args = append(args, "--allow-all")
		env = append(env, "COPILOT_ALLOW_ALL=true")
	case mode == "interactive" || mode == "plan" || mode == "autopilot":
		args = append(args, "--mode="+mode)
		if !IsFullAccess(opts.PermissionMode) {
			args = append(args, "--sandbox")
		}
		env = append(env, "COPILOT_ALLOW_ALL=false")
	case IsFullAccess(opts.PermissionMode):
		args = append(args, "--allow-all")
		env = append(env, "COPILOT_ALLOW_ALL=true")
	default:
		args = append(args, "--sandbox")
		if !interactive {
			args = append(args, "--allow-tool=write", "--allow-tool=shell")
		}
		env = append(env, "COPILOT_ALLOW_ALL=false")
	}
	if m := strings.TrimSpace(opts.Model); m != "" {
		args = append(args, "--model="+m)
	}
	if e := flagValue(opts.Effort); e != "" {
		args = append(args, "--reasoning-effort="+e)
	}
	args = append(args, "--session-id="+d.newTask(opts))
	if interactive {
		if strings.TrimSpace(opts.Prompt) != "" {
			args = append(args, "--interactive="+opts.Prompt)
		}
	} else {
		args = append(args, "--prompt="+opts.Prompt)
	}
	return LaunchSpec{Binary: bin, Args: args, Env: env}
}

func (d *CopilotDriver) newTask(opts LaunchOptions) string {
	t := &copilotTask{
		sessionID:   ids.UUID(),
		interactive: opts.Mode == "interactive",
		home:        agentHomeDir(opts.Env, "COPILOT_HOME", ".copilot"),
		cacheDir:    copilotCacheDir(opts.Env),
	}
	if opts.TaskID != "" {
		d.mu.Lock()
		if d.tasks == nil {
			d.tasks = make(map[string]*copilotTask)
		}
		d.tasks[opts.TaskID] = t
		d.mu.Unlock()
	}
	return t.sessionID
}

func (d *CopilotDriver) task(taskID string) *copilotTask {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tasks[taskID]
}

// sessionFor returns the copilot session UUID assigned to taskID ("" if none).
func (d *CopilotDriver) sessionFor(taskID string) string {
	if t := d.task(taskID); t != nil {
		return t.sessionID
	}
	return ""
}

// copilotTrustPrompt is the TUI's folder trust dialog, shown before the first prompt runs
// in an untrusted folder (sandboxed interactive tasks; full access trusts the folder).
const copilotTrustPrompt = "Do you trust the files in this folder?"

func (d *CopilotDriver) WatchTelemetry(ctx context.Context, opts WatchOptions, emit func(TelemetryEvent)) {
	t := d.task(opts.TaskID)
	var (
		offset        int64
		trustPrompted bool
	)
	poll := func() {
		if t == nil || t.home == "" {
			return // no session of our own to tail; never read other sessions' logs
		}
		path := filepath.Join(t.home, "session-state", t.sessionID, "events.jsonl")
		if _, err := os.Stat(path); err != nil {
			// The trust dialog blocks before events.jsonl is created, and is drawn with
			// cursor addressing, so it is found in the PTY ring.
			if t.interactive && !trustPrompted && ringContains(opts.RingSnapshot, copilotTrustPrompt) {
				trustPrompted = true
				emit(TelemetryEvent{Kind: "waiting_input", Message: copilotTrustPrompt + " (choose Yes to proceed)"})
			}
			return
		}
		offset = tailJSONL(path, offset, func(line []byte) { d.parseEvent(t, line, emit) })
	}
	watchLoop(ctx, opts.OutputLines, poll, func(line string) {
		if ev, ok := scanCLIOutput(line); ok {
			emit(ev)
		}
	})
}

// copilotEvent is one events.jsonl record: {"type":..,"data":{..},"id":..,"timestamp":..}.
type copilotEvent struct {
	Type string         `json:"type"`
	Data jsontext.Value `json:"data"`
}

type copilotUsage struct {
	Model           string  `json:"model"`
	InputTokens     int64   `json:"inputTokens"` // includes cacheReadTokens
	OutputTokens    int64   `json:"outputTokens"`
	CacheReadTokens int64   `json:"cacheReadTokens"`
	Cost            float64 `json:"cost"` // premium-request multiplier units
	CopilotUsage    struct {
		TotalNanoAiu int64 `json:"totalNanoAiu"` // 1e9 nano AIU = 1 AI credit
	} `json:"copilotUsage"`
}

func (d *CopilotDriver) parseEvent(t *copilotTask, raw []byte, emit func(TelemetryEvent)) {
	var ev copilotEvent
	if err := unmarshal(raw, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "session.start":
		// The CLI refreshes its account cache (with the plan quota snapshots) while
		// starting, before it writes session.start.
		d.emitQuotas(t, emit)
	case "user.message":
		d.mu.Lock()
		t.finalPending = false
		d.mu.Unlock()
	case "assistant.message":
		var m struct {
			Content      string           `json:"content"`
			ToolRequests []jsontext.Value `json:"toolRequests"`
		}
		if unmarshal(ev.Data, &m) != nil {
			return
		}
		d.mu.Lock()
		if s := strings.TrimSpace(m.Content); s != "" {
			t.lastMessage = s
		}
		t.finalPending = len(m.ToolRequests) == 0
		d.mu.Unlock()
	case "tool.execution_start":
		var m struct {
			ToolName  string         `json:"toolName"`
			Arguments map[string]any `json:"arguments"`
		}
		if unmarshal(ev.Data, &m) == nil && m.ToolName != "" {
			emit(copilotToolEvent(m.ToolName, m.Arguments))
		}
	case "session.usage_record":
		var m struct {
			Usage copilotUsage `json:"usage"`
		}
		if unmarshal(ev.Data, &m) == nil && (m.Usage.InputTokens > 0 || m.Usage.OutputTokens > 0) {
			emit(copilotUsageEvent(m.Usage))
		}
	case "permission.requested":
		var m struct {
			PermissionRequest map[string]any `json:"permissionRequest"`
		}
		_ = unmarshal(ev.Data, &m)
		msg := "copilot is asking for permission"
		if s := firstString(m.PermissionRequest, "intention", "fullCommandText", "kind"); s != "" {
			msg += ": " + truncateRunes(s, 200)
		}
		emit(TelemetryEvent{Kind: "waiting_input", Message: msg})
	case "session.error":
		var m struct {
			Message string `json:"message"`
		}
		if unmarshal(ev.Data, &m) == nil && m.Message != "" {
			emit(TelemetryEvent{Kind: "error", Message: truncateRunes(m.Message, 300)})
		}
	case "assistant.turn_end":
		// Copilot ends an assistant turn after every model call; the user-visible turn is
		// over only when the last message requested no tools.
		d.mu.Lock()
		done := t.finalPending
		t.finalPending = false
		d.mu.Unlock()
		if !done {
			return
		}
		emit(TelemetryEvent{Kind: "milestone", Message: "Turn completed"})
		if t.interactive {
			emit(TelemetryEvent{Kind: "waiting_input", Message: "copilot is waiting for your next prompt"})
		}
	case "session.shutdown":
		var m struct {
			AccountingSnapshot struct {
				TotalPremiumRequestCost float64 `json:"totalPremiumRequestCost"`
				TotalNanoAiu            float64 `json:"totalNanoAiu"`
				CodeChanges             struct {
					FilesModifiedCount int `json:"filesModifiedCount"`
				} `json:"codeChanges"`
			} `json:"accountingSnapshot"`
		}
		if unmarshal(ev.Data, &m) != nil {
			return
		}
		d.emitQuotas(t, emit) // in case the cache was refreshed during the session
		a := m.AccountingSnapshot
		emit(TelemetryEvent{
			Kind:    "milestone",
			Message: fmt.Sprintf("Session ended: %d files modified, %.2f AI credits", a.CodeChanges.FilesModifiedCount, a.TotalNanoAiu/1e9),
			Meta: map[string]any{
				"premium_requests": a.TotalPremiumRequestCost,
				"ai_credits":       a.TotalNanoAiu / 1e9,
				"files_modified":   a.CodeChanges.FilesModifiedCount,
			},
		})
	}
}

// emitQuotas reports changed plan quota percentages as "quota" events, which the hub
// records as subscription quotas.
func (d *CopilotDriver) emitQuotas(t *copilotTask, emit func(TelemetryEvent)) {
	if t.quotaSeen == nil {
		t.quotaSeen = make(map[string]float64)
	}
	for _, ev := range copilotQuotaEvents(t.cacheDir, t.quotaSeen) {
		emit(ev)
	}
}

func copilotUsageEvent(u copilotUsage) TelemetryEvent {
	label := u.Model
	if label == "" {
		label = "unknown model"
	}
	meta := map[string]any{
		"provider":      "copilot",
		"model":         u.Model,
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
	}
	if u.CacheReadTokens > 0 {
		meta["cached_input_tokens"] = u.CacheReadTokens
	}
	if u.Cost > 0 {
		meta["premium_requests"] = u.Cost
	}
	if u.CopilotUsage.TotalNanoAiu > 0 {
		meta["ai_credits"] = float64(u.CopilotUsage.TotalNanoAiu) / 1e9
	}
	return TelemetryEvent{
		Kind:    "usage",
		Message: fmt.Sprintf("Copilot usage (%s): +%d in / +%d out", label, u.InputTokens, u.OutputTokens),
		Meta:    meta,
	}
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// copilotToolEvent maps a tool.execution_start record to a telemetry event. Built-in tool
// names include bash/powershell, view, create, edit, str_replace, apply_patch, ask_user.
func copilotToolEvent(name string, args map[string]any) TelemetryEvent {
	ev := TelemetryEvent{Kind: "tool_call", Message: name, Meta: map[string]any{"tool": name}}
	lower := strings.ToLower(name)
	switch {
	case lower == "bash" || lower == "powershell" || lower == "shell":
		if cmd := firstString(args, "command"); cmd != "" {
			ev.Message = "Run: " + truncateRunes(cmd, 120)
		}
	case lower == "view" || lower == "read" || lower == "read_file":
		if fp := firstString(args, "path", "file_path"); fp != "" {
			ev.Message = "Read: " + fp
		}
	case lower == "create" || lower == "edit" || lower == "write" || lower == "str_replace" ||
		lower == "str_replace_editor" || lower == "apply_patch" || lower == "insert":
		ev.Kind = "file_edit"
		if fp := firstString(args, "path", "file_path"); fp != "" {
			ev.Message = name + ": " + fp
		}
	case lower == "ask_user":
		ev.Kind = "waiting_input"
		ev.Message = "copilot is asking a question"
		if q := firstString(args, "question", "message"); q != "" {
			ev.Message = truncateRunes(q, 300)
		}
	}
	return ev
}

// copilotStatsPrefixes start the usage summary copilot prints after a -p run; they are
// dropped from the PTY-tail summary fallback.
var copilotStatsPrefixes = []string{"Changes ", "AI Credits ", "Requests ", "Tokens ", "Resume ", "Duration "}

func (d *CopilotDriver) ExtractSummary(_ context.Context, opts SummaryOptions) string {
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
	// Prompt mode prints the reply then the usage summary, so the PTY tail minus the stats
	// is a usable fallback. The TUI repaints with cursor addressing; skip it.
	if opts.RingSnapshot != nil && (t == nil || !t.interactive) {
		var kept []string
		for _, line := range strings.Split(TailCleanSummary(opts.RingSnapshot(), 25, 3000), "\n") {
			stats := false
			for _, p := range copilotStatsPrefixes {
				if strings.HasPrefix(line, p) {
					stats = true
					break
				}
			}
			if !stats && line != "" {
				kept = append(kept, line)
			}
		}
		if len(kept) > 15 {
			kept = kept[len(kept)-15:]
		}
		if out := strings.Join(kept, "\n"); out != "" {
			return out
		}
	}
	return fmt.Sprintf("copilot exited with code %d", opts.ExitCode)
}
