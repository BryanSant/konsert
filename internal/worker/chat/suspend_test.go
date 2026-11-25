package chat

import (
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// Suspending a chat mid-turn freezes the engine (no events), keeps the watchdog quiet
// however long it stays suspended, refuses new turns, and resuming continues the turn.
func TestManagerSuspendResumeMidTurn(t *testing.T) {
	tm := newTestManager(t, func(o *ManagerOptions) { o.TurnWatchdog = time.Minute })
	tm.mock.SlowDuration = 2 * time.Second
	cwd := t.TempDir()
	tm.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: MockAgent, Cwd: cwd})
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t1", Text: "slow"})
	_, i := tm.col.waitFor(t, 0, "first delta", kind(protocol.EventMessageDelta))

	tm.Suspend("c1")
	_, si := tm.col.waitFor(t, i, "suspended status", func(e protocol.AgentChatEvent) bool {
		return e.Kind == protocol.EventStatus && e.Status == protocol.ChatStatusSuspended
	})
	// A delta already in flight may land; after that the engine is frozen.
	time.Sleep(300 * time.Millisecond)
	frozenAt := len(tm.col.all())
	time.Sleep(700 * time.Millisecond)
	for _, e := range tm.col.all()[frozenAt:] {
		if e.Kind == protocol.EventMessageDelta {
			t.Fatalf("suspended engine kept emitting: %+v", e)
		}
	}

	// The watchdog never fires while suspended, however long.
	tm.clock.Advance(time.Hour)
	tm.Sweep()
	for _, e := range tm.col.all()[si:] {
		if e.Kind == protocol.EventError {
			t.Fatalf("watchdog or error while suspended: %+v", e)
		}
	}
	// No new turn while suspended.
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t2", Text: "hello"})
	tm.col.waitFor(t, si, "refusal", func(e protocol.AgentChatEvent) bool {
		return e.Kind == protocol.EventError && e.TurnID == "t2"
	})
	if info := tm.chatInfo("c1"); !info.TurnActive || info.TurnID != "t1" {
		t.Fatalf("suspended turn lost: %+v", info)
	}

	tm.Resume("c1")
	_, ri := tm.col.waitFor(t, si, "working again", func(e protocol.AgentChatEvent) bool {
		return e.Kind == protocol.EventStatus && e.Status == protocol.ChatStatusWorking
	})
	tm.col.waitFor(t, ri, "deltas resume", kind(protocol.EventMessageDelta))
	end, _ := tm.col.waitFor(t, ri, "turn_end", kind(protocol.EventTurnEnd))
	if end.TurnID != "t1" || end.StopReason != "end_turn" {
		t.Fatalf("resumed turn did not finish normally: %+v", end)
	}
	// Resume restarted the watchdog clock: no stale timeout right after.
	tm.Sweep()
	for _, e := range tm.col.all()[ri:] {
		if e.Kind == protocol.EventError && e.Error != nil && e.Error.Kind == protocol.ErrKindTimeout {
			t.Fatalf("watchdog fired after resume: %+v", e)
		}
	}
}

// Suspending an idle chat (no engine running) is remembered: the next prompt is refused
// until it is resumed.
func TestManagerSuspendIdleChatRefusesPrompt(t *testing.T) {
	tm := newTestManager(t, nil)
	tm.Open(protocol.AgentChatOpen{ChatID: "c2", Agent: MockAgent, Cwd: t.TempDir()})
	tm.Suspend("c2")
	tm.col.waitFor(t, 0, "suspended", func(e protocol.AgentChatEvent) bool {
		return e.Kind == protocol.EventStatus && e.Status == protocol.ChatStatusSuspended
	})
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c2", TurnID: "t1", Text: "hello"})
	tm.col.waitFor(t, 0, "refusal", func(e protocol.AgentChatEvent) bool { return e.Kind == protocol.EventError && e.TurnID == "t1" })
	tm.Resume("c2")
	n := len(tm.col.all())
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c2", TurnID: "t2", Text: "hello"})
	tm.col.waitFor(t, n, "turn_end", func(e protocol.AgentChatEvent) bool { return e.Kind == protocol.EventTurnEnd && e.TurnID == "t2" })
}
