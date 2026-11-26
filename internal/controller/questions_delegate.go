package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

// "Let the commander decide" (PRD §5.8b). A delegated question goes to the
// commander chat, which must answer it with the answer_question tool: a choice and a
// rationale, recorded as a receipt. The decision is held for a short grace period in
// which the user can override it before the agent sees it; after that, an override
// reaches the agent as a correction note. Optional per-category rules delegate questions
// automatically (default off).

// Settings keys.
const (
	settingQuestionAutoDelegate  = "question_auto_delegate"    // comma-separated categories
	settingQuestionDelegateGrace = "question_delegation_grace" // seconds
)

// defaultDelegationGrace is how long the commander's decision waits for an override.
var defaultDelegationGrace = 30 * time.Second

// QuestionSettings are the question preferences (GET/PUT /api/questions/settings).
type QuestionSettings struct {
	// AutoDelegate lists question categories the commander decides without asking
	// (e.g. "permission" for the agents' routine tool approvals). Default none.
	AutoDelegate []string `json:"auto_delegate"`
	// DelegationGraceSeconds is how long a delegated decision waits before reaching the
	// agent, so the user can override it.
	DelegationGraceSeconds int `json:"delegation_grace_seconds"`
}

func (h *Hub) questionSettings(ctx context.Context) QuestionSettings {
	out := QuestionSettings{AutoDelegate: []string{}, DelegationGraceSeconds: int(defaultDelegationGrace / time.Second)}
	saved, _ := h.store.GetAllSettings(ctx)
	for _, c := range strings.Split(saved[settingQuestionAutoDelegate], ",") {
		if c = strings.ToLower(strings.TrimSpace(c)); c != "" {
			out.AutoDelegate = append(out.AutoDelegate, c)
		}
	}
	if n, err := strconv.Atoi(saved[settingQuestionDelegateGrace]); err == nil && n >= 0 {
		out.DelegationGraceSeconds = n
	}
	return out
}

func (h *Hub) saveQuestionSettings(ctx context.Context, s QuestionSettings) (QuestionSettings, error) {
	var cats []string
	for _, c := range s.AutoDelegate {
		if c = strings.ToLower(strings.TrimSpace(c)); c != "" && !slices.Contains(cats, c) {
			cats = append(cats, c)
		}
	}
	if s.DelegationGraceSeconds < 0 || s.DelegationGraceSeconds > 3600 {
		return s, errors.New("delegation_grace_seconds must be between 0 and 3600")
	}
	if err := h.store.SetSettings(ctx, map[string]string{
		settingQuestionAutoDelegate:  strings.Join(cats, ","),
		settingQuestionDelegateGrace: strconv.Itoa(s.DelegationGraceSeconds),
	}); err != nil {
		return s, err
	}
	return h.questionSettings(ctx), nil
}

// autoDelegates reports whether a new question matches an auto-delegate rule.
func (h *Hub) autoDelegates(ctx context.Context, q store.Question) bool {
	if q.Category == "" {
		return false
	}
	return slices.Contains(h.questionSettings(ctx).AutoDelegate, q.Category)
}

