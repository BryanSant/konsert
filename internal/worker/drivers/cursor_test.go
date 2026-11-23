package drivers

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
)

// Fixtures in testdata/cursor were captured from cursor-agent 2026.11.19-f005e9d and
// sanitized (paths rewritten):
//   - oneshot.transcript.jsonl / oneshot.pty: `cursor-agent --print --force --trust
//     --resume <uuid> -- "Create a file named ok.txt containing the word OK, then reply
//     with the single word DONE"`.
//   - interactive.transcript.jsonl / interactive.pty: the TUI driven through a PTY with
//     `--force --trust --resume=<uuid> -- <same prompt>`, then /exit and Ctrl+C.

func installCursorChat(t *testing.T, taskID, mode, fixture string) *CursorDriver {
	t.Helper()
	dataDir := t.TempDir()
	d := &CursorDriver{}
	d.BuildCommand(LaunchOptions{TaskID: taskID, Prompt: "x", Mode: mode, Env: []string{"CURSOR_DATA_DIR=" + dataDir}})
	id := d.chatFor(taskID)
	dir := filepath.Join(dataDir, "projects", "private-var-tmp-scratch", "agent-transcripts", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), readFixture(t, "cursor", fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCursorArgv(t *testing.T) {
	d := &CursorDriver{}
	full := d.BuildCommand(LaunchOptions{TaskID: "a", Prompt: "fix it", Model: "composer-2.5", Mode: protocol.TaskModeOneshot, PermissionMode: protocol.PermissionFullAccess})
	if want := []string{"--force", "--trust", "--model=composer-2.5", "--resume=" + d.chatFor("a"), "--print", "--output-format", "text", "--", "fix it"}; !slices.Equal(full.Args, want) {
		t.Fatalf("full oneshot argv = %q, want %q", full.Args, want)
	}
	sand := d.BuildCommand(LaunchOptions{TaskID: "b", Prompt: "fix it", Mode: protocol.TaskModeOneshot, PermissionMode: protocol.PermissionSandboxed})
	if want := []string{"--sandbox", "enabled", "--trust", "--resume=" + d.chatFor("b"), "--print", "--output-format", "text", "--", "fix it"}; !slices.Equal(sand.Args, want) {
		t.Fatalf("sandboxed oneshot argv = %q, want %q", sand.Args, want)
	}
	inter := d.BuildCommand(LaunchOptions{TaskID: "c", Prompt: "fix it", Mode: protocol.TaskModeInteractive, PermissionMode: protocol.PermissionSandboxed})
	if want := []string{"--sandbox", "enabled", "--resume=" + d.chatFor("c"), "--", "fix it"}; !slices.Equal(inter.Args, want) {
		t.Fatalf("sandboxed interactive argv = %q, want %q", inter.Args, want)
	}
	interFull := d.BuildCommand(LaunchOptions{TaskID: "d", Mode: protocol.TaskModeInteractive, PermissionMode: protocol.PermissionFullAccess})
	if want := []string{"--force", "--trust", "--resume=" + d.chatFor("d")}; !slices.Equal(interFull.Args, want) {
		t.Fatalf("empty interactive argv = %q, want %q", interFull.Args, want)
	}
	if d.chatFor("a") == "" || d.chatFor("a") == d.chatFor("b") {
		t.Fatal("expected distinct chat ids per launch")
	}
	_ = d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "a"})
	if d.chatFor("a") != "" {
		t.Fatal("task state not cleared after the task finished")
	}
}

func TestCursorOneshotFixture(t *testing.T) {
	d := installCursorChat(t, "one", protocol.TaskModeOneshot, "oneshot.transcript.jsonl")
	pty := strings.Split(string(readFixture(t, "cursor", "oneshot.pty")), "\n")
	events := collectEvents(t, d, WatchOptions{TaskID: "one", OutputLines: feedLines(pty...)})
	if want := []string{"file_edit", "milestone"}; !slices.Equal(kinds(events), want) {
		t.Fatalf("event kinds = %v, want %v: %+v", kinds(events), want, events)
	}
	if events[0].Message != "Write: /private/var/tmp/scratch/oneshot/ok.txt" || events[0].Meta["tool"] != "Write" {
		t.Fatalf("unexpected file_edit: %+v", events[0])
	}
	if events[1].Message != "Turn completed" {
		t.Fatalf("unexpected milestone: %+v", events[1])
	}
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "one"}); got != "DONE" {
		t.Fatalf("summary = %q, want DONE", got)
	}
}

