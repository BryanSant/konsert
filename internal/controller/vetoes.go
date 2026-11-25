package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

// Veto acknowledgement (SYR-X52J9): a veto is told to the agents it concerns — the
// commander and, for a chat veto, the vetoed chat — as a note prepended to their next
// prompt (delivery.go). The turn that carried the note ending normally is the agent's
// acknowledgement: the veto then resolves itself. A veto nobody acknowledged within the
// deadline (setting veto_ack_minutes, default 10) escalates in Needs you, where the user
// can dismiss it. A veto with nobody to tell (no commander, not a chat) closes at once.

// vetoAckSetting configures the acknowledgement deadline in minutes.
const vetoAckSetting = "veto_ack_minutes"

// defaultVetoAckTimeout is the acknowledgement deadline when not configured.
const defaultVetoAckTimeout = 10 * time.Minute

// vetoNoAgent is DismissedBy of a veto there was no agent to tell.
const vetoNoAgent = "syre: no agent to tell"

// Veto statuses as browsers see them (VetoView.Status).
const (
	VetoSent         = "sent"         // recorded, not yet told to an agent
	VetoDelivered    = "delivered"    // told to an agent, its turn still running
	VetoAcknowledged = "acknowledged" // the agent's turn with the veto ended normally
	VetoEscalated    = "escalated"    // not acknowledged within the deadline
	VetoDismissed    = "dismissed"    // closed by the user (or: nobody to tell)
)

// AuditVetoDismiss is the audit action for dismissing an unacknowledged veto.
const AuditVetoDismiss = "veto.dismiss"

// VetoView is a veto with its derived status, deadline and the agents' labels.
type VetoView struct {
	store.Veto
	Status      string    `json:"status"`
	AckDeadline time.Time `json:"ack_deadline"`
	// DeliveredTo / AckedBy name the chat ("the commander (chat c-…)").
	DeliveredTo string `json:"delivered_to,omitempty"`
	AckedBy     string `json:"acked_by,omitempty"`
}

// vetoAckTimeout returns the configured acknowledgement deadline.
func (h *Hub) vetoAckTimeout(ctx context.Context) time.Duration {
	if saved, err := h.store.GetAllSettings(ctx); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(saved[vetoAckSetting])); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return defaultVetoAckTimeout
}

func vetoStatus(v store.Veto) string {
	switch {
	case v.AckedAt != nil:
		return VetoAcknowledged
	case v.DismissedAt != nil:
		return VetoDismissed
	case v.EscalatedAt != nil:
		return VetoEscalated
	case v.DeliveredAt != nil:
		return VetoDelivered
	}
	return VetoSent
}

// chatLabel names a chat for people: "the commander (chat c-…)", "chat c-… (title)".
func (h *Hub) chatLabel(ctx context.Context, chatID string) string {
	if chatID == "" {
		return ""
	}
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil {
		return "chat " + chatID
	}
	if cs.FleetTools {
		return "the commander (chat " + cs.SID + ")"
	}
	return fmt.Sprintf("chat %s (%s)", cs.SID, cs.Title)
}

func (h *Hub) vetoView(ctx context.Context, v store.Veto, timeout time.Duration) VetoView {
	return VetoView{
		Veto: v, Status: vetoStatus(v), AckDeadline: v.CreatedAt.Add(timeout),
		DeliveredTo: h.chatLabel(ctx, v.DeliveredChatID), AckedBy: h.chatLabel(ctx, v.AckedChatID),
	}
}

func (h *Hub) broadcastVeto(ctx context.Context, v store.Veto) {
	h.broadcastFleetUpdate("veto", h.vetoView(ctx, v, h.vetoAckTimeout(ctx)))
}

// trackVeto records a veto (its fleet event ev) for acknowledgement. chatID is the
// chat the veto concerns (the vetoed chat, or the chat that dispatched the vetoed task).
func (h *Hub) trackVeto(ctx context.Context, ev store.FleetEvent, reason, note string) {
	ctx = context.WithoutCancel(ctx)
	v := store.Veto{
		Reason: reason, Note: strings.TrimSpace(note), Summary: ev.Summary,
		WorkerID: ev.WorkerID, TaskID: ev.TaskID, ChatID: ev.ChatID, FleetEventID: ev.ID, CreatedAt: ev.CreatedAt,
	}
	if ev.ChatID == "" && len(h.commanderChats(ctx)) == 0 {
		// Nobody to tell: the veto lives on in commander memory only.
		at := ev.CreatedAt
		v.DismissedAt, v.DismissedBy = &at, vetoNoAgent
	}
	v, err := h.store.CreateVeto(ctx, v)
	if err != nil {
		h.logger.Warn("track veto failed", "err", err)
		return
	}
	h.broadcastVeto(ctx, v)
}

// markVetoesDelivered records that the veto events among events were told to chatID in
// its turn turnID.
func (h *Hub) markVetoesDelivered(ctx context.Context, events []store.FleetEvent, chatID, turnID string) {
	var evIDs []int64
	for _, e := range events {
		if e.Kind == store.EventKindVeto {
			evIDs = append(evIDs, e.ID)
		}
	}
	if len(evIDs) == 0 || turnID == "" {
		return
	}
	updated, err := h.store.MarkVetoesDelivered(context.WithoutCancel(ctx), evIDs, chatID, turnID, time.Now().UTC())
	if err != nil {
		h.logger.Warn("mark vetoes delivered failed", "chat_id", chatID, "err", err)
		return
	}
	for _, v := range updated {
		h.broadcastVeto(ctx, v)
	}
}

