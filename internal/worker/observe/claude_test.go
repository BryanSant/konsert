package observe

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/shells"
)

// testdata/claude_session.jsonl is a real claude 2.1 transcript with every text, path
// and tool payload replaced: one prompt, five tool calls (one Edit), a final answer,
// claude's turn markers, then two slash commands.
func readFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "claude_session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func kinds(evs []protocol.AgentChatEvent) map[string]int {
	m := map[string]int{}
	for _, ev := range evs {
		m[ev.Kind]++
	}
	return m
}

func TestClaudeEventsFixture(t *testing.T) {
	evs := ClaudeEvents("sid", readFixture(t))
	k := kinds(evs)
	if k[protocol.EventUserMessage] != 1 {
		t.Fatalf("user messages = %d, want 1 (slash commands are not prompts)", k[protocol.EventUserMessage])
	}
	if k[protocol.EventToolCall] != 10 {
		t.Fatalf("tool_call events = %d, want 10 (5 started, 5 finished)", k[protocol.EventToolCall])
	}
	if k[protocol.EventTurnEnd] != 1 {
		t.Fatalf("turn_end events = %d, want 1", k[protocol.EventTurnEnd])
	}
	if k[protocol.EventTitle] == 0 || k[protocol.EventUsage] == 0 || k[protocol.EventMessage] != 1 {
		t.Fatalf("kinds = %v", k)
	}

	var statuses []string
	var edit *protocol.AgentToolCall
	for _, ev := range evs {
		if ev.AgentSessionID != "sid" || ev.At.IsZero() {
			t.Fatalf("event without session id or time: %+v", ev)
		}
		switch ev.Kind {
		case protocol.EventStatus:
			statuses = append(statuses, ev.Status)
		case protocol.EventUserMessage:
			if ev.Text != "Run the unit tests and tell me what fails." {
				t.Fatalf("prompt = %q", ev.Text)
			}
		case protocol.EventToolCall:
			if ev.ToolCall.Name == "Edit" && ev.ToolCall.Status == "running" {
				edit = ev.ToolCall
			}
		case protocol.EventMessage:
			if ev.Model != "claude-fable-5-2" || !strings.HasPrefix(ev.Text, "All 12 tests pass") {
				t.Fatalf("message = %+v", ev)
			}
		}
	}
	if got := strings.Join(statuses, ","); got != "working,idle" {
		t.Fatalf("statuses = %s, want working,idle", got)
	}
	if edit == nil || len(edit.Paths) != 1 || edit.Paths[0] != "/work/demo/main.go" || edit.Kind != "edit" {
		t.Fatalf("edit card = %+v", edit)
	}
	first := evs[0]
	if first.Kind != protocol.EventUserMessage {
		t.Fatalf("first event = %s, want the prompt", first.Kind)
	}
}

func TestClaudeEventsResultsAndInterrupt(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"fix it"},"uuid":"u1","timestamp":"2026-11-20T10:00:00Z"}`,
		`{"type":"assistant","message":{"id":"m1","model":"x","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"false"}}]},"uuid":"a1","timestamp":"2026-11-20T10:00:01Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"exit 1","is_error":true}]},"uuid":"u2","timestamp":"2026-11-20T10:00:02Z"}`,
		`{"type":"assistant","message":{"id":"m2","model":"x","stop_reason":"end_turn","content":[{"type":"thinking","thinking":""}]},"uuid":"a2","timestamp":"2026-11-20T10:00:03Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]},"uuid":"u3","timestamp":"2026-11-20T10:00:04Z"}`,
		`{"type":"user","isSidechain":true,"message":{"role":"user","content":"sub-agent prompt"},"uuid":"u4","timestamp":"2026-11-20T10:00:05Z"}`,
		`not json`,
	}, "\n") + "\n"
	evs := ClaudeEvents("sid", []byte(lines))
	var got []string
	for _, ev := range evs {
		s := ev.Kind
		switch ev.Kind {
		case protocol.EventStatus:
			s += ":" + ev.Status
		case protocol.EventToolCall:
			s += ":" + ev.ToolCall.Name + ":" + ev.ToolCall.Status
		case protocol.EventTurnEnd:
			s += ":" + ev.StopReason
		}
		got = append(got, s)
	}
	want := "user_message,status:working,tool_call:Bash:running,tool_call:Bash:failed,usage,turn_end:cancelled,status:idle"
	// No usage in these records: the m1/m2 usage lines are absent.
	want = strings.Replace(want, "usage,", "", 1)
	if strings.Join(got, ",") != want {
		t.Fatalf("events:\n got %s\nwant %s", strings.Join(got, ","), want)
	}
}

func TestReadRecordsCompleteLinesOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	long := `{"type":"x","pad":"` + strings.Repeat("a", 100) + `"}`
	content := "{\"a\":1}\n" + long + "\n{\"b\":2}\n{\"partial\":"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// A window smaller than the long line: it is skipped, the others are read once.
	var got []string
	off := int64(0)
	for range 10 {
		chunk, next, err := readRecords(path, off, 64)
		if err != nil {
			t.Fatal(err)
		}
		for line := range bytes.Lines(chunk) {
			got = append(got, strings.TrimSpace(string(line)))
		}
		if next == off {
			break
		}
		off = next
	}
	if strings.Join(got, "|") != `{"a":1}|{"b":2}` {
		t.Fatalf("records = %v", got)
	}
	if want := int64(len(content) - len(`{"partial":`)); off != want {
		t.Fatalf("offset = %d, want %d (the partial record is left)", off, want)
	}
}

