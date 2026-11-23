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

// Fixtures in testdata/grok were captured from grok 1.0.92 and sanitized:
//   - oneshot.updates.jsonl / oneshot.pty: `grok --always-approve --session-id <uuid>
//     --single="reply with the single word OK"`.
//   - interactive.updates.jsonl / interactive.pty: the TUI driven through a PTY with
//     "Create a file named ok.txt containing the word OK, then reply with the single word DONE".

func readGrokFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "grok", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// installGrokSession writes fixture as the updates.jsonl of the task's session under a
// temporary GROK_HOME (passed via the task env) and returns the driver.
func installGrokSession(t *testing.T, taskID, mode, fixture string) *GrokDriver {
	t.Helper()
	grokHome := t.TempDir()
	d := &GrokDriver{}
	d.BuildCommand(LaunchOptions{TaskID: taskID, Prompt: "x", Mode: mode, Env: []string{"GROK_HOME=" + grokHome}})
	dir := filepath.Join(grokHome, "sessions", "%2Fprivate%2Fvar%2Ftmp%2Fscratch", d.sessionFor(taskID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "updates.jsonl"), readGrokFixture(t, fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

func runGrokWatch(t *testing.T, d *GrokDriver, taskID string, ptyLines ...string) []TelemetryEvent {
	t.Helper()
	lines := make(chan string, len(ptyLines))
	for _, l := range ptyLines {
		lines <- l
	}
	close(lines)
	var events []TelemetryEvent
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.WatchTelemetry(ctx, WatchOptions{TaskID: taskID, OutputLines: lines}, func(ev TelemetryEvent) {
		events = append(events, ev)
	})
	return events
}

func kinds(events []TelemetryEvent) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func TestGrokArgv(t *testing.T) {
	d := &GrokDriver{}
	full := d.BuildCommand(LaunchOptions{TaskID: "a", Prompt: "fix it", Model: "grok-4.5", Mode: protocol.TaskModeOneshot, PermissionMode: protocol.PermissionFullAccess})
	if want := []string{"--always-approve", "--model", "grok-4.5", "--session-id", d.sessionFor("a"), "--single=fix it"}; !slices.Equal(full.Args, want) {
		t.Fatalf("oneshot argv = %q, want %q", full.Args, want)
	}
	inter := d.BuildCommand(LaunchOptions{TaskID: "b", Prompt: "fix it", Mode: protocol.TaskModeInteractive, PermissionMode: protocol.PermissionSandboxed})
	if want := []string{"--sandbox", "workspace", "--session-id", d.sessionFor("b"), "--", "fix it"}; !slices.Equal(inter.Args, want) {
		t.Fatalf("interactive argv = %q, want %q", inter.Args, want)
	}
	empty := d.BuildCommand(LaunchOptions{TaskID: "c", Mode: protocol.TaskModeInteractive})
	if slices.Contains(empty.Args, "--") {
		t.Fatalf("empty interactive prompt must not add --: %q", empty.Args)
	}
	if d.sessionFor("a") == "" || d.sessionFor("a") == d.sessionFor("b") {
		t.Fatalf("expected distinct session ids per launch")
	}
	_ = d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "a"})
	if d.sessionFor("a") != "" {
		t.Fatal("task state not cleared after the task finished")
	}
}

func TestGrokOneshotFixture(t *testing.T) {
	d := installGrokSession(t, "one", protocol.TaskModeOneshot, "oneshot.updates.jsonl")
	events := runGrokWatch(t, d, "one", strings.Split(string(readGrokFixture(t, "oneshot.pty")), "\n")...)
	if want := []string{"usage", "milestone"}; !slices.Equal(kinds(events), want) {
		t.Fatalf("event kinds = %v, want %v: %+v", kinds(events), want, events)
	}
	u := events[0]
	if u.Meta["model"] != "grok-4.5-build" || u.Meta["input_tokens"] != int64(18159) || u.Meta["output_tokens"] != int64(23) || u.Meta["provider"] != "grok" {
		t.Fatalf("unexpected usage event: %+v", u)
	}
	if !strings.HasPrefix(events[1].Message, "Turn completed in 3.8s") {
		t.Fatalf("unexpected milestone: %+v", events[1])
	}
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "one"}); got != "OK" {
		t.Fatalf("summary = %q, want OK", got)
	}
}

