package controller

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/nowline"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

// Session activity (KON-NAYVC): konsertd derives it from engine streams and session logs
// and sends session.activity at a bounded rate; the hub keeps the latest per session in
// memory, fills in what only it knows (queued, suspended, observed sessions, hub-side
// errors), and fans it out to every browser as fleet.update kind "activity". The
// GET /api/fleet snapshot carries the current set. See docs/design/activity.md.

// maxActivityEntries bounds the in-memory activity map; the oldest finished sessions go
// first.
const maxActivityEntries = 2000

type activityState struct {
	mu      sync.Mutex
	entries map[string]protocol.SessionActivity
	// dirs caches each session's working directory (see activityDir).
	dirs map[string]string
}

// activityList returns every known session activity, newest first.
func (h *Hub) activityList() []protocol.SessionActivity {
	h.activity.mu.Lock()
	out := make([]protocol.SessionActivity, 0, len(h.activity.entries))
	for _, a := range h.activity.entries {
		out = append(out, a)
	}
	h.activity.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// sessionActivity returns the activity of key ("chat:<id>" / "task:<id>").
func (h *Hub) sessionActivity(key string) (protocol.SessionActivity, bool) {
	h.activity.mu.Lock()
	defer h.activity.mu.Unlock()
	a, ok := h.activity.entries[key]
	return a, ok
}

// setActivity records a and broadcasts it when it differs from what browsers show.
func (h *Hub) setActivity(a protocol.SessionActivity) {
	if a.State == "" || (a.ChatID == "" && a.TaskID == "") {
		return
	}
	if a.At.IsZero() {
		a.At = time.Now().UTC()
	}
	a.Summary = ""
	if a.State == protocol.ActivityWorking && (a.Label != "" || a.Tool != "") {
		a.Summary = nowline.Summarize(a.Tool, a.Label, nowline.Context{Dir: h.activityDir(a)})
	}
	key := a.Key()
	h.activity.mu.Lock()
	if h.activity.entries == nil {
		h.activity.entries = map[string]protocol.SessionActivity{}
	}
	prev, had := h.activity.entries[key]
	if had {
		if a.WorkerID == "" {
			a.WorkerID = prev.WorkerID
		}
		if a.At.Before(prev.At) {
			h.activity.mu.Unlock()
			return // an older report (a re-send after reconnect)
		}
		if prev.State == a.State && prev.Tool == a.Tool && prev.Label == a.Label &&
			prev.TokensPerSec == a.TokensPerSec && prev.Coarse == a.Coarse {
			h.activity.mu.Unlock()
			return
		}
		if prev.State == a.State && prev.Tool == a.Tool && !prev.Since.IsZero() {
			a.Since = prev.Since
		}
	}
	if a.Since.IsZero() {
		a.Since = a.At
	}
	h.activity.entries[key] = a
	if len(h.activity.entries) > maxActivityEntries {
		h.pruneActivityLocked()
	}
	h.activity.mu.Unlock()
	h.broadcastFleetUpdate("activity", a)
}

// pruneActivityLocked drops the oldest finished sessions down to 3/4 of the cap.
func (h *Hub) pruneActivityLocked() {
	type kv struct {
		k  string
		at time.Time
	}
	var done []kv
	for k, a := range h.activity.entries {
		if protocol.ActivityTerminal(a.State) {
			done = append(done, kv{k, a.At})
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].at.Before(done[j].at) })
	for _, d := range done {
		if len(h.activity.entries) <= maxActivityEntries*3/4 {
			return
		}
		delete(h.activity.entries, d.k)
	}
}

// forgetActivity drops a session (its chat was deleted).
func (h *Hub) forgetActivity(key string) {
	h.activity.mu.Lock()
	delete(h.activity.entries, key)
	delete(h.activity.dirs, key)
	h.activity.mu.Unlock()
}

// onSessionActivity handles session.activity from a worker: only for sessions it hosts.
func (h *Hub) onSessionActivity(ctx context.Context, wc *workerConn, env protocol.Envelope) {
	a, err := protocol.DecodePayload[protocol.SessionActivity](env)
	if err != nil || a.State == "" || (a.ChatID == "" && a.TaskID == "") {
		return
	}
	if prev, ok := h.sessionActivity(a.Key()); !ok || prev.WorkerID != wc.id {
		// First report for this session (or from another worker): check the host.
		switch {
		case a.ChatID != "":
			cs, err := h.store.GetChatSession(ctx, a.ChatID)
			if err != nil || cs.WorkerID != wc.id {
				return
			}
		default:
			t, err := h.store.GetTask(ctx, a.TaskID)
			if err != nil || t.WorkerID != wc.id {
				return
			}
			if protocol.IsTerminalTaskStatus(t.Status) && !protocol.ActivityTerminal(a.State) {
				return // a late report for a task that already ended
			}
		}
	}
	a.WorkerID = wc.id
	a.Label = oneLine(a.Label, maxActivityLabel(a.Label))
	h.setActivity(a)
}

