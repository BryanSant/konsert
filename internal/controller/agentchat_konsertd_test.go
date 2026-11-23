package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/BryanSant/konsert/internal/worker"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

// TestAgentChatWithRealKonsertdMockEngine drives a real konsertd (with
// --enable-mock-driver) through the hub using only the agent chat protocol. It
// skips until konsertd reports a chat-capable "mock" account (the worker's mock
// chat engine). The mock engine reacts to prompt keywords: "permission", "ask",
// "fail auth", "limit", "crash", "slow".
func TestAgentChatWithRealKonsertdMockEngine(t *testing.T) {
	cs := newChatServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wCfg := config.DefaultWorkerConfig()
	wCfg.ControllerURL = cs.wsURL("/ws/worker")
	wCfg.WorkerID = "worker-chat-e2e"
	wCfg.Name = "chat-e2e"
	wCfg.HostGroup = "chat-e2e"
	wCfg.EnableMockDriver = true
	daemon := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = daemon.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	var mockAcct *AgentAccountView
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && mockAcct == nil {
		views, _ := cs.h.agentAccountViews(ctx, "worker-chat-e2e")
		for i := range views {
			if views[i].Agent == "mock" && views[i].ChatSupported {
				mockAcct = &views[i]
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mockAcct == nil {
		t.Skip("konsertd did not report a chat-capable mock account (mock chat engine not available in this build)")
	}

	b := cs.dialBrowser(t)
	chat := cs.createChat(t, map[string]any{"worker_id": "worker-chat-e2e", "agent": "mock", "account_id": mockAcct.ID})

	// waitTurn collects events until the turn ends (turn_end or error) and returns them.
	waitTurn := func(onEvent func(protocol.AgentChatEvent)) []protocol.AgentChatEvent {
		t.Helper()
		var evs []protocol.AgentChatEvent
		until := time.Now().Add(20 * time.Second)
		for time.Now().Before(until) {
			env := b.expect(protocol.MsgAgentChatEvent, time.Until(until))
			ev, _ := protocol.DecodePayload[protocol.AgentChatEvent](env)
			if ev.ChatID != chat.ID {
				continue
			}
			evs = append(evs, ev)
			if onEvent != nil {
				onEvent(ev)
			}
			if ev.Kind == protocol.EventTurnEnd || ev.Kind == protocol.EventError {
				return evs
			}
		}
		t.Fatalf("turn did not end; events: %+v", evs)
		return nil
	}
	send := func(text string) {
		b.send(protocol.MsgChatSend, chatSendRequest{SessionID: chat.ID, Content: text})
	}
	last := func(evs []protocol.AgentChatEvent) protocol.AgentChatEvent { return evs[len(evs)-1] }

	send("hello")
	if ev := last(waitTurn(nil)); ev.Kind != protocol.EventTurnEnd {
		t.Fatalf("hello turn ended with %+v", ev)
	}
	if s := mustSession(t, cs.st, chat.ID); s.Status != protocol.ChatStatusDone {
		t.Fatalf("status after hello: %q", s.Status)
	}

	// Elicitations (permission from the engine, a question for "ask") are answered
	// with the first option, or free text.
	answer := func(ev protocol.AgentChatEvent) {
		if ev.Kind != protocol.EventElicitation || ev.Elicitation == nil {
			return
		}
		req := chatRespondRequest{SessionID: chat.ID, ElicitationID: ev.Elicitation.ID, Text: "yes"}
		if len(ev.Elicitation.Options) > 0 {
			req.OptionID = ev.Elicitation.Options[0].ID
		}
		b.send(msgChatRespond, req)
	}
	for _, kw := range []string{"permission", "ask"} {
		send(kw)
		evs := waitTurn(answer)
		sawElicit := false
		for _, ev := range evs {
			sawElicit = sawElicit || ev.Kind == protocol.EventElicitation
		}
		if !sawElicit || last(evs).Kind != protocol.EventTurnEnd {
			t.Fatalf("%q turn: elicitation=%v last=%+v", kw, sawElicit, last(evs))
		}
	}

	for kw, kind := range map[string]string{"fail auth": protocol.ErrKindAuth, "limit": protocol.ErrKindUsageLimit, "crash": protocol.ErrKindExit} {
		send(kw)
		ev := last(waitTurn(nil))
		if ev.Kind != protocol.EventError || ev.Error == nil || ev.Error.Kind != kind {
			t.Fatalf("%q: want error %s, got %+v", kw, kind, ev)
		}
		if s := mustSession(t, cs.st, chat.ID); s.Status != protocol.ChatStatusFailed || !strings.Contains(s.LastErrorJSON, `"`+kind+`"`) {
			t.Fatalf("%q: session %+v", kw, s)
		}
	}

	// "slow" can be cancelled.
	send("slow")
	time.Sleep(300 * time.Millisecond)
	b.send(msgChatControl, chatControlRequest{SessionID: chat.ID, Cancel: true})
	ev := last(waitTurn(nil))
	if ev.Kind == protocol.EventTurnEnd && ev.StopReason != "cancelled" {
		t.Fatalf("slow turn after cancel: %+v", ev)
	}

	// The transcript is persisted and coalesced.
	code, raw := cs.api(t, http.MethodGet, "/api/chats/"+chat.ID+"/events", nil)
	var tr struct {
		Events []store.ChatEvent `json:"events"`
	}
	_ = json.Unmarshal(raw, &tr)
	if code != 200 || len(tr.Events) == 0 {
		t.Fatalf("transcript: %d %s", code, raw)
	}
}
