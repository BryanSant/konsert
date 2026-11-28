package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

// Override requests (break-glass, PRD §5.8a): a commander asks the user for a
// time-boxed exception — one more session on a machine than its slot limit, or a hold
// lifted for a while — with request_override. The request shows in Needs you with what
// approving allows. Approval is enforced (the scheduler, dispatch, steer, cancel,
// restarts and fan-out honour it for its scope) until it expires, and measured (every
// action it lets through is counted). The decision, and the expiry, reach the
// commander as fleet events through the normal delivery path.

// EventKindOverride is the fleet event kind of override requests (Action: created |
// approved | denied | used | expired).
const EventKindOverride = "override"

// AuditOverrideDecide is the audit action for approving or denying an override.
const AuditOverrideDecide = "override.decide"

// maxOverrideMinutes bounds an override's duration (one day).
const maxOverrideMinutes = 24 * 60

// errOverrideDecided is returned when deciding an override that is no longer pending.
var errOverrideDecided = errors.New("the override request was already decided")

// requestOverrideInput is the request_override MCP tool input (and POST /api/overrides).
type requestOverrideInput struct {
	Scope           string   `json:"scope" jsonschema:"machine_slots: run more sessions at once on a machine than its slot limit; hold: lift one hold (h-…) for a while; session: lift the session holds on one task or chat; machine: lift the machine and resource holds on a machine; policy: suspend machine policy rules (rules) on a machine"`
	Rules           []string `json:"rules,omitempty" jsonschema:"For policy: the machine policy rules to suspend: role, allowed_work, quiet_hours, max_sessions, max_load, reserve_cpu, min_free_ram"`
	Machine         string   `json:"machine,omitempty" jsonschema:"For machine_slots and machine: the machine (id, sid m-… or name)"`
	HoldID          string   `json:"hold_id,omitempty" jsonschema:"For hold: the hold id or sid (h-…)"`
	TaskID          string   `json:"task_id,omitempty" jsonschema:"For session: the task id or sid (t-…)"`
	ChatID          string   `json:"chat_id,omitempty" jsonschema:"For session: the agent chat id or sid (c-…)"`
	ExtraSlots      int      `json:"extra_slots,omitzero" jsonschema:"For machine_slots: sessions allowed beyond the limit (default 1, max 8)"`
	DurationMinutes int      `json:"duration_minutes" jsonschema:"How long the override lasts once approved, in minutes (1-1440)"`
	Reason          string   `json:"reason" jsonschema:"Why you need it, in one or two sentences the user can judge"`
}

