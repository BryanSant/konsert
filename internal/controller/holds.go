package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/store"
)

// Holds (PRD §5.8a): an intervention no agent asked for places a hold on its scope (a
// session, a machine, or a resource such as a service or port on a machine). While a
// hold is active nothing automatic happens there: the scheduler doesn't place work,
// requeue/orphan logic doesn't restart it, fan-out skips it, and the orchestrator's
// dispatch/steer tools refuse with an error that tells the agent to ask the user. The
// user releases a hold in the UI (or tells an agent to call release_hold). Holds never
// suspend anything by themselves.

// ResourceRestarts is the resource kind of a machine-scope hold that only blocks
// automatic restarts of tasks there (a terminal intervention whose target could not be
// derived from the command).
const ResourceRestarts = "restarts"

// AuditHoldRelease is the audit action for releasing a hold.
const AuditHoldRelease = "hold.release"

// holdCheck describes an automatic action to vet against active holds.
type holdCheck struct {
	workerID string
	taskID   string
	chatID   string
	// text is the action's free text (a prompt, steer input): resource holds on the
	// machine block it when it names the resource.
	text string
	// restart marks an automatic restart (requeue after a lost worker): every hold on
	// the machine blocks it.
	restart bool
	// sessionOnly checks only session-scope holds (e.g. an orchestrator cancel).
	sessionOnly bool
}

// holdBlocking returns the first active hold that refuses the action described by c.
func (h *Hub) holdBlocking(ctx context.Context, c holdCheck) (store.Hold, bool) {
	holds, err := h.store.ListHolds(ctx, store.HoldFilter{ActiveOnly: true})
	if err != nil {
		return store.Hold{}, false
	}
	for _, hd := range holds {
		if holdCovers(hd, c) {
			return hd, true
		}
	}
	return store.Hold{}, false
}

// holdCovers reports whether hold hd refuses the action c.
func holdCovers(hd store.Hold, c holdCheck) bool {
	switch hd.ScopeKind {
	case store.HoldScopeSession:
		return (c.taskID != "" && hd.TaskID == c.taskID) || (c.chatID != "" && hd.ChatID == c.chatID)
	}
	if c.sessionOnly || c.workerID == "" || hd.WorkerID != c.workerID {
		return false
	}
	switch hd.ScopeKind {
	case store.HoldScopeMachine:
		if hd.Resource != nil && hd.Resource.Kind == ResourceRestarts {
			return c.restart
		}
		return true
	case store.HoldScopeResource:
		return c.restart || textMentionsResource(c.text, hd.Resource)
	}
	return false
}

// textMentionsResource reports whether free text names a resource (its name as a word,
// or its port).
func textMentionsResource(text string, r *store.Resource) bool {
	if r == nil || strings.TrimSpace(text) == "" {
		return false
	}
	lower := strings.ToLower(text)
	if r.Name != "" {
		re := regexp.MustCompile(`(^|[^a-z0-9_.-])` + regexp.QuoteMeta(strings.ToLower(r.Name)) + `($|[^a-z0-9_-])`)
		if re.MatchString(lower) {
			return true
		}
	}
	if r.Port > 0 {
		re := regexp.MustCompile(`(^|[^0-9])` + strconv.Itoa(r.Port) + `($|[^0-9])`)
		if re.MatchString(lower) {
			return true
		}
	}
	return false
}

// holdError is the error an automatic action gets on a held scope. It tells the agent
// what happened and that it must ask the user.
func holdError(hd store.Hold, where string) error {
	scope := where
	switch hd.ScopeKind {
	case store.HoldScopeSession:
		scope = "this session"
		if hd.TaskID != "" {
			scope = "task " + hd.TaskID
		}
	case store.HoldScopeResource:
		scope = describeResource(hd.Resource) + " on " + where
	}
	return fmt.Errorf("%s is on hold since %s: %s (hold %s). Nothing automatic may happen there until the user releases it: "+
		"do not retry, restart or work around it — ask the user (ask_user) what to do. They can release the hold in Konsert, or tell you to call release_hold",
		scope, hd.CreatedAt.Local().Format("15:04"), hd.Summary, hd.SID)
}

