package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Shell cleanup (SYR-ZAY87): the shells syred counts under a session (activity
// shell_list) can be cleaned up from Fleet: ask the agent to stop them, or kill them
// (syred terminates each shell's process tree after checking it still belongs to that
// session). Both are human actions: audited and recorded in the fleet timeline.

// Audit actions for shell cleanup.
const (
	AuditShellKill = "session.shell_kill"
	AuditShellAsk  = "session.shell_ask"
)

// ShellAction is the body of POST /api/{tasks|chats}/{id}/shells/{kill|ask}: the shells
// to act on (pid + started_at, as listed in the session's activity); empty means all.
type ShellAction struct {
	Shells []protocol.ShellIdent `json:"shells,omitempty"`
}

// ShellActionResult answers a shell action.
type ShellActionResult struct {
	OK bool `json:"ok"`
	// Shells is how many shells the action covers.
	Shells int `json:"shells"`
	// Delivery (ask): "now" (sent as a message or typed into the session) or "after_turn"
	// (the chat is mid-turn: sent as soon as its turn ends).
	Delivery string `json:"delivery,omitempty"`
}

// errNoShells is returned when the session has none of the named shells.
var errNoShells = errors.New("this session has no such running shell (it may have exited)")

// sessionShells returns the session's activity and the shells an action names (all
// listed ones when idents is empty).
func (h *Hub) sessionShells(key string, idents []protocol.ShellIdent) (protocol.SessionActivity, []protocol.ShellInfo, error) {
	a, ok := h.sessionActivity(key)
	if !ok || a.Shells == 0 {
		return a, nil, errNoShells
	}
	if len(idents) == 0 {
		return a, a.ShellList, nil
	}
	var out []protocol.ShellInfo
	for _, id := range idents {
		for _, s := range a.ShellList {
			if s.PID == id.PID && s.StartedAt.Equal(id.StartedAt) {
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		return a, nil, errNoShells
	}
	return a, out, nil
}

// shellSession resolves a task or chat for a shell action: its key, worker, label.
func (h *Hub) shellSession(ctx context.Context, taskID, chatID string) (key, workerID, label string, err error) {
	if chatID != "" {
		cs, err := h.store.GetChatSession(ctx, chatID)
		if err != nil {
			return "", "", "", err
		}
		return "chat:" + cs.ID, cs.WorkerID, "chat " + cs.SID, nil
	}
	t, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		return "", "", "", err
	}
	return "task:" + t.ID, t.WorkerID, "task " + t.SID, nil
}

// KillShells asks the session's syred to terminate shells (all when idents is empty).
func (h *Hub) KillShells(ctx context.Context, taskID, chatID string, idents []protocol.ShellIdent) (ShellActionResult, error) {
	return h.killShells(ctx, taskID, chatID, idents, "")
}

// killShells is KillShells with why appended to the timeline and audit lines (an
// automatic kill says which policy did it).
func (h *Hub) killShells(ctx context.Context, taskID, chatID string, idents []protocol.ShellIdent, why string) (ShellActionResult, error) {
	key, workerID, label, err := h.shellSession(ctx, taskID, chatID)
	if err != nil {
		return ShellActionResult{}, err
	}
	a, list, err := h.sessionShells(key, idents)
	if err != nil {
		return ShellActionResult{}, err
	}
	if a.WorkerID != "" {
		workerID = a.WorkerID
	}
	wc := h.workerConnFor(workerID)
	if wc == nil {
		return ShellActionResult{}, fmt.Errorf("%s is not connected", h.workerLabel(ctx, workerID))
	}
	msg := protocol.SessionShellKill{TaskID: a.TaskID, ChatID: a.ChatID}
	n := a.Shells
	if len(idents) > 0 {
		n = len(list)
		for _, s := range list {
			msg.Shells = append(msg.Shells, protocol.ShellIdent{PID: s.PID, StartedAt: s.StartedAt})
		}
	}
	if !h.sendWorkerJSON(wc, protocol.MsgSessionShellKill, msg) {
		return ShellActionResult{}, errors.New("the worker's send queue is full; try again")
	}
	cause := causeFrom(ctx)
	what := shellsPhrase(list, n)
	suffix := ""
	if why != "" {
		suffix = " " + why
	}
	h.recordIntervention(ctx, cause, "shell_kill", workerID, a.TaskID, a.ChatID,
		fmt.Sprintf("%s killed %s of %s on %s%s", capitalize(describeActor(cause)), what, label, h.workerLabel(ctx, workerID), suffix))
	h.recordAudit(ctx, store.AuditEvent{Action: AuditShellKill, WorkerID: workerID, TaskID: a.TaskID,
		Message: "Killed " + what + " of " + label + suffix, MetaJSON: auditMeta(map[string]any{"chat_id": a.ChatID, "pids": shellPIDs(list), "all": len(idents) == 0, "policy": why != ""})})
	h.resolveShellReminder(key)
	return ShellActionResult{OK: true, Shells: n}, nil
}

// AskStopShells asks the session's agent to stop shells: typed into a running task's
// terminal, sent as a message to an idle chat, or queued for a chat mid-turn (sent when
// its turn ends). A finished task or a suspended session can't be asked.
func (h *Hub) AskStopShells(ctx context.Context, taskID, chatID string, idents []protocol.ShellIdent) (ShellActionResult, error) {
	key, workerID, label, err := h.shellSession(ctx, taskID, chatID)
	if err != nil {
		return ShellActionResult{}, err
	}
	a, list, err := h.sessionShells(key, idents)
	if err != nil {
		return ShellActionResult{}, err
	}
	text := askStopText(list, a.Shells, len(idents) == 0)
	res := ShellActionResult{OK: true, Shells: len(list)}
	if len(idents) == 0 {
		res.Shells = a.Shells
	}
	if chatID != "" {
		cs, err := h.store.GetChatSession(ctx, chatID)
		if err != nil {
			return ShellActionResult{}, err
		}
		if cs.Backend != store.ChatBackendAgent {
			return ShellActionResult{}, errors.New("only agent chats run shells")
		}
		switch {
		case cs.Status == protocol.ChatStatusSuspended:
			return ShellActionResult{}, errors.New("the chat is suspended: resume it first, or kill the shell")
		case chatBusy(cs.Status):
			rt := h.chatRT(cs.ID)
			rt.mu.Lock()
			rt.followUps = append(rt.followUps, text)
			rt.mu.Unlock()
			res.Delivery = "after_turn"
		default:
			if err := h.sendAgentChat(ctx, cs, chatSendRequest{SessionID: cs.ID, Content: text}); err != nil {
				return ShellActionResult{}, err
			}
			res.Delivery = "now"
		}
	} else {
		t, err := h.store.GetTask(ctx, taskID)
		if err != nil {
			return ShellActionResult{}, err
		}
		switch {
		case protocol.IsTerminalTaskStatus(t.Status):
			return ShellActionResult{}, fmt.Errorf("task %s has ended, so its agent can't be asked: kill the shell instead", t.SID)
		case t.Status == protocol.TaskStatusSuspended:
			return ShellActionResult{}, fmt.Errorf("task %s is suspended: resume it first, or kill the shell", t.SID)
		}
		if err := h.SendSteer(t.ID, oneLine(text, 2000), true); err != nil {
			return ShellActionResult{}, err
		}
		res.Delivery = "now"
	}
	cause := causeFrom(ctx)
	what := shellsPhrase(list, res.Shells)
	h.recordIntervention(ctx, cause, "shell_ask", workerID, a.TaskID, a.ChatID,
		fmt.Sprintf("%s asked the agent of %s to stop %s", capitalize(describeActor(cause)), label, what))
	h.recordAudit(ctx, store.AuditEvent{Action: AuditShellAsk, WorkerID: workerID, TaskID: a.TaskID,
		Message:  "Asked the agent of " + label + " to stop " + what,
		MetaJSON: auditMeta(map[string]any{"chat_id": a.ChatID, "pids": shellPIDs(list), "delivery": res.Delivery})})
	h.markShellsAsked(key, list)
	h.resolveShellReminder(key)
	return res, nil
}

// takeFollowUpsLocked sends what was queued for a chat while it was mid-turn (rt.mu held).
func (h *Hub) takeFollowUpsLocked(rt *agentChatRT, chatID string) {
	if len(rt.followUps) == 0 {
		return
	}
	text := strings.Join(rt.followUps, "\n\n")
	rt.followUps = nil
	h.goBackground(func(ctx context.Context) {
		cs, err := h.store.GetChatSession(ctx, chatID)
		if err != nil || cs.Status == protocol.ChatStatusSuspended {
			return
		}
		if err := h.sendAgentChat(ctx, cs, chatSendRequest{SessionID: chatID, Content: text}); err != nil {
			h.logger.Warn("queued shell follow-up not sent", "chat_id", chatID, "err", err)
		}
	})
}

func shellPIDs(list []protocol.ShellInfo) []int {
	out := make([]int, 0, len(list))
	for _, s := range list {
		out = append(out, s.PID)
	}
	return out
}

// shellsPhrase names the shells for the timeline: "shell `vite dev` (pid 123)" or "3
// shells".
func shellsPhrase(list []protocol.ShellInfo, n int) string {
	if len(list) == 1 && n == 1 {
		return fmt.Sprintf("shell `%s` (pid %d)", oneLine(list[0].Command, 80), list[0].PID)
	}
	if n == 1 {
		return "1 shell"
	}
	return fmt.Sprintf("%d shells", n)
}

// askStopText is the message the agent gets.
func askStopText(list []protocol.ShellInfo, total int, all bool) string {
	var b strings.Builder
	if all && total > 1 {
		fmt.Fprintf(&b, "Please stop the %d shell processes you started that are still running", total)
	} else {
		b.WriteString("Please stop this process you started; it is still running")
	}
	b.WriteString(":\n")
	for _, s := range list {
		age := ""
		if !s.StartedAt.IsZero() {
			age = ", running for " + time.Since(s.StartedAt).Round(time.Second).String()
		}
		fmt.Fprintf(&b, "- `%s` (pid %d%s)\n", oneLine(s.Command, 200), s.PID, age)
	}
	b.WriteString("If you still need it, say why instead of stopping it.")
	return b.String()
}

// --- HTTP ---------------------------------------------------------------------------

// handleShellAction serves POST /api/{tasks|chats}/{id}/shells/{kill|ask}.
func (s *Server) handleShellAction(kind, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body ShellAction
		if requestHasBody(r) && !decodeJSON(w, r, &body) {
			return
		}
		taskID, chatID := r.PathValue("id"), ""
		if kind == "chat" {
			taskID, chatID = "", taskID
		}
		fn := s.hub.KillShells
		if action == "ask" {
			fn = s.hub.AskStopShells
		}
		res, err := fn(r.Context(), taskID, chatID, body.Shells)
		if err != nil {
			writeControlError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}
