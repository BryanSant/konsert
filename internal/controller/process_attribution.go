package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Process-level attribution (SYR-AT7T1). A daemon reports a watched daemon process
// exiting (worker.process_exit). The hub looks for who did it, in order of evidence:
//   - the user killed it from the UI (high: Syre did it for them);
//   - a Syre terminal command that names it (already attributed, high: nothing new);
//   - an agent's shell tool call that names it (the agent, likely);
//   - an interactive SSH shell outside Syre had input when it exited (the user,
//     likely): the same hold as a stop in a Syre terminal, so no agent restarts it on
//     its own;
//   - nothing: a crash or a timer; logged, not attributed.

// Attribution windows around an exit, which happened between After and Before.
const (
	exitAgentLead = 60 * time.Second // an agent command this long before the exit counts
	exitSSHLead   = 20 * time.Second // terminal input this long before the exit counts
	exitSlack     = 3 * time.Second  // clocks and sampling jitter after it
	recentKeep    = 10 * time.Minute
)

type recentCommand struct {
	worker, chatID, chatSID, text string
	at                            time.Time
}

type recentKill struct {
	worker string
	pid    int
	at     time.Time
}

// recentActions remembers agent shell commands and UI kills for exit attribution.
type recentActions struct {
	mu    sync.Mutex
	cmds  []recentCommand
	kills []recentKill
}

func (r *recentActions) addCommand(c recentCommand) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(pruneRecent(r.cmds, c.at, func(c recentCommand) time.Time { return c.at }), c)
}

func (r *recentActions) addKill(k recentKill) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kills = append(pruneRecent(r.kills, k.at, func(k recentKill) time.Time { return k.at }), k)
}

func pruneRecent[T any](list []T, now time.Time, at func(T) time.Time) []T {
	out := list[:0]
	for _, x := range list {
		if now.Sub(at(x)) < recentKeep {
			out = append(out, x)
		}
	}
	if len(out) > 1000 {
		out = out[len(out)-1000:]
	}
	return out
}

// noteAgentCommand records an agent's shell tool call (from its chat events).
func (h *Hub) noteAgentCommand(workerID string, cs store.ChatSession, ev protocol.AgentChatEvent) {
	tc := ev.ToolCall
	if tc == nil || tc.Kind != "execute" || tc.Status != "running" {
		return
	}
	text := strings.TrimSpace(strings.TrimPrefix(tc.Title, "Run: "))
	if text == "" {
		text = tc.Input
	}
	h.recent.addCommand(recentCommand{worker: workerID, chatID: cs.ID, chatSID: cs.SID, text: text, at: time.Now()})
}

// namesProcess reports whether a command line names a process: as a word (nginx,
// nginx.service) or as a path ending in it.
func namesProcess(cmd, name string) bool {
	if name == "" {
		return false
	}
	re := regexp.MustCompile(`(^|[\s/'"=])` + regexp.QuoteMeta(name) + `($|[\s.'"@:;|&)])`)
	return re.MatchString(cmd)
}

func (h *Hub) onProcessExit(ctx context.Context, wc *workerConn, env protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.ProcessExitPayload](env)
	if err != nil || p.Name == "" {
		return
	}
	h.attributeProcessExit(ctx, wc.id, p)
}

