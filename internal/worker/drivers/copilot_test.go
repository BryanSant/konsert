package drivers

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// Fixtures in testdata/copilot were captured from copilot 1.0.114 and sanitized (hook
// records, the system prompt and request ids removed; paths rewritten):
//   - oneshot.events.jsonl / oneshot.pty: `copilot --allow-all --session-id <uuid>
//     --prompt="reply with the single word OK"`.
//   - interactive.events.jsonl / interactive.pty: the TUI driven through a PTY with
//     COPILOT_ALLOW_ALL=true, `--allow-all` and --interactive="Create a file named ok.txt
//     containing the word OK, then reply with the single word DONE", then /exit.

func readFixture(t *testing.T, agent, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", agent, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func feedLines(lines ...string) chan string {
	ch := make(chan string, len(lines))
	for _, l := range lines {
		ch <- l
	}
	close(ch)
	return ch
}

func collectEvents(t *testing.T, d AgentDriver, opts WatchOptions) []TelemetryEvent {
	t.Helper()
	var events []TelemetryEvent
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.WatchTelemetry(ctx, opts, func(ev TelemetryEvent) { events = append(events, ev) })
	return events
}

func installCopilotSession(t *testing.T, taskID, mode, fixture string) *CopilotDriver {
	t.Helper()
	home := t.TempDir()
	d := &CopilotDriver{}
	d.BuildCommand(LaunchOptions{TaskID: taskID, Prompt: "x", Mode: mode, Env: []string{"COPILOT_HOME=" + home}})
	dir := filepath.Join(home, "session-state", d.sessionFor(taskID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), readFixture(t, "copilot", fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCopilotArgv(t *testing.T) {
	d := &CopilotDriver{}
	full := d.BuildCommand(LaunchOptions{TaskID: "a", Prompt: "fix it", Model: "gpt-5.6-sol", Mode: protocol.TaskModeOneshot, PermissionMode: protocol.PermissionFullAccess})
	if want := []string{"--allow-all", "--model=gpt-5.6-sol", "--session-id=" + d.sessionFor("a"), "--prompt=fix it"}; !slices.Equal(full.Args, want) {
		t.Fatalf("full oneshot argv = %q, want %q", full.Args, want)
	}
	if !slices.Equal(full.Env, []string{"COPILOT_ALLOW_ALL=true"}) {
		t.Fatalf("full access env = %q", full.Env)
	}

	sand := d.BuildCommand(LaunchOptions{TaskID: "b", Prompt: "fix it", Mode: protocol.TaskModeOneshot, PermissionMode: protocol.PermissionSandboxed, Env: []string{"A=1"}})
	if want := []string{"--sandbox", "--allow-tool=write", "--allow-tool=shell", "--session-id=" + d.sessionFor("b"), "--prompt=fix it"}; !slices.Equal(sand.Args, want) {
		t.Fatalf("sandboxed oneshot argv = %q, want %q", sand.Args, want)
	}
	// An inherited COPILOT_ALLOW_ALL must be overridden (the last duplicate key wins).
	if !slices.Equal(sand.Env, []string{"A=1", "COPILOT_ALLOW_ALL=false"}) {
		t.Fatalf("sandboxed env = %q", sand.Env)
	}

	inter := d.BuildCommand(LaunchOptions{TaskID: "c", Prompt: "fix it", Mode: protocol.TaskModeInteractive, PermissionMode: protocol.PermissionSandboxed})
	if want := []string{"--sandbox", "--session-id=" + d.sessionFor("c"), "--interactive=fix it"}; !slices.Equal(inter.Args, want) {
		t.Fatalf("sandboxed interactive argv = %q, want %q", inter.Args, want)
	}
	interFull := d.BuildCommand(LaunchOptions{TaskID: "d", Mode: protocol.TaskModeInteractive, PermissionMode: protocol.PermissionFullAccess})
	if want := []string{"--allow-all", "--session-id=" + d.sessionFor("d")}; !slices.Equal(interFull.Args, want) {
		t.Fatalf("empty interactive argv = %q, want %q", interFull.Args, want)
	}
	if d.sessionFor("a") == "" || d.sessionFor("a") == d.sessionFor("b") {
		t.Fatal("expected distinct session ids per launch")
	}
	_ = d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "a"})
	if d.sessionFor("a") != "" {
		t.Fatal("task state not cleared after the task finished")
	}
}

func TestCopilotOneshotFixture(t *testing.T) {
	d := installCopilotSession(t, "one", protocol.TaskModeOneshot, "oneshot.events.jsonl")
	pty := strings.Split(string(readFixture(t, "copilot", "oneshot.pty")), "\n")
	events := collectEvents(t, d, WatchOptions{TaskID: "one", OutputLines: feedLines(pty...)})
	if want := []string{"usage", "milestone", "milestone"}; !slices.Equal(kinds(events), want) {
		t.Fatalf("event kinds = %v, want %v: %+v", kinds(events), want, events)
	}
	u := events[0]
	if u.Meta["provider"] != "copilot" || u.Meta["model"] != "mai-code-2-flash" || u.Meta["input_tokens"] != int64(19856) ||
		u.Meta["output_tokens"] != int64(5) || u.Meta["cached_input_tokens"] != int64(7680) || u.Meta["premium_requests"] != 1.0 {
		t.Fatalf("unexpected usage event: %+v", u)
	}
	if events[1].Message != "Turn completed" {
		t.Fatalf("unexpected turn milestone: %+v", events[1])
	}
	if events[2].Message != "Session ended: 0 files modified, 0.13 AI credits" {
		t.Fatalf("unexpected shutdown milestone: %+v", events[2])
	}
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "one"}); got != "OK" {
		t.Fatalf("summary = %q, want OK", got)
	}
}

