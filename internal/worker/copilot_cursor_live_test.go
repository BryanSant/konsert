package worker

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

// Live smoke tests against the real copilot and cursor-agent CLIs, run through the konsertd
// Runner (PTY, driver argv, session-log telemetry, summary). They use the caller's logins
// and paid subscriptions, so they are opt-in and send one tiny prompt each:
//
//	KONSERT_LIVE_COPILOT=1 go test ./internal/worker -run TestLiveCopilot -v
//	KONSERT_LIVE_CURSOR=1 go test ./internal/worker -run TestLiveCursor -v
//
// Session state the CLIs create for the test's temp workdir is removed afterwards
// (copilot: $COPILOT_HOME/session-state/<id>; cursor: the workdir's projects/ and chats/
// entries, which include its .workspace-trusted marker).

func liveSetup(t *testing.T, envVar string, d drivers.AgentDriver) (string, *Runner, *statusRecorder, *liveEvents) {
	t.Helper()
	if os.Getenv(envVar) != "1" {
		t.Skipf("set %s=1 to run live %s smoke tests", envVar, d.Name())
	}
	if _, ok := d.Detect(); !ok {
		t.Skipf("%s CLI not installed", d.Name())
	}
	workdir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workdir); err == nil {
		workdir = resolved
	}
	rec := newStatusRecorder()
	ev := &liveEvents{ch: make(chan protocol.TaskEventPayload, 256)}
	r := NewRunner(drivers.DefaultRegistry, false, nil, RunnerCallbacks{OnTaskStatus: rec.record, OnTaskEvent: ev.record, OnPtyFrame: ev.recordPty}, nil)
	t.Cleanup(r.ShutdownAll)
	return workdir, r, rec, ev
}

func runLiveOneshot(t *testing.T, r *Runner, rec *statusRecorder, ev *liveEvents, payload protocol.TaskStartPayload) protocol.TaskStatusPayload {
	t.Helper()
	if err := r.StartTask(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	final := waitGrokTerminal(t, rec, 200*time.Second)
	t.Logf("final: status=%s exit=%v summary=%q", final.Status, grokDerefInt(final.ExitCode), final.Summary)
	for _, e := range ev.snapshot() {
		t.Logf("event %s: %s", e.Kind, e.Message)
	}
	if final.Status != protocol.TaskStatusCompleted {
		t.Fatalf("expected completed, got %+v\nscreen:\n%s", final, ev.screenTail())
	}
	if !strings.Contains(strings.ToUpper(final.Summary), "OK") {
		t.Fatalf("summary %q does not contain the reply", final.Summary)
	}
	return final
}

// Oneshot with full access: `--allow-all --session-id=<uuid> --prompt=...`, usage and turn
// milestones from the session's events.jsonl, and the reply as the summary.
func TestLiveCopilotOneshot(t *testing.T) {
	workdir, r, rec, ev := liveSetup(t, "KONSERT_LIVE_COPILOT", &drivers.CopilotDriver{})
	t.Cleanup(func() { removeCopilotSessionsFor(t, workdir) })
	runLiveOneshot(t, r, rec, ev, protocol.TaskStartPayload{
		TaskID: "22222222-6eaf-4000-8000-000000000001", Agent: "copilot", Workdir: workdir,
		Prompt: "reply with the single word OK", Mode: protocol.TaskModeOneshot,
		PermissionMode: protocol.PermissionFullAccess, TimeoutSeconds: 180,
	})
	if !ev.has("usage", "Copilot usage") || !ev.has("milestone", "Turn completed") {
		t.Fatalf("expected usage and turn milestone events from the session log")
	}
}

// Oneshot in sandboxed mode: `--sandbox enabled --trust --resume=<uuid> --print ...`, the
// chat transcript tailed by its pre-assigned ID, and the reply as the summary.
func TestLiveCursorOneshot(t *testing.T) {
	workdir, r, rec, ev := liveSetup(t, "KONSERT_LIVE_CURSOR", &drivers.CursorDriver{})
	t.Cleanup(func() { removeCursorStateFor(t, workdir) })
	runLiveOneshot(t, r, rec, ev, protocol.TaskStartPayload{
		TaskID: "22222222-6eaf-4000-8000-000000000002", Agent: "cursor", Workdir: workdir,
		Prompt: "reply with the single word OK", Mode: protocol.TaskModeOneshot,
		PermissionMode: protocol.PermissionSandboxed, TimeoutSeconds: 180,
	})
	if !ev.has("milestone", "Turn completed") {
		t.Fatalf("expected a turn milestone from the chat transcript")
	}
}

var copilotCwdLine = regexp.MustCompile(`(?m)^cwd:\s*(.+)$`)

// removeCopilotSessionsFor deletes $COPILOT_HOME/session-state/<id> directories whose
// workspace.yaml records the test's workdir.
func removeCopilotSessionsFor(t *testing.T, workdir string) {
	home := os.Getenv("COPILOT_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return
		}
		home = filepath.Join(h, ".copilot")
	}
	base := filepath.Join(home, "session-state")
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(base, e.Name(), "workspace.yaml"))
		if err != nil {
			continue
		}
		m := copilotCwdLine.FindSubmatch(raw)
		if m == nil || filepath.Clean(strings.TrimSpace(string(m[1]))) != workdir {
			continue
		}
		if err := os.RemoveAll(filepath.Join(base, e.Name())); err != nil {
			t.Logf("cleanup %s: %v", e.Name(), err)
		}
	}
}

var cursorShortSlug = regexp.MustCompile(`^(.{20,})-[0-9a-f]{7}$`)

// removeCursorStateFor deletes cursor-agent's per-workspace state for the test's workdir:
// projects/<slug> (transcripts, trust marker), the shortened projects/<slug prefix>-<hash>
// directory holding its worker.sock (Unix socket paths are length-limited), and
// chats/<md5(path)> (chat stores).
func removeCursorStateFor(t *testing.T, workdir string) {
	dataDir := os.Getenv("CURSOR_DATA_DIR")
	if dataDir == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return
		}
		dataDir = filepath.Join(h, ".cursor")
	}
	slug := strings.Trim(regexp.MustCompile(`[^a-zA-Z0-9]+`).ReplaceAllString(workdir, "-"), "-")
	if slug == "" {
		return
	}
	sum := md5.Sum([]byte(workdir))
	targets := []string{filepath.Join(dataDir, "chats", hex.EncodeToString(sum[:]))}
	entries, _ := os.ReadDir(filepath.Join(dataDir, "projects"))
	for _, e := range entries {
		name := e.Name()
		if m := cursorShortSlug.FindStringSubmatch(name); name == slug || (m != nil && strings.HasPrefix(slug, m[1])) {
			targets = append(targets, filepath.Join(dataDir, "projects", name))
		}
	}
	for _, p := range targets {
		if err := os.RemoveAll(p); err != nil {
			t.Logf("cleanup %s: %v", p, err)
		}
	}
}