// describeResource renders a resource: "service nginx", "port 80".
func describeResource(r *store.Resource) string {
	if r == nil {
		return "the machine"
	}
	switch {
	case r.Kind == ResourceRestarts:
		return "automatic restarts"
	case r.Port > 0 && r.Name == "":
		return "port " + strconv.Itoa(r.Port)
	case r.Name != "" && r.Kind == "":
		return r.Name
	case r.Name != "":
		return r.Kind + " " + r.Name
	}
	return r.Kind
}

// holdSpec describes a hold to place after an intervention.
type holdSpec struct {
	scope    string
	workerID string
	taskID   string
	chatID   string
	resource *store.Resource
	summary  string
}

// holdAfterIntervention places a hold for an unrequested user intervention (cause is a
// user action without a request correlation). An equal active hold is reused. It
// returns the hold and whether one is in place.
func (h *Hub) holdAfterIntervention(ctx context.Context, cause store.Cause, spec holdSpec) (store.Hold, bool) {
	if !isUserCause(cause) || cause.RequestID != "" {
		return store.Hold{}, false
	}
	ctx = context.WithoutCancel(ctx)
	if existing, ok := h.findEqualHold(ctx, spec); ok {
		return existing, true
	}
	hd, err := h.store.CreateHold(ctx, store.Hold{
		ScopeKind: spec.scope, WorkerID: spec.workerID, TaskID: spec.taskID, ChatID: spec.chatID,
		Resource: spec.resource, Summary: spec.summary, Cause: cause,
	})
	if err != nil {
		h.logger.Warn("create hold failed", "err", err)
		return store.Hold{}, false
	}
	h.broadcastFleetUpdate("hold", hd)
	ec := cause
	ec.HoldID = hd.ID
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindHold, Action: "created", Cause: ec,
		WorkerID: hd.WorkerID, TaskID: hd.TaskID, ChatID: hd.ChatID,
		Summary:  fmt.Sprintf("Hold %s placed on %s: %s. Nothing automatic happens there until the user releases it.", hd.SID, h.holdScopeLabel(ctx, hd), hd.Summary),
		MetaJSON: auditMeta(map[string]any{"hold_sid": hd.SID, "scope": hd.ScopeKind, "resource": hd.Resource}),
	})
	return hd, true
}

func (h *Hub) findEqualHold(ctx context.Context, spec holdSpec) (store.Hold, bool) {
	holds, err := h.store.ListHolds(ctx, store.HoldFilter{ActiveOnly: true, WorkerID: spec.workerID})
	if err != nil {
		return store.Hold{}, false
	}
	for _, hd := range holds {
		if hd.ScopeKind != spec.scope || hd.TaskID != spec.taskID || hd.ChatID != spec.chatID {
			continue
		}
		if describeResource(hd.Resource) == describeResource(spec.resource) {
			return hd, true
		}
	}
	return store.Hold{}, false
}

// holdScopeLabel renders a hold's scope for people: "task t-7qk3m on host-1",
// "service nginx on host-1", "host-1".
func (h *Hub) holdScopeLabel(ctx context.Context, hd store.Hold) string {
	where := h.workerLabel(ctx, hd.WorkerID)
	switch hd.ScopeKind {
	case store.HoldScopeSession:
		if hd.TaskID != "" {
			if t, err := h.store.GetTask(ctx, hd.TaskID); err == nil {
				return "task " + t.SID + " on " + where
			}
			return "task " + hd.TaskID
		}
		if cs, err := h.store.GetChatSession(ctx, hd.ChatID); err == nil {
			return "chat " + cs.SID + " (" + cs.Title + ")"
		}
		return "chat " + hd.ChatID
	case store.HoldScopeResource:
		return describeResource(hd.Resource) + " on " + where
	}
	if hd.Resource != nil && hd.Resource.Kind == ResourceRestarts {
		return "automatic restarts on " + where
	}
	return where
}

