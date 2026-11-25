package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/commander"
	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
)

// blockingProvider streams one delta and then blocks until released or until
// the turn context ends, recording concurrency and cancellations.
type blockingProvider struct {
	release  chan struct{}
	started  chan struct{}
	canceled chan struct{}

	mu        sync.Mutex
	active    int
	maxActive int
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{
		release:  make(chan struct{}, 16),
		started:  make(chan struct{}, 16),
		canceled: make(chan struct{}, 16),
	}
}

func (p *blockingProvider) Name() string { return "blocking-test" }

func (p *blockingProvider) Complete(ctx context.Context, _, _ string, _ []commander.Message, _ []commander.ToolDef, onDelta func(string)) (commander.Message, error) {
	p.mu.Lock()
	p.active++
	if p.active > p.maxActive {
		p.maxActive = p.active
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.active--
		p.mu.Unlock()
	}()
	p.started <- struct{}{}
	if onDelta != nil {
		onDelta("partial")
	}
	select {
	case <-p.release:
		return commander.Message{Role: "assistant", Content: "done"}, nil
	case <-ctx.Done():
		p.canceled <- struct{}{}
		return commander.Message{}, ctx.Err()
	}
}

func (p *blockingProvider) max() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxActive
}

func newChatTestHub(t *testing.T) (*testHub, *blockingProvider) {
	t.Helper()
	th := newTestHub(t)
	orch := commander.NewService(config.CommanderConfig{Provider: "test", Model: "m", APIKey: "k"}, th.st, th.svc)
	prov := newBlockingProvider()
	orch.SetProvider(prov)
	th.h.SetCommander(orch)
	return th, prov
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestChatEventsGoOnlyToSessionSubscribers(t *testing.T) {
	th, prov := newChatTestHub(t)
	sender := th.dialBrowser(t)
	other := th.dialBrowser(t)
	watcher := th.dialBrowser(t)
	watcher.send(msgChatSubscribe, chatSubscribePayload{SessionID: "s1"})
	time.Sleep(50 * time.Millisecond) // let the subscribe frame land first

	sender.send(protocol.MsgChatSend, protocol.ChatSendPayload{SessionID: "s1", Content: "hi"})
	waitSignal(t, prov.started, "turn to start")
	prov.release <- struct{}{}

	for name, fw := range map[string]*fakeWorker{"sender": sender, "watcher": watcher} {
		env := fw.expect(protocol.MsgChatDelta, 3*time.Second)
		if p, _ := protocol.DecodePayload[protocol.ChatDeltaPayload](env); p.SessionID != "s1" {
			t.Fatalf("%s got delta for %q", name, p.SessionID)
		}
		fw.expect(protocol.MsgChatDone, 3*time.Second)
	}
	for _, typ := range []string{protocol.MsgChatDelta, protocol.MsgChatDone, protocol.MsgChatTool} {
		if got := other.drain(typ, 100*time.Millisecond); len(got) != 0 {
			t.Fatalf("unsubscribed client received %d %s frames", len(got), typ)
		}
	}
}

func TestChatTurnsAreSerializedPerSession(t *testing.T) {
	th, prov := newChatTestHub(t)
	c := th.dialBrowser(t)
	for i := 0; i < 3; i++ {
		c.send(protocol.MsgChatSend, protocol.ChatSendPayload{SessionID: "s1", Content: "msg"})
	}
	for i := 0; i < 3; i++ {
		waitSignal(t, prov.started, "turn to start")
		// The next turn must not start while this one is running.
		select {
		case <-prov.started:
			t.Fatal("a second turn of the same session started concurrently")
		case <-time.After(100 * time.Millisecond):
		}
		prov.release <- struct{}{}
		c.expect(protocol.MsgChatDone, 3*time.Second)
	}
	if prov.max() != 1 {
		t.Fatalf("max concurrent turns for one session = %d, want 1", prov.max())
	}
	waitFor(t, 2*time.Second, "chat session state to be released", func() bool {
		th.h.mu.RLock()
		defer th.h.mu.RUnlock()
		return len(th.h.chatSessions) == 0
	})
}

func TestChatTurnsBeyondLimitAreRejected(t *testing.T) {
	th, prov := newChatTestHub(t)
	c := th.dialBrowser(t)
	for i := 0; i < maxChatTurnsPerSession+1; i++ {
		c.send(protocol.MsgChatSend, protocol.ChatSendPayload{SessionID: "s1", Content: "spam"})
	}
	waitSignal(t, prov.started, "first turn to start")
	env := c.expect(protocol.MsgChatError, 3*time.Second)
	if p, _ := protocol.DecodePayload[protocol.ChatErrorPayload](env); p.SessionID != "s1" {
		t.Fatalf("unexpected error payload %+v", p)
	}
	for i := 0; i < maxChatTurnsPerSession; i++ {
		prov.release <- struct{}{}
	}
}

func TestChatTurnCanceledWhenClientDisconnects(t *testing.T) {
	th, prov := newChatTestHub(t)
	c := th.dialBrowser(t)
	c.send(protocol.MsgChatSend, protocol.ChatSendPayload{SessionID: "s1", Content: "long"})
	waitSignal(t, prov.started, "turn to start")
	c.close()
	waitSignal(t, prov.canceled, "turn to be canceled after disconnect")
}

func TestChatTurnCanceledOnShutdownAndTimeout(t *testing.T) {
	th, prov := newChatTestHub(t)
	c := th.dialBrowser(t)
	c.send(protocol.MsgChatSend, protocol.ChatSendPayload{SessionID: "s1", Content: "long"})
	waitSignal(t, prov.started, "turn to start")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := th.h.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown did not drain: %v", err)
	}
	waitSignal(t, prov.canceled, "turn to be canceled on shutdown")

	th2, prov2 := newChatTestHub(t)
	th2.h.ChatTurnTimeout = 200 * time.Millisecond
	c2 := th2.dialBrowser(t)
	c2.send(protocol.MsgChatSend, protocol.ChatSendPayload{SessionID: "s2", Content: "slow"})
	waitSignal(t, prov2.started, "turn to start")
	waitSignal(t, prov2.canceled, "turn to hit its deadline")
	c2.expect(protocol.MsgChatError, 3*time.Second)
}
