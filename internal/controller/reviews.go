package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Reviews and delivery (SYR-H50YW): the Work board is private between the user and their
// agents. Review decisions (approve / request changes) and Expedite live on the hub.
// Request changes returns the note to the agent; Approve delivers the branch according to
// the repo's delivery mode (direct / pr_merge / pr_open / keep_branch). Expedited queued
// tasks are placed before the rest (SchedulePending).

// ReviewResult is the reply to a review decision.
type ReviewResult struct {
	Review store.Review `json:"review"`
	// FollowUpTaskID/SID: the session started with the requested changes (or conflict).
	FollowUpTaskID  string `json:"follow_up_task_id,omitempty"`
	FollowUpTaskSID string `json:"follow_up_task_sid,omitempty"`
	// Delivered says how requested changes reached the agent: "steered" (typed into the
	// running session) or "follow_up" (a new session in the same checkout).
	Delivered string `json:"delivered,omitempty"`
}

// saveReview applies change to the session's review (a new one when it has none),
// stores it and tells every browser. Reviews are read-modify-written under reviewMu.
func (h *Hub) saveReview(ctx context.Context, taskID, chatID string, change func(*store.Review)) (store.Review, error) {
	h.reviewMu.Lock()
	defer h.reviewMu.Unlock()
	r, err := h.store.GetReview(ctx, taskID, chatID)
	if errors.Is(err, store.ErrNotFound) {
		r, err = store.Review{TaskID: taskID, ChatID: chatID}, nil
	}
	if err != nil {
		return r, err
	}
	change(&r)
	saved, err := h.store.SaveReview(ctx, r)
	if err != nil {
		return saved, err
	}
	h.broadcastFleetUpdate("review", saved)
	return saved, nil
}

// SetExpedite puts a task or chat in (or takes it out of) the Expedite lane. An
// expedited queued task is placed before the rest of the queue.
func (h *Hub) SetExpedite(ctx context.Context, taskID, chatID string, on bool) (store.Review, error) {
	r, err := h.saveReview(ctx, taskID, chatID, func(r *store.Review) {
		if r.Expedite == on {
			return
		}
		r.Expedite = on
		r.ExpeditedAt = nil
		if on {
			now := time.Now().UTC()
			r.ExpeditedAt = &now
		}
	})
	if err == nil && on && taskID != "" {
		h.kickScheduler()
	}
	return r, err
}

// expeditedTasks returns the ids of expedited tasks.
func (h *Hub) expeditedTasks(ctx context.Context) map[string]bool {
	all, err := h.store.ListReviews(ctx)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, r := range all {
		if r.Expedite && r.TaskID != "" {
			out[r.TaskID] = true
		}
	}
	return out
}

// ReviewTask records the user's decision on finished work. Request changes hands the note
// back to the agent (steer or follow-up session). Approve runs the repo's delivery mode
// (direct, pr_merge, pr_open, or keep_branch). Outcomes are recorded on the task timeline
// and audit log.
func (h *Hub) ReviewTask(ctx context.Context, svc *FleetService, taskID, decision, note string) (ReviewResult, error) {
	var res ReviewResult
	if decision != store.ReviewApproved && decision != store.ReviewChangesRequested {
		return res, fmt.Errorf("%w: decision must be %q or %q", errBadRequest, store.ReviewApproved, store.ReviewChangesRequested)
	}
	note = strings.TrimSpace(note)
	if decision == store.ReviewChangesRequested && note == "" {
		return res, fmt.Errorf("%w: say what should change", errBadRequest)
	}
	t, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		return res, err
	}
	cause := causeFrom(ctx)
	now := h.deliveryClock()
	cur, err := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
		r.Decision, r.Note, r.DecidedBy, r.DecidedAt = decision, note, firstNonEmpty(cause.ActorID, cause.Actor), &now
		r.FollowUpTaskID = ""
	})
	if err != nil {
		return res, err
	}
	verb := "Approved"
	if decision == store.ReviewChangesRequested {
		verb = "Changes requested: " + oneLine(note, 200)
	}
	h.appendTaskEvent(ctx, t.ID, "review", verb)
	h.recordAudit(ctx, store.AuditEvent{
		Action:   AuditTaskReview,
		WorkerID: t.WorkerID,
		TaskID:   t.ID,
		Message:  verb,
		MetaJSON: auditMeta(map[string]any{"decision": decision}),
	})

	if decision == store.ReviewChangesRequested {
		res.Delivered, res.FollowUpTaskID, res.FollowUpTaskSID, err = h.deliverChanges(ctx, svc, t, note)
		var delMsg string
		if err != nil {
			h.logger.Warn("could not deliver requested changes", "task_id", t.ID, "err", err)
			delMsg = "The note did not reach the agent: " + err.Error()
		}
		res.Review, err = h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.FollowUpTaskID = res.FollowUpTaskID
			if delMsg != "" {
				r.DeliveryMessage = delMsg
			}
		})
		return res, err
	}

	return h.deliverApproved(ctx, svc, t, cur)
}