// releaseHold releases an active hold with the cause in ctx.
func (h *Hub) releaseHold(ctx context.Context, id, note string) (store.Hold, error) {
	hd, err := h.store.GetHold(ctx, id)
	if err != nil {
		return hd, err
	}
	if !hd.Active() {
		return hd, nil
	}
	cause := causeFrom(ctx)
	by := describeActor(cause)
	if cause.ActorID != "" && isUserCause(cause) {
		by = cause.ActorID
	}
	ok, err := h.store.ReleaseHold(context.WithoutCancel(ctx), hd.ID, by, strings.TrimSpace(note), time.Now().UTC())
	if err != nil {
		return hd, err
	}
	if updated, err := h.store.GetHold(ctx, hd.ID); err == nil {
		hd = updated
	}
	if !ok {
		return hd, nil
	}
	h.broadcastFleetUpdate("hold", hd)
	ec := cause
	ec.HoldID = hd.ID
	msg := fmt.Sprintf("Hold %s on %s released by %s", hd.SID, h.holdScopeLabel(ctx, hd), describeActor(cause))
	if n := strings.TrimSpace(note); n != "" {
		msg += ": " + n
	}
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindHold, Action: "released", Cause: ec,
		WorkerID: hd.WorkerID, TaskID: hd.TaskID, ChatID: hd.ChatID, Summary: msg,
		MetaJSON: auditMeta(map[string]any{"hold_sid": hd.SID}),
	})
	h.recordAudit(ctx, store.AuditEvent{
		Action: AuditHoldRelease, WorkerID: hd.WorkerID, TaskID: hd.TaskID, Message: msg,
		MetaJSON: auditMeta(map[string]any{"hold_id": hd.ID, "hold_sid": hd.SID}),
	})
	// Work that waited for the scope may run now.
	h.kickScheduler()
	return hd, nil
}

