package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

func TestDaemonRefusesWorkThatBreaksADirective(t *testing.T) {
	hub := newFakeHub(t)
	startTestDaemon(t, hub, nil)
	c := hub.accept(t)
	c.waitFor(t, 10*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))

	past := time.Now().Add(-time.Minute)
	sendDirectives(t, c, protocol.WorkerDirective{
		ID: "d-past", Kind: protocol.DirectiveKindMachinePause, Until: &past,
	})
	c.send(t, protocol.MsgTaskStart, "", protocol.TaskStartPayload{
		TaskID: "t-past1", Agent: "mock", Workdir: t.TempDir(), Prompt: "EXIT=0", Mode: protocol.TaskModeOneshot,
	})
	if st := waitTaskTerminal(t, c, "t-past1"); strings.Contains(st.Summary, "paused") {
		t.Fatalf("a until in the past still paused the machine: %s", st.Summary)
	}

	sendDirectives(t, c, protocol.WorkerDirective{ID: "d-pause", Kind: protocol.DirectiveKindMachinePause})
	c.send(t, protocol.MsgTaskStart, "", protocol.TaskStartPayload{
		TaskID: "t-pause1", Agent: "mock", Workdir: t.TempDir(), Prompt: "EXIT=0", Mode: protocol.TaskModeOneshot,
	})
	assertRefused(t, c, "t-pause1", "paused")

	sendDirectives(t, c, protocol.WorkerDirective{ID: "d-drain", Kind: protocol.DirectiveKindMachineDrain})
	c.send(t, protocol.MsgTaskStart, "", protocol.TaskStartPayload{
		TaskID: "t-drain1", Agent: "mock", Workdir: t.TempDir(), Prompt: "EXIT=0", Mode: protocol.TaskModeOneshot,
	})
	assertRefused(t, c, "t-drain1", "draining")

	zero := 0
	sendDirectives(t, c, protocol.WorkerDirective{
		ID: "d-zero", Kind: protocol.DirectiveKindMachineThrottle,
		Params: protocol.WorkerDirectiveParams{MaxConcurrency: &zero},
	})
	c.send(t, protocol.MsgTaskStart, "", protocol.TaskStartPayload{
		TaskID: "t-zero1", Agent: "mock", Workdir: t.TempDir(), Prompt: "EXIT=0", Mode: protocol.TaskModeOneshot,
	})
	assertRefused(t, c, "t-zero1", "throttled")

	room := 5
	sendDirectives(t, c, protocol.WorkerDirective{
		ID: "d-room", Kind: protocol.DirectiveKindMachineThrottle,
		Params: protocol.WorkerDirectiveParams{MaxConcurrency: &room},
	})
	c.send(t, protocol.MsgTaskStart, "", protocol.TaskStartPayload{
		TaskID: "t-room1", Agent: "mock", Workdir: t.TempDir(), Prompt: "EXIT=0", Mode: protocol.TaskModeOneshot,
	})
	// The cap-1 case below needs this machine idle. The first status is "running",
	// which on Windows arrives while the ConPTY task is still hosted.
	if st := waitTaskTerminal(t, c, "t-room1"); strings.Contains(st.Summary, "throttled") {
		t.Fatalf("roomy cap refused an idle machine: %s", st.Summary)
	}

	// The first start reserves a slot before the next message is read.
	repo := initGitRepo(t)
	entered, release := stubSlowWorktree(t)
	one := 1
	sendDirectives(t, c, protocol.WorkerDirective{
		ID: "d-one", Kind: protocol.DirectiveKindMachineThrottle,
		Params: protocol.WorkerDirectiveParams{MaxConcurrency: &one},
	})
	c.send(t, protocol.MsgTaskStart, "", protocol.TaskStartPayload{
		TaskID: "t-held1", Agent: "mock", Workdir: repo, Branch: "held", UseWorktree: true,
		Prompt: "EXIT=0", Mode: protocol.TaskModeOneshot,
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first start never reached worktree setup")
	}
	c.send(t, protocol.MsgTaskStart, "", protocol.TaskStartPayload{
		TaskID: "t-cap1", Agent: "mock", Workdir: t.TempDir(), Prompt: "EXIT=0", Mode: protocol.TaskModeOneshot,
	})
	assertRefused(t, c, "t-cap1", "throttled")
	release()
	// Let the held task finish so its worktree and process release the temp repo;
	// Windows can't remove a directory that is still in use.
	waitTaskTerminal(t, c, "t-held1")

	sendDirectives(t, c, protocol.WorkerDirective{ID: "d-chat", Kind: protocol.DirectiveKindMachinePause})
	c.send(t, protocol.MsgAgentChatOpen, "", protocol.AgentChatOpen{ChatID: "c-dir1", Agent: "mock"})
	evEnv := c.waitFor(t, 5*time.Second, "chat error", func(e protocol.Envelope) bool {
		if e.Type != protocol.MsgAgentChatEvent {
			return false
		}
		p, err := protocol.DecodePayload[protocol.AgentChatEvent](e)
		return err == nil && p.ChatID == "c-dir1" && p.Kind == protocol.EventError
	})
	ev, err := protocol.DecodePayload[protocol.AgentChatEvent](evEnv)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != protocol.ChatStatusFailed || ev.Error == nil || ev.Error.Kind != protocol.ErrKindOther || !strings.Contains(ev.Error.Message, "paused") {
		t.Fatalf("chat refusal: %+v", ev)
	}
}

func sendDirectives(t *testing.T, c *hubConn, dirs ...protocol.WorkerDirective) {
	t.Helper()
	c.send(t, protocol.MsgWorkerDirectives, "", protocol.WorkerDirectivesPayload{Directives: dirs})
}

func waitTaskStatus(t *testing.T, c *hubConn, id string) protocol.TaskStatusPayload {
	t.Helper()
	return waitTaskStatusUntil(t, c, id, 10*time.Second, func(protocol.TaskStatusPayload) bool { return true })
}

// waitTaskTerminal waits until id reports a terminal status. A task emits "running"
// as soon as its PTY starts, and on Windows that process can still be hosted when
// the next assertion runs.
func waitTaskTerminal(t *testing.T, c *hubConn, id string) protocol.TaskStatusPayload {
	t.Helper()
	return waitTaskStatusUntil(t, c, id, 15*time.Second, func(p protocol.TaskStatusPayload) bool {
		return protocol.IsTerminalTaskStatus(p.Status)
	})
}

func waitTaskStatusUntil(t *testing.T, c *hubConn, id string, timeout time.Duration, done func(protocol.TaskStatusPayload) bool) protocol.TaskStatusPayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last protocol.TaskStatusPayload
	for time.Now().Before(deadline) {
		env := c.waitFor(t, time.Until(deadline), "task.status "+id, func(e protocol.Envelope) bool {
			if e.Type != protocol.MsgTaskStatus {
				return false
			}
			p, err := protocol.DecodePayload[protocol.TaskStatusPayload](e)
			return err == nil && p.TaskID == id
		})
		p, err := protocol.DecodePayload[protocol.TaskStatusPayload](env)
		if err != nil {
			t.Fatal(err)
		}
		last = p
		if done(p) {
			return p
		}
	}
	t.Fatalf("task %s did not reach the expected status, last %+v", id, last)
	return last
}

func assertRefused(t *testing.T, c *hubConn, id, word string) {
	t.Helper()
	st := waitTaskStatus(t, c, id)
	if st.Status != protocol.TaskStatusFailed || !strings.Contains(st.Summary, word) {
		t.Fatalf("task %s: status %s summary %q, want failed containing %q", id, st.Status, st.Summary, word)
	}
}
