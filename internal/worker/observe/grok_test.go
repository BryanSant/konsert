package observe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/shells"
)

func grokSummary(evs []protocol.AgentChatEvent) []string {
	var got []string
	for _, ev := range evs {
		s := ev.Kind
		switch ev.Kind {
		case protocol.EventStatus:
			s += ":" + ev.Status
		case protocol.EventUserMessage, protocol.EventMessage, protocol.EventTitle:
			s += ":" + firstWords(ev.Text, 6)
		case protocol.EventToolCall:
			s += ":" + ev.ToolCall.Status + ":" + ev.ToolCall.Kind + ":" + firstWords(ev.ToolCall.Title, 4)
		case protocol.EventTurnEnd:
			s += ":" + ev.StopReason
		}
		got = append(got, s)
	}
	return got
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}

func TestGrokEventsFromUpdates(t *testing.T) {
	raw, err := os.ReadFile("testdata/grok-updates.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	evs := GrokEvents("s1", raw)
	want := []string{
		"status:working",
		"user_message:Run the curl|bash command to install",
		"message:I'll look up the current official",
		"tool_call:running:search:Web search:",
		"tool_call:completed:search:Web search:",
		"message:The official installer is `https://claude.ai/install.sh`. You",
		"tool_call:running::run_terminal_command",
		"tool_call:running:execute:Execute `curl -fsSL https://claude.ai/install.sh",
		"tool_call:completed:execute:Execute `curl -fsSL https://claude.ai/install.sh",
		"message:Claude Code **2.1.317** is installed at",
		"turn_end:end_turn",
		"usage",
		"status:idle",
	}
	got := grokSummary(evs)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, ev := range evs {
		if ev.AgentSessionID != "s1" || ev.At.Year() < 2026 {
			t.Fatalf("event without session or time: %+v", ev)
		}
		if ev.Kind == protocol.EventToolCall && ev.ToolCall.Status == "completed" && ev.ToolCall.Kind == "execute" &&
			!strings.Contains(ev.ToolCall.Output, "Claude Code") {
			t.Fatalf("command output: %q", ev.ToolCall.Output)
		}
		if ev.Kind == protocol.EventUsage && (ev.Usage.CacheReadTokens != 49024 || ev.Usage.InputTokens != 77199-49024) {
			t.Fatalf("usage: %+v", ev.Usage)
		}
	}
}

func TestGrokSessionEndIsDone(t *testing.T) {
	lines := `{"timestamp_us":1795836699326103,"method":"session/update","params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"hi"}}}}
{"timestamp_us":1795836700228967,"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"turn_completed","stop_reason":"error"}}}
{"timestamp_us":1795836701092533,"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"session_end"}}}
`
	got := grokSummary(GrokEvents("s", []byte(lines)))
	want := "status:working user_message:hi turn_end:error status:idle status:done"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %q", strings.Join(got, " "))
	}
}

func TestGrokDiscoverAndReadAcrossPolls(t *testing.T) {
	home := t.TempDir()
	group := filepath.Join(home, "sessions", "%2Fwork%2Fdemo")
	dir := filepath.Join(group, "01a1a399-a50b-7571-991b-0254cde1b95a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile("testdata/grok-updates.jsonl")
	half := len(raw) / 2
	half = strings.LastIndexByte(string(raw[:half]), '\n') + 1
	path := filepath.Join(dir, "updates.jsonl")
	os.WriteFile(path, raw[:half], 0o644)
	os.WriteFile(filepath.Join(dir, "summary.json"), []byte(`{"info":{"cwd":"/work/demo"},"session_summary":"Install claude","current_model_id":"grok-4.7"}`), 0o644)
	procs := &argvProcs{fakeProcs: fakeProcs{procs: []shells.Proc{{PID: 9, PPID: 1, Name: "grok"}}}}
	src := &GrokSource{Dirs: func() []GrokDir { return []GrokDir{{Dir: home}} }, Procs: procs,
		OpenFiles: func(pid int) []string { return []string{filepath.Join(dir, "updates.jsonl.lock")} }, ListEvery: time.Hour, ProcTTL: -1}
	got, _ := src.Discover(context.Background(), time.Now().Add(-time.Hour))
	if len(got) != 1 || got[0].Cwd != "/work/demo" || got[0].Model != "grok-4.7" || !got[0].Alive || got[0].PID != 9 {
		t.Fatalf("discover: %+v", got)
	}
	evs1, next, err := src.Read(context.Background(), got[0], 0)
	if err != nil || next != int64(half) {
		t.Fatalf("read 1: %v %d", err, next)
	}
	os.WriteFile(path, raw, 0o644)
	got, _ = src.Discover(context.Background(), time.Now().Add(-time.Hour))
	evs2, _, _ := src.Read(context.Background(), got[0], next)
	all := grokSummary(append(evs1, evs2...))
	// Read in two polls, the same events as in one (the replay continues), titled.
	whole := append([]string{"title:Install claude"}, grokSummary(GrokEvents("x", raw))...)
	if strings.Join(all, "\n") != strings.Join(whole, "\n") {
		t.Fatalf("two reads:\n%s\nwant:\n%s", strings.Join(all, "\n"), strings.Join(whole, "\n"))
	}
}