// deliverChanges hands requested changes to the agent the way a human message reaches a
// task: typed into the session while it still runs, else a follow-up session in the
// same checkout with the note and the original task.
func (h *Hub) deliverChanges(ctx context.Context, svc *FleetService, t store.Task, note string) (how, followID, followSID string, err error) {
	if !protocol.IsTerminalTaskStatus(t.Status) {
		if err := h.SendSteer(t.ID, note, true); err != nil {
			return "", "", "", err
		}
		h.appendTaskEvent(ctx, t.ID, "steer", note)
		return "steered", "", "", nil
	}
	if svc == nil {
		return "", "", "", errors.New("no dispatcher")
	}
	title := firstNonEmpty(t.Title, firstLine(t.Prompt))
	follow, err := svc.DispatchTask(ctx, DispatchRequest{
		WorkerSelector: WorkerSelector{WorkerID: t.WorkerID},
		Agent:          t.Agent,
		Model:          t.Model,
		ProjectID:      t.ProjectID,
		RepoID:         t.RepoID,
		WorktreeID:     t.WorktreeID,
		RepoPath:       firstNonEmpty(t.EffectiveDir, t.Workdir),
		Branch:         t.Branch,
		Title:          oneLine("Changes: "+title, 80),
		Prompt:         changeRequestPrompt(t, note),
		Mode:           "interactive",
		Origin:         protocol.TaskOriginDirect,
		PermissionMode: string(t.PermissionMode),
	})
	if err != nil {
		return "", "", "", err
	}
	return "follow_up", follow.TaskID, follow.TaskSID, nil
}

// changeRequestPrompt is the prompt of the follow-up session that applies requested
// changes to finished work.
func changeRequestPrompt(t store.Task, note string) string {
	return strings.Join([]string{
		fmt.Sprintf("Changes requested on %s (%q):", t.SID, firstNonEmpty(t.Title, firstLine(t.Prompt))),
		strings.TrimSpace(note),
		"",
		"The original task was:",
		strings.TrimSpace(t.Prompt),
	}, "\n")
}

// appendTaskEvent adds a task timeline entry and tells the browsers.
func (h *Hub) appendTaskEvent(ctx context.Context, taskID, kind, msg string) {
	ev := store.TaskEvent{TaskID: taskID, Kind: kind, Message: msg, CreatedAt: time.Now().UTC()}
	if id, err := h.store.AppendEvent(ctx, ev); err == nil {
		ev.ID = id
		h.broadcastFleetUpdate("event", ev)
	}
}

// --- acceptance criteria --------------------------------------------------------------

// AcceptanceItem is one acceptance criterion and whether it is met (ticket items only).
type AcceptanceItem struct {
	Text string `json:"text"`
	Done bool   `json:"done"`
}

// Acceptance is what a task was asked to achieve: the linked ka ticket's acceptance
// items, else the bullet lines of its prompt, else its goal.
type Acceptance struct {
	Source      string           `json:"source"` // ticket | prompt | goal
	TicketID    string           `json:"ticket_id,omitempty"`
	TicketTitle string           `json:"ticket_title,omitempty"`
	Items       []AcceptanceItem `json:"items"`
}

// maxAcceptance caps the criteria shown.
const maxAcceptance = 20