// ackVetoes acknowledges the vetoes delivered in chatID's turn turnID, which ended
// normally.
func (h *Hub) ackVetoes(ctx context.Context, chatID, turnID string) {
	acked, err := h.store.AckVetoes(context.WithoutCancel(ctx), chatID, turnID, time.Now().UTC())
	if err != nil {
		h.logger.Warn("ack vetoes failed", "chat_id", chatID, "err", err)
		return
	}
	for _, v := range acked {
		h.broadcastVeto(ctx, v)
		h.recordFleetEvent(ctx, store.FleetEvent{
			Kind: store.EventKindNotice, Action: "veto_ack", ChatID: chatID, WorkerID: v.WorkerID, TaskID: v.TaskID,
			Cause:    store.Cause{Actor: store.ActorSystem, Source: "delivery", ActorID: chatID, Confidence: store.ConfidenceCertain},
			Summary:  fmt.Sprintf("%s acknowledged veto %s on its next turn", capitalize(h.chatLabel(ctx, chatID)), v.SID),
			MetaJSON: auditMeta(map[string]any{"veto_sid": v.SID}),
		})
	}
}

// escalateVetoes escalates the open vetoes past their acknowledgement deadline at now.
func (h *Hub) escalateVetoes(ctx context.Context, now time.Time) {
	open, err := h.store.ListVetoes(ctx, store.VetoFilter{OpenOnly: true})
	if err != nil || len(open) == 0 {
		return
	}
	timeout := h.vetoAckTimeout(ctx)
	for _, v := range open {
		if v.EscalatedAt != nil || now.Before(v.CreatedAt.Add(timeout)) {
			continue
		}
		ok, err := h.store.EscalateVeto(context.WithoutCancel(ctx), v.ID, now)
		if err != nil || !ok {
			continue
		}
		cur, err := h.store.GetVeto(ctx, v.ID)
		if err != nil {
			continue
		}
		h.broadcastVeto(ctx, cur)
		state := "it has not reached an agent yet (no commander or vetoed chat has had a turn since)"
		if cur.DeliveredAt != nil {
			state = "it was told to " + h.chatLabel(ctx, cur.DeliveredChatID) + " but that turn has not ended normally"
		}
		h.recordFleetEvent(ctx, store.FleetEvent{
			Kind: store.EventKindNotice, Action: "veto_unacknowledged", ChatID: cur.ChatID, WorkerID: cur.WorkerID, TaskID: cur.TaskID,
			Cause:    store.Cause{Actor: store.ActorSystem, Source: "delivery", Confidence: store.ConfidenceCertain},
			Summary:  fmt.Sprintf("Veto %s was not acknowledged within %s: %s", cur.SID, formatMinutes(int(timeout.Seconds())), state),
			MetaJSON: auditMeta(map[string]any{"veto_sid": cur.SID}),
		})
	}
}

// dismissVeto closes an open veto without acknowledgement (user action).
func (h *Hub) dismissVeto(ctx context.Context, id string) (store.Veto, error) {
	v, err := h.store.GetVeto(ctx, id)
	if err != nil {
		return v, err
	}
	cause := causeFrom(ctx)
	if !isUserCause(cause) {
		return v, errors.New("only the user can dismiss a veto")
	}
	by := describeActor(cause)
	if cause.ActorID != "" {
		by = cause.ActorID
	}
	ok, err := h.store.DismissVeto(context.WithoutCancel(ctx), v.ID, by, time.Now().UTC())
	if err != nil {
		return v, err
	}
	if cur, err := h.store.GetVeto(ctx, v.ID); err == nil {
		v = cur
	}
	if ok {
		h.broadcastVeto(ctx, v)
		h.recordAudit(ctx, store.AuditEvent{Action: AuditVetoDismiss, WorkerID: v.WorkerID, TaskID: v.TaskID,
			Message: "Dismissed unacknowledged veto " + v.SID, MetaJSON: auditMeta(map[string]any{"veto_id": v.ID})})
	}
	return v, nil
}

// sweepControl runs the time-based human-control transitions: overrides expire and
// unacknowledged vetoes escalate. The reaper calls it on every tick.
func (h *Hub) sweepControl(ctx context.Context, now time.Time) {
	h.expireOverrides(ctx, now)
	h.escalateVetoes(ctx, now)
}

// --- REST ---------------------------------------------------------------------------------

func (s *Server) handleListVetoes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.VetoFilter{OpenOnly: q.Get("open") == "1" || q.Get("open") == "true"}
	if !f.OpenOnly {
		f.Since = time.Now().Add(-24 * time.Hour)
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		f.Limit = n
	}
	list, err := s.store.ListVetoes(r.Context(), f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	timeout := s.hub.vetoAckTimeout(r.Context())
	out := make([]VetoView, 0, len(list))
	for _, v := range list {
		out = append(out, s.hub.vetoView(r.Context(), v, timeout))
	}
	writeJSON(w, http.StatusOK, map[string]any{"vetoes": out, "ack_minutes": int(timeout.Minutes())})
}

func (s *Server) handleDismissVeto(w http.ResponseWriter, r *http.Request) {
	v, err := s.hub.dismissVeto(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "veto not found"})
	case err != nil:
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, s.hub.vetoView(r.Context(), v, s.hub.vetoAckTimeout(r.Context())))
	}
}
