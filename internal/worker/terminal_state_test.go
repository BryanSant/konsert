package worker

import (
	"runtime"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/activity"
	"github.com/BryanSant/syre/internal/worker/shells"
)

func TestShellBusyFromTheProcessTable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process groups")
	}
	table := func(ps ...shells.Proc) (map[int]shells.Proc, map[int]int) {
		by, kids := map[int]shells.Proc{}, map[int]int{}
		for _, p := range ps {
			by[p.PID] = p
			kids[p.PPID]++
		}
		return by, kids
	}
	// At its prompt: the terminal's foreground group is the shell's own.
	by, kids := table(shells.Proc{PID: 100, PPID: 1, PGID: 100, TPGID: 100, Name: "fish"})
	if busy, _ := shellBusy(by, kids, 100); busy {
		t.Fatal("a shell at its prompt is busy")
	}
	// `sleep 30` in the foreground: its own group holds the terminal.
	by, kids = table(shells.Proc{PID: 100, PPID: 1, PGID: 100, TPGID: 200, Name: "fish"}, shells.Proc{PID: 200, PPID: 100, PGID: 200, Name: "sleep"})
	if busy, label := shellBusy(by, kids, 100); !busy || label != "sleep" {
		t.Fatalf("foreground command: %v %q", busy, label)
	}
	// A background job (`cmd &`) doesn't hold the terminal: the shell is at its prompt.
	by, kids = table(shells.Proc{PID: 100, PPID: 1, PGID: 100, TPGID: 100, Name: "fish"}, shells.Proc{PID: 300, PPID: 100, PGID: 300, Name: "make"})
	if busy, _ := shellBusy(by, kids, 100); busy {
		t.Fatal("a background job made the shell busy")
	}
}

func TestTerminalStateFromCommandEvents(t *testing.T) {
	var sent []protocol.SessionActivity
	d := &Daemon{}
	d.activity = activity.NewPublisher(activity.PublisherOptions{}, func(a protocol.SessionActivity) { sent = append(sent, a) })
	now := time.Now()
	d.terminalCommand(protocol.OperatorCommandPayload{TaskID: "t1", CommandID: "c1", Command: "sleep 30", StartedAt: now})
	code := 0
	end := now.Add(30 * time.Second)
	d.terminalCommand(protocol.OperatorCommandPayload{TaskID: "t1", CommandID: "c1", Command: "sleep 30", StartedAt: now, EndedAt: &end, ExitCode: &code})
	d.activity.Flush("task:t1")
	if len(sent) == 0 {
		t.Fatal("nothing published")
	}
	var states []string
	for _, a := range sent {
		states = append(states, a.State)
	}
	if states[0] != protocol.ActivityWorking || sent[0].Label != "sleep 30" || states[len(states)-1] != protocol.ActivityDone {
		t.Fatalf("published: %+v", sent)
	}
}
