package worker

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/drivers"
)

// Live smoke tests against the real grok CLI, run through the syred Runner (PTY,
// driver argv, session-log telemetry, summary). They use the caller's grok login and a
// paid subscription, so they are opt-in and keep prompts tiny:
//
//	SYRE_LIVE_GROK=1 go test ./internal/worker -run TestLiveGrok -v
//
// Sessions created in the test's temp workdirs are removed from $GROK_HOME afterwards.

type liveEvents struct {
	mu     sync.Mutex
	events []protocol.TaskEventPayload
	ch     chan protocol.TaskEventPayload
	pty    []byte
}

func (l *liveEvents) recordPty(_ byte, _ string, data []byte) {
	l.mu.Lock()
	l.pty = append(l.pty, data...)
	l.mu.Unlock()
}

// screenTail returns the last ANSI-stripped PTY output, for failure diagnostics.
func (l *liveEvents) screenTail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return drivers.TailCleanSummary(l.pty, 40, 4000)
}

func (l *liveEvents) record(p protocol.TaskEventPayload) {
	l.mu.Lock()
	l.events = append(l.events, p)
	l.mu.Unlock()
	select {
	case l.ch <- p:
	default:
	}
}

func (l *liveEvents) snapshot() []protocol.TaskEventPayload {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]protocol.TaskEventPayload(nil), l.events...)
}

func (l *liveEvents) has(kind, substr string) bool {
	for _, e := range l.snapshot() {
		if e.Kind == kind && strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}

func liveGrokSetup(t *testing.T) (string, *Runner, *statusRecorder, *liveEvents) {
	t.Helper()
	if os.Getenv("SYRE_LIVE_GROK") != "1" {
		t.Skip("set SYRE_LIVE_GROK=1 to run live grok smoke tests")
	}
	if _, ok := (&drivers.GrokDriver{}).Detect(); !ok {
		t.Skip("grok CLI not installed")
	}
	workdir := t.TempDir()
	t.Cleanup(func() { removeGrokSessionsFor(t, workdir) })
	rec := newStatusRecorder()
	ev := &liveEvents{ch: make(chan protocol.TaskEventPayload, 256)}
	r := NewRunner(drivers.DefaultRegistry, false, nil, RunnerCallbacks{OnTaskStatus: rec.record, OnTaskEvent: ev.record, OnPtyFrame: ev.recordPty}, nil)
	t.Cleanup(r.ShutdownAll)
	return workdir, r, rec, ev
}

func waitGrokTerminal(t *testing.T, rec *statusRecorder, timeout time.Duration) protocol.TaskStatusPayload {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case p := <-rec.ch:
			if protocol.IsTerminalTaskStatus(p.Status) {
				return p
			}
		case <-deadline:
			t.Fatalf("task did not finish within %s; statuses: %+v", timeout, rec.snapshot())
		}
	}
}

// removeGrokSessionsFor deletes $GROK_HOME/sessions/<url-encoded workdir> for the test's
// own temp workdir.
func removeGrokSessionsFor(t *testing.T, workdir string) {
	grokHome := os.Getenv("GROK_HOME")
	if grokHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		grokHome = filepath.Join(home, ".grok")
	}
	want := map[string]bool{filepath.Clean(workdir): true}
	if resolved, err := filepath.EvalSymlinks(workdir); err == nil {
		want[resolved] = true
	}
	base := filepath.Join(grokHome, "sessions")
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		name, err := url.PathUnescape(e.Name())
		if err != nil || !e.IsDir() || !want[filepath.Clean(name)] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(base, e.Name())); err != nil {
			t.Logf("cleanup %s: %v", e.Name(), err)
		}
	}
}

// Oneshot in sandboxed mode: verifies `--sandbox workspace --session-id <uuid> --single=...`
// is accepted, the session log is tailed for usage, and the reply becomes the summary.
func TestLiveGrokOneshot(t *testing.T) {
	workdir, r, rec, ev := liveGrokSetup(t)
	err := r.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID: "t-gkw5a", Agent: "grok", Workdir: workdir,
		Prompt: "reply with the single word OK", Mode: protocol.TaskModeOneshot,
		PermissionMode: protocol.PermissionSandboxed, TimeoutSeconds: 180,
	})
	if err != nil {
		t.Fatal(err)
	}
	final := waitGrokTerminal(t, rec, 200*time.Second)
	t.Logf("final: status=%s exit=%v summary=%q", final.Status, grokDerefInt(final.ExitCode), final.Summary)
	for _, e := range ev.snapshot() {
		t.Logf("event %s: %s", e.Kind, e.Message)
	}
	if final.Status != protocol.TaskStatusCompleted {
		t.Fatalf("expected completed, got %+v", final)
	}
	if !strings.Contains(strings.ToUpper(final.Summary), "OK") {
		t.Fatalf("summary %q does not contain the reply", final.Summary)
	}
	if !ev.has("usage", "Grok usage") || !ev.has("milestone", "Turn completed") {
		t.Fatalf("expected usage and turn milestone events from the session log")
	}
}

// Interactive TUI with full access: the turn writes a file, telemetry reports the edit and
// flags the idle prompt as waiting_input, then the task is canceled like a user closing it.
func TestLiveGrokInteractive(t *testing.T) {
	workdir, r, rec, ev := liveGrokSetup(t)
	const id = "t-yqbtw"
	err := r.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID: id, Agent: "grok", Workdir: workdir,
		Prompt:         "Create a file named ok.txt containing the word OK, then reply with the single word DONE",
		Mode:           protocol.TaskModeInteractive,
		PermissionMode: protocol.PermissionFullAccess, TimeoutSeconds: 300, Cols: 120, Rows: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.SubscribePty(id)
	deadline := time.After(90 * time.Second)
wait:
	for {
		select {
		case e := <-ev.ch:
			if e.Kind != "waiting_input" {
				continue
			}
			if strings.Contains(e.Message, "trust") {
				// First run in a new folder: answer the trust dialog as the user would.
				t.Logf("answering trust prompt: %s", e.Message)
				if err := r.WriteStdin(id, []byte("y")); err != nil {
					t.Fatal(err)
				}
				continue
			}
			break wait
		case p := <-rec.ch:
			if protocol.IsTerminalTaskStatus(p.Status) {
				t.Fatalf("interactive task ended before its turn completed: %+v", p)
			}
		case <-deadline:
			t.Fatalf("no waiting_input after the first turn; events: %+v\nscreen:\n%s", ev.snapshot(), ev.screenTail())
		}
	}
	if b, err := os.ReadFile(filepath.Join(workdir, "ok.txt")); err != nil || !strings.Contains(string(b), "OK") {
		t.Fatalf("ok.txt not written: %q %v", b, err)
	}
	if !ev.has("file_edit", "ok.txt") {
		t.Fatalf("expected a file_edit event for ok.txt; events: %+v", ev.snapshot())
	}
	if err := r.CancelTask(id); err != nil {
		t.Fatal(err)
	}
	final := waitGrokTerminal(t, rec, 30*time.Second)
	t.Logf("final: status=%s exit=%v summary=%q", final.Status, grokDerefInt(final.ExitCode), final.Summary)
	for _, e := range ev.snapshot() {
		t.Logf("event %s: %s", e.Kind, e.Message)
	}
	if !strings.Contains(strings.ToUpper(final.Summary), "DONE") {
		t.Fatalf("summary %q does not contain the reply", final.Summary)
	}
}

func grokDerefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