// attributeProcessExit records who most likely stopped p, if anyone can be named.
func (h *Hub) attributeProcessExit(ctx context.Context, workerID string, p protocol.ProcessExitPayload) {
	from, to := p.After, p.Before.Add(exitSlack)
	if p.After.IsZero() {
		from = p.Before.Add(-time.Minute)
	}
	where := h.workerLabel(ctx, workerID)
	what := fmt.Sprintf("%s (pid %d)", p.Name, p.PID)

	h.recent.mu.Lock()
	var killed bool
	for _, k := range h.recent.kills {
		killed = killed || (k.worker == workerID && k.pid == p.PID)
	}
	var agent *recentCommand
	for i := len(h.recent.cmds) - 1; i >= 0; i-- {
		c := h.recent.cmds[i]
		if c.worker == workerID && !c.at.Before(from.Add(-exitAgentLead)) && !c.at.After(to) && namesProcess(c.text, p.Name) {
			agent = &c
			break
		}
	}
	h.recent.mu.Unlock()
	if killed {
		return // the UI kill was recorded when it was requested
	}
	if h.terminalCommandNamed(ctx, workerID, p.Name, from.Add(-exitAgentLead), to) {
		return // a Syre terminal command: attributed (and held) when it ended
	}
	meta := map[string]any{"process": p.Name, "pid": p.PID, "started_at": p.StartedAt, "exited_after": p.After, "exited_before": p.Before}
	if agent != nil {
		cause := store.Cause{Actor: store.ActorAgent, ActorID: agent.chatID, Source: "process_exit", Confidence: store.ConfidenceLikely}
		meta["command"] = agent.text
		h.recordFleetEvent(ctx, store.FleetEvent{
			Kind: store.EventKindAction, Action: "process_exit", Cause: cause, WorkerID: workerID, ChatID: agent.chatID, CreatedAt: p.Before,
			Summary:  fmt.Sprintf("%s on %s exited, likely because chat %s ran `%s`", what, where, agent.chatSID, agent.text),
			MetaJSON: auditMeta(meta),
		})
		return
	}
	var typed *protocol.SSHShell
	for i := range p.SSHShells {
		s := p.SSHShells[i]
		if !s.LastInput.IsZero() && !s.LastInput.Before(from.Add(-exitSSHLead)) && !s.LastInput.After(to) {
			typed = &s
			break
		}
	}
	if typed == nil {
		h.logger.Debug("process exited; no one to attribute it to", "worker_id", workerID, "process", p.Name, "pid", p.PID, "ssh_shells", len(p.SSHShells))
		return
	}
	cause := store.Cause{Actor: store.ActorUser, Source: store.SourceSSH, Confidence: store.ConfidenceLikely}
	meta["ssh_tty"], meta["ssh_last_input"] = typed.TTY, typed.LastInput
	summary := fmt.Sprintf("The user likely stopped %s on %s over SSH (%s had input when it exited; not a Syre terminal or an agent)", what, where, cmpOr(typed.TTY, "a terminal"))
	defer h.beginDeliveryBatch()()
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindAction, Action: "process_exit", Cause: cause, WorkerID: workerID, CreatedAt: p.Before,
		Summary: summary, MetaJSON: auditMeta(meta),
	})
	h.holdAfterIntervention(ctx, cause, holdSpec{scope: store.HoldScopeResource, workerID: workerID,
		resource: &store.Resource{Kind: "service", Name: p.Name}, summary: summary})
}

// terminalCommandNamed reports whether a Syre terminal command naming name ended on
// workerID in [from, to].
func (h *Hub) terminalCommandNamed(ctx context.Context, workerID, name string, from, to time.Time) bool {
	evs, err := h.store.ListFleetEvents(ctx, store.FleetEventFilter{WorkerID: workerID, Kind: store.EventKindAction, Since: from, Limit: 200})
	if err != nil {
		return false
	}
	for _, e := range evs {
		if e.Action != "command" || e.CreatedAt.After(to) {
			continue
		}
		var m struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(e.MetaJSON), &m) == nil && namesProcess(m.Command, name) {
			return true
		}
	}
	return false
}

// handleKillProcess serves POST /api/workers/{id}/processes/kill {pid, started_at}:
// the user stops one process (audited; the daemon refuses syred itself and a reused PID).
func (s *Server) handleKillProcess(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body protocol.ProcessKillPayload
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.PID <= 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "pid is required"})
		return
	}
	ctx := r.Context()
	env, err := s.hub.RequestWorker(ctx, id, protocol.MsgProcessKill, body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	res, err := protocol.DecodePayload[protocol.ProcessKillResult](env)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if !res.Killed {
		writeJSON(w, http.StatusConflict, res)
		return
	}
	s.hub.recent.addKill(recentKill{worker: id, pid: body.PID, at: time.Now()})
	where := s.hub.workerLabel(ctx, id)
	how := "stopped"
	if res.Forced {
		how = "killed (it ignored the request to stop)"
	}
	cause := causeFrom(ctx)
	s.hub.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindAction, Action: "process_kill", Cause: cause, WorkerID: id,
		Summary:  fmt.Sprintf("%s %s %s (pid %d) on %s", capitalize(describeActor(cause)), how, res.Name, body.PID, where),
		MetaJSON: auditMeta(map[string]any{"process": res.Name, "pid": body.PID, "forced": res.Forced}),
	})
	s.hub.recordAudit(ctx, store.AuditEvent{Action: AuditProcessKill, WorkerID: id,
		Message: fmt.Sprintf("Killed %s (pid %d) on %s", res.Name, body.PID, where)})
	writeJSON(w, http.StatusOK, res)
}

// AuditProcessKill is the audit action of a user's process kill.
const AuditProcessKill = "process.kill"
