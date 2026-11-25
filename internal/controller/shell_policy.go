package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Dangling-shell remedy (SYR-DCEZV; docs/design/activity.md "Shells left running"). A
// session that ended (a task finished, a chat turn ended) can leave shells running: dev
// servers, watchers, forgotten background jobs. Killing a user's dev server on its own
// is destructive, so the remedy is a policy, global with a per-project override:
//
//   - remind (default): after the session has been idle IdleMinutes with shells left,
//     raise one Needs-you item per session ("2 shells left running by <title>") offering
//     Kill all, Ask agent to clean up, Keep running (snoozes it).
//   - ask: shortly after the session ends with shells left, send the agent one follow-up
//     asking it to stop what it started (once per set of shells), unless the user is in
//     control (a hold on the session). When the agent can't be asked (the task ended, the
//     session is suspended or held) it falls back to the reminder.
//   - stop: kill the shells of a session idle IdleMinutes, audit-logged, never when the
//     task or chat is pinned with Keep or held (then it reminds instead).
//
// "Idle" is the session's: how long since it ended (its activity has been done/error
// since then). A dev server's shell keeps running a command, so a per-shell idle
// measure would never fire for the very shells this is about.
//
// State is in memory, like the activity it watches: after a hub restart syred
// re-reports the shells, the idle period starts again and snoozes are forgotten.

// Shell policy modes.
const (
	ShellPolicyRemind = "remind"
	ShellPolicyAsk    = "ask"
	ShellPolicyStop   = "stop"
)

// Settings keys of the shell policy (settings table).
const (
	shellPolicyModeSetting    = "shell_policy_mode"
	shellPolicyIdleSetting    = "shell_policy_idle_minutes"
	shellPolicyProjectSetting = "shell_policy_project:" // + project id
)

const (
	// defaultShellIdleMinutes is how long a session sits idle with shells left before
	// the reminder (or the automatic kill).
	defaultShellIdleMinutes = 30
	minShellIdleMinutes     = 1
	maxShellIdleMinutes     = 7 * 24 * 60
	// shellAskGrace lets shells that exit right after the session ended go before the
	// agent is asked about them.
	shellAskGrace = time.Minute
	// shellSnooze is how long Keep running hides a reminder.
	shellSnooze = 24 * time.Hour
	// shellKillRetry is how long after an automatic kill the shells may still be listed
	// before it is tried again.
	shellKillRetry = 5 * time.Minute
	// shellPolicyEvery is how often the hub looks at the shells left running.
	shellPolicyEvery = 30 * time.Second
)

// causeShellPolicy attributes automatic remedies.
var causeShellPolicy = store.Cause{Actor: store.ActorSystem, Source: "shell_policy", Confidence: store.ConfidenceCertain}

// ShellPolicySettings is GET/PUT /api/settings/shells: the global mode and idle period,
// and per-project modes ("" or missing = the global mode).
type ShellPolicySettings struct {
	Mode        string            `json:"mode"`
	IdleMinutes int               `json:"idle_minutes"`
	Projects    map[string]string `json:"projects"`
}

// modeFor is the mode of a session in project projectID.
func (p ShellPolicySettings) modeFor(projectID string) string {
	if m := p.Projects[projectID]; projectID != "" && validShellMode(m) {
		return m
	}
	if validShellMode(p.Mode) {
		return p.Mode
	}
	return ShellPolicyRemind
}

func validShellMode(m string) bool {
	return m == ShellPolicyRemind || m == ShellPolicyAsk || m == ShellPolicyStop
}

