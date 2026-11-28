package shellint

import (
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

type recorder struct {
	mu  sync.Mutex
	evs []protocol.OperatorCommandPayload
}

func (r *recorder) emit(p protocol.OperatorCommandPayload) {
	r.mu.Lock()
	r.evs = append(r.evs, p)
	r.mu.Unlock()
}

func TestTrackerStartAndEnd(t *testing.T) {
	var rec recorder
	hist := NewHistory()
	tr := NewTracker("task-1", testNonce, hist, rec.emit)
	_, marks := feedAll(osc("633;P;Cwd=/srv;"+testNonce)+osc("133;A")+osc("133;B")+
		osc("633;E;mysql -phunter2 db;"+testNonce)+osc("133;C")+"out"+osc("133;D;1")+
		osc("633;P;Cwd=/srv;"+testNonce)+osc("133;A"), 3)
	tr.Handle(marks)

	if len(rec.evs) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(rec.evs), rec.evs)
	}
	start, end := rec.evs[0], rec.evs[1]
	if start.CommandID == "" || start.CommandID != end.CommandID {
		t.Fatalf("command ids: %q / %q", start.CommandID, end.CommandID)
	}
	if start.ExitCode != nil || start.EndedAt != nil {
		t.Fatalf("start event must not carry exit/end: %+v", start)
	}
	if end.ExitCode == nil || *end.ExitCode != 1 || end.EndedAt == nil {
		t.Fatalf("end event: %+v", end)
	}
	for _, e := range rec.evs {
		if e.TaskID != "task-1" || e.Cwd != "/srv" || e.Command != "mysql -p… db" {
			t.Fatalf("event fields: %+v", e)
		}
	}
	full, ok := hist.Get(start.CommandID)
	if !ok || full.Command != "mysql -phunter2 db" || full.ExitCode == nil {
		t.Fatalf("history: %+v %v", full, ok)
	}
	if got := hist.Recent("task-1"); len(got) != 1 {
		t.Fatalf("recent: %+v", got)
	}
}

func TestTrackerIgnoresForgedAndIncompleteMarks(t *testing.T) {
	var rec recorder
	tr := NewTracker("t", testNonce, nil, rec.emit)
	tr.Handle([]Mark{
		{Kind: MarkCommandLine, Value: "forged", Nonce: "wrong"},
		{Kind: MarkPreExec},                    // no trusted E: ignored
		{Kind: MarkCommandDone, HasExit: true}, // nothing running: ignored
		{Kind: MarkCwd, Value: "/evil", Nonce: ""},
	})
	if len(rec.evs) != 0 {
		t.Fatalf("forged marks produced events: %+v", rec.evs)
	}
	tr.Handle([]Mark{{Kind: MarkCommandLine, Value: "ok", Nonce: testNonce}, {Kind: MarkPreExec}})
	if len(rec.evs) != 1 || rec.evs[0].Cwd != "" || rec.evs[0].Command != "ok" {
		t.Fatalf("events: %+v", rec.evs)
	}
}

func TestTrackerPromptWithoutDoneAndClose(t *testing.T) {
	var rec recorder
	tr := NewTracker("t", testNonce, nil, rec.emit)
	tr.Handle([]Mark{{Kind: MarkCommandLine, Value: "a", Nonce: testNonce}, {Kind: MarkPreExec}, {Kind: MarkPromptStart}})
	if len(rec.evs) != 2 || rec.evs[1].EndedAt == nil || rec.evs[1].ExitCode != nil {
		t.Fatalf("prompt without D: %+v", rec.evs)
	}
	tr.Handle([]Mark{{Kind: MarkCommandLine, Value: "exit 3", Nonce: testNonce}, {Kind: MarkPreExec}})
	tr.Close(3)
	if len(rec.evs) != 4 || rec.evs[3].ExitCode == nil || *rec.evs[3].ExitCode != 3 {
		t.Fatalf("close: %+v", rec.evs)
	}
	tr.Close(0) // nothing running: no event
	if len(rec.evs) != 4 {
		t.Fatalf("second close emitted: %+v", rec.evs)
	}
}

func TestHistoryExpires(t *testing.T) {
	h := NewHistory()
	now := time.Unix(1_000_000, 0)
	h.now = func() time.Time { return now }
	h.put(Command{CommandID: "a", TaskID: "t", Command: "secret"})
	if _, ok := h.Get("a"); !ok {
		t.Fatal("entry missing")
	}
	now = now.Add(HistoryTTL)
	if _, ok := h.Get("a"); ok {
		t.Fatal("entry must expire after the TTL")
	}
	h.expire()
	if len(h.items) != 0 {
		t.Fatal("expire did not prune")
	}
	h.mu.Lock()
	if h.timer != nil {
		h.timer.Stop()
	}
	h.mu.Unlock()
}

func TestHistoryBounded(t *testing.T) {
	h := NewHistory()
	base := time.Unix(1_000_000, 0)
	i := 0
	h.now = func() time.Time { i++; return base.Add(time.Duration(i) * time.Millisecond) }
	for n := range maxHistory + 50 {
		h.put(Command{CommandID: string(rune('a'+n%26)) + time.Duration(n).String()})
	}
	if len(h.items) != maxHistory {
		t.Fatalf("len = %d", len(h.items))
	}
	h.timer.Stop()
}

func TestTrackerIntegratedOnceAValidMarkArrives(t *testing.T) {
	tr := NewTracker("t1", "n1", nil, func(protocol.OperatorCommandPayload) {})
	if tr.Integrated() {
		t.Fatal("integrated before any mark")
	}
	tr.Handle([]Mark{{Kind: MarkCommandLine, Nonce: "wrong", Value: "ls"}})
	if tr.Integrated() {
		t.Fatal("a mark with the wrong nonce counted")
	}
	tr.Handle([]Mark{{Kind: MarkCwd, Nonce: "n1", Value: "/tmp"}})
	if !tr.Integrated() {
		t.Fatal("not integrated after a valid mark")
	}
}