// delegateQuestion hands a question to the commander chat. It starts a commander
// turn now if it is idle; otherwise the question waits for its current turn to end.
func (h *Hub) delegateQuestion(ctx context.Context, id string, auto bool) (store.Question, error) {
	ctx = context.WithoutCancel(ctx)
	orch, ok := h.commanderChat(ctx)
	if !ok {
		return store.Question{}, errors.New("there is no commander chat to decide; open the commander first")
	}
	cause := causeFrom(ctx)
	h.questions.mu.Lock()
	q, err := h.store.GetQuestion(ctx, id)
	if err != nil {
		h.questions.mu.Unlock()
		return q, err
	}
	if q.Status != store.QuestionOpen {
		h.questions.mu.Unlock()
		return q, errQuestionClosed
	}
	if q.ChatID == orch.ID {
		h.questions.mu.Unlock()
		return q, errors.New("the commander asked this question itself; answer it yourself")
	}
	q.Status = store.QuestionDelegated
	q.Delegation = &store.QuestionDelegation{State: store.DelegationPending, RequestedAt: time.Now().UTC(), RequestedBy: cause, Auto: auto, ChatID: orch.ID}
	err = h.store.UpdateQuestion(ctx, q)
	h.questions.mu.Unlock()
	if err != nil {
		return q, err
	}
	h.broadcastQuestion(ctx, q)
	how := "delegated"
	if auto {
		how = "auto-delegated (category " + q.Category + ")"
	}
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindQuestion, Action: "delegated", Cause: cause, WorkerID: q.WorkerID, ChatID: q.ChatID,
		Summary:  fmt.Sprintf("%s %s question %s (%s) to the commander", capitalize(describeActor(cause)), how, q.SID, oneLine(q.Prompt, 120)),
		MetaJSON: auditMeta(map[string]any{"question_sid": q.SID, "auto": auto}),
	})
	if chatIdle(orch.Status) {
		h.sendNextDelegation(ctx, orch)
	}
	return h.questionByID(ctx, q.ID), nil
}

// commanderIdle runs when a commander chat's turn ended: a delegated question it
// was given but didn't decide goes back to the user; the next waiting one is handed over.
func (h *Hub) commanderIdle(ctx context.Context, orch store.ChatSession) {
	qs, err := h.store.ListQuestions(ctx, store.QuestionFilter{Statuses: []string{store.QuestionDelegated}, Limit: 200})
	if err != nil {
		return
	}
	for _, q := range qs {
		d := q.Delegation
		if d == nil || d.ChatID != orch.ID || d.State != store.DelegationPending || d.SentAt == nil {
			continue
		}
		h.delegationFailed(ctx, q.ID, "The commander ended its turn without deciding; the question is back in your queue.")
	}
	h.sendNextDelegation(ctx, orch)
}