// releaseSessionHolds releases the active session holds on a task or chat (resume,
// hand back after Take control).
func (h *Hub) releaseSessionHolds(ctx context.Context, taskID, chatID, note string) []store.Hold {
	holds, err := h.store.ListHolds(ctx, store.HoldFilter{ActiveOnly: true, TaskID: taskID, ChatID: chatID})
	if err != nil {
		return nil
	}
	var out []store.Hold
	for _, hd := range holds {
		if hd.ScopeKind != store.HoldScopeSession {
			continue
		}
		if r, err := h.releaseHold(ctx, hd.ID, note); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// holdLabelsFor lists active holds covering a task's session or machine as "h-… summary".
func (h *Hub) holdLabelsFor(ctx context.Context, workerID, taskID, chatID string) []string {
	holds, err := h.store.ListHolds(ctx, store.HoldFilter{ActiveOnly: true})
	if err != nil {
		return nil
	}
	var out []string
	for _, hd := range holds {
		session := hd.ScopeKind == store.HoldScopeSession && ((taskID != "" && hd.TaskID == taskID) || (chatID != "" && hd.ChatID == chatID))
		machine := hd.ScopeKind != store.HoldScopeSession && workerID != "" && hd.WorkerID == workerID
		if session || machine {
			out = append(out, hd.SID+" "+h.holdScopeLabel(ctx, hd)+": "+hd.Summary)
		}
	}
	return out
}

// --- terminal command heuristics ---------------------------------------------------------

// commandEffect is what a (redacted) terminal command does to the machine, as far as a
// heuristic can tell.
type commandEffect struct {
	// intervention: the command acts on something an agent may depend on (stops, kills,
	// restarts, reconfigures), as opposed to reading or editing files.
	intervention bool
	verb         string // stop, start, restart, kill, reload, ...
	resource     *store.Resource
}

var envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// commandWords splits a redacted command into words, dropping sudo/doas, env
// assignments and quotes; only the first pipeline/sequence segment is considered.
func commandWords(cmd string) []string {
	for _, sep := range []string{"&&", "||", ";", "|"} {
		if i := strings.Index(cmd, sep); i >= 0 {
			cmd = cmd[:i]
		}
	}
	var out []string
	for _, f := range strings.Fields(cmd) {
		f = strings.Trim(f, `"'`)
		if f == "" {
			continue
		}
		if len(out) == 0 && (f == "sudo" || f == "doas" || f == "env" || f == "nohup" || f == "time" || envAssign.MatchString(f)) {
			continue
		}
		if len(out) == 0 && strings.HasPrefix(f, "-") {
			continue // sudo flags
		}
		out = append(out, f)
	}
	return out
}

// positional returns the non-flag words of args.
func positional(args []string) []string {
	var out []string
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(a, "-") {
			// Flags that take a value.
			switch a {
			case "-u", "--user", "-H", "--host", "-s", "--signal", "-n", "--namespace":
				skipNext = true
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

var serviceVerbs = map[string]bool{
	"stop": true, "start": true, "restart": true, "reload": true, "kill": true, "disable": true,
	"enable": true, "mask": true, "unmask": true, "try-restart": true, "reload-or-restart": true, "condrestart": true,
}

// analyzeCommand derives the effect of a redacted terminal command.
func analyzeCommand(cmd string) commandEffect {
	w := commandWords(cmd)
	if len(w) == 0 {
		return commandEffect{}
	}
	head := strings.ToLower(w[0])
	if i := strings.LastIndexAny(head, `/\`); i >= 0 {
		head = head[i+1:]
	}
	head = strings.TrimSuffix(head, ".exe")
	args := w[1:]
	pos := positional(args)
	svc := func(verb, name string) commandEffect {
		name = strings.TrimSuffix(strings.Trim(name, `"'`), ".service")
		if name == "" || name == "…" {
			return commandEffect{intervention: true, verb: verb}
		}
		return commandEffect{intervention: true, verb: verb, resource: &store.Resource{Kind: "service", Name: name}}
	}
	switch head {
	case "systemctl":
		if len(pos) >= 1 && serviceVerbs[strings.ToLower(pos[0])] {
			if len(pos) >= 2 {
				return svc(strings.ToLower(pos[0]), pos[1])
			}
			return commandEffect{intervention: true, verb: strings.ToLower(pos[0])}
		}
		if len(pos) >= 1 && (pos[0] == "reboot" || pos[0] == "poweroff" || pos[0] == "halt" || pos[0] == "suspend") {
			return commandEffect{intervention: true, verb: pos[0]}
		}
	case "service":
		if len(pos) >= 2 && serviceVerbs[strings.ToLower(pos[1])] {
			return svc(strings.ToLower(pos[1]), pos[0])
		}
	case "launchctl":
		if len(pos) >= 1 {
			switch v := strings.ToLower(pos[0]); v {
			case "stop", "start", "kickstart", "bootout", "unload", "load", "kill", "bootstrap", "disable", "enable":
				if len(pos) >= 2 {
					name := pos[len(pos)-1]
					if i := strings.LastIndex(name, "/"); i >= 0 {
						name = name[i+1:]
					}
					name = strings.TrimSuffix(name, ".plist")
					return commandEffect{intervention: true, verb: v, resource: &store.Resource{Kind: "service", Name: name}}
				}
				return commandEffect{intervention: true, verb: v}
			}
		}
	case "brew":
		if len(pos) >= 2 && pos[0] == "services" && serviceVerbs[strings.ToLower(pos[1])] {
			if len(pos) >= 3 {
				return svc(strings.ToLower(pos[1]), pos[2])
			}
			return commandEffect{intervention: true, verb: strings.ToLower(pos[1])}
		}
	case "sc", "net":
		if len(pos) >= 2 {
			switch v := strings.ToLower(pos[0]); v {
			case "stop", "start", "pause", "continue":
				return svc(v, pos[1])
			}
		}
	case "stop-service", "start-service", "restart-service", "suspend-service":
		verb := strings.TrimSuffix(head, "-service")
		name := ""
		for i := 0; i < len(args); i++ {
			if strings.EqualFold(args[i], "-name") && i+1 < len(args) {
				name = args[i+1]
				break
			}
		}
		if name == "" && len(pos) > 0 {
			name = pos[0]
		}
		return svc(verb, name)
	case "stop-process", "taskkill":
		name := ""
		for i := 0; i < len(args); i++ {
			if (strings.EqualFold(args[i], "-name") || strings.EqualFold(args[i], "/im")) && i+1 < len(args) {
				name = strings.TrimSuffix(args[i+1], ".exe")
				break
			}
		}
		if name != "" {
			return commandEffect{intervention: true, verb: "kill", resource: &store.Resource{Kind: "process", Name: name}}
		}
		return commandEffect{intervention: true, verb: "kill"}
	case "pkill", "killall":
		if len(pos) >= 1 {
			return commandEffect{intervention: true, verb: "kill", resource: &store.Resource{Kind: "process", Name: pos[len(pos)-1]}}
		}
		return commandEffect{intervention: true, verb: "kill"}
	case "kill":
		if len(pos) >= 1 {
			if _, err := strconv.Atoi(pos[0]); err == nil {
				return commandEffect{intervention: true, verb: "kill", resource: &store.Resource{Kind: "process", Name: "pid " + pos[0]}}
			}
		}
		return commandEffect{intervention: true, verb: "kill"}
	case "fuser":
		for _, a := range pos {
			p, _, _ := strings.Cut(a, "/")
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				return commandEffect{intervention: true, verb: "kill", resource: &store.Resource{Kind: "port", Port: n}}
			}
		}
	case "docker", "podman", "nerdctl":
		if len(pos) >= 1 {
			v := strings.ToLower(pos[0])
			if v == "container" && len(pos) >= 2 {
				v, pos = strings.ToLower(pos[1]), pos[1:]
			}
			switch v {
			case "stop", "kill", "restart", "rm", "start", "pause", "unpause":
				if len(pos) >= 2 {
					return commandEffect{intervention: true, verb: v, resource: &store.Resource{Kind: "container", Name: pos[1]}}
				}
				return commandEffect{intervention: true, verb: v}
			case "compose":
				if len(pos) >= 2 {
					switch cv := strings.ToLower(pos[1]); cv {
					case "down", "stop", "restart", "up", "kill", "rm":
						if len(pos) >= 3 {
							return commandEffect{intervention: true, verb: cv, resource: &store.Resource{Kind: "container", Name: pos[2]}}
						}
						return commandEffect{intervention: true, verb: cv}
					}
				}
			}
		}
	case "docker-compose":
		if len(pos) >= 1 {
			switch cv := strings.ToLower(pos[0]); cv {
			case "down", "stop", "restart", "up", "kill", "rm":
				if len(pos) >= 2 {
					return commandEffect{intervention: true, verb: cv, resource: &store.Resource{Kind: "container", Name: pos[1]}}
				}
				return commandEffect{intervention: true, verb: cv}
			}
		}
	case "nginx", "apachectl", "httpd", "caddy":
		for i, a := range args {
			if (a == "-s" || a == "-k") && i+1 < len(args) {
				return commandEffect{intervention: true, verb: args[i+1], resource: &store.Resource{Kind: "service", Name: head}}
			}
		}
		if len(pos) >= 1 && (pos[0] == "stop" || pos[0] == "reload" || pos[0] == "restart" || pos[0] == "graceful") {
			return commandEffect{intervention: true, verb: pos[0], resource: &store.Resource{Kind: "service", Name: head}}
		}
	case "pg_ctl", "pg_ctlcluster":
		for _, a := range pos {
			switch a {
			case "stop", "start", "restart", "reload":
				return commandEffect{intervention: true, verb: a, resource: &store.Resource{Kind: "service", Name: "postgres"}}
			}
		}
	case "shutdown", "reboot", "halt", "poweroff":
		return commandEffect{intervention: true, verb: head}
	}
	return commandEffect{}
}

// sameResource reports whether two resources name the same thing (case-insensitive
// names; ports by number). Services and processes of the same name match each other.
func sameResource(a, b *store.Resource) bool {
	if a == nil || b == nil {
		return false
	}
	if a.Port > 0 && a.Port == b.Port {
		return true
	}
	if a.Name == "" || b.Name == "" {
		return false
	}
	an, bn := strings.ToLower(a.Name), strings.ToLower(b.Name)
	if an != bn && strings.TrimSuffix(an, "d") != strings.TrimSuffix(bn, "d") {
		return false
	}
	kinds := func(k string) string {
		switch k {
		case "service", "process", "daemon":
			return "service"
		}
		return k
	}
	return kinds(a.Kind) == kinds(b.Kind) || a.Kind == "" || b.Kind == ""
}

// --- REST --------------------------------------------------------------------------------

func (s *Server) handleListHolds(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.HoldFilter{
		ActiveOnly: q.Get("active") == "1" || q.Get("active") == "true",
		WorkerID:   resolveRef(r.Context(), s.store, ids.Machine, strings.TrimSpace(q.Get("worker_id"))),
		TaskID:     resolveRef(r.Context(), s.store, ids.Task, strings.TrimSpace(q.Get("task_id"))),
		ChatID:     resolveRef(r.Context(), s.store, ids.Chat, strings.TrimSpace(q.Get("chat_id"))),
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		f.Limit = n
	}
	holds, err := s.store.ListHolds(r.Context(), f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if holds == nil {
		holds = []store.Hold{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"holds": holds})
}

func (s *Server) handleReleaseHold(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Note string `json:"note"`
	}
	if requestHasBody(r) && !decodeJSON(w, r, &body) {
		return
	}
	hd, err := s.hub.releaseHold(r.Context(), r.PathValue("id"), body.Note)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "hold not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, hd)
}

// handleCreateHold places a hold the user asked for directly (the Hold button on a
// session or machine): a session hold for a task or chat, else a machine hold. A hold
// never suspends anything; it stops automatic actions there until it is released.
func (s *Server) handleCreateHold(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TaskID   string `json:"task_id"`
		ChatID   string `json:"chat_id"`
		WorkerID string `json:"worker_id"`
		Note     string `json:"note"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	ctx := r.Context()
	cause := causeFrom(ctx)
	if !isUserCause(cause) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "only a user can place a hold"})
		return
	}
	note := strings.TrimSpace(body.Note)
	summary := func(label string) string {
		if note != "" {
			return "The user held " + label + ": " + note
		}
		return "The user held " + label
	}
	var spec holdSpec
	var label string
	switch {
	case strings.TrimSpace(body.TaskID) != "":
		t, err := s.store.GetTask(ctx, resolveRef(ctx, s.store, ids.Task, strings.TrimSpace(body.TaskID)))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		label = fmt.Sprintf("task %s on %s", t.SID, s.hub.workerLabel(ctx, t.WorkerID))
		spec = holdSpec{scope: store.HoldScopeSession, workerID: t.WorkerID, taskID: t.ID, summary: summary(label)}
	case strings.TrimSpace(body.ChatID) != "":
		cs, err := s.store.GetChatSession(ctx, resolveRef(ctx, s.store, ids.Chat, strings.TrimSpace(body.ChatID)))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "chat not found"})
			return
		}
		label = fmt.Sprintf("chat %s (%s)", cs.SID, cs.Title)
		spec = holdSpec{scope: store.HoldScopeSession, workerID: cs.WorkerID, chatID: cs.ID, summary: summary(label)}
	case strings.TrimSpace(body.WorkerID) != "":
		wk, err := s.store.GetWorker(ctx, resolveRef(ctx, s.store, ids.Machine, strings.TrimSpace(body.WorkerID)))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "machine not found"})
			return
		}
		label = wk.Name
		spec = holdSpec{scope: store.HoldScopeMachine, workerID: wk.ID, summary: summary(wk.Name)}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id, chat_id or worker_id is required"})
		return
	}
	hd, ok := s.hub.holdAfterIntervention(ctx, cause, spec)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not place the hold"})
		return
	}
	s.hub.recordIntervention(ctx, cause, "hold", spec.workerID, spec.taskID, spec.chatID,
		fmt.Sprintf("%s held %s", capitalize(describeActor(cause)), label))
	writeJSON(w, http.StatusOK, hd)
}