// ShellReminder is the Needs-you item for a session that left shells running.
type ShellReminder struct {
	// ID is the session key: "task:<id>" or "chat:<id>".
	ID       string `json:"id"`
	TaskID   string `json:"task_id,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
	WorkerID string `json:"worker_id,omitempty"`
	// Title is the item's line ("2 shells left running by Fix login"); SessionTitle the
	// task or chat title.
	Title        string               `json:"title"`
	SessionTitle string               `json:"session_title"`
	Shells       int                  `json:"shells"`
	ShellList    []protocol.ShellInfo `json:"shell_list,omitempty"`
	// IdleSince is when the session ended.
	IdleSince time.Time `json:"idle_since"`
	// CanAsk: the agent can still be asked to clean up (a running task or an agent chat
	// that is not suspended).
	CanAsk bool `json:"can_ask"`
	// Mode is the session's policy; Note says why it reminds instead of acting ("pinned
	// with Keep", "you are in control", "the agent can't be asked").
	Mode      string    `json:"mode"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Resolved marks, in a fleet.update, that the item is gone.
	Resolved bool `json:"resolved,omitzero"`
}

// shellWatch is what the policy remembers about one session with shells.
type shellWatch struct {
	// asked: shells (pid@start) the agent was already asked to stop.
	asked map[string]bool
	// reminder is the open Needs-you item, if any.
	reminder *ShellReminder
	// snoozedUntil: Keep running hides the reminder until then.
	snoozedUntil time.Time
	// killedAt: when the policy last killed the session's shells.
	killedAt time.Time
}

type shellPolicyState struct {
	mu      sync.Mutex
	watches map[string]*shellWatch
}

func (s *shellPolicyState) watch(key string) *shellWatch {
	if s.watches == nil {
		s.watches = map[string]*shellWatch{}
	}
	w := s.watches[key]
	if w == nil {
		w = &shellWatch{asked: map[string]bool{}}
		s.watches[key] = w
	}
	return w
}

func shellIdentKey(pid int, started time.Time) string {
	return strconv.Itoa(pid) + "@" + strconv.FormatInt(started.UnixNano(), 10)
}

// --- settings ---------------------------------------------------------------------------

// shellPolicySettings reads the policy from the settings table.
func (h *Hub) shellPolicySettings(ctx context.Context) ShellPolicySettings {
	p := ShellPolicySettings{Mode: ShellPolicyRemind, IdleMinutes: defaultShellIdleMinutes, Projects: map[string]string{}}
	saved, err := h.store.GetAllSettings(ctx)
	if err != nil {
		return p
	}
	if m := saved[shellPolicyModeSetting]; validShellMode(m) {
		p.Mode = m
	}
	if n, err := strconv.Atoi(saved[shellPolicyIdleSetting]); err == nil && n >= minShellIdleMinutes && n <= maxShellIdleMinutes {
		p.IdleMinutes = n
	}
	for k, v := range saved {
		if id, ok := strings.CutPrefix(k, shellPolicyProjectSetting); ok && id != "" && validShellMode(v) {
			p.Projects[id] = v
		}
	}
	return p
}

// saveShellPolicySettings validates and stores the policy. Projects missing from p (or
// set to "") go back to the global mode.
func (h *Hub) saveShellPolicySettings(ctx context.Context, p ShellPolicySettings) (ShellPolicySettings, error) {
	if !validShellMode(p.Mode) {
		return ShellPolicySettings{}, fmt.Errorf("mode must be %q, %q or %q", ShellPolicyRemind, ShellPolicyAsk, ShellPolicyStop)
	}
	if p.IdleMinutes < minShellIdleMinutes || p.IdleMinutes > maxShellIdleMinutes {
		return ShellPolicySettings{}, fmt.Errorf("idle_minutes must be between %d and %d", minShellIdleMinutes, maxShellIdleMinutes)
	}
	kv := map[string]string{shellPolicyModeSetting: p.Mode, shellPolicyIdleSetting: strconv.Itoa(p.IdleMinutes)}
	for id := range h.shellPolicySettings(ctx).Projects {
		kv[shellPolicyProjectSetting+id] = "" // cleared unless set again below
	}
	for id, m := range p.Projects {
		if m != "" && !validShellMode(m) {
			return ShellPolicySettings{}, fmt.Errorf("project %s: unknown mode %q", id, m)
		}
		if id != "" {
			kv[shellPolicyProjectSetting+id] = m
		}
	}
	if err := h.store.SetSettings(ctx, kv); err != nil {
		return ShellPolicySettings{}, err
	}
	return h.shellPolicySettings(ctx), nil
}