// createOverride validates and stores an override request from chatID ("" over REST)
// and tells browsers about it.
func (h *Hub) createOverride(ctx context.Context, chatID string, in requestOverrideInput) (store.Override, error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return store.Override{}, errors.New("reason is required: say why you need the override")
	}
	if in.DurationMinutes < 1 || in.DurationMinutes > maxOverrideMinutes {
		return store.Override{}, fmt.Errorf("duration_minutes must be between 1 and %d", maxOverrideMinutes)
	}
	o := store.Override{ChatID: chatID, ScopeKind: strings.TrimSpace(in.Scope), DurationSeconds: in.DurationMinutes * 60, Reason: reason}
	switch o.ScopeKind {
	case store.OverrideScopeMachineSlots, store.OverrideScopeMachine, store.OverrideScopePolicy:
		m := strings.TrimSpace(in.Machine)
		if m == "" {
			return store.Override{}, fmt.Errorf("machine is required for scope %s", o.ScopeKind)
		}
		w, err := h.store.GetWorker(ctx, h.resolveMachine(ctx, m))
		if err != nil {
			return store.Override{}, fmt.Errorf("unknown machine %q", m)
		}
		o.WorkerID = w.ID
		if o.ScopeKind == store.OverrideScopePolicy {
			rules, err := overrideRules(in.Rules)
			if err != nil {
				return store.Override{}, err
			}
			o.Rules = rules
		}
		if o.ScopeKind == store.OverrideScopeMachineSlots {
			o.ExtraSlots = in.ExtraSlots
			if o.ExtraSlots == 0 {
				o.ExtraSlots = 1
			}
			if o.ExtraSlots < 1 || o.ExtraSlots > 8 {
				return store.Override{}, errors.New("extra_slots must be between 1 and 8")
			}
		}
	case store.OverrideScopeHold:
		ref := strings.TrimSpace(in.HoldID)
		hd, err := h.store.GetHold(ctx, resolveRef(ctx, h.store, ids.Hold, ref))
		if err != nil {
			return store.Override{}, fmt.Errorf("hold %q not found", ref)
		}
		if !hd.Active() {
			return store.Override{}, fmt.Errorf("hold %s was already released: nothing to override", hd.SID)
		}
		o.HoldID, o.WorkerID, o.TaskID, o.TargetChatID = hd.ID, hd.WorkerID, hd.TaskID, hd.ChatID
	case store.OverrideScopeSession:
		switch {
		case strings.TrimSpace(in.TaskID) != "":
			t, err := h.store.GetTask(ctx, resolveRef(ctx, h.store, ids.Task, strings.TrimSpace(in.TaskID)))
			if err != nil {
				return store.Override{}, fmt.Errorf("task %q not found", in.TaskID)
			}
			o.TaskID, o.WorkerID = t.ID, t.WorkerID
		case strings.TrimSpace(in.ChatID) != "":
			cs, err := h.store.GetChatSession(ctx, resolveRef(ctx, h.store, ids.Chat, strings.TrimSpace(in.ChatID)))
			if err != nil {
				return store.Override{}, fmt.Errorf("chat %q not found", in.ChatID)
			}
			o.TargetChatID, o.WorkerID = cs.ID, cs.WorkerID
		default:
			return store.Override{}, errors.New("task_id or chat_id is required for scope session")
		}
	default:
		return store.Override{}, fmt.Errorf("unknown scope %q (want machine_slots, hold, session, machine or policy)", in.Scope)
	}
	o.Consequences = h.overrideConsequences(ctx, o)
	o, err := h.store.CreateOverride(context.WithoutCancel(ctx), o)
	if err != nil {
		return o, err
	}
	h.broadcastFleetUpdate("override", o)
	who := "The commander"
	if cs, err := h.store.GetChatSession(ctx, chatID); err == nil {
		who = fmt.Sprintf("The commander (chat %s)", cs.SID)
	} else if chatID == "" {
		who = capitalize(describeActor(causeFrom(ctx)))
	}
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: EventKindOverride, Action: "created", WorkerID: o.WorkerID, ChatID: chatID,
		Summary:  fmt.Sprintf("%s asked to %s for %s (override %s): %s", who, h.overrideScopeLabel(ctx, o), formatMinutes(o.DurationSeconds), o.SID, o.Reason),
		MetaJSON: auditMeta(map[string]any{"override_sid": o.SID, "scope": o.ScopeKind}),
	})
	if chatID != "" && h.overrideSelfApprove(ctx) {
		// The user let the commander approve its own requests (Settings): the decision
		// is recorded as the commander's, not the user's.
		cause := store.Cause{Actor: store.ActorAgent, ActorID: "commander", Source: "override_self_approve", Confidence: store.ConfidenceCertain}
		if dec, err := h.applyOverrideDecision(ctx, o, true, "self-approved: override_self_approve is on", cause); err == nil {
			o = dec
		}
	}
	return o, nil
}

// overrideScopeLabel renders what an override lifts: "run 1 extra session on box",
// "lift hold h-… on box".
func (h *Hub) overrideScopeLabel(ctx context.Context, o store.Override) string {
	where := h.workerLabel(ctx, o.WorkerID)
	switch o.ScopeKind {
	case store.OverrideScopePolicy:
		return fmt.Sprintf("suspend %s on %s", strings.Join(o.Rules, ", "), where)
	case store.OverrideScopeMachineSlots:
		return fmt.Sprintf("run %d extra session%s at once on %s", o.ExtraSlots, plural(o.ExtraSlots), where)
	case store.OverrideScopeHold:
		if hd, err := h.store.GetHold(ctx, o.HoldID); err == nil {
			return fmt.Sprintf("lift hold %s on %s", hd.SID, h.holdScopeLabel(ctx, hd))
		}
		return "lift hold " + o.HoldID
	case store.OverrideScopeSession:
		if o.TaskID != "" {
			if t, err := h.store.GetTask(ctx, o.TaskID); err == nil {
				return fmt.Sprintf("lift the holds on task %s on %s", t.SID, where)
			}
			return "lift the holds on task " + o.TaskID
		}
		if cs, err := h.store.GetChatSession(ctx, o.TargetChatID); err == nil {
			return fmt.Sprintf("lift the holds on chat %s (%s)", cs.SID, cs.Title)
		}
		return "lift the holds on chat " + o.TargetChatID
	case store.OverrideScopeMachine:
		return "lift the holds on " + where
	}
	return o.ScopeKind
}

