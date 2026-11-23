package worker

import (
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
)

func chatEvent(t *testing.T, env protocol.Envelope) protocol.AgentChatEvent {
	t.Helper()
	ev, err := protocol.DecodePayload[protocol.AgentChatEvent](env)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func isChatEvent(pred func(protocol.AgentChatEvent) bool) func(protocol.Envelope) bool {
	return func(e protocol.Envelope) bool {
		if e.Type != protocol.MsgAgentChatEvent {
			return false
		}
		ev, err := protocol.DecodePayload[protocol.AgentChatEvent](e)
		return err == nil && pred(ev)
	}
}

func capsWith(pred func(protocol.AgentCapabilitiesReport) bool) func(protocol.Envelope) bool {
	return func(e protocol.Envelope) bool {
		if e.Type != protocol.MsgAgentCapabilities {
			return false
		}
		rep, err := protocol.DecodePayload[protocol.AgentCapabilitiesReport](e)
		return err == nil && pred(rep)
	}
}

func accountByID(rep protocol.AgentCapabilitiesReport, id string) (protocol.AgentAccount, bool) {
	for _, a := range rep.Accounts {
		if a.ID == id {
			return a, true
		}
	}
	return protocol.AgentAccount{}, false
}

func TestDaemonAgentChatRouting(t *testing.T) {
	hub := newFakeHub(t)
	startTestDaemon(t, hub, nil)
	c := hub.accept(t)
	c.waitFor(t, 5*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))

	// Capabilities follow hello: the mock accounts with models and usage.
	capEnv := c.waitFor(t, 10*time.Second, "agent.capabilities", isType(protocol.MsgAgentCapabilities))
	rep, err := protocol.DecodePayload[protocol.AgentCapabilitiesReport](capEnv)
	if err != nil {
		t.Fatal(err)
	}
	mock2, ok := accountByID(rep, "mock2")
	if !ok || !mock2.ChatSupported || len(mock2.Models) == 0 || len(mock2.Approvals) == 0 || len(mock2.Usage) == 0 || mock2.Label != "Mock 2" {
		t.Fatalf("capabilities = %+v", rep)
	}

	// Open + prompt → streamed events ending in turn_end, with per-chat seq.
	c.send(t, protocol.MsgAgentChatOpen, "", protocol.AgentChatOpen{ChatID: "chat-1", Agent: "mock", AccountID: "mock2", Cwd: t.TempDir()})
	c.send(t, protocol.MsgAgentChatPrompt, "", protocol.AgentChatPrompt{ChatID: "chat-1", TurnID: "turn-1", Text: "hello"})
	var last int64
	for {
		env := c.waitFor(t, 10*time.Second, "chat event", isType(protocol.MsgAgentChatEvent))
		ev := chatEvent(t, env)
		if ev.ChatID != "chat-1" || ev.Seq <= last {
			t.Fatalf("bad event order/stamp: %+v (last seq %d)", ev, last)
		}
		last = ev.Seq
		if ev.Kind == protocol.EventTurnEnd {
			if ev.TurnID != "turn-1" || ev.StopReason != "end_turn" {
				t.Fatalf("turn_end = %+v", ev)
			}
			break
		}
	}

	// Permission elicitation → respond.
	c.send(t, protocol.MsgAgentChatPrompt, "", protocol.AgentChatPrompt{ChatID: "chat-1", TurnID: "turn-2", Text: "ask permission"})
	elEnv := c.waitFor(t, 10*time.Second, "elicitation", isChatEvent(func(e protocol.AgentChatEvent) bool { return e.Kind == protocol.EventElicitation }))
	el := chatEvent(t, elEnv).Elicitation
	c.send(t, protocol.MsgAgentChatRespond, "", protocol.AgentChatRespond{ChatID: "chat-1", ElicitationID: el.ID, OptionID: "reject_once"})
	c.waitFor(t, 10*time.Second, "turn-2 end", isChatEvent(func(e protocol.AgentChatEvent) bool {
		return e.Kind == protocol.EventTurnEnd && e.TurnID == "turn-2"
	}))

	// Usage limit → usage_limit error, then capabilities pushed with the window exhausted.
	c.send(t, protocol.MsgAgentChatPrompt, "", protocol.AgentChatPrompt{ChatID: "chat-1", TurnID: "turn-3", Text: "limit"})
	errEnv := c.waitFor(t, 10*time.Second, "usage_limit", isChatEvent(func(e protocol.AgentChatEvent) bool {
		return e.Kind == protocol.EventError && e.Error != nil && e.Error.Kind == protocol.ErrKindUsageLimit
	}))
	if ev := chatEvent(t, errEnv); ev.Error.Window != "five_hour" || ev.Error.ResetsAt == nil {
		t.Fatalf("usage_limit = %+v", ev.Error)
	}
	c.waitFor(t, 10*time.Second, "exhausted capabilities", capsWith(func(r protocol.AgentCapabilitiesReport) bool {
		a, ok := accountByID(r, "mock2")
		if !ok {
			return false
		}
		for _, u := range a.Usage {
			if u.Window == "five_hour" && u.State == "exhausted" && u.ResetsAt != nil {
				return true
			}
		}
		return false
	}))

	// An explicit refresh re-probes: the mock now also reports seven_day exhausted.
	c.send(t, protocol.MsgAgentCapabilitiesRefresh, "", map[string]string{"account_id": "mock2"})
	c.waitFor(t, 10*time.Second, "refreshed capabilities", capsWith(func(r protocol.AgentCapabilitiesReport) bool {
		a, _ := accountByID(r, "mock2")
		for _, u := range a.Usage {
			if u.Window == "seven_day" && u.State == "exhausted" {
				return true
			}
		}
		return false
	}))

	// Control + close are routed; a closed chat resumes on the next prompt.
	c.send(t, protocol.MsgAgentChatControl, "", protocol.AgentChatControl{ChatID: "chat-1", Model: "mock-fast"})
	c.waitFor(t, 10*time.Second, "model change", isChatEvent(func(e protocol.AgentChatEvent) bool {
		return e.Kind == protocol.EventSessionReady && e.Model == "mock-fast"
	}))
	c.send(t, protocol.MsgAgentChatClose, "", protocol.AgentChatClose{ChatID: "chat-1"})
	c.waitFor(t, 10*time.Second, "idle after close", isChatEvent(func(e protocol.AgentChatEvent) bool {
		return e.Kind == protocol.EventStatus && e.Status == protocol.ChatStatusIdle
	}))
	c.send(t, protocol.MsgAgentChatPrompt, "", protocol.AgentChatPrompt{ChatID: "chat-1", TurnID: "turn-4", Text: "hi again"})
	c.waitFor(t, 10*time.Second, "resumed turn end", isChatEvent(func(e protocol.AgentChatEvent) bool {
		return e.Kind == protocol.EventTurnEnd && e.TurnID == "turn-4"
	}))
}

func TestDaemonBuffersChatEventsWhileDisconnected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d := NewDaemon(config.WorkerConfig{WorkerID: "buf-worker", Name: "buf-worker", EnableMockDriver: true}, nil, nil)
	defer d.shutdownChat()
	d.sendChatEvent(protocol.AgentChatEvent{ChatID: "c", Seq: 1, Kind: protocol.EventMessageDelta})
	d.sendChatEvent(protocol.AgentChatEvent{ChatID: "c", Seq: 2, Kind: protocol.EventTurnEnd})
	q := newConnQueues()
	d.attachQueues(q)
	for want := int64(1); want <= 2; want++ {
		msg, ok := q.popControl()
		if !ok {
			t.Fatalf("event %d not flushed", want)
		}
		env, err := protocol.DecodeEnvelope(msg.data)
		if err != nil {
			t.Fatal(err)
		}
		if ev := chatEvent(t, env); ev.Seq != want {
			t.Fatalf("flushed seq %d, want %d", ev.Seq, want)
		}
	}
}