func TestCursorInteractiveFixture(t *testing.T) {
	d := installCursorChat(t, "tui", protocol.TaskModeInteractive, "interactive.transcript.jsonl")
	pty := strings.Split(string(readFixture(t, "cursor", "interactive.pty")), "\n")
	events := collectEvents(t, d, WatchOptions{TaskID: "tui", OutputLines: feedLines(pty...)})
	if want := []string{"file_edit", "milestone", "waiting_input"}; !slices.Equal(kinds(events), want) {
		t.Fatalf("event kinds = %v, want %v: %+v", kinds(events), want, events)
	}
	ring := readFixture(t, "cursor", "interactive.pty")
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "tui", RingSnapshot: func() []byte { return ring }}); got != "DONE" {
		t.Fatalf("summary = %q, want DONE", got)
	}
}

func TestCursorSummaryFallbacks(t *testing.T) {
	d := &CursorDriver{}
	d.BuildCommand(LaunchOptions{TaskID: "h", Mode: protocol.TaskModeOneshot, Env: []string{"CURSOR_DATA_DIR=" + t.TempDir()}})
	ring := readFixture(t, "cursor", "oneshot.pty")
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "h", RingSnapshot: func() []byte { return ring }}); got != "DONE" {
		t.Fatalf("print-mode fallback summary = %q, want DONE", got)
	}
	d.BuildCommand(LaunchOptions{TaskID: "i", Mode: protocol.TaskModeInteractive, Env: []string{"CURSOR_DATA_DIR=" + t.TempDir()}})
	tui := readFixture(t, "cursor", "interactive.pty")
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "i", ExitCode: 1, RingSnapshot: func() []byte { return tui }}); got != "cursor-agent exited with code 1" {
		t.Fatalf("interactive fallback summary = %q", got)
	}
}

func TestCursorToolEvents(t *testing.T) {
	cases := []struct {
		name  string
		input map[string]any
		kind  string
		msg   string
	}{
		{"Shell", map[string]any{"command": "go test ./..."}, "tool_call", "Run: go test ./..."},
		{"StrReplace", map[string]any{"path": "a.go"}, "file_edit", "StrReplace: a.go"},
		{"Delete", map[string]any{"path": "b.go"}, "file_edit", "Delete: b.go"},
		{"Read", map[string]any{"path": "c.go"}, "tool_call", "Read: c.go"},
		{"Grep", map[string]any{"pattern": "x"}, "tool_call", "Grep"},
	}
	for _, c := range cases {
		ev := cursorToolEvent(c.name, c.input)
		if ev.Kind != c.kind || ev.Message != c.msg {
			t.Errorf("%s: got %+v, want %s %q", c.name, ev, c.kind, c.msg)
		}
	}
}

func TestCursorWatchIsolatedPerChat(t *testing.T) {
	dataDir := t.TempDir()
	d := &CursorDriver{}
	d.BuildCommand(LaunchOptions{TaskID: "mine", Prompt: "x", Env: []string{"CURSOR_DATA_DIR=" + dataDir}})
	write := func(id, text string) {
		dir := filepath.Join(dataDir, "projects", "some-dir", "agent-transcripts", id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		line := `{"role":"assistant","message":{"content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("00000000-0000-0000-0000-000000000000", "OTHER")
	write(d.chatFor("mine"), "mine")
	collectEvents(t, d, WatchOptions{TaskID: "mine", OutputLines: feedLines()})
	if got := d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "mine"}); got != "mine" {
		t.Fatalf("summary = %q, want only this chat's text", got)
	}
}

func TestCursorTrustPromptFromRing(t *testing.T) {
	d := &CursorDriver{}
	d.BuildCommand(LaunchOptions{TaskID: "trust", Mode: protocol.TaskModeInteractive, PermissionMode: protocol.PermissionSandboxed, Env: []string{"CURSOR_DATA_DIR=" + t.TempDir()}})
	ring := []byte("\x1b[2J\x1b[4;3H⚠ Workspace Trust Required\x1b[6;3HDo you trust the contents of this directory?")
	events := collectEvents(t, d, WatchOptions{TaskID: "trust", OutputLines: feedLines(), RingSnapshot: func() []byte { return ring }})
	if len(events) != 1 || events[0].Kind != "waiting_input" || !strings.Contains(events[0].Message, "trust") {
		t.Fatalf("expected one trust waiting_input, got %+v", events)
	}
}
