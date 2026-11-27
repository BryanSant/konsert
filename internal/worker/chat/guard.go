package chat

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/guard"
)

// GuardSpec marks a launch as a managed session for the machine's guardrails
// (internal/worker/guard): contained, deprioritised and sheddable under memory
// pressure. Probes and quick commands launch without one.
type GuardSpec struct {
	// Owner is the chat id ("verify:<chat id>" for verify commands).
	Owner    string
	Kind     string // "chat" or "verify"
	Priority guard.Priority
	Headless bool
}

// chatGuard is the guard spec for a chat's engine: ticket runs are shed before a
// person's own chat.
func chatGuard(opts protocol.AgentChatOpen) *GuardSpec {
	g := &GuardSpec{Owner: opts.ChatID, Kind: "chat", Priority: guard.PriorityInteractive}
	if opts.TicketSID != "" {
		g.Priority, g.Headless = guard.PriorityTicket, true
	}
	return g
}

// guardedCommand builds the command for binary/args, prepared by the guard when gs is
// set (on Linux that may wrap it in a systemd scope). Call attach after Start.
func guardedCommand(gs *GuardSpec, binary string, args []string) (*exec.Cmd, *guard.Launch) {
	if gs == nil {
		return exec.Command(binary, args...), nil
	}
	l := guard.Default().Prepare(guard.LaunchSpec{
		Binary: binary, Args: args, Owner: gs.Owner, Kind: gs.Kind, Priority: gs.Priority, Headless: gs.Headless,
	})
	return exec.Command(l.Binary, l.Args...), l
}

func attachGuard(l *guard.Launch, cmd *exec.Cmd) *guard.Session {
	if l == nil || cmd.Process == nil {
		return nil
	}
	return l.Attach(cmd.Process.Pid)
}

type guardCtxKey struct{}

// withGuard attaches a guard spec for commands started under ctx (verify steps).
func withGuard(ctx context.Context, gs *GuardSpec) context.Context {
	return context.WithValue(ctx, guardCtxKey{}, gs)
}

func guardFrom(ctx context.Context) *GuardSpec {
	gs, _ := ctx.Value(guardCtxKey{}).(*GuardSpec)
	return gs
}

func verifyOwner(chatID string) string { return "verify:" + chatID }

// ownerOOMKilled reports whether the chat's engine was last ended by the OOM killer
// (a variable so tests can stand in for the kernel).
var ownerOOMKilled = func(chatID string) bool { return guard.Default().OwnerOOMKilled(chatID) }

const mib = 1 << 20

// ShedEvidence converts the guard's evidence for the protocol.
func ShedEvidence(ev guard.Evidence) *protocol.ShedEvidence {
	return &protocol.ShedEvidence{
		Session:        ev.Kind,
		Reason:         ev.Reason,
		RSSMB:          ev.RSS / mib,
		HostAvailMB:    ev.HostAvail / mib,
		ReserveMB:      ev.Reserve / mib,
		ActiveSessions: ev.ActiveSessions,
		HostUsedMB:     ev.HostUsed / mib,
		Bystander:      ev.Bystander,
	}
}

// ShedMessage is the one-line explanation the Commander and the user see.
func ShedMessage(ev guard.Evidence) string {
	what := "the agent"
	if ev.Kind == "verify" {
		what = "the verification command"
	}
	msg := fmt.Sprintf("Shed by host memory pressure: the machine dropped below its %d MiB survival reserve", ev.Reserve/mib)
	switch ev.Reason {
	case guard.ReasonCritical:
		msg = "Shed by host memory pressure: the kernel reported critical memory pressure"
	case guard.ReasonPoolStall:
		msg = "Shed by host memory pressure: agent work stalled at the machine's memory ceiling"
	case guard.ReasonSwapGrowth:
		msg = "Shed by host memory pressure: the machine was paging agent work out to disk"
	case guard.ReasonCommitLow:
		msg = fmt.Sprintf("Shed by host memory pressure: Windows was within its %d MiB survival reserve of the commit limit (RAM plus page file), where every allocation on the machine fails", ev.Reserve/mib)
	}
	if ev.Commit > 0 {
		msg += fmt.Sprintf(", so Syre stopped %s holding %.1f GiB of commit", what, float64(ev.Commit)/float64(1<<30))
	} else if ev.RSS > 0 {
		msg += fmt.Sprintf(", so Syre stopped %s at %.1f GiB", what, float64(ev.RSS)/float64(1<<30))
	} else {
		msg += ", so Syre stopped " + what
	}
	if ev.Bystander {
		inUse := "in use"
		if ev.Commit > 0 {
			inUse = "committed"
		}
		msg += fmt.Sprintf(". Most of the %.1f GiB %s was elsewhere on the machine, so this says nothing about what the work needs",
			float64(ev.HostUsed)/float64(1<<30), inUse)
	}
	return msg + ". Not a code error; the work can run again."
}
