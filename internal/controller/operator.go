package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

// Terminal awareness (PRD §5.8a): konsertd's shell integration reports each command the
// user runs in a Konsert terminal as worker.operator_command (a redacted summary). The
// hub records it as a user action (actor user, source terminal, confidence high), links
// it to an open request it answers, or — for an unrequested intervention — places a hold
// on the resource it touched; the event is broadcast and delivered to affected agents.

// operatorCommandTTL bounds how long command ids are remembered for de-duplication.
const operatorCommandTTL = time.Hour

type seenCommands struct {
	mu   sync.Mutex
	done map[string]time.Time // command id -> when its end was processed
}

func (s *seenCommands) markDone(id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		s.done = map[string]time.Time{}
	}
	for k, t := range s.done {
		if now.Sub(t) > operatorCommandTTL {
			delete(s.done, k)
		}
	}
	if _, ok := s.done[id]; ok {
		return false
	}
	s.done[id] = now
	return true
}

func (h *Hub) onOperatorCommand(ctx context.Context, wc *workerConn, env protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.OperatorCommandPayload](env)
	if err != nil || p.TaskID == "" || strings.TrimSpace(p.Command) == "" {
		return
	}
	h.ingestOperatorCommand(ctx, wc.id, p)
}

// ingestOperatorCommand processes one reported terminal command from workerID.
func (h *Hub) ingestOperatorCommand(ctx context.Context, workerID string, p protocol.OperatorCommandPayload) {
	t, err := h.store.GetTask(ctx, p.TaskID)
	if err != nil || (t.WorkerID != "" && t.WorkerID != workerID) {
		h.logger.Debug("dropping operator command for a task this worker does not run", "task_id", p.TaskID, "worker_id", workerID)
		return
	}
	if p.EndedAt == nil && p.ExitCode == nil {
		// Started: its effect is judged when it ends (a failed command changes nothing).
		return
	}
	if p.CommandID == "" {
		p.CommandID = fmt.Sprintf("%s-%d", p.TaskID, p.StartedAt.UnixNano())
	}
	now := time.Now().UTC()
	if !h.operatorCommands.markDone(p.CommandID, now) {
		return
	}
	cmd := strings.TrimSpace(p.Command)
	where := h.workerLabel(ctx, workerID)
	eff := analyzeCommand(cmd)
	succeeded := p.ExitCode == nil || *p.ExitCode == 0
	exit := "exit status unknown"
	if p.ExitCode != nil {
		exit = fmt.Sprintf("exit %d", *p.ExitCode)
	}
	at := now
	if p.EndedAt != nil {
		at = p.EndedAt.UTC()
	}
	cause := store.Cause{Actor: store.ActorUser, Source: store.SourceTerminal, Confidence: store.ConfidenceHigh, CommandID: p.CommandID}

	// The command shows on the terminal task's own timeline too.
	te := store.TaskEvent{TaskID: t.ID, Kind: "command", Message: fmt.Sprintf("$ %s (%s)", cmd, exit), CreatedAt: at,
		MetaJSON: auditMeta(map[string]any{"command_id": p.CommandID, "exit_code": p.ExitCode, "cwd": p.Cwd})}
	if id, err := h.store.AppendEvent(ctx, te); err == nil {
		te.ID = id
	}
	h.broadcastFleetUpdate("event", te)

	intervention := eff.intervention && succeeded
	var req store.UserRequest
	matched := false
	if intervention {
		req, matched = h.matchOpenRequest(ctx, workerID, eff, cmd, at)
		if matched {
			cause.RequestID = req.ID
		}
	}
	summary := fmt.Sprintf("The user ran `%s` in a Konsert terminal on %s (%s)", cmd, where, exit)
	if intervention {
		target := "something on " + where
		if eff.resource != nil {
			target = describeResource(eff.resource) + " on " + where
		}
		summary = fmt.Sprintf("The user %s %s via a Konsert terminal (`%s`, %s)", pastTense(eff.verb), target, cmd, exit)
		if matched {
			summary += fmt.Sprintf(", answering request %s", req.SID)
		}
	}
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindAction, Action: "command", Cause: cause, WorkerID: workerID, TaskID: t.ID, CreatedAt: at,
		Summary: summary,
		MetaJSON: auditMeta(map[string]any{
			"command": cmd, "exit_code": p.ExitCode, "cwd": p.Cwd, "intervention": intervention,
			"verb": eff.verb, "resource": eff.resource, "started_at": p.StartedAt,
		}),
	})
	if !intervention {
		return
	}
	if matched {
		resolution := fmt.Sprintf("ran `%s` on %s at %s (%s)", cmd, where, at.Local().Format("15:04"), exit)
		_, _ = h.resolveRequest(withCause(ctx, cause), req.ID, store.RequestDone, resolution, p.CommandID)
		return
	}
	// Unrequested: hold the resource (or, if the target is unknown, automatic restarts
	// on the machine) so no agent undoes it on its own.
	spec := holdSpec{scope: store.HoldScopeResource, workerID: workerID, resource: eff.resource,
		summary: fmt.Sprintf("The user %s %s via a Konsert terminal (`%s`)", pastTense(eff.verb), describeResource(eff.resource), cmd)}
	if eff.resource == nil {
		spec.scope = store.HoldScopeMachine
		spec.resource = &store.Resource{Kind: ResourceRestarts}
		spec.summary = fmt.Sprintf("The user ran `%s` on %s via a Konsert terminal", cmd, where)
	}
	h.holdAfterIntervention(ctx, cause, spec)
}

// pastTense renders a command verb for a sentence: stop → stopped.
func pastTense(verb string) string {
	switch verb {
	case "":
		return "acted on"
	case "stop":
		return "stopped"
	case "kill":
		return "killed"
	case "rm":
		return "removed"
	case "down":
		return "brought down"
	case "up":
		return "brought up"
	case "kickstart":
		return "restarted"
	case "bootout", "unload":
		return "unloaded"
	case "load", "bootstrap":
		return "loaded"
	case "shutdown", "poweroff", "halt":
		return "shut down"
	case "reboot":
		return "rebooted"
	}
	if strings.HasSuffix(verb, "e") {
		return verb + "d"
	}
	return verb + "ed"
}