// overrideConsequences describes, for the user deciding, what approving allows.
func (h *Hub) overrideConsequences(ctx context.Context, o store.Override) string {
	where := h.workerLabel(ctx, o.WorkerID)
	dur := formatMinutes(o.DurationSeconds)
	switch o.ScopeKind {
	case store.OverrideScopePolicy:
		return fmt.Sprintf("For %s, %s may take work its machine policy rules out (%s): the hub and the machine both let it through. The rules apply again when the override expires.",
			dur, where, strings.Join(o.Rules, ", "))
	case store.OverrideScopeMachineSlots:
		limit := 1
		if w, err := h.store.GetWorker(ctx, o.WorkerID); err == nil && w.MaxConcurrent > 0 {
			limit = w.MaxConcurrent
		}
		return fmt.Sprintf("%s may run up to %d sessions at once instead of %d for %s after you approve. It may run hotter and slower; the limit comes back by itself when the override expires.",
			where, limit+o.ExtraSlots, limit, dur)
	case store.OverrideScopeHold:
		label := o.HoldID
		if hd, err := h.store.GetHold(ctx, o.HoldID); err == nil {
			label = fmt.Sprintf("%s (%s)", hd.SID, hd.Summary)
		}
		return fmt.Sprintf("For %s, hold %s stops blocking automatic actions: the commander may dispatch, steer, cancel or restart work it covers. The hold stays in place and blocks again when the override expires.", dur, label)
	case store.OverrideScopeSession:
		return fmt.Sprintf("For %s, the commander may steer, cancel or restart this session automatically despite its holds. They stay in place and block again when the override expires.", dur)
	case store.OverrideScopeMachine:
		n := 0
		if holds, err := h.store.ListHolds(ctx, store.HoldFilter{ActiveOnly: true, WorkerID: o.WorkerID}); err == nil {
			for _, hd := range holds {
				if hd.ScopeKind != store.HoldScopeSession {
					n++
				}
			}
		}
		return fmt.Sprintf("For %s, nothing on %s blocks automatic work: its %d machine and resource hold%s are lifted (not released) and block again when the override expires.", dur, where, n, plural(n))
	}
	return ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// formatMinutes renders a duration in seconds as "30m", "2h", "1h30m".
func formatMinutes(secs int) string {
	m := secs / 60
	switch {
	case m < 60:
		return strconv.Itoa(m) + "m"
	case m%60 == 0:
		return strconv.Itoa(m/60) + "h"
	}
	return fmt.Sprintf("%dh%dm", m/60, m%60)
}

// decideOverride approves or denies a pending override (user only) and reports the
// decision to the commander.
func (h *Hub) decideOverride(ctx context.Context, id string, approve bool, note string) (store.Override, error) {
	o, err := h.store.GetOverride(ctx, id)
	if err != nil {
		return o, err
	}
	cause := causeFrom(ctx)
	if !isUserCause(cause) {
		return o, errors.New("only the user can decide an override request")
	}
	return h.applyOverrideDecision(ctx, o, approve, note, cause)
}

// applyOverrideDecision records a decision on a pending override by cause (the user,
// or the commander when self-approval is on) and tells the commander.
func (h *Hub) applyOverrideDecision(ctx context.Context, o store.Override, approve bool, note string, cause store.Cause) (store.Override, error) {
	if o.Status != store.OverridePending {
		return o, errOverrideDecided
	}
	now := time.Now().UTC()
	status, verb := store.OverrideDenied, "denied"
	var expires *time.Time
	if approve {
		status, verb = store.OverrideApproved, "approved"
		e := now.Add(time.Duration(o.DurationSeconds) * time.Second)
		expires = &e
	}
	by := describeActor(cause)
	if cause.ActorID != "" {
		by = cause.ActorID
	}
	note = strings.TrimSpace(note)
	ok, err := h.store.DecideOverride(context.WithoutCancel(ctx), o.ID, status, by, note, now, expires)
	if err != nil {
		return o, err
	}
	if !ok {
		return o, errOverrideDecided
	}
	if cur, err := h.store.GetOverride(ctx, o.ID); err == nil {
		o = cur
	}
	h.broadcastFleetUpdate("override", o)
	scope := h.overrideScopeLabel(ctx, o)
	var msg string
	who := "The user"
	if !isUserCause(cause) {
		who = "The commander (self-approval is on)"
	}
	if approve {
		msg = fmt.Sprintf("%s approved override %s (%s) for %s, until %s", who, o.SID, scope, formatMinutes(o.DurationSeconds), o.ExpiresAt.Local().Format("15:04"))
		if note != "" {
			msg += ` ("` + note + `")`
		}
		msg += ". Syre enforces it until then; afterwards the limit or hold applies again."
	} else {
		msg = fmt.Sprintf("The user denied override %s (%s)", o.SID, scope)
		if note != "" {
			msg += ` ("` + note + `")`
		}
		msg += ". Stay within the limit or hold: don't work around it; ask the user (ask_user) if you're stuck."
	}
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: EventKindOverride, Action: status, Cause: cause, WorkerID: o.WorkerID, ChatID: o.ChatID, Summary: msg,
		MetaJSON: auditMeta(map[string]any{"override_sid": o.SID, "scope": o.ScopeKind, "note": note}),
	})
	h.recordAudit(ctx, store.AuditEvent{Action: AuditOverrideDecide, WorkerID: o.WorkerID,
		Message:  fmt.Sprintf("%s override %s (%s)", capitalize(verb), o.SID, scope),
		MetaJSON: auditMeta(map[string]any{"override_id": o.ID, "status": status})})
	if approve {
		if o.ScopeKind == store.OverrideScopePolicy {
			h.pushMachineDirectives(ctx, o.WorkerID) // the daemon honours the lift too
		}
		// Work waiting for the slot or behind the hold may run now.
		h.kickScheduler()
	}
	return o, nil
}