type fakeProcs struct{ procs []shells.Proc }

func (f *fakeProcs) Snapshot() ([]shells.Proc, error) { return f.procs, nil }
func (f *fakeProcs) Details(*shells.Proc)             {}

func writeFile(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClaudeDiscoverLiveStatus(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-3 * time.Hour)
	started := time.Now().Add(-4 * time.Hour)
	// A live session idle for hours: listed for its pid file, whatever its mtime.
	writeFile(t, filepath.Join(dir, "projects", "-work-demo", "live.jsonl"), string(readFixture(t)), old)
	writeFile(t, filepath.Join(dir, "sessions", "4242.json"),
		`{"pid":4242,"sessionId":"live","cwd":"/work/demo","startedAt":`+strconv.FormatInt(started.UnixMilli(), 10)+
			`,"status":"waiting","waitingFor":"approve Bash","kind":"interactive"}`, time.Time{})
	// An old transcript with no process: history, not listed.
	writeFile(t, filepath.Join(dir, "projects", "-work-demo", "history.jsonl"), "{}\n", old)
	// A recent one whose process is gone: done.
	writeFile(t, filepath.Join(dir, "projects", "-work-demo", "ended.jsonl"), "{}\n", time.Time{})
	// A recent one in an account an older claude uses (no sessions/ dir): listed with no
	// live status, cwd from its records.
	olderDir := t.TempDir()
	writeFile(t, filepath.Join(olderDir, "projects", "-work-other", "recent.jsonl"),
		`{"type":"attachment","cwd":"/work/other"}`+"\n", time.Time{})
	// A sub-agent's transcript is part of its parent session.
	writeFile(t, filepath.Join(dir, "projects", "-work-demo", "live", "subagents", "agent-1.jsonl"), "{}\n", time.Time{})

	procs := &fakeProcs{procs: []shells.Proc{
		{PID: 4242, PPID: 100, Start: started.Add(-time.Second)},
		{PID: 100, PPID: 1},
	}}
	src := &ClaudeSource{Dirs: func() []ClaudeDir { return []ClaudeDir{{Account: "claude2", Dir: dir}, {Dir: olderDir}} },
		Procs: procs, ProcTTL: -1, ListEvery: -1}
	since := time.Now().Add(-time.Minute)

	found, err := src.Discover(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Session{}
	for _, s := range found {
		byID[s.ID] = s
	}
	if len(byID) != 3 {
		t.Fatalf("found %v, want live, ended and recent", byID)
	}
	if e := byID["ended"]; e.Status != protocol.ChatStatusDone {
		t.Fatalf("ended session = %+v", e)
	}
	live := byID["live"]
	if live.Status != protocol.ChatStatusNeedsYou || live.Waiting != "approve Bash" || live.PID != 4242 ||
		live.Cwd != "/work/demo" || live.Account != "claude2" || !live.Live() {
		t.Fatalf("live session = %+v", live)
	}
	if len(live.Ancestors) != 2 || live.Ancestors[0] != 100 || live.Ancestors[1] != 1 {
		t.Fatalf("ancestors = %v", live.Ancestors)
	}
	if r := byID["recent"]; r.Cwd != "/work/other" || r.Status != "" {
		t.Fatalf("recent session = %+v", r)
	}

	// The process exits (its file lingers): reported done once, then history.
	procs.procs = procs.procs[1:]
	found, _ = src.Discover(context.Background(), time.Now())
	if len(found) != 1 || found[0].ID != "live" || found[0].Status != protocol.ChatStatusDone || found[0].Live() {
		t.Fatalf("after exit: %+v", found)
	}
	found, _ = src.Discover(context.Background(), time.Now())
	if len(found) != 0 {
		t.Fatalf("after done: %+v", found)
	}

	// A new process with the same PID, started long after the session: not its process.
	procs.procs = append(procs.procs, shells.Proc{PID: 4242, PPID: 100, Start: time.Now()})
	found, _ = src.Discover(context.Background(), time.Now())
	if len(found) != 0 {
		t.Fatalf("reused pid: %+v", found)
	}
}

func TestClaudeSourceInRegistry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects", "-work-demo", "s1.jsonl")
	fixture := readFixture(t)
	half := bytes.Index(fixture, []byte(`"stop_reason":"end_turn"`))
	half = bytes.LastIndexByte(fixture[:half], '\n') + 1
	writeFile(t, path, string(fixture[:half]), time.Time{})

	var updates []Update
	src := &ClaudeSource{Dirs: func() []ClaudeDir { return []ClaudeDir{{Dir: dir}} }}
	reg := New(Config{Sources: []Source{src}, Grace: -1, Emit: func(u Update) { updates = append(updates, u) }})
	reg.Poll(context.Background())
	if len(updates) != 1 || kinds(updates[0].Events)[protocol.EventTurnEnd] != 0 {
		t.Fatalf("first poll: %+v", updates)
	}
	// The rest of the turn is appended: only the new records are reported.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(fixture[half:]); err != nil {
		t.Fatal(err)
	}
	f.Close()
	future := time.Now().Add(time.Second)
	_ = os.Chtimes(path, future, future)
	reg.Poll(context.Background())
	if len(updates) != 2 {
		t.Fatalf("second poll: %d updates", len(updates))
	}
	k := kinds(updates[1].Events)
	if k[protocol.EventTurnEnd] != 1 || k[protocol.EventUserMessage] != 0 {
		t.Fatalf("second poll kinds = %v", k)
	}
}