// sendNextDelegation starts a commander turn for the oldest delegated question it
// hasn't been given yet.
func (h *Hub) sendNextDelegation(ctx context.Context, orch store.ChatSession) {
	qs, err := h.store.ListQuestions(ctx, store.QuestionFilter{Statuses: []string{store.QuestionDelegated}, Limit: 200})
	if err != nil {
		return
	}
	var next *store.Question
	for i := len(qs) - 1; i >= 0; i-- { // oldest first
		d := qs[i].Delegation
		if d != nil && d.ChatID == orch.ID && d.State == store.DelegationPending && d.SentAt == nil {
			next = &qs[i]
			break
		}
	}
	if next == nil {
		return
	}
	h.questions.mu.Lock()
	q, err := h.store.GetQuestion(ctx, next.ID)
	if err != nil || q.Delegation == nil || q.Delegation.SentAt != nil || q.Status != store.QuestionDelegated {
		h.questions.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	q.Delegation.SentAt = &now
	err = h.store.UpdateQuestion(ctx, q)
	h.questions.mu.Unlock()
	if err != nil {
		return
	}
	h.broadcastQuestion(ctx, q)
	dctx := withCause(ctx, store.Cause{Actor: store.ActorSystem, Source: "delegation", Confidence: store.ConfidenceCertain})
	if err := h.sendAgentChat(dctx, orch, chatSendRequest{SessionID: orch.ID, Content: h.delegationPrompt(ctx, q)}); err != nil {
		h.delegationFailed(ctx, q.ID, "Syre could not reach the commander: "+err.Error())
	}
}

// delegationPrompt asks the commander to decide a question.
func (h *Hub) delegationPrompt(ctx context.Context, q store.Question) string {
	var b strings.Builder
	b.WriteString(eventNotePrefix + " The user delegated a question to you (\"Let the commander decide\"). Decide it on their behalf: call the answer_question tool of the syre MCP server with ")
	fmt.Fprintf(&b, "question %q, the option you choose (its label or number) or a text answer, and a rationale: one or two sentences the user sees as the receipt of your decision. ", q.SID)
	b.WriteString("Decide from what you know and the context below; you may check the fleet first. The user can override you.\n\n")
	b.WriteString(questionText(q))
	if cs, err := h.store.GetChatSession(ctx, q.ChatID); err == nil {
		fmt.Fprintf(&b, "\nAsked by %s in chat %s (%q) on %s.", agentLabel(cs), cs.SID, cs.Title, h.workerLabel(ctx, cs.WorkerID))
	}
	return b.String()
}

// delegationFailed hands a delegated question back to the user.
func (h *Hub) delegationFailed(ctx context.Context, id, why string) {
	h.questions.mu.Lock()
	q, err := h.store.GetQuestion(ctx, id)
	if err != nil || q.Status != store.QuestionDelegated || q.Delegation == nil {
		h.questions.mu.Unlock()
		return
	}
	q.Status = store.QuestionOpen
	q.Delegation.State, q.Delegation.Note = store.DelegationFailed, why
	err = h.store.UpdateQuestion(ctx, q)
	h.questions.mu.Unlock()
	if err == nil {
		h.broadcastQuestion(ctx, q)
	}
}

// answerQuestionInput is the commander's answer_question MCP tool input.
type answerQuestionInput struct {
	Question  string `json:"question" jsonschema:"The question's sid (q-…) or id"`
	Option    string `json:"option,omitempty" jsonschema:"The option you choose: its exact label or its number (1-based)"`
	Text      string `json:"text,omitempty" jsonschema:"A free-form answer (when the question accepts one), or a note with the option"`
	Rationale string `json:"rationale" jsonschema:"Why you chose it, in one or two sentences (shown to the user as the receipt)"`
}

// decideDelegated records the commander's decision on a delegated question; it is
// delivered to the agent after the grace period unless the user overrides it.
func (h *Hub) decideDelegated(ctx context.Context, orchChatID string, in answerQuestionInput) (string, error) {
	ctx = context.WithoutCancel(ctx)
	id := resolveRef(ctx, h.store, ids.Question, strings.TrimSpace(in.Question))
	if strings.TrimSpace(in.Rationale) == "" {
		return "", errors.New("rationale is required: say in one or two sentences why you chose this (the user sees it as the receipt)")
	}
	h.questions.mu.Lock()
	q, err := h.store.GetQuestion(ctx, id)
	if err != nil {
		h.questions.mu.Unlock()
		return "", fmt.Errorf("unknown question %q", in.Question)
	}
	if q.Status != store.QuestionDelegated || q.Delegation == nil || q.Delegation.ChatID != orchChatID || q.Delegation.State != store.DelegationPending {
		h.questions.mu.Unlock()
		return "", fmt.Errorf("question %s is not waiting for your decision (status %s)", q.SID, q.Status)
	}
	d := q.Delegation
	opt := strings.TrimSpace(in.Option)
	if opt != "" {
		found := false
		for i, o := range q.Options {
			if strings.EqualFold(o.Label, opt) || opt == strconv.Itoa(i+1) || strings.EqualFold(opt, o.ID) {
				d.OptionID, d.Label, found = o.ID, o.Label, true
				break
			}
		}
		if !found {
			h.questions.mu.Unlock()
			return "", fmt.Errorf("option %q is not one of: %s", opt, strings.Join(optionLabels(q.Options), " | "))
		}
	}
	d.Text = strings.TrimSpace(in.Text)
	if d.OptionID == "" && (d.Text == "" || (!q.AllowText && len(q.Options) > 0)) {
		h.questions.mu.Unlock()
		return "", fmt.Errorf("choose one of the options: %s", strings.Join(optionLabels(q.Options), " | "))
	}
	now := time.Now().UTC()
	grace := time.Duration(h.questionSettings(ctx).DelegationGraceSeconds) * time.Second
	deliverAt := now.Add(grace)
	d.State, d.Rationale, d.DecidedAt, d.DeliverAt = store.DelegationDecided, strings.TrimSpace(in.Rationale), &now, &deliverAt
	err = h.store.UpdateQuestion(ctx, q)
	h.questions.mu.Unlock()
	if err != nil {
		return "", err
	}
	h.broadcastQuestion(ctx, q)
	cause := causeFrom(ctx)
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindQuestion, Action: "decided", Cause: cause, WorkerID: q.WorkerID, ChatID: q.ChatID,
		Summary:  fmt.Sprintf("The commander decided question %s (%s): %q because %s", q.SID, oneLine(q.Prompt, 100), firstNonEmpty(d.Label, d.Text), oneLine(d.Rationale, 200)),
		MetaJSON: auditMeta(map[string]any{"question_sid": q.SID, "option_id": d.OptionID}),
	})
	h.scheduleDelegationDelivery(q.ID, grace)
	return fmt.Sprintf("Recorded: %q for %s. The user sees your rationale and can override it; it reaches the asking agent in %s otherwise.", firstNonEmpty(d.Label, d.Text), q.SID, grace.Round(time.Second)), nil
}