// --- the loop ---------------------------------------------------------------------------

// shellPolicyLoop applies the policy every ShellPolicyEvery (0 disables it).
func (h *Hub) shellPolicyLoop(ctx context.Context) {
	if h.store == nil || h.ShellPolicyEvery <= 0 {
		return
	}
	t := time.NewTicker(h.ShellPolicyEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.shellPolicyTick(ctx, h.shellNow())
		}
	}
}

// shellSessionInfo is what the policy needs to know about a session.
type shellSessionInfo struct {
	title     string
	projectID string
	workerID  string
	keep      bool
	canAsk    bool
}

func (h *Hub) shellSessionInfoFor(ctx context.Context, a protocol.SessionActivity) (shellSessionInfo, bool) {
	var in shellSessionInfo
	if a.ChatID != "" {
		cs, err := h.store.GetChatSession(ctx, a.ChatID)
		if err != nil {
			return in, false
		}
		in.title = firstNonEmpty(cs.Title, "chat "+cs.SID)
		in.workerID, in.keep = cs.WorkerID, cs.Keep
		in.canAsk = cs.Backend == store.ChatBackendAgent && cs.Status != protocol.ChatStatusSuspended
		in.projectID = h.projectForPath(ctx, cs.WorkerID, cs.Cwd)
		return in, true
	}
	t, err := h.store.GetTask(ctx, a.TaskID)
	if err != nil {
		return in, false
	}
	in.title = firstNonEmpty(t.Title, oneLine(firstLine(t.Prompt), 80), "task "+t.SID)
	in.workerID, in.keep = t.WorkerID, t.Keep
	in.canAsk = !protocol.IsTerminalTaskStatus(t.Status) && t.Status != protocol.TaskStatusSuspended
	in.projectID = t.ProjectID
	if in.projectID == "" {
		in.projectID = h.projectForPath(ctx, t.WorkerID, firstNonEmpty(t.EffectiveDir, t.Workdir))
	}
	return in, true
}

