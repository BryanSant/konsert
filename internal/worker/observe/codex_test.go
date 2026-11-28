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

func (f *fakeProcs) Argv(int) []string { return nil }

// argvProcs adds command lines to fakeProcs.
type argvProcs struct {
	fakeProcs
	argv map[int][]string
}

func (a *argvProcs) Argv(pid int) []string { return a.argv[pid] }

func TestCodexEventsFromRollout(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex-rollout.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	evs := CodexEvents("sess-1", raw)
	var got []string
	for _, ev := range evs {
		if ev.AgentSessionID != "sess-1" || ev.At.IsZero() {
			t.Fatalf("event without session or time: %+v", ev)
		}
		s := ev.Kind
		switch ev.Kind {
		case protocol.EventStatus:
			s += ":" + ev.Status
		case protocol.EventUserMessage, protocol.EventMessage:
			s += ":" + ev.Text
		case protocol.EventToolCall:
			s += ":" + ev.ToolCall.Status + ":" + ev.ToolCall.Kind + ":" + ev.ToolCall.Title
		case protocol.EventTurnEnd:
			s += ":" + ev.StopReason
		case protocol.EventUsage:
			s += ":" + ev.MessageID[:5]
		}
		got = append(got, s)
	}
	want := []string{
		"status:working",
		"user_message:Run the shell command: sleep 8; echo syre-observe-test. Then reply with only its output.",
		"message:I’ll run the command.",
		"tool_call:running:execute:Run: sleep 8; echo syre-observe-test",
		"usage:resp_",
		"tool_call:completed:execute:Run: sleep 8; echo syre-observe-test",
		"message:syre-observe-test",
		"usage:resp_",
		"turn_end:end_turn",
		"status:idle",
		// The second turn: an edit, a command still running when the user interrupts.
		"status:working",
		"user_message:Fix the typo in README.md",
		"tool_call:running:edit:Edit: README.md",
		"tool_call:completed:edit:Edit: README.md",
		"tool_call:running:execute:Run: make build",
		"tool_call:failed:execute:Run: make build",
		"turn_end:cancelled",
		"status:idle",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, ev := range evs {
		if ev.Kind == protocol.EventToolCall && ev.ToolCall.Status == "completed" && ev.ToolCall.Kind == "execute" &&
			!strings.Contains(ev.ToolCall.Output, "syre-observe-test") {
			t.Fatalf("command output: %q", ev.ToolCall.Output)
		}
		if ev.Kind == protocol.EventToolCall && ev.ToolCall.Kind == "edit" && (len(ev.ToolCall.Paths) != 1 || ev.ToolCall.Paths[0] != "README.md") {
			t.Fatalf("edit paths: %+v", ev.ToolCall)
		}
		if ev.Kind == protocol.EventUsage && (ev.Usage.CacheReadTokens == 0 || ev.Usage.InputTokens >= 17242) {
			t.Fatalf("usage splits cached input: %+v", ev.Usage)
		}
		if ev.Kind == protocol.EventMessage && ev.Model == "" {
			t.Fatalf("no model on %+v", ev)
		}
	}
}

// writeRollout puts a rollout under home/sessions/YYYY/MM/DD.
func writeRollout(t *testing.T, home, id, cwd string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "11", "28")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-11-28T03-13-42-"+id+".jsonl")
	body := `{"timestamp":"2026-11-28T03:13:43Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"` + cwd + `"}}` + "\n" +
		`{"timestamp":"2026-11-28T03:13:44Z","type":"turn_context","payload":{"model":"gpt-6.2-sol","cwd":"` + cwd + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexDiscoverMatchesTheProcessHoldingTheRollout(t *testing.T) {
	home := t.TempDir()
	const id = "01a22024-f31e-7fe0-acb5-f276bb5ffd41"
	path := writeRollout(t, home, id, "/work/demo")
	procs := &argvProcs{fakeProcs: fakeProcs{procs: []shells.Proc{
		{PID: 1, Name: "launchd"},
		{PID: 40, PPID: 1, Name: "zsh"},
		{PID: 50, PPID: 40, Name: "codex"},
		{PID: 51, PPID: 50, Name: "codex"}, // its helper
	}}}
	open := map[int][]string{50: {path}}
	src := &CodexSource{Dirs: func() []CodexDir { return []CodexDir{{Dir: home}} }, Procs: procs,
		OpenFiles: func(pid int) []string { return open[pid] }, ListEvery: -1, ProcTTL: -1}
	got, err := src.Discover(context.Background(), time.Now().Add(-time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("discover: %+v %v", got, err)
	}
	s := got[0]
	if s.ID != id || s.Agent != "codex" || s.Cwd != "/work/demo" || s.Model != "gpt-6.2-sol" || !s.Alive || s.PID != 50 ||
		len(s.Ancestors) < 1 || s.Ancestors[0] != 40 || !s.Live() || s.Status != "" {
		t.Fatalf("session: %+v", s)
	}
	// codex exits: the session is over.
	procs.procs = procs.procs[:2]
	delete(open, 50)
	got, _ = src.Discover(context.Background(), time.Now().Add(-time.Hour))
	if len(got) != 1 || got[0].Alive || got[0].Status != protocol.ChatStatusDone {
		t.Fatalf("after exit: %+v", got)
	}
}

func TestCodexDiscoverMatchesByCwdWhenUnambiguous(t *testing.T) {
	home := t.TempDir()
	const id = "01a22024-f31e-7fe0-acb5-f276bb5ffd42"
	writeRollout(t, home, id, "/work/demo")
	procs := &argvProcs{
		fakeProcs: fakeProcs{procs: []shells.Proc{{PID: 60, PPID: 1, Name: "codex.exe"}, {PID: 70, PPID: 1, Name: "codex"}}},
		argv:      map[int][]string{70: {"codex", "app-server"}},
	}
	cwds := map[int]string{60: "/work/demo", 70: "/work/demo"}
	src := &CodexSource{Dirs: func() []CodexDir { return []CodexDir{{Dir: home}} }, Procs: procs,
		Cwd: func(pid int) string { return cwds[pid] }, ListEvery: -1, ProcTTL: -1}
	got, _ := src.Discover(context.Background(), time.Now().Add(-time.Hour))
	// The app-server (Syre's own codex chats) is never matched by cwd.
	if len(got) != 1 || !got[0].Alive || got[0].PID != 60 {
		t.Fatalf("cwd match: %+v", got)
	}
	// Two interactive codex in one dir: which session is whose is unknown.
	procs.procs = append(procs.procs, shells.Proc{PID: 80, PPID: 1, Name: "codex"})
	cwds[80] = "/work/demo"
	got, _ = src.Discover(context.Background(), time.Now().Add(-time.Hour))
	if len(got) != 1 || got[0].Alive || got[0].PID != 0 {
		t.Fatalf("ambiguous: %+v", got)
	}
}

func TestCodexNames(t *testing.T) {
	for name, want := range map[string]bool{
		"codex": true, "codex.exe": true, "Codex.EXE": true, "codex-x86_64-unknown-linux-musl": true,
		"codex-aarch64-apple-darwin": true, "codex-linux-sandbox": false, "codexd": false, "node": false,
	} {
		if isCodexName(name) != want {
			t.Errorf("isCodexName(%q) = %v", name, !want)
		}
	}
	if codexSessionID("/h/sessions/2026/11/28/rollout-2026-11-28T03-13-42-01a22024-f31e-7fe0-acb5-f276bb5ffd41.jsonl") != "01a22024-f31e-7fe0-acb5-f276bb5ffd41" {
		t.Fatal("session id from the file name")
	}
	if codexSessionID("/h/sessions/notes.jsonl") != "" {
		t.Fatal("not a rollout")
	}
}

func TestCodexSeesANewSessionBetweenFullListings(t *testing.T) {
	home := t.TempDir()
	src := &CodexSource{Dirs: func() []CodexDir { return []CodexDir{{Dir: home}} }, ListEvery: time.Hour}
	if got, _ := src.Discover(context.Background(), time.Now().Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("empty home: %+v", got)
	}
	// Started after the full listing, in today's dir (local date).
	now := time.Now()
	dir := filepath.Join(home, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "01a22024-f31e-7fe0-acb5-f276bb5ffd43"
	if err := os.WriteFile(filepath.Join(dir, "rollout-x-"+id+".jsonl"), []byte(`{"type":"session_meta","payload":{"cwd":"/w"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := src.Discover(context.Background(), time.Now().Add(-time.Hour))
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("new session not seen before the next full listing: %+v", got)
	}
}
