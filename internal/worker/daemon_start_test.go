package worker

import (
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// task.start used to run synchronously on the WebSocket read loop, so a slow `git worktree
// add` stalled stdin, cancel, resize and ping handling.
func TestDaemonReadLoopResponsiveDuringSlowWorktree(t *testing.T) {
	repo := initGitRepo(t)
	entered, release := stubSlowWorktree(t)
	hub := newFakeHub(t)
	startTestDaemon(t, hub, nil)
	c := hub.accept(t)
	c.waitFor(t, 10*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))

	const id = "t-by2aj"
	c.send(t, protocol.MsgTaskStart, "", protocol.TaskStartPayload{
		TaskID: id, Agent: "mock", Workdir: repo, Branch: "slow", UseWorktree: true,
		Prompt: "EXIT=0", Mode: protocol.TaskModeOneshot,
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worktree stub never entered")
	}

	// While the worktree stub blocks, the read loop must still serve requests.
	c.send(t, protocol.MsgFSList, "fs-1", protocol.FSListRequest{Path: repo})
	c.waitFor(t, 3*time.Second, "fs.list.result during slow setup", func(e protocol.Envelope) bool {
		return e.Type == protocol.MsgFSListResult && e.ID == "fs-1"
	})

	release()
	c.waitFor(t, 10*time.Second, "task completion", func(e protocol.Envelope) bool {
		if e.Type != protocol.MsgTaskStatus {
			return false
		}
		p, err := protocol.DecodePayload[protocol.TaskStatusPayload](e)
		return err == nil && p.TaskID == id && protocol.IsTerminalTaskStatus(p.Status)
	})
}