// taskStatusActivity derives what the hub knows about a task from its status: queued and
// suspended (the worker can't tell), and a fallback for workers that report nothing.
// Observed sessions get theirs from handleExternalSession.
func (h *Hub) taskStatusActivity(t store.Task) {
	if t.ID == "" || t.Origin == protocol.TaskOriginExternal {
		return
	}
	cur, had := h.sessionActivity("task:" + t.ID)
	a := protocol.SessionActivity{TaskID: t.ID, WorkerID: t.WorkerID, Source: protocol.ActivitySourceHub, At: time.Now().UTC()}
	switch t.Status {
	case protocol.TaskStatusQueued:
		a.State, a.Label = protocol.ActivityQueued, "Waiting for a machine"
	case protocol.TaskStatusAssigned:
		a.State, a.Label = protocol.ActivityQueued, "Starting"
	case protocol.TaskStatusSuspended:
		a.State, a.Label = protocol.ActivityBlocked, "Suspended"
		if t.SuspendedBy == "pause" {
			a.Label = "Paused (Pause everything)"
		}
	case protocol.TaskStatusWaitingInput:
		if had && (cur.State == protocol.ActivityAsking || cur.State == protocol.ActivityDone) {
			return
		}
		a.State, a.Label, a.Coarse = protocol.ActivityAsking, "Waiting for input", true
	case protocol.TaskStatusRunning:
		if had && cur.State != protocol.ActivityQueued && cur.State != protocol.ActivityBlocked && cur.State != protocol.ActivityAsking {
			return // the worker's report is better
		}
		a.State, a.Coarse = protocol.ActivityThinking, true
	case protocol.TaskStatusIdle, protocol.TaskStatusCompleted:
		a.State = protocol.ActivityDone
	case protocol.TaskStatusCanceled:
		a.State, a.Label = protocol.ActivityDone, "Cancelled"
	case protocol.TaskStatusFailed:
		a.State, a.Label = protocol.ActivityError, oneLine(firstNonEmpty(firstLine(t.Summary), "Failed"), 160)
	default:
		return
	}
	h.setActivity(a)
}

// externalActivity is the coarse activity of an observed (user-started) session from a
// hook or process report.
func externalActivity(taskID, workerID string, p protocol.ExternalSessionPayload) (protocol.SessionActivity, bool) {
	a := protocol.SessionActivity{TaskID: taskID, WorkerID: workerID, Coarse: true, At: time.Now().UTC()}
	a.Source = protocol.ActivitySourcePassive
	if p.Source == "hook" {
		a.Source = protocol.ActivitySourceHook
	}
	switch strings.ToLower(strings.TrimSpace(p.Status)) {
	case "working", "running":
		switch {
		case p.Tool != "":
			a.State, a.Tool, a.Label = protocol.ActivityWorking, p.Tool, p.Summary
		case strings.EqualFold(p.Event, "PostToolUse"), strings.EqualFold(p.Event, "UserPromptSubmit"):
			a.State = protocol.ActivityThinking // the tool result (or the prompt) went to the model
		default:
			a.State, a.Label = protocol.ActivityWorking, p.Summary // process seen busy: we can't tell more
		}
	case "needs_you", "waiting_input":
		a.State, a.Label = protocol.ActivityAsking, p.Summary
	case "idle", "done", "completed":
		a.State = protocol.ActivityDone
	case "fail", "failed":
		a.State, a.Label = protocol.ActivityError, p.Summary
	default:
		return a, false
	}
	a.Label = oneLine(a.Label, 160)
	return a, true
}

// hubChatActivity covers chat events the hub generates itself (no worker derived them:
// a lost connection, an engine that could not be reached).
func (h *Hub) hubChatActivity(cs store.ChatSession, ev protocol.AgentChatEvent) {
	a := protocol.SessionActivity{ChatID: cs.ID, WorkerID: cs.WorkerID, Source: protocol.ActivitySourceHub, At: time.Now().UTC()}
	switch ev.Kind {
	case protocol.EventError:
		a.State, a.Label = protocol.ActivityError, "Failed"
		if ev.Error != nil && ev.Error.Message != "" {
			a.Label = oneLine(ev.Error.Message, 160)
		}
	case protocol.EventTurnEnd:
		a.State = protocol.ActivityDone
		if ev.StopReason == "cancelled" {
			a.Label = "Cancelled"
		}
	default:
		return
	}
	h.setActivity(a)
}

// maxActivityLabel caps a reported label (runes): one line, or a few joined lines of
// tool calls running at once (nowline.Separator).
func maxActivityLabel(label string) int {
	if strings.Contains(label, nowline.Separator) {
		return 600
	}
	return 160
}

// activityDir is the session's working directory, for shortening paths in its now line:
// a task's checkout (effective dir, else workdir) or a chat's cwd. Cached per session.
func (h *Hub) activityDir(a protocol.SessionActivity) string {
	if h.store == nil {
		return ""
	}
	key := a.Key()
	h.activity.mu.Lock()
	dir, ok := h.activity.dirs[key]
	h.activity.mu.Unlock()
	if ok {
		return dir
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if a.ChatID != "" {
		if cs, err := h.store.GetChatSession(ctx, a.ChatID); err == nil {
			dir = cs.Cwd
		}
	} else if t, err := h.store.GetTask(ctx, a.TaskID); err == nil {
		dir = firstNonEmpty(t.EffectiveDir, t.Workdir)
	}
	if dir != "" {
		h.activity.mu.Lock()
		if h.activity.dirs == nil {
			h.activity.dirs = map[string]string{}
		}
		if len(h.activity.dirs) > maxActivityEntries {
			clear(h.activity.dirs)
		}
		h.activity.dirs[key] = dir
		h.activity.mu.Unlock()
	}
	return dir
}
