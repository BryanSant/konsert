package worker

import (
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
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
	// Exhausted windows persist in the state dir's usage cache: a fresh one keeps an
	// earlier run (-count=N) from pre-exhausting mock2, which suppresses the push below.
	t.Setenv(StateDirEnv, t.TempDir())
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
	// The capabilities push (triggered by the rate_limit event) and the error event
	// race each other, so wait for both in either order.
	c.send(t, protocol.MsgAgentChatPrompt, "", protocol.AgentChatPrompt{ChatID: "chat-1", TurnID: "turn-3", Text: "limit"})
	isUsageLimit := isChatEvent(func(e protocol.AgentChatEvent) bool {
		return e.Kind == protocol.EventError && e.Error != nil && e.Error.Kind == protocol.ErrKindUsageLimit
	})
	isExhausted := capsWith(func(r protocol.AgentCapabilitiesReport) bool {
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
	})
	var errEnv *protocol.Envelope
	sawExhausted := false
	for errEnv == nil || !sawExhausted {
		env := c.waitFor(t, 10*time.Second, "usage_limit error and exhausted capabilities", func(e protocol.Envelope) bool {
			return (errEnv == nil && isUsageLimit(e)) || (!sawExhausted && isExhausted(e))
		})
		if isUsageLimit(env) {
			errEnv = &env
		} else {
			sawExhausted = true
		}
	}
	if ev := chatEvent(t, *errEnv); ev.Error.Window != "five_hour" || ev.Error.ResetsAt == nil {
		t.Fatalf("usage_limit = %+v", ev.Error)
	}

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

func TestDaemonSpoolsChatEventsWhileDisconnected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(StateDirEnv, t.TempDir())
	d := NewDaemon(config.WorkerConfig{WorkerID: "buf-worker", Name: "buf-worker", EnableMockDriver: true}, nil, nil)
	defer d.shutdown()
	d.sendChatEvent(protocol.AgentChatEvent{ChatID: "c", Seq: 1, Kind: protocol.EventMessageDelta})
	d.sendChatEvent(protocol.AgentChatEvent{ChatID: "c", Seq: 2, Kind: protocol.EventTurnEnd})
	recs, _ := d.spool.Read(1, 10, 1<<20)
	if len(recs) != 2 {
		t.Fatalf("spooled %d events, want 2", len(recs))
	}
	for i, r := range recs {
		env, err := protocol.DecodeEnvelope(r.Data)
		if err != nil {
			t.Fatal(err)
		}
		if ev := chatEvent(t, env); ev.Seq != int64(i+1) {
			t.Fatalf("spooled chat seq %d, want %d", ev.Seq, i+1)
		}
	}
}
