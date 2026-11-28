package worker

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/observe"
	"github.com/BryanSant/syre/internal/worker/shells"
)

type obsProcs struct{ procs []shells.Proc }

func (f *obsProcs) Snapshot() ([]shells.Proc, error) { return f.procs, nil }
func (f *obsProcs) Details(*shells.Proc)             {}

// obsHarness is a claude config dir, a fake process table and the payloads reported.
type obsHarness struct {
	t     *testing.T
	dir   string
	procs *obsProcs
	now   time.Time
	o     *observedSessions
	got   []protocol.ExternalSessionPayload
}

func newObsHarness(t *testing.T, managed func(int) bool) *obsHarness {
	h := &obsHarness{t: t, dir: t.TempDir(), procs: &obsProcs{}, now: time.Now()}
	h.o = newObservedSessions(observedConfig{
		ClaudeDirs: func() []observe.ClaudeDir { return []observe.ClaudeDir{{Account: "claude2", Dir: h.dir}} },
		Procs:      h.procs,
		Fresh:      true,
		IsManaged:  managed,
		Grace:      -1,
		Now:        func() time.Time { return h.now },
	}, func(p protocol.ExternalSessionPayload) { h.got = append(h.got, p) })
	return h
}

func (h *obsHarness) write(rel, content string, mod time.Time) {
	h.t.Helper()
	path := filepath.Join(h.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		h.t.Fatal(err)
	}
}

func (h *obsHarness) pidFile(pid int, sid, status, waiting string) {
	h.write(filepath.Join("sessions", strconv.Itoa(pid)+".json"),
		`{"pid":`+strconv.Itoa(pid)+`,"sessionId":"`+sid+`","cwd":"/work/demo","startedAt":`+
			strconv.FormatInt(h.now.Add(-time.Hour).UnixMilli(), 10)+`,"status":"`+status+`","waitingFor":"`+waiting+`"}`, h.now)
}

func (h *obsHarness) poll() []protocol.ExternalSessionPayload {
	h.got = nil
	h.o.Poll(context.Background())
	return h.got
}