// activeOverrides lists the overrides in force at now.
func (h *Hub) activeOverrides(ctx context.Context, now time.Time) []store.Override {
	list, err := h.store.ListOverrides(ctx, store.OverrideFilter{Status: store.OverrideApproved})
	if err != nil {
		return nil
	}
	out := list[:0]
	for _, o := range list {
		if o.ActiveAt(now) {
			out = append(out, o)
		}
	}
	return out
}

// overrideLifts reports whether override o lifts hold hd.
func overrideLifts(o store.Override, hd store.Hold) bool {
	switch o.ScopeKind {
	case store.OverrideScopeHold:
		return o.HoldID == hd.ID
	case store.OverrideScopeSession:
		return hd.ScopeKind == store.HoldScopeSession &&
			((o.TaskID != "" && hd.TaskID == o.TaskID) || (o.TargetChatID != "" && hd.ChatID == o.TargetChatID))
	case store.OverrideScopeMachine:
		return o.WorkerID != "" && hd.WorkerID == o.WorkerID && hd.ScopeKind != store.HoldScopeSession
	}
	return false
}

// useOverride counts one action an override let through; the first use is recorded in
// the timeline.
func (h *Hub) useOverride(ctx context.Context, o store.Override) {
	ctx = context.WithoutCancel(ctx)
	if err := h.store.MarkOverrideUsed(ctx, o.ID, time.Now().UTC()); err != nil {
		h.logger.Warn("mark override used failed", "override_id", o.ID, "err", err)
		return
	}
	cur, err := h.store.GetOverride(ctx, o.ID)
	if err != nil {
		return
	}
	h.broadcastFleetUpdate("override", cur)
	if cur.UseCount == 1 {
		h.recordFleetEvent(ctx, store.FleetEvent{
			Kind: EventKindOverride, Action: "used", WorkerID: cur.WorkerID, ChatID: cur.ChatID,
			Cause:    store.Cause{Actor: store.ActorSystem, Source: "override", Confidence: store.ConfidenceCertain},
			Summary:  fmt.Sprintf("Override %s (%s) was used for the first time", cur.SID, h.overrideScopeLabel(ctx, cur)),
			MetaJSON: auditMeta(map[string]any{"override_sid": cur.SID}),
		})
	}
}

// slotPlan applies the approved machine_slots overrides to a worker list for placement.
type slotPlan struct {
	base map[string]int
	ovs  map[string][]store.Override
}