func TestGrokInteractiveFixture(t *testing.T) {
	d := installGrokSession(t, "tui", protocol.TaskModeInteractive, "interactive.updates.jsonl")
	// The TUI repaints with cursor addressing, so its PTY output yields no structured events.
	events := runGrokWatch(t, d, "tui", strings.Split(string(readGrokFixture(t, "interactive.pty")), "\n")...)
	if want := []string{"file_edit", "usage", "milestone", "waiting_input"}; !slices.Equal(kinds(events), want) {
		t.Fatalf("event kinds = %v, want %v: %+v", kinds(events), want, events)
	}
	if events[0].Message != "write: /private/var/tmp/scratch/interactive/ok.txt" || events[0].Meta["tool"] != "write" {
		t.Fatalf("unexpected file_edit: %+v", events[0])
	}
	if events[1].Meta["input_tokens"] != int64(36587) || events[1].Meta["output_tokens"] != int64(73) {
		t.Fatalf("unexpected usage: %+v", events[1])
	}
	// The ring holds TUI paint, which must not leak into the summary.
	ring := readGrokFixture(t, "interactive.pty")
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "tui", RingSnapshot: func() []byte { return ring }}); got != "DONE" {
		t.Fatalf("summary = %q, want DONE", got)
	}
}

func TestGrokSummaryFallbacks(t *testing.T) {
	d := &GrokDriver{}
	d.BuildCommand(LaunchOptions{TaskID: "h", Mode: protocol.TaskModeOneshot, Env: []string{"GROK_HOME=" + t.TempDir()}})
	ring := readGrokFixture(t, "oneshot.pty")
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "h", RingSnapshot: func() []byte { return ring }}); got != "OK" {
		t.Fatalf("headless fallback summary = %q, want OK", got)
	}
	d.BuildCommand(LaunchOptions{TaskID: "i", Mode: protocol.TaskModeInteractive, Env: []string{"GROK_HOME=" + t.TempDir()}})
	tui := readGrokFixture(t, "interactive.pty")
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "i", ExitCode: 2, RingSnapshot: func() []byte { return tui }}); got != "grok exited with code 2" {
		t.Fatalf("interactive fallback summary = %q", got)
	}
}

func TestGrokToolEvents(t *testing.T) {
	cases := []struct {
		line, kind, msg string
	}{
		{`{"params":{"update":{"sessionUpdate":"tool_call","title":"run_terminal_command","rawInput":{"command":"go test ./..."},"_meta":{"x.ai/tool":{"name":"run_terminal_command","kind":"execute"}}}}}`, "tool_call", "Run: go test ./..."},
		{`{"params":{"update":{"sessionUpdate":"tool_call","title":"search_replace","rawInput":{"file_path":"a.go","old_string":"x","new_string":"y"},"_meta":{"x.ai/tool":{"name":"search_replace","kind":"edit"}}}}}`, "file_edit", "search_replace: a.go"},
		{`{"params":{"update":{"sessionUpdate":"tool_call","title":"read_file","rawInput":{"target_file":"b.go"},"_meta":{"x.ai/tool":{"name":"read_file","kind":"read"}}}}}`, "tool_call", "Read: b.go"},
		{`{"params":{"update":{"sessionUpdate":"tool_call","title":"Web search:","kind":"search","status":"in_progress","rawInput":{"variant":"WebSearch","backend":true}}}}`, "tool_call", "Web search"},
		{`{"params":{"update":{"sessionUpdate":"tool_call","title":"ask_user_question","rawInput":{"questions":[{"question":"Proceed?"}]},"_meta":{"x.ai/tool":{"name":"ask_user_question","kind":"ask_user"}}}}}`, "waiting_input", "Proceed?"},
		{`{"params":{"update":{"sessionUpdate":"plan","entries":[{"content":"a","status":"completed"},{"content":"b","status":"in_progress"},{"content":"c","status":"pending"}]}}}`, "milestone", "Plan: 1/3 done (now: b)"},
	}
	for _, c := range cases {
		d := &GrokDriver{}
		var got []TelemetryEvent
		d.parseUpdate(&grokTask{}, []byte(c.line), func(ev TelemetryEvent) { got = append(got, ev) })
		if len(got) != 1 || got[0].Kind != c.kind || got[0].Message != c.msg {
			t.Errorf("%s: got %+v, want %s %q", c.line, got, c.kind, c.msg)
		}
	}
}

// The summary is the last assistant message segment: text before a tool call is replaced
// by the text after it, and consecutive chunks of one segment are concatenated.
func TestGrokSummaryLastSegment(t *testing.T) {
	d := &GrokDriver{}
	task := &grokTask{}
	for _, l := range []string{
		`{"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Let me look."}}}}`,
		`{"params":{"update":{"sessionUpdate":"tool_call","title":"list_dir","_meta":{"x.ai/tool":{"name":"list_dir","kind":"read"}}}}}`,
		`{"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"All "}}}}`,
		`{"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done."}}}}`,
		`{"params":{"update":{"sessionUpdate":"turn_completed","stop_reason":"max_turns","usage":{"inputTokens":5,"outputTokens":1}}}}`,
	} {
		d.parseUpdate(task, []byte(l), func(TelemetryEvent) {})
	}
	if task.lastMessage != "All done." {
		t.Fatalf("lastMessage = %q", task.lastMessage)
	}
}

