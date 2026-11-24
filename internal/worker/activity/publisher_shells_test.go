package activity

import (
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

func shellsN(n, busy int) Shells {
	s := Shells{Count: n, Busy: busy}
	for i := range n {
		s.List = append(s.List, protocol.ShellInfo{PID: 100 + i, Command: "sleep 300", Busy: i < busy})
	}
	return s
}

func TestPublisherShellCountGoesOutWithinASecond(t *testing.T) {
	p, clk, out := newTestPublisher()
	// Shells known before the session's first activity wait for it.
	p.SetShells("chat:c1", shellsN(1, 1))
	if len(*out) != 0 {
		t.Fatal("published shells without an activity")
	}
	p.Update(act(protocol.ActivityThinking, ""))
	if len(*out) != 1 || (*out)[0].a.Shells != 1 || (*out)[0].a.ShellsBusy != 1 {
		t.Fatalf("first publish %+v", *out)
	}
	clk.advance(2 * time.Second)
	// A background shell starts while the agent keeps thinking.
	setAt := clk.Now()
	p.SetShells("chat:c1", shellsN(2, 1))
	clk.advance(DefaultMaxWait)
	if len(*out) != 2 || (*out)[1].a.Shells != 2 || (*out)[1].a.State != protocol.ActivityThinking {
		t.Fatalf("count change: %+v", *out)
	}
	if lag := (*out)[1].at.Sub(setAt); lag != DefaultDebounce {
		t.Fatalf("count change took %v, want the debounce", lag)
	}
	// Later activity keeps the shells.
	clk.advance(time.Second)
	p.Update(act(protocol.ActivityDone, ""))
	if last := (*out)[len(*out)-1].a; last.State != protocol.ActivityDone || last.Shells != 2 {
		t.Fatalf("done lost the shells: %+v", last)
	}
	// Dangling shells after the end: changes still go out; the session is not pruned
	// while a shell runs.
	clk.advance(2 * time.Hour)
	p.Prune(time.Hour)
	if len(p.Snapshot()) != 1 {
		t.Fatal("a done session with running shells was pruned")
	}
	p.SetShells("chat:c1", shellsN(0, 0))
	clk.advance(DefaultMaxWait)
	if last := (*out)[len(*out)-1].a; last.Shells != 0 || len(last.ShellList) != 0 || last.State != protocol.ActivityDone {
		t.Fatalf("shells gone: %+v", last)
	}
	clk.advance(2 * time.Hour)
	p.Prune(time.Hour)
	if len(p.Snapshot()) != 0 {
		t.Fatal("finished session kept after its shells exited")
	}
	// No entry, no shells: nothing is created.
	p.SetShells("task:none", Shells{})
	if len(p.Snapshot()) != 0 {
		t.Fatal("empty shells created an entry")
	}
}

func TestPublisherBoundsAShellAndSubagentFlood(t *testing.T) {
	p, clk, out := newTestPublisher()
	p.Update(act(protocol.ActivityThinking, ""))
	// 2000 changes over 2 s: shell counts rising and falling every millisecond, busy
	// flags flipping and sub-agents changing state.
	for i := range 2000 {
		if i%2 == 0 {
			p.SetShells("chat:c1", shellsN(i%7, i%3))
		} else {
			a := act(protocol.ActivityWorking, "Task")
			a.Waiting = true
			a.Subagents = []protocol.SubagentActivity{{ID: "A", State: []string{protocol.ActivityThinking, protocol.ActivityWorking, protocol.ActivitySpeaking}[i%3]}}
			p.Update(a)
		}
		clk.advance(time.Millisecond)
	}
	clk.advance(2 * time.Second)
	if n := len(*out); n > 10 || n < 3 {
		t.Fatalf("2000 changes in 2 s published %d times, want a few per second", n)
	}
	for i := 1; i < len(*out); i++ {
		if gap := (*out)[i].at.Sub((*out)[i-1].at); gap < DefaultMinInterval {
			t.Fatalf("publishes %d and %d only %v apart", i-1, i, gap)
		}
	}
	// The latest of both lands: i=1998 set shells (1998%7=3, 1998%3=0); i=1999 the
	// sub-agent (1999%3=1 → working).
	last := (*out)[len(*out)-1].a
	if last.Shells != 3 || last.ShellsBusy != 0 || len(last.Subagents) != 1 || last.Subagents[0].State != protocol.ActivityWorking || !last.Waiting {
		t.Fatalf("last published %+v", last)
	}
}