// projectForPath is the project of the repo on workerID whose path is the longest prefix
// of path ("" = none).
func (h *Hub) projectForPath(ctx context.Context, workerID, path string) string {
	p := strings.TrimRight(strings.TrimSpace(path), `/\`)
	if p == "" || workerID == "" {
		return ""
	}
	repos, err := h.store.ListRepos(ctx, "", workerID)
	if err != nil {
		return ""
	}
	best, bestLen := "", 0
	for _, r := range repos {
		rp := strings.TrimRight(r.Path, `/\`)
		if rp == "" || len(rp) <= bestLen {
			continue
		}
		if p == rp || strings.HasPrefix(p, rp+"/") || strings.HasPrefix(p, rp+`\`) {
			best, bestLen = r.ProjectID, len(rp)
		}
	}
	return best
}

// shellPolicyTick looks at every session with shells once, as of now.
func (h *Hub) shellPolicyTick(ctx context.Context, now time.Time) {
	policy := h.shellPolicySettings(ctx)
	idle := time.Duration(policy.IdleMinutes) * time.Minute
	seen := map[string]bool{}
	for _, a := range h.activityList() {
		if a.Shells == 0 {
			continue
		}
		key := a.Key()
		seen[key] = true
		if !protocol.ActivityTerminal(a.State) {
			// Running again: what it left is its business for now.
			h.resolveShellReminder(key)
			continue
		}
		info, ok := h.shellSessionInfoFor(ctx, a)
		if !ok {
			continue
		}
		since := a.Since
		if since.IsZero() {
			since = a.At
		}
		idleFor := now.Sub(since)
		mode := policy.modeFor(info.projectID)
		_, held, _ := h.holdVerdict(ctx, holdCheck{workerID: info.workerID, taskID: a.TaskID, chatID: a.ChatID, sessionOnly: true})
		note := ""
		switch mode {
		case ShellPolicyAsk:
			switch {
			case held:
				note = "you are in control of this session, so Syre didn't ask the agent"
			case !info.canAsk:
				note = "the session has ended, so its agent can't be asked"
			case idleFor >= shellAskGrace && h.hasUnaskedShells(key, a.ShellList):
				_, err := h.AskStopShells(withCause(ctx, causeShellPolicy), a.TaskID, a.ChatID, nil)
				h.markShellsAsked(key, a.ShellList)
				if err == nil {
					continue
				}
				h.logger.Info("shell policy: could not ask the agent", "session", key, "err", err)
				note = "Syre couldn't ask the agent: " + err.Error()
			default:
				if idleFor < shellAskGrace || h.hasUnaskedShells(key, a.ShellList) {
					continue
				}
				note = "the agent was asked to stop them"
			}
		case ShellPolicyStop:
			switch {
			case info.keep:
				note = "pinned with Keep, so Syre won't stop them"
			case held:
				note = "you are in control of this session, so Syre won't stop them"
			case idleFor >= idle:
				if h.recentlyKilled(key, now) {
					continue
				}
				why := fmt.Sprintf("after %s idle (Stop after idle)", humanMinutes(policy.IdleMinutes))
				_, err := h.killShells(withCause(ctx, causeShellPolicy), a.TaskID, a.ChatID, nil, why)
				if err == nil {
					h.shellPolicy.mu.Lock()
					h.shellPolicy.watch(key).killedAt = now
					h.shellPolicy.mu.Unlock()
					continue
				}
				note = "Syre couldn't stop them: " + err.Error()
			default:
				continue
			}
		}
		if idleFor < idle {
			continue
		}
		h.raiseShellReminder(a, info, mode, note, since, now)
	}
	// Sessions whose shells are all gone: forget them.
	h.shellPolicy.mu.Lock()
	var gone []string
	for key := range h.shellPolicy.watches {
		if !seen[key] {
			gone = append(gone, key)
		}
	}
	h.shellPolicy.mu.Unlock()
	for _, key := range gone {
		h.resolveShellReminder(key)
		h.shellPolicy.mu.Lock()
		delete(h.shellPolicy.watches, key)
		h.shellPolicy.mu.Unlock()
	}
}

func humanMinutes(n int) string {
	switch {
	case n == 1:
		return "1 minute"
	case n%60 == 0 && n >= 60:
		if n == 60 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", n/60)
	}
	return fmt.Sprintf("%d minutes", n)
}

func (h *Hub) recentlyKilled(key string, now time.Time) bool {
	h.shellPolicy.mu.Lock()
	defer h.shellPolicy.mu.Unlock()
	w := h.shellPolicy.watch(key)
	return !w.killedAt.IsZero() && now.Sub(w.killedAt) < shellKillRetry
}

// hasUnaskedShells reports whether list has a shell the agent was not asked about yet.
func (h *Hub) hasUnaskedShells(key string, list []protocol.ShellInfo) bool {
	h.shellPolicy.mu.Lock()
	defer h.shellPolicy.mu.Unlock()
	w := h.shellPolicy.watch(key)
	for _, s := range list {
		if !w.asked[shellIdentKey(s.PID, s.StartedAt)] {
			return true
		}
	}
	return false
}

// markShellsAsked remembers that the agent was asked about list.
func (h *Hub) markShellsAsked(key string, list []protocol.ShellInfo) {
	h.shellPolicy.mu.Lock()
	defer h.shellPolicy.mu.Unlock()
	w := h.shellPolicy.watch(key)
	for _, s := range list {
		w.asked[shellIdentKey(s.PID, s.StartedAt)] = true
	}
}

// shellsTitle is the reminder's line: "2 shells left running by Fix login".
func shellsTitle(n int, title string) string {
	what := "1 shell"
	if n != 1 {
		what = fmt.Sprintf("%d shells", n)
	}
	return what + " left running by " + title
}

// raiseShellReminder opens (or refreshes) the session's reminder unless it is snoozed.
func (h *Hub) raiseShellReminder(a protocol.SessionActivity, info shellSessionInfo, mode, note string, since, now time.Time) {
	key := a.Key()
	h.shellPolicy.mu.Lock()
	w := h.shellPolicy.watch(key)
	if now.Before(w.snoozedUntil) {
		h.shellPolicy.mu.Unlock()
		return
	}
	r := ShellReminder{
		ID: key, TaskID: a.TaskID, ChatID: a.ChatID, WorkerID: firstNonEmpty(a.WorkerID, info.workerID),
		Title: shellsTitle(a.Shells, info.title), SessionTitle: info.title,
		Shells: a.Shells, ShellList: a.ShellList, IdleSince: since.UTC(), CanAsk: info.canAsk,
		Mode: mode, Note: note, CreatedAt: now.UTC(),
	}
	if prev := w.reminder; prev != nil {
		r.CreatedAt = prev.CreatedAt
		if prev.Title == r.Title && prev.CanAsk == r.CanAsk && prev.Note == r.Note && sameShellIdents(prev.ShellList, r.ShellList) {
			h.shellPolicy.mu.Unlock()
			return
		}
	}
	w.reminder = &r
	h.shellPolicy.mu.Unlock()
	h.broadcastFleetUpdate("shell_reminder", r)
}

func sameShellIdents(a, b []protocol.ShellInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].PID != b[i].PID || !a[i].StartedAt.Equal(b[i].StartedAt) {
			return false
		}
	}
	return true
}

// resolveShellReminder closes the session's reminder, if open.
func (h *Hub) resolveShellReminder(key string) {
	h.shellPolicy.mu.Lock()
	w := h.shellPolicy.watches[key]
	if w == nil || w.reminder == nil {
		h.shellPolicy.mu.Unlock()
		return
	}
	r := *w.reminder
	w.reminder = nil
	h.shellPolicy.mu.Unlock()
	r.Resolved = true
	h.broadcastFleetUpdate("shell_reminder", r)
}

// ShellReminders lists the open reminders, oldest first.
func (h *Hub) ShellReminders() []ShellReminder {
	h.shellPolicy.mu.Lock()
	out := []ShellReminder{}
	for _, w := range h.shellPolicy.watches {
		if w.reminder != nil {
			out = append(out, *w.reminder)
		}
	}
	h.shellPolicy.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// KeepShellsRunning snoozes a session's reminder (Keep running): it comes back after
// shellSnooze if the shells still run.
func (h *Hub) KeepShellsRunning(ctx context.Context, taskID, chatID string) error {
	key, workerID, label, err := h.shellSession(ctx, taskID, chatID)
	if err != nil {
		return err
	}
	h.shellPolicy.mu.Lock()
	h.shellPolicy.watch(key).snoozedUntil = h.shellNow().Add(shellSnooze)
	h.shellPolicy.mu.Unlock()
	h.resolveShellReminder(key)
	cause := causeFrom(ctx)
	h.recordIntervention(ctx, cause, "shell_keep", workerID, taskID, chatID,
		fmt.Sprintf("%s kept the shells of %s running", capitalize(describeActor(cause)), label))
	return nil
}

// --- HTTP ---------------------------------------------------------------------------

func (s *Server) handleGetShellPolicy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.shellPolicySettings(r.Context()))
}

func (s *Server) handlePutShellPolicy(w http.ResponseWriter, r *http.Request) {
	var body ShellPolicySettings
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := s.hub.saveShellPolicySettings(r.Context(), body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListShellReminders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"reminders": s.hub.ShellReminders()})
}

// handleKeepShells serves POST /api/{tasks|chats}/{id}/shells/keep.
func (s *Server) handleKeepShells(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		taskID, chatID := r.PathValue("id"), ""
		if kind == "chat" {
			taskID, chatID = "", taskID
		}
		if err := s.hub.KeepShellsRunning(r.Context(), taskID, chatID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeControlError(w, err)
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