func TestCopilotInteractiveFixture(t *testing.T) {
	d := installCopilotSession(t, "tui", protocol.TaskModeInteractive, "interactive.events.jsonl")
	pty := strings.Split(string(readFixture(t, "copilot", "interactive.pty")), "\n")
	events := collectEvents(t, d, WatchOptions{TaskID: "tui", OutputLines: feedLines(pty...)})
	// The first model call runs a tool, so its turn_end is not the end of the turn.
	if want := []string{"usage", "tool_call", "usage", "milestone", "waiting_input", "milestone"}; !slices.Equal(kinds(events), want) {
		t.Fatalf("event kinds = %v, want %v: %+v", kinds(events), want, events)
	}
	if events[1].Message != "Run: printf 'OK' > ok.txt" || events[1].Meta["tool"] != "bash" {
		t.Fatalf("unexpected tool event: %+v", events[1])
	}
	ring := readFixture(t, "copilot", "interactive.pty")
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "tui", RingSnapshot: func() []byte { return ring }}); got != "DONE" {
		t.Fatalf("summary = %q, want DONE", got)
	}
}

func TestCopilotSummaryFallbacks(t *testing.T) {
	d := &CopilotDriver{}
	d.BuildCommand(LaunchOptions{TaskID: "h", Mode: protocol.TaskModeOneshot, Env: []string{"COPILOT_HOME=" + t.TempDir()}})
	ring := readFixture(t, "copilot", "oneshot.pty")
	// The usage stats printed after the reply are dropped.
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "h", RingSnapshot: func() []byte { return ring }}); got != "OK" {
		t.Fatalf("prompt-mode fallback summary = %q, want OK", got)
	}
	d.BuildCommand(LaunchOptions{TaskID: "i", Mode: protocol.TaskModeInteractive, Env: []string{"COPILOT_HOME=" + t.TempDir()}})
	tui := readFixture(t, "copilot", "interactive.pty")
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "i", ExitCode: 3, RingSnapshot: func() []byte { return tui }}); got != "copilot exited with code 3" {
		t.Fatalf("interactive fallback summary = %q", got)
	}
}

func TestCopilotEvents(t *testing.T) {
	cases := []struct {
		line, kind, msg string
	}{
		{`{"type":"tool.execution_start","data":{"toolName":"view","arguments":{"path":"/w/a.go"}}}`, "tool_call", "Read: /w/a.go"},
		{`{"type":"tool.execution_start","data":{"toolName":"edit","arguments":{"path":"/w/a.go","old_str":"x","new_str":"y"}}}`, "file_edit", "edit: /w/a.go"},
		{`{"type":"tool.execution_start","data":{"toolName":"create","arguments":{"path":"/w/b.go","file_text":"x"}}}`, "file_edit", "create: /w/b.go"},
		{`{"type":"tool.execution_start","data":{"toolName":"ask_user","arguments":{"question":"Proceed?"}}}`, "waiting_input", "Proceed?"},
		{`{"type":"permission.requested","data":{"requestId":"r","permissionRequest":{"kind":"shell","intention":"Run go test"}}}`, "waiting_input", "copilot is asking for permission: Run go test"},
		{`{"type":"session.error","data":{"errorType":"quota","message":"You've hit your rate limit."}}`, "error", "You've hit your rate limit."},
	}
	for _, c := range cases {
		d := &CopilotDriver{}
		var got []TelemetryEvent
		d.parseEvent(&copilotTask{}, []byte(c.line), func(ev TelemetryEvent) { got = append(got, ev) })
		if len(got) != 1 || got[0].Kind != c.kind || got[0].Message != c.msg {
			t.Errorf("%s: got %+v, want %s %q", c.line, got, c.kind, c.msg)
		}
	}
}