func TestGrokScanOutput(t *testing.T) {
	if ev, ok := grokScanOutput("error: unexpected argument '--yes' found"); !ok || ev.Kind != "error" {
		t.Fatalf("CLI error not detected: %+v %v", ev, ok)
	}
	if ev, ok := grokScanOutput("\x1b[1mOverwrite file? [y/N]\x1b[0m"); !ok || ev.Kind != "waiting_input" {
		t.Fatalf("y/n prompt not detected: %+v %v", ev, ok)
	}
	if _, ok := grokScanOutput("Edit the config and Run the tests"); ok {
		t.Fatal("ordinary output must not produce events")
	}
}

func TestGrokPollUpdatesKeepsPartialLines(t *testing.T) {
	d := &GrokDriver{}
	task := &grokTask{}
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	line := `{"params":{"update":{"sessionUpdate":"tool_call","title":"run_terminal_command","rawInput":{"command":"ls"},"_meta":{"x.ai/tool":{"name":"run_terminal_command","kind":"execute"}}}}}` + "\n"
	half := len(line) / 2
	if err := os.WriteFile(path, []byte(line[:half]), 0o600); err != nil {
		t.Fatal(err)
	}
	var events []TelemetryEvent
	emit := func(ev TelemetryEvent) { events = append(events, ev) }
	off := d.pollUpdates(task, path, 0, emit)
	if off != 0 || len(events) != 0 {
		t.Fatalf("partial line must not be consumed: off=%d events=%v", off, events)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(line[half:])
	_ = f.Close()
	off = d.pollUpdates(task, path, off, emit)
	if off != int64(len(line)) || len(events) != 1 || events[0].Message != "Run: ls" {
		t.Fatalf("completed line not processed on next poll: off=%d events=%+v", off, events)
	}
}

// Telemetry must come only from the task's own session, never from another session
// (a concurrent task or the user's own grok) under the same GROK_HOME.
func TestGrokWatchTelemetryIsolatedPerSession(t *testing.T) {
	grokHome := t.TempDir()
	d := &GrokDriver{}
	d.BuildCommand(LaunchOptions{TaskID: "mine", Prompt: "x", Env: []string{"GROK_HOME=" + grokHome}})
	cwdDir := filepath.Join(grokHome, "sessions", "%2Fsome%2Fdir")
	write := func(sid, text string) {
		dir := filepath.Join(cwdDir, sid)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		line := `{"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + text + `"}}}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, "updates.jsonl"), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("00000000-0000-0000-0000-000000000000", "OTHER")
	write(d.sessionFor("mine"), "mine")
	runGrokWatch(t, d, "mine")
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "mine"}); got != "mine" {
		t.Fatalf("summary = %q, want only this session's text", got)
	}
}

func TestGrokHomeDir(t *testing.T) {
	t.Setenv("GROK_HOME", "/from/daemon")
	if got := grokHomeDir([]string{"A=1", "GROK_HOME=/from/task"}); got != "/from/task" {
		t.Fatalf("task env GROK_HOME not preferred: %q", got)
	}
	if got := grokHomeDir(nil); got != "/from/daemon" {
		t.Fatalf("daemon GROK_HOME not used: %q", got)
	}
}

// The TUI's folder-trust dialog blocks before any session log exists and has no newline,
// so it is detected in the PTY ring and reported once as waiting_input.
func TestGrokTrustPromptFromRing(t *testing.T) {
	d := &GrokDriver{}
	d.BuildCommand(LaunchOptions{TaskID: "trust", Mode: protocol.TaskModeInteractive, Env: []string{"GROK_HOME=" + t.TempDir()}})
	ring := readGrokFixture(t, "interactive.pty")
	lines := make(chan string)
	close(lines)
	var events []TelemetryEvent
	d.WatchTelemetry(context.Background(), WatchOptions{TaskID: "trust", OutputLines: lines, RingSnapshot: func() []byte { return ring }}, func(ev TelemetryEvent) {
		events = append(events, ev)
	})
	if len(events) != 1 || events[0].Kind != "waiting_input" || !strings.Contains(events[0].Message, "trust") {
		t.Fatalf("expected one trust waiting_input, got %+v", events)
	}

	// Headless runs never show the dialog, so their rings are not scanned.
	d.BuildCommand(LaunchOptions{TaskID: "headless", Mode: protocol.TaskModeOneshot, Env: []string{"GROK_HOME=" + t.TempDir()}})
	events = nil
	d.WatchTelemetry(context.Background(), WatchOptions{TaskID: "headless", OutputLines: lines, RingSnapshot: func() []byte { return ring }}, func(ev TelemetryEvent) {
		events = append(events, ev)
	})
	if len(events) != 0 {
		t.Fatalf("oneshot task must not scan the ring: %+v", events)
	}
}
