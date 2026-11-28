package controller

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// What reaches devices as Web Push (SYR-Z15J6): a session waiting for you, failed or
// completed (never scratch sessions or shells, like the page alerts), and questions: a P1
// one (its session works a P1 ticket) at once, any other once it has blocked longer
// than push_question_after_minutes (default 15). One notification per session or
// question: repeats replace it.

// settingPushQuestionAfter is the minutes a question may block before it is pushed.
const settingPushQuestionAfter = "push_question_after_minutes"

const defaultPushQuestionAfter = 15 * time.Minute

// pushTaskTransition pushes a task's move to waiting, failed or completed.
func (h *Hub) pushTaskTransition(t store.Task, from string) {
	if t.Origin == protocol.TaskOriginScratch || t.Agent == "shell" || t.SessionKind == "terminal" || from == t.Status {
		return
	}
	name := cmpOr(t.Title, t.SID)
	m := pushMessage{URL: "/command/" + t.SID, Tag: "session:" + t.ID}
	switch t.Status {
	case protocol.TaskStatusWaitingInput:
		m.Category, m.Title, m.Body, m.Urgency = PushWaiting, name+" waits for you", cmpOr(t.Narration, t.Agent+" needs your input"), webpush.UrgencyHigh
	case protocol.TaskStatusFailed:
		m.Category, m.Title, m.Body = PushFailed, name+" failed", cmpOr(pushFirstLine(t.Summary), t.Agent+" stopped with an error")
	case protocol.TaskStatusCompleted:
		m.Category, m.Title, m.Body, m.Urgency = PushCompleted, name+" is done", cmpOr(pushFirstLine(t.Summary), t.Agent+" finished"), webpush.UrgencyLow
	default:
		return
	}
	h.notifyDevices(m)
}

// pushChatTransition pushes an agent chat that needs you or failed.
func (h *Hub) pushChatTransition(cs store.ChatSession, from string) {
	if cs.Status == from || cs.QuestionID != "" {
		return
	}
	name := cmpOr(cs.Title, cs.SID)
	m := pushMessage{URL: "/command/" + cs.SID, Tag: "session:" + cs.ID}
	switch cs.Status {
	case protocol.ChatStatusNeedsYou:
		m.Category, m.Title, m.Body, m.Urgency = PushWaiting, name+" waits for you", cs.Agent+" needs your answer", webpush.UrgencyHigh
	case protocol.ChatStatusFailed:
		m.Category, m.Title, m.Body = PushFailed, name+" failed", cs.Agent+" stopped with an error"
	default:
		return
	}
	h.notifyDevices(m)
}

func pushFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// pushQuestions tracks the questions already pushed.
type pushQuestions struct {
	mu   sync.Mutex
	sent map[string]time.Time
}

// pushQuestionsLoop escalates pending questions every 30 seconds.
func (h *Hub) pushQuestionsLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.pushQuestionSweep(ctx, time.Now())
		}
	}
}

// pushQuestionSweep pushes the pending questions due: P1 ones at once, others once they
// have blocked longer than the threshold. Each question is pushed once.
func (h *Hub) pushQuestionSweep(ctx context.Context, now time.Time) int {
	if h.store == nil {
		return 0
	}
	after := defaultPushQuestionAfter
	if kv, err := h.store.GetAllSettings(ctx); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(kv[settingPushQuestionAfter])); err == nil && n > 0 {
			after = time.Duration(n) * time.Minute
		}
	}
	qs, err := h.store.ListQuestions(ctx, store.QuestionFilter{Statuses: []string{store.QuestionOpen}, Limit: 200})
	if err != nil {
		return 0
	}
	h.pushedQuestions.mu.Lock()
	if h.pushedQuestions.sent == nil {
		h.pushedQuestions.sent = map[string]time.Time{}
	}
	h.pushedQuestions.mu.Unlock()
	pushed := 0
	for _, q := range qs {
		h.pushedQuestions.mu.Lock()
		_, done := h.pushedQuestions.sent[q.ID]
		h.pushedQuestions.mu.Unlock()
		if done || q.SnoozedUntil != nil && q.SnoozedUntil.After(now) {
			continue
		}
		p1 := h.questionIsP1(ctx, q)
		if !p1 && now.Sub(q.BlockedSince) < after {
			continue
		}
		title := "A question waits for you"
		if p1 {
			title = "P1: a question waits for you"
		}
		h.notifyDevices(pushMessage{Category: PushQuestion, Title: title, Body: pushFirstLine(q.Prompt), URL: "/inbox/" + q.SID,
			Tag: "question:" + q.ID, Urgency: webpush.UrgencyHigh})
		h.pushedQuestions.mu.Lock()
		h.pushedQuestions.sent[q.ID] = now
		h.pushedQuestions.mu.Unlock()
		pushed++
	}
	return pushed
}

// questionIsP1 reports a question whose session works a P1 ticket.
func (h *Hub) questionIsP1(ctx context.Context, q store.Question) bool {
	ticketID := ""
	if q.ChatID != "" {
		if cs, err := h.store.GetChatSession(ctx, q.ChatID); err == nil {
			ticketID = cs.TicketID
		}
	}
	if ticketID == "" && q.TaskID != "" {
		if t, err := h.store.GetTask(ctx, q.TaskID); err == nil {
			ticketID = t.TicketID
		}
	}
	if ticketID == "" {
		return false
	}
	tk, err := h.store.GetTicket(ctx, ticketID)
	return err == nil && strings.EqualFold(tk.Priority, "P1")
}