// Telemetry comes only from the task's own session, never from another session under
// the same COPILOT_HOME.
func TestCopilotWatchIsolatedPerSession(t *testing.T) {
	home := t.TempDir()
	d := &CopilotDriver{}
	d.BuildCommand(LaunchOptions{TaskID: "mine", Prompt: "x", Env: []string{"COPILOT_HOME=" + home}})
	write := func(sid, text string) {
		dir := filepath.Join(home, "session-state", sid)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		line := `{"type":"assistant.message","data":{"content":"` + text + `","toolRequests":[]}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("00000000-0000-0000-0000-000000000000", "OTHER")
	write(d.sessionFor("mine"), "mine")
	collectEvents(t, d, WatchOptions{TaskID: "mine", OutputLines: feedLines()})
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "mine"}); got != "mine" {
		t.Fatalf("summary = %q, want only this session's text", got)
	}
}

// The folder-trust dialog blocks before events.jsonl exists and has no newline, so it is
// detected in the PTY ring and reported once.
func TestCopilotTrustPromptFromRing(t *testing.T) {
	d := &CopilotDriver{}
	d.BuildCommand(LaunchOptions{TaskID: "trust", Mode: protocol.TaskModeInteractive, PermissionMode: protocol.PermissionSandboxed, Env: []string{"COPILOT_HOME=" + t.TempDir()}})
	ring := []byte("\x1b[2J\x1b[3;3H│ Confirm folder trust\x1b[12;3H│ Do you trust the files in this folder?\x1b[14;3H│ ❯ 1. Yes")
	events := collectEvents(t, d, WatchOptions{TaskID: "trust", OutputLines: feedLines(), RingSnapshot: func() []byte { return ring }})
	if len(events) != 1 || events[0].Kind != "waiting_input" || !strings.Contains(events[0].Message, "trust") {
		t.Fatalf("expected one trust waiting_input, got %+v", events)
	}
}

// testdata/copilot/copilot-user-cache.json has the shape of copilot 1.0.114's account cache
// (ids replaced), as a request-billed plan: a chat bucket at 87.5% remaining, an unused
// completions bucket, no premium requests, plus an older second account to be ignored.
// (The captured account was token-billed; see TestReadCopilotQuotasPremium.)
func TestCopilotQuotaFromAccountCache(t *testing.T) {
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, "copilot-user-cache.json"), readFixture(t, "copilot", "copilot-user-cache.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	d := &CopilotDriver{}
	d.BuildCommand(LaunchOptions{TaskID: "q", Prompt: "x", Mode: protocol.TaskModeOneshot, Env: []string{"COPILOT_HOME=" + home, "COPILOT_CACHE_HOME=" + cache}})
	dir := filepath.Join(home, "session-state", d.sessionFor("q"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), readFixture(t, "copilot", "oneshot.events.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	events := collectEvents(t, d, WatchOptions{TaskID: "q", OutputLines: feedLines()})
	// One quota event at session start; the unchanged cache is not re-reported at shutdown.
	if want := []string{"quota", "usage", "milestone", "milestone"}; !slices.Equal(kinds(events), want) {
		t.Fatalf("event kinds = %v, want %v: %+v", kinds(events), want, events)
	}
	q := events[0]
	if q.Message != "Copilot Chat: 12.5% used" || q.Meta["provider"] != "copilot" || q.Meta["label"] != "Copilot Chat" ||
		q.Meta["window_label"] != "chat" || q.Meta["used_percent"] != 12.5 || q.Meta["reset_at"] != "2026-12-01T00:00:00.000Z" {
		t.Fatalf("unexpected quota event: %+v", q)
	}
}

func TestReadCopilotQuotasPremium(t *testing.T) {
	cache := t.TempDir()
	body := `{"copilotUserCache":{"v1:a":{"retrievedAt":"2026-11-23T00:00:00Z","response":{"quota_snapshots":{` +
		`"premium_interactions":{"entitlement":300,"percent_remaining":40,"unlimited":false,"has_quota":true},` +
		`"chat":{"entitlement":0,"percent_remaining":100,"unlimited":true}}}}}}`
	if err := os.WriteFile(filepath.Join(cache, "copilot-user-cache.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	qs := readCopilotQuotas(cache)
	if len(qs) != 1 || qs[0].Label != "Copilot Premium" || qs[0].WindowLabel != "premium" || qs[0].UsedPercent != 60 {
		t.Fatalf("unexpected premium quota: %+v", qs)
	}
	if readCopilotQuotas(t.TempDir()) != nil {
		t.Fatal("missing cache must yield no quotas")
	}

	// AI-credit (token-billed) accounts: request buckets do not track CLI usage, so none
	// are reported.
	body = `{"copilotUserCache":{"v1:a":{"retrievedAt":"2026-11-23T00:00:00Z","response":{"token_based_billing":true,"quota_snapshots":{` +
		`"chat":{"entitlement":200,"percent_remaining":100,"unlimited":false,"has_quota":true,"token_based_billing":true}}}}}}`
	if err := os.WriteFile(filepath.Join(cache, "copilot-user-cache.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if qs := readCopilotQuotas(cache); len(qs) != 0 {
		t.Fatalf("token-billed buckets must not be reported: %+v", qs)
	}
}

func TestAgentHomeDirOverrides(t *testing.T) {
	t.Setenv("COPILOT_HOME", "/from/daemon")
	if got := agentHomeDir([]string{"COPILOT_HOME=/from/task"}, "COPILOT_HOME", ".copilot"); got != "/from/task" {
		t.Fatalf("task env not preferred: %q", got)
	}
	if got := agentHomeDir(nil, "COPILOT_HOME", ".copilot"); got != "/from/daemon" {
		t.Fatalf("daemon env not used: %q", got)
	}
}