// scheduleDelegationDelivery delivers a decided delegation after d.
func (h *Hub) scheduleDelegationDelivery(id string, d time.Duration) {
	h.questions.mu.Lock()
	defer h.questions.mu.Unlock()
	if h.questions.closed {
		return
	}
	h.questions.init()
	if t := h.questions.timers[id]; t != nil {
		t.Stop()
	}
	h.questions.timers[id] = time.AfterFunc(d, func() {
		h.goBackground(func(ctx context.Context) { h.deliverDelegation(ctx, id) })
	})
}

// stopDelegationTimer cancels a pending delegation delivery. Caller holds h.questions.mu.
func (h *Hub) stopDelegationTimer(id string) {
	if t := h.questions.timers[id]; t != nil {
		t.Stop()
		delete(h.questions.timers, id)
	}
}

// stopAllDelegationTimers stops every pending delegation timer and prevents new ones
// from being armed once the hub begins shutting down.
func (h *Hub) stopAllDelegationTimers() {
	h.questions.mu.Lock()
	defer h.questions.mu.Unlock()
	h.questions.closed = true
	for id, t := range h.questions.timers {
		if t != nil {
			t.Stop()
		}
		delete(h.questions.timers, id)
	}
}

// deliverDelegation turns a decided delegation into the answer, unless overridden.
func (h *Hub) deliverDelegation(ctx context.Context, id string) {
	h.questions.mu.Lock()
	delete(h.questions.timers, id)
	q, err := h.store.GetQuestion(ctx, id)
	h.questions.mu.Unlock()
	if err != nil || q.Status != store.QuestionDelegated || q.Delegation == nil || q.Delegation.State != store.DelegationDecided {
		return
	}
	d := q.Delegation
	actx := withCause(ctx, store.Cause{Actor: store.ActorCommander, Source: "delegation", ActorID: d.ChatID, Confidence: store.ConfidenceCertain})
	if _, err := h.answerQuestion(actx, id, questionAnswerInput{OptionID: d.OptionID, Text: d.Text}); err != nil {
		h.logger.Warn("delivering a delegated answer failed", "question", q.SID, "err", err)
	}
}

// resumeDelegations re-arms decided delegations after a hub restart.
func (h *Hub) resumeDelegations(ctx context.Context) {
	qs, err := h.store.ListQuestions(ctx, store.QuestionFilter{Statuses: []string{store.QuestionDelegated}, Limit: 200})
	if err != nil {
		return
	}
	for _, q := range qs {
		if q.Delegation != nil && q.Delegation.State == store.DelegationDecided && q.Delegation.DeliverAt != nil {
			h.scheduleDelegationDelivery(q.ID, max(0, time.Until(*q.Delegation.DeliverAt)))
		}
	}
}