// applySlotOverrides returns workers with their slot limits raised by the machine_slots
// overrides in force, and the plan to measure their use with.
func (h *Hub) applySlotOverrides(ctx context.Context, workers []store.Worker) ([]store.Worker, slotPlan) {
	plan := slotPlan{base: map[string]int{}, ovs: map[string][]store.Override{}}
	for _, o := range h.activeOverrides(ctx, time.Now().UTC()) {
		if o.ScopeKind == store.OverrideScopeMachineSlots {
			plan.ovs[o.WorkerID] = append(plan.ovs[o.WorkerID], o)
		}
	}
	if len(plan.ovs) == 0 {
		return workers, plan
	}
	out := make([]store.Worker, len(workers))
	for i, w := range workers {
		limit := w.MaxConcurrent
		if limit <= 0 {
			limit = 1
		}
		plan.base[w.ID] = limit
		for _, o := range plan.ovs[w.ID] {
			limit += o.ExtraSlots
		}
		w.MaxConcurrent = limit
		out[i] = w
	}
	return out, plan
}

// placed measures a placement on workerID that found active sessions there: when the
// machine was at (or over) its own limit, the slot override made it possible.
func (h *Hub) placed(ctx context.Context, p slotPlan, workerID string, active int) {
	ovs := p.ovs[workerID]
	if len(ovs) == 0 || active < p.base[workerID] {
		return
	}
	h.useOverride(ctx, ovs[0])
}

// expireOverrides ends the approved overrides whose time is up and tells the
// commander (enforcement already stopped at expires_at).
func (h *Hub) expireOverrides(ctx context.Context, now time.Time) {
	list, err := h.store.ListOverrides(ctx, store.OverrideFilter{Status: store.OverrideApproved})
	if err != nil {
		return
	}
	for _, o := range list {
		if o.ExpiresAt == nil || now.Before(*o.ExpiresAt) {
			continue
		}
		ok, err := h.store.ExpireOverride(context.WithoutCancel(ctx), o.ID, now)
		if err != nil || !ok {
			continue
		}
		cur, err := h.store.GetOverride(ctx, o.ID)
		if err != nil {
			continue
		}
		h.broadcastFleetUpdate("override", cur)
		usage := "it was never used"
		if cur.UseCount > 0 {
			usage = fmt.Sprintf("it was used %d time%s", cur.UseCount, plural(cur.UseCount))
		}
		h.recordFleetEvent(ctx, store.FleetEvent{
			Kind: EventKindOverride, Action: store.OverrideExpired, WorkerID: cur.WorkerID, ChatID: cur.ChatID,
			Cause:    store.Cause{Actor: store.ActorSystem, Source: "override", Confidence: store.ConfidenceCertain},
			Summary:  fmt.Sprintf("Override %s (%s) expired; %s. The limit or hold applies again: ask the user (request_override) if you still need it.", cur.SID, h.overrideScopeLabel(ctx, cur), usage),
			MetaJSON: auditMeta(map[string]any{"override_sid": cur.SID, "use_count": cur.UseCount}),
		})
		h.NotifyDirectiveExpiring(ctx, "", cur.ChatID, fmt.Sprintf("override %s expired", cur.SID))
		if cur.ScopeKind == store.OverrideScopePolicy {
			h.pushMachineDirectives(ctx, cur.WorkerID) // the rules apply on the daemon again
		}
	}
}

// requestOverrideReply tells the commander what happens next.
func requestOverrideReply(o store.Override) string {
	return fmt.Sprintf("Override request %s is waiting for the user in Syre (Needs you): %s "+
		"Don't act as if it were approved. Syre tells you the decision (approved, denied, and later when it expires) "+
		"in a note before your next turn, or starts a turn if you are idle. Continue with other work or end your turn.", o.SID, o.Consequences)
}

// --- REST ---------------------------------------------------------------------------------

func (s *Server) handleListOverrides(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.OverrideFilter{
		Status: strings.TrimSpace(q.Get("status")),
		ChatID: resolveRef(r.Context(), s.store, ids.Chat, strings.TrimSpace(q.Get("chat_id"))),
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		f.Limit = n
	}
	list, err := s.store.ListOverrides(r.Context(), f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []store.Override{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"overrides": list})
}

// handleCreateOverride files an override request over REST (syre mcp, scripts); the
// commander's own requests come through its request_override tool.
func (s *Server) handleCreateOverride(w http.ResponseWriter, r *http.Request) {
	var body requestOverrideInput
	if !decodeJSON(w, r, &body) {
		return
	}
	o, err := s.hub.createOverride(r.Context(), "", body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *Server) decideOverrideHandler(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Note string `json:"note"`
		}
		if requestHasBody(r) && !decodeJSON(w, r, &body) {
			return
		}
		o, err := s.hub.decideOverride(r.Context(), r.PathValue("id"), approve, body.Note)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "override request not found"})
		case err != nil:
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusOK, o)
		}
	}
}
