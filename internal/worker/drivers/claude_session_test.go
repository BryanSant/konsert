package drivers

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

func TestClaudeSessionIDPerLaunch(t *testing.T) {
	d := &ClaudeDriver{}
	a := d.BuildCommand(LaunchOptions{TaskID: "A", Prompt: "x", Mode: protocol.TaskModeOneshot})
	b := d.BuildCommand(LaunchOptions{TaskID: "B", Prompt: "x", Mode: protocol.TaskModeInteractive})
	sa, sb := d.sessionFor("A"), d.sessionFor("B")
	if sa == "" || sb == "" || sa == sb {
		t.Fatalf("expected distinct session ids, got %q %q", sa, sb)
	}
	for _, c := range []struct {
		args []string
		sid  string
	}{{a.Args, sa}, {b.Args, sb}} {
		i := slices.Index(c.args, "--session-id")
		if i < 0 || i+1 >= len(c.args) || c.args[i+1] != c.sid {
			t.Fatalf("missing --session-id %s in %q", c.sid, c.args)
		}
	}
	_ = d.ExtractSummary(context.Background(), SummaryOptions{TaskID: "A"})
	if d.sessionFor("A") != "" {
		t.Fatal("session mapping not cleared after the task finished")
	}
}

func TestClaudePollJSONLFileKeepsPartialLines(t *testing.T) {
	d := &ClaudeDriver{}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}` + "\n"
	half := len(line) / 2
	if err := os.WriteFile(path, []byte(line[:half]), 0o600); err != nil {
		t.Fatal(err)
	}
	var events []TelemetryEvent
	emit := func(ev TelemetryEvent) { events = append(events, ev) }

	off := d.pollJSONLFile("t", path, 0, emit)
	if off != 0 || len(events) != 0 {
		t.Fatalf("partial line must not be consumed: off=%d events=%v", off, events)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(line[half:])
	_ = f.Close()

	off = d.pollJSONLFile("t", path, off, emit)
	if off != int64(len(line)) || len(events) != 1 || events[0].Message != "Bash: ls" {
		t.Fatalf("completed line not processed on next poll: off=%d events=%+v", off, events)
	}
}

// Telemetry must come only from the task's own session file, not from another session
// (a concurrent task or the user's own claude) in the same project directory.
func TestClaudeWatchTelemetryIsolatedPerSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads USERPROFILE on Windows
	workdir := t.TempDir()
	d := &ClaudeDriver{}
	d.BuildCommand(LaunchOptions{TaskID: "mine", Prompt: "x"})
	sid := d.sessionFor("mine")

	projDir := filepath.Join(home, ".claude", "projects", encodeClaudeProjectDir(workdir))
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mine := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"mine"}]}}` + "\n"
	other := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"OTHER"}]},"x":1}` + "\n"
	if err := os.WriteFile(filepath.Join(projDir, "00000000-0000-0000-0000-000000000000.jsonl"), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}

	lines := make(chan string)
	close(lines)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.WatchTelemetry(ctx, WatchOptions{TaskID: "mine", Workdir: workdir, OutputLines: lines}, func(TelemetryEvent) {})
	if got := d.ExtractSummary(ctx, SummaryOptions{TaskID: "mine"}); got != "mine" {
		t.Fatalf("summary = %q, want only this session's text", got)
	}
}
