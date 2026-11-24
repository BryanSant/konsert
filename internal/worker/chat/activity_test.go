package chat

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

type activityLog struct {
	mu  sync.Mutex
	all []protocol.SessionActivity
}

func (l *activityLog) add(a protocol.SessionActivity) {
	l.mu.Lock()
	l.all = append(l.all, a)
	l.mu.Unlock()
}

func (l *activityLog) states() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, a := range l.all {
		if len(out) == 0 || out[len(out)-1] != a.State {
			out = append(out, a.State)
		}
	}
	return out
}

func (l *activityLog) seen(state string) bool { return slices.Contains(l.states(), state) }

// The mock engine drives every activity state through the manager (for tests of the hub
// and UI): queued, thinking, working, speaking, done ("activity"), asking ("ask"),
// blocked (suspend) and error ("crash").
func TestMockDrivesEveryActivityState(t *testing.T) {
	log := &activityLog{}
	tm := newTestManager(t, func(o *ManagerOptions) { o.OnActivity = log.add })
	tm.mock.ActivityHold = 5 * time.Millisecond
	cwd := t.TempDir()
	tm.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: MockAgent, Cwd: cwd})

	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t1", Text: "show activity"})
	tm.col.waitFor(t, 0, "turn_end", func(e protocol.AgentChatEvent) bool { return e.Kind == protocol.EventTurnEnd && e.TurnID == "t1" })
	want := []string{protocol.ActivityQueued, protocol.ActivityThinking, protocol.ActivityWorking, protocol.ActivityThinking, protocol.ActivitySpeaking, protocol.ActivityThinking, protocol.ActivityDone}
	waitUntil(t, "done", func() bool { return log.seen(protocol.ActivityDone) })
	got := log.states()
	if !containsInOrder(got, want) {
		t.Fatalf("activity states %v, want %v in order", got, want)
	}
	var working protocol.SessionActivity
	log.mu.Lock()
	all := append([]protocol.SessionActivity(nil), log.all...)
	log.mu.Unlock()
	for _, a := range all {
		if a.State == protocol.ActivityWorking {
			working = a
		}
	}
	if working.Tool != "Bash" || working.Label != "Run: go test ./..." || working.ChatID != "c1" || working.Source != protocol.ActivitySourceStream {
		t.Fatalf("working activity %+v", working)
	}

	n := len(tm.col.all())
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t2", Text: "ask me"})
	el, _ := tm.col.waitFor(t, n, "question", kind(protocol.EventElicitation))
	waitUntil(t, "asking", func() bool { return log.seen(protocol.ActivityAsking) })
	tm.Respond(protocol.AgentChatRespond{ChatID: "c1", ElicitationID: el.Elicitation.ID, OptionID: "blue"})
	tm.col.waitFor(t, n, "turn_end", func(e protocol.AgentChatEvent) bool { return e.Kind == protocol.EventTurnEnd && e.TurnID == "t2" })

	n = len(tm.col.all())
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t3", Text: "slow"})
	tm.col.waitFor(t, n, "delta", kind(protocol.EventMessageDelta))
	tm.Suspend("c1")
	waitUntil(t, "blocked", func() bool { return log.seen(protocol.ActivityBlocked) })
	tm.Resume("c1")
	tm.Control(protocol.AgentChatControl{ChatID: "c1", Cancel: true})
	tm.col.waitFor(t, n, "turn_end", func(e protocol.AgentChatEvent) bool { return e.Kind == protocol.EventTurnEnd && e.TurnID == "t3" })

	n = len(tm.col.all())
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t4", Text: "crash"})
	tm.col.waitFor(t, n, "exit error", errKind(protocol.ErrKindExit))
	waitUntil(t, "error", func() bool { got := log.states(); return got[len(got)-1] == protocol.ActivityError })
}

// Sub-agents (mock "subagents"): their text and thoughts feed the activity only, never
// the transcript; a sub-agent asking makes the whole session ask, naming it.
func TestManagerSubagentActivity(t *testing.T) {
	log := &activityLog{}
	tm := newTestManager(t, func(o *ManagerOptions) { o.OnActivity = log.add })
	tm.mock.ActivityHold = 5 * time.Millisecond
	tm.Open(protocol.AgentChatOpen{ChatID: "c1", Agent: MockAgent, Cwd: t.TempDir()})
	tm.Prompt(protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t1", Text: "use subagents"})
	el, _ := tm.col.waitFor(t, 0, "sub-agent permission", kind(protocol.EventElicitation))
	if el.ParentToolCallID != "task-b-t1" || el.Elicitation.Title != "Sub-agent “Docs for cache flags” asks: Allow Write: docs/flags.md?" {
		t.Fatalf("elicitation parent %q, title %q", el.ParentToolCallID, el.Elicitation.Title)
	}
	waitUntil(t, "asking", func() bool { return log.seen(protocol.ActivityAsking) })
	tm.Respond(protocol.AgentChatRespond{ChatID: "c1", ElicitationID: el.Elicitation.ID, OptionID: "allow_once"})
	tm.col.waitFor(t, 0, "turn_end", kind(protocol.EventTurnEnd))
	waitUntil(t, "done", func() bool { return log.seen(protocol.ActivityDone) })

	for _, ev := range tm.col.all() {
		if subagentChatter(ev) {
			t.Fatalf("sub-agent chatter reached the transcript: %+v", ev)
		}
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	var maxSubs int
	var askLabel string
	var waiting bool
	for _, a := range log.all {
		maxSubs = max(maxSubs, len(a.Subagents))
		waiting = waiting || a.Waiting
		if a.State == protocol.ActivityAsking {
			askLabel = a.Label
		}
	}
	last := log.all[len(log.all)-1]
	if maxSubs != 2 || !waiting || askLabel != "Sub-agent “Docs for cache flags” asks: Allow Write: docs/flags.md?" || len(last.Subagents) != 0 {
		t.Fatalf("max %d sub-agents, waiting %v, ask label %q, last %+v", maxSubs, waiting, askLabel, last)
	}
}

// containsInOrder reports whether want appears in got as a subsequence.
func containsInOrder(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}