func fixture(t *testing.T) string {
	raw, err := os.ReadFile(filepath.Join("observe", "testdata", "claude_session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestObservedClaudeLiveStatus(t *testing.T) {
	h := newObsHarness(t, nil)
	h.write("projects/-work-demo/s1.jsonl", fixture(t), h.now.Add(-2*time.Hour))
	h.pidFile(4242, "s1", "waiting", "approve Bash")
	h.procs.procs = []shells.Proc{{PID: 4242, PPID: 1, Start: h.now.Add(-time.Hour)}}

	got := h.poll()
	if len(got) != 1 {
		t.Fatalf("payloads = %+v", got)
	}
	p := got[0]
	if p.ExternalID != "s1" || p.Agent != "claude" || p.Account != "claude2" || p.Source != "transcript" ||
		p.Status != "needs_you" || p.Waiting != "approve Bash" || p.Cwd != "/work/demo" ||
		p.Model != "claude-fable-5-2" || p.Summary != "Run the demo unit tests" {
		t.Fatalf("payload = %+v", p)
	}

	// The user approves: busy, with no new records yet.
	h.pidFile(4242, "s1", "busy", "")
	got = h.poll()
	if len(got) != 1 || got[0].Status != "working" || got[0].Waiting != "" {
		t.Fatalf("after approval: %+v", got)
	}
	// Nothing changed: nothing reported.
	if got = h.poll(); len(got) != 0 {
		t.Fatalf("idle poll: %+v", got)
	}
	// The process exits.
	h.procs.procs = nil
	got = h.poll()
	if len(got) != 1 || got[0].Status != "done" {
		t.Fatalf("after exit: %+v", got)
	}
	// claude writes its transcript once more on the way out: still done, nothing new.
	h.write("projects/-work-demo/s1.jsonl", fixture(t)+`{"type":"last-prompt"}`+"\n", h.now)
	if got = h.poll(); len(got) != 0 {
		t.Fatalf("after the exit write: %+v", got)
	}
}

func TestObservedSkipsManagedSessions(t *testing.T) {
	const syred = 900
	h := newObsHarness(t, func(pid int) bool { return pid == syred })
	h.write("projects/-work-demo/chat.jsonl", fixture(t), h.now)
	h.pidFile(4242, "chat", "busy", "")
	h.procs.procs = []shells.Proc{
		{PID: 4242, PPID: 4000, Start: h.now.Add(-time.Hour)},
		{PID: 4000, PPID: syred},
		{PID: syred, PPID: 1},
	}
	if got := h.poll(); len(got) != 0 {
		t.Fatalf("a session under syred was reported: %+v", got)
	}

	// A hand-started session is reported, then withdrawn once known as managed (a chat's
	// session_ready that arrived late).
	h.write("projects/-work-demo/late.jsonl", fixture(t), h.now)
	h.pidFile(5000, "late", "busy", "")
	h.procs.procs = append(h.procs.procs, shells.Proc{PID: 5000, PPID: 1, Start: h.now.Add(-time.Hour)})
	if got := h.poll(); len(got) != 1 || got[0].ExternalID != "late" {
		t.Fatalf("late: %+v", got)
	}
	h.o.MarkManaged("", "late")
	if got := h.poll(); len(got) != 1 || got[0].ExternalID != "late" || got[0].Status != "done" {
		t.Fatalf("after mark: %+v", got)
	}
}

func TestObservedRecordStatusAges(t *testing.T) {
	h := newObsHarness(t, nil)
	// No pid file (an older claude): status from the records, mid-tool here.
	lines := fixture(t)
	cut := strings.Index(lines, `"name":"Edit"`)
	cut = strings.IndexByte(lines[cut:], '\n') + cut + 1
	h.write("projects/-work-demo/s1.jsonl", lines[:cut], h.now)
	got := h.poll()
	if len(got) != 1 || got[0].Status != "working" || got[0].Tool != "Edit" {
		t.Fatalf("mid-tool: %+v", got)
	}
	h.now = h.now.Add(observedStaleWorking + time.Minute)
	if got = h.poll(); len(got) != 1 || got[0].Status != "idle" {
		t.Fatalf("stale: %+v", got)
	}
	h.now = h.now.Add(observedDoneAfter)
	if got = h.poll(); len(got) != 1 || got[0].Status != "done" {
		t.Fatalf("aged out: %+v", got)
	}
	if got = h.poll(); len(got) != 0 {
		t.Fatalf("after done: %+v", got)
	}
}

func TestObservedHistoryIsQuiet(t *testing.T) {
	h := newObsHarness(t, nil)
	// Written 40 minutes ago, no process: within the lookback but already done.
	h.write("projects/-work-demo/old.jsonl", fixture(t), h.now.Add(-40*time.Minute))
	if got := h.poll(); len(got) != 0 {
		t.Fatalf("history reported: %+v", got)
	}
}

func TestObservedChangedFiles(t *testing.T) {
	h := newObsHarness(t, nil)
	at := func(d time.Duration) string { return h.now.Add(d).UTC().Format(time.RFC3339Nano) }
	use := `{"type":"assistant","message":{"id":"m1","model":"x","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"/work/demo/a.go"}}]},"uuid":"a1","timestamp":"` + at(-2*time.Second) + `"}` + "\n"
	h.write("projects/-work-demo/s1.jsonl", use, h.now)
	if got := h.poll(); len(got) != 1 || len(got[0].ChangedFiles) != 0 || got[0].Tool != "Edit" {
		t.Fatalf("edit started: %+v", got)
	}
	// The result arrives in a later pass.
	res := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},"uuid":"u1","timestamp":"` + at(0) + `"}` + "\n"
	h.write("projects/-work-demo/s1.jsonl", use+res, h.now.Add(time.Second))
	got := h.poll()
	if len(got) != 1 || len(got[0].ChangedFiles) != 1 || got[0].ChangedFiles[0] != "/work/demo/a.go" || got[0].Tool != "" {
		t.Fatalf("edit finished: %+v", got)
	}
}

func TestPassiveScanSkipsTranscriptSessions(t *testing.T) {
	disc := NewSessionDiscovery(t.TempDir(), nil, nil)
	disc.resolveCwd = func(context.Context, int) string { return "/work/demo" }
	var got []protocol.ExternalSessionPayload
	disc.emit = func(p protocol.ExternalSessionPayload) { got = append(got, p) }
	disc.emitIfChanged(protocol.ExternalSessionPayload{ExternalID: "s1", Agent: "claude", Cwd: "/work/demo", Status: "idle", Source: "transcript"})
	got = nil
	disc.scanProcessList(context.Background(), []psEntry{{pid: 4242, ppid: 1, comm: "claude"}})
	if len(got) != 0 {
		t.Fatalf("passive scan duplicated a transcript session: %+v", got)
	}
}

func TestManagedHookSession(t *testing.T) {
	agent, sid := managedHookSession([]byte(`{"agent":"Claude","event":"Stop","session_id":"s1","task_id":"t1"}`))
	if agent != "claude" || sid != "s1" {
		t.Fatalf("got %q %q", agent, sid)
	}
	if _, sid := managedHookSession([]byte(`{"agent":"claude","event":"Stop","session_id":"s1"}`)); sid != "" {
		t.Fatalf("unmanaged hook marked: %q", sid)
	}
}

func TestClaudeConfigDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, d := range []string{".claude2/projects", ".claude-backup/projects", ".claude3"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dirs := claudeConfigDirs(nil)
	// ~/.claude3 holds no sessions and ~/.claude-backup is not an account.
	if len(dirs) != 2 || dirs[0].Dir != filepath.Join(home, ".claude") || dirs[0].Account != "" ||
		dirs[1].Dir != filepath.Join(home, ".claude2") || dirs[1].Account != "claude2" {
		t.Fatalf("dirs = %+v", dirs)
	}
}

func TestObservedTranscriptAndWatch(t *testing.T) {
	h := newObsHarness(t, nil)
	lines := fixture(t)
	cut := strings.Index(lines, `"stop_reason":"end_turn"`)
	cut = strings.LastIndexByte(lines[:cut], '\n') + 1
	h.write("projects/-work-demo/s1.jsonl", lines[:cut], h.now)
	h.poll() // the registry learns the session

	if res := h.o.Transcript(context.Background(), protocol.ObservedTranscriptRequest{ExternalID: "nope"}); res.Available || res.Reason == "" {
		t.Fatalf("unknown session: %+v", res)
	}
	res := h.o.Transcript(context.Background(), protocol.ObservedTranscriptRequest{ExternalID: "s1", Watch: true})
	if !res.Available || len(res.Events) == 0 || res.Events[0].Kind != protocol.EventUserMessage || res.Events[0].Seq != 1 {
		t.Fatalf("transcript: %+v", res)
	}
	turn := res.Events[0].TurnID
	for i, ev := range res.Events {
		if ev.Seq != int64(i+1) || ev.TurnID != turn {
			t.Fatalf("event %d: seq %d turn %q (want %d, %q)", i, ev.Seq, ev.TurnID, i+1, turn)
		}
	}
	last := res.Events[len(res.Events)-1].Seq

	// The session goes on: the watch sends only what's new, numbered on.
	h.write("projects/-work-demo/s1.jsonl", lines, h.now.Add(time.Second))
	var got []protocol.ObservedEvents
	h.o.pollWatches(context.Background(), func(e protocol.ObservedEvents) { got = append(got, e) })
	if len(got) != 1 || got[0].ExternalID != "s1" || got[0].Events[0].Seq != last+1 {
		t.Fatalf("watch: %+v", got)
	}
	if k := got[0].Events; k[len(k)-1].Kind == "" {
		t.Fatal("empty event")
	}
	// Nothing new: nothing sent. Past its TTL the watch lapses.
	got = nil
	h.o.pollWatches(context.Background(), func(e protocol.ObservedEvents) { got = append(got, e) })
	if len(got) != 0 {
		t.Fatalf("resent: %+v", got)
	}
	h.now = h.now.Add(protocol.ObservedWatchTTL + time.Minute)
	h.o.pollWatches(context.Background(), func(protocol.ObservedEvents) {})
	if len(h.o.watches) != 0 {
		t.Fatal("watch did not lapse")
	}
}