// TaskAcceptance returns the task's acceptance criteria.
func (h *Hub) TaskAcceptance(ctx context.Context, t store.Task) Acceptance {
	if t.TicketID != "" {
		rctx, cancel := context.WithTimeout(ctx, refResolveTimeout)
		raw, err := h.kaShow(rctx, t.TicketID)
		cancel()
		if err == nil {
			var tk struct {
				Title string           `json:"title"`
				Items []AcceptanceItem `json:"acceptance_items"`
			}
			if json.Unmarshal(raw, &tk) == nil && len(tk.Items) > 0 {
				items := make([]AcceptanceItem, 0, min(len(tk.Items), maxAcceptance))
				for _, it := range tk.Items[:min(len(tk.Items), maxAcceptance)] {
					if s := strings.TrimSpace(it.Text); s != "" {
						items = append(items, AcceptanceItem{Text: s, Done: it.Done})
					}
				}
				if len(items) > 0 {
					return Acceptance{Source: "ticket", TicketID: t.TicketID, TicketTitle: firstNonEmpty(t.TicketTitle, tk.Title), Items: items}
				}
			}
		}
	}
	return acceptanceFromPrompt(t.Prompt)
}

var (
	bulletRe   = regexp.MustCompile(`^(?:[-*•+]|\d+[.)])\s+`)
	checkboxRe = regexp.MustCompile(`(?i)^\[([ x])\]\s+`)
)

// acceptanceFromPrompt takes the prompt's bullet, numbered and checkbox lines ("- [x]"
// counts as met), else its first line as the goal.
func acceptanceFromPrompt(prompt string) Acceptance {
	var items []AcceptanceItem
	for line := range strings.SplitSeq(prompt, "\n") {
		l := strings.TrimSpace(line)
		bullet := bulletRe.MatchString(l)
		l = bulletRe.ReplaceAllString(l, "")
		done := false
		if m := checkboxRe.FindStringSubmatch(l); m != nil {
			done, bullet = strings.EqualFold(m[1], "x"), true
			l = l[len(m[0]):]
		}
		if !bullet {
			continue
		}
		if l = strings.TrimSpace(l); l != "" {
			items = append(items, AcceptanceItem{Text: l, Done: done})
		}
		if len(items) == maxAcceptance {
			break
		}
	}
	if len(items) > 0 {
		return Acceptance{Source: "prompt", Items: items}
	}
	if goal := firstLine(prompt); goal != "" {
		return Acceptance{Source: "goal", Items: []AcceptanceItem{{Text: goal}}}
	}
	return Acceptance{Source: "goal", Items: []AcceptanceItem{}}
}

// --- HTTP -----------------------------------------------------------------------------

var errBadRequest = errors.New("bad request")

func writeReviewError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, errBadRequest):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": strings.TrimPrefix(err.Error(), errBadRequest.Error()+": ")})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

// GET /api/reviews
func (s *Server) handleListReviews(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListReviews(r.Context())
	if err != nil {
		writeReviewError(w, err)
		return
	}
	if all == nil {
		all = []store.Review{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": all})
}

// POST /api/tasks/{id}/review {"decision": "approved"|"changes_requested", "note": "…"}
func (s *Server) handleReviewTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	res, err := s.hub.ReviewTask(r.Context(), s.service, r.PathValue("id"), body.Decision, body.Note)
	if err != nil {
		writeReviewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// PUT /api/tasks/{id}/expedite and /api/chats/{id}/expedite {"expedite": true|false}
func (s *Server) handleExpedite(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Expedite bool `json:"expedite"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		id := r.PathValue("id")
		var err error
		if kind == "task" {
			_, err = s.store.GetTask(r.Context(), id)
		} else {
			_, err = s.store.GetChatSession(r.Context(), id)
		}
		if err != nil {
			writeReviewError(w, err)
			return
		}
		taskID, chatID := id, ""
		if kind != "task" {
			taskID, chatID = "", id
		}
		rv, err := s.hub.SetExpedite(r.Context(), taskID, chatID, body.Expedite)
		if err != nil {
			writeReviewError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"review": rv})
	}
}

// GET /api/tasks/{id}/acceptance
func (s *Server) handleTaskAcceptance(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeReviewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.hub.TaskAcceptance(r.Context(), t))
}
