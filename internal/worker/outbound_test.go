package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/coder/websocket"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Control messages are never dropped under PTY backpressure; PTY data frames are.
func TestConnQueuesPriorityAndDropPolicy(t *testing.T) {
	q := newConnQueues()
	const ptyFrames, controls = 5000, 3000
	ptyQueued := 0
	for i := 0; i < ptyFrames; i++ {
		if q.push(outboundMsg{msgType: websocket.MessageBinary, data: []byte{protocol.FramePtyData}, droppable: true}) {
			ptyQueued++
		}
		if i < controls {
			if !q.push(outboundMsg{msgType: websocket.MessageText, data: []byte(`{"type":"task.event"}`)}) {
				t.Fatalf("control message %d was refused", i)
			}
		}
	}
	if ptyQueued != ptyQueueSize {
		t.Fatalf("expected %d PTY frames queued (rest dropped), got %d", ptyQueueSize, ptyQueued)
	}
	if got := q.controlLen(); got != controls {
		t.Fatalf("expected all %d control messages kept, got %d", controls, got)
	}

	// The writer emits every control message before any PTY frame, and outbox statuses
	// before PTY frames too.
	d := &Daemon{outbox: newStatusOutbox("", discardLogger()), logger: discardLogger()}
	d.outbox.add(protocol.TaskStatusPayload{TaskID: "t1", Status: protocol.TaskStatusCompleted})
	var order []string
	ctx, cancel := context.WithCancel(context.Background())
	stop := errors.New("stop")
	err := d.writeLoop(ctx, q, func(m outboundMsg) error {
		switch {
		case m.droppable:
			order = append(order, "pty")
		case len(m.data) > 0 && string(m.data[:8]) == `{"type":`:
			env, _ := protocol.DecodeEnvelope(m.data)
			order = append(order, env.Type)
		}
		if len(order) == controls+1+ptyQueueSize {
			cancel()
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("writeLoop: %v", err)
	}
	for i := 0; i < controls; i++ {
		if order[i] != "task.event" {
			t.Fatalf("position %d: expected control message, got %s", i, order[i])
		}
	}
	if order[controls] != protocol.MsgTaskStatus {
		t.Fatalf("expected outbox status after control messages, got %s", order[controls])
	}
	for _, o := range order[controls+1:] {
		if o != "pty" {
			t.Fatalf("expected only PTY frames last, got %s", o)
		}
	}
	if len(d.outbox.pending()) != 0 {
		t.Fatal("written outbox entry not removed")
	}
}

func TestStatusOutboxPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox-w.json")
	o := newStatusOutbox(path, discardLogger())
	o.add(protocol.TaskStatusPayload{TaskID: "a", Status: protocol.TaskStatusFailed})
	o.add(protocol.TaskStatusPayload{TaskID: "b", Status: protocol.TaskStatusCompleted})
	o.add(protocol.TaskStatusPayload{TaskID: "a", Status: protocol.TaskStatusCanceled}) // replaces a

	o2 := newStatusOutbox(path, discardLogger())
	got := o2.pending()
	if len(got) != 2 || got[0].Status.TaskID != "b" || got[1].Status.TaskID != "a" || got[1].Status.Status != protocol.TaskStatusCanceled {
		t.Fatalf("unexpected reloaded outbox: %+v", got)
	}
	select {
	case <-o2.signal:
	default:
		t.Fatal("reloaded non-empty outbox should be signalled for flushing")
	}
	o2.remove(got[0].Seq)
	o2.remove(got[1].Seq)
	if o3 := newStatusOutbox(path, discardLogger()); len(o3.pending()) != 0 {
		t.Fatalf("outbox not cleared on disk: %+v", o3.pending())
	}
}

// A task that finishes while the worker is disconnected has its final status delivered
// right after worker.hello on the next connection.
func TestDaemonRedeliversFinalStatusAfterReconnect(t *testing.T) {
	hub := newFakeHub(t)
	startTestDaemon(t, hub, nil)
	c1 := hub.accept(t)
	c1.waitFor(t, 10*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))

	const id = "t-c5qvs"
	c1.send(t, protocol.MsgTaskStart, "", protocol.TaskStartPayload{
		TaskID: id, Agent: "mock", Workdir: t.TempDir(), Prompt: "SLEEP=1", Mode: protocol.TaskModeOneshot,
	})
	c1.waitFor(t, 10*time.Second, "running status", func(e protocol.Envelope) bool {
		p, err := protocol.DecodePayload[protocol.TaskStatusPayload](e)
		return e.Type == protocol.MsgTaskStatus && err == nil && p.TaskID == id && p.Status == protocol.TaskStatusRunning
	})

	// Drop the connection and refuse the first reconnect so the task ends while disconnected.
	hub.reject.Store(true)
	_ = c1.conn.Close(websocket.StatusGoingAway, "test disconnect")
	time.Sleep(2500 * time.Millisecond)
	hub.reject.Store(false)

	c2 := hub.accept(t)
	first := c2.waitFor(t, 10*time.Second, "first message", func(protocol.Envelope) bool { return true })
	if first.Type != protocol.MsgWorkerHello {
		t.Fatalf("expected worker.hello first on reconnect, got %s", first.Type)
	}
	hello, _ := protocol.DecodePayload[protocol.WorkerHelloPayload](first)
	for _, at := range hello.ActiveTasks {
		if at.TaskID == id {
			t.Fatalf("finished task still listed as active in hello: %+v", at)
		}
	}
	env := c2.waitFor(t, 10*time.Second, "final status after reconnect", func(e protocol.Envelope) bool {
		p, err := protocol.DecodePayload[protocol.TaskStatusPayload](e)
		return e.Type == protocol.MsgTaskStatus && err == nil && p.TaskID == id && protocol.IsTerminalTaskStatus(p.Status)
	})
	p, _ := protocol.DecodePayload[protocol.TaskStatusPayload](env)
	if p.Status != protocol.TaskStatusCompleted || p.ExitCode == nil || *p.ExitCode != 0 {
		t.Fatalf("unexpected final status: %+v", p)
	}
}
