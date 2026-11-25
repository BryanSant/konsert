package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

// REST API of the question queue (PRD §5.8b):
//
//	GET  /api/questions?status=open|all&chat_id=   the fleet-wide queue (sorted)
//	GET  /api/questions/{id}                       one question with its side conversation
//	POST /api/questions/{id}/answer   {option_id?, text?}
//	POST /api/questions/{id}/ask      {text}        "Ask about this" (side conversation)
//	POST /api/questions/{id}/delegate               "Let the commander decide"
//	POST /api/questions/{id}/snooze   {minutes}     (0 = unsnooze)
//	GET  /api/questions/metrics?days=7
//	GET/PUT /api/questions/settings
//
// Live updates reach browsers as fleet.update frames of kind "question".

func (s *Server) registerQuestionRoutes() {
	s.mux.Handle("GET /api/questions", s.requireBrowserAuth(s.handleListQuestions))
	s.mux.Handle("GET /api/questions/metrics", s.requireBrowserAuth(s.handleQuestionMetrics))
	s.mux.Handle("GET /api/questions/settings", s.requireBrowserAuth(s.handleGetQuestionSettings))
	s.mux.Handle("PUT /api/questions/settings", s.requireBrowserAuth(s.handlePutQuestionSettings))
	s.mux.Handle("GET /api/questions/{id}", s.requireBrowserAuth(s.byRef(ids.Question, s.handleGetQuestion)))
	s.mux.Handle("POST /api/questions/{id}/answer", s.requireBrowserAuth(s.byRef(ids.Question, s.handleAnswerQuestion)))
	s.mux.Handle("POST /api/questions/{id}/ask", s.requireBrowserAuth(s.byRef(ids.Question, s.handleAskAboutQuestion)))
	s.mux.Handle("POST /api/questions/{id}/delegate", s.requireBrowserAuth(s.byRef(ids.Question, s.handleDelegateQuestion)))
	s.mux.Handle("POST /api/questions/{id}/snooze", s.requireBrowserAuth(s.byRef(ids.Question, s.handleSnoozeQuestion)))
}

// questionQueue returns the queue: pending questions (and, with all, recent closed
// ones), sorted by blocking impact then age.
func (h *Hub) questionQueue(ctx context.Context, all bool, chatID string) ([]QuestionView, error) {
	f := store.QuestionFilter{ChatID: chatID, Limit: 500}
	if !all {
		f.Statuses = []string{store.QuestionOpen, store.QuestionDelegated}
	} else {
		f.Since = time.Now().Add(-7 * 24 * time.Hour)
	}
	qs, err := h.store.ListQuestions(ctx, f)
	if err != nil {
		return nil, err
	}
	v := h.newViewContext()
	views := make([]QuestionView, 0, len(qs))
	for _, q := range qs {
		views = append(views, h.questionView(ctx, v, q, true))
	}
	sortQueue(views, time.Now())
	return views, nil
}

func (s *Server) handleListQuestions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	all := q.Get("status") == "all"
	chatID := resolveRef(r.Context(), s.store, ids.Chat, strings.TrimSpace(q.Get("chat_id")))
	views, err := s.hub.questionQueue(r.Context(), all, chatID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"questions": views})
}

func (s *Server) handleGetQuestion(w http.ResponseWriter, r *http.Request) {
	q, err := s.store.GetQuestion(r.Context(), r.PathValue("id"))
	if err != nil {
		writeQuestionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.hub.questionView(r.Context(), s.hub.newViewContext(), q, true))
}

func writeQuestionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "question not found"})
	case errors.Is(err, errQuestionClosed), errors.Is(err, errThreadBusy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}

func (s *Server) questionResponse(w http.ResponseWriter, r *http.Request, q store.Question, err error) {
	if err != nil {
		writeQuestionError(w, err)
		return
	}
	cur, gerr := s.store.GetQuestion(r.Context(), q.ID)
	if gerr != nil {
		cur = q
	}
	writeJSON(w, http.StatusOK, s.hub.questionView(r.Context(), s.hub.newViewContext(), cur, true))
}

func (s *Server) handleAnswerQuestion(w http.ResponseWriter, r *http.Request) {
	var body questionAnswerInput
	if !decodeJSON(w, r, &body) {
		return
	}
	q, err := s.hub.answerQuestion(r.Context(), r.PathValue("id"), body)
	if err == nil {
		s.hub.recordAudit(r.Context(), store.AuditEvent{Action: "question.answer", WorkerID: q.WorkerID,
			Message:  fmt.Sprintf("Answered question %s", q.SID),
			MetaJSON: auditMeta(map[string]any{"question_id": q.ID, "option_id": body.OptionID})})
	}
	s.questionResponse(w, r, q, err)
}

func (s *Server) handleAskAboutQuestion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	q, err := s.hub.askAboutQuestion(r.Context(), r.PathValue("id"), body.Text)
	s.questionResponse(w, r, q, err)
}

func (s *Server) handleDelegateQuestion(w http.ResponseWriter, r *http.Request) {
	q, err := s.hub.delegateQuestion(r.Context(), r.PathValue("id"), false)
	if err == nil {
		s.hub.recordAudit(r.Context(), store.AuditEvent{Action: "question.delegate", WorkerID: q.WorkerID,
			Message: fmt.Sprintf("Delegated question %s to the commander", q.SID), MetaJSON: auditMeta(map[string]any{"question_id": q.ID})})
	}
	s.questionResponse(w, r, q, err)
}

func (s *Server) handleSnoozeQuestion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Minutes int `json:"minutes"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Minutes < 0 || body.Minutes > 7*24*60 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "minutes must be between 0 and 10080"})
		return
	}
	q, err := s.hub.snoozeQuestion(r.Context(), r.PathValue("id"), time.Duration(body.Minutes)*time.Minute)
	s.questionResponse(w, r, q, err)
}

func (s *Server) handleGetQuestionSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.questionSettings(r.Context()))
}

func (s *Server) handlePutQuestionSettings(w http.ResponseWriter, r *http.Request) {
	var body QuestionSettings
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := s.hub.saveQuestionSettings(r.Context(), body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// --- metrics ----------------------------------------------------------------------------

// QuestionMetrics are question-quality figures over a window (settings → insights).
type QuestionMetrics struct {
	Days                     int     `json:"days"`
	Asked                    int     `json:"asked"`
	Answered                 int     `json:"answered"`
	AnsweredWithClarify      int     `json:"answered_with_clarification"`
	AnsweredWithoutClarify   int     `json:"answered_without_clarification"`
	Delegated                int     `json:"delegated"`
	DelegationOverrides      int     `json:"delegation_overrides"`
	Rejected                 int     `json:"rejected"` // under-specified ask_user calls sent back
	Open                     int     `json:"open"`
	MedianBlockedSeconds     float64 `json:"median_blocked_seconds"`
	P90BlockedSeconds        float64 `json:"p90_blocked_seconds"`
	TotalBlockedSeconds      float64 `json:"total_blocked_seconds"`
	OldestOpenBlockedSeconds float64 `json:"oldest_open_blocked_seconds"`
	// Agents: per agent, how often its questions needed clarification or were rejected.
	Agents []AgentQuestionMetrics `json:"agents"`
}

// AgentQuestionMetrics is one agent's question quality.
type AgentQuestionMetrics struct {
	Agent      string `json:"agent"`
	Asked      int    `json:"asked"`
	Clarified  int    `json:"clarified"` // the user opened a side conversation first
	Rejected   int    `json:"rejected"`  // under-specified attempts
	Delegated  int    `json:"delegated"`
	Unclear    int    `json:"unclear"`      // questions that needed clarification or were rejected first
	UnclearPct int    `json:"unclear_rate"` // percent of asked
}

func (h *Hub) questionMetrics(ctx context.Context, days int) (QuestionMetrics, error) {
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	out := QuestionMetrics{Days: days, Agents: []AgentQuestionMetrics{}}
	qs, err := h.store.ListQuestions(ctx, store.QuestionFilter{Since: since, Limit: 5000})
	if err != nil {
		return out, err
	}
	rejects, err := h.store.ListQuestionRejects(ctx, since)
	if err != nil {
		return out, err
	}
	per := map[string]*AgentQuestionMetrics{}
	agent := func(name string) *AgentQuestionMetrics {
		if name == "" {
			name = "unknown"
		}
		if per[name] == nil {
			per[name] = &AgentQuestionMetrics{Agent: name}
		}
		return per[name]
	}
	var blocked []float64
	now := time.Now()
	for _, q := range qs {
		a := agent(q.Agent)
		out.Asked++
		a.Asked++
		clarified := false
		if q.Thread != nil {
			clarified = h.threadHasUserMessages(ctx, q.ID)
		}
		if clarified {
			a.Clarified++
		}
		if clarified || q.Rejections > 0 {
			a.Unclear++
		}
		if q.Delegation != nil {
			out.Delegated++
			a.Delegated++
			if q.Delegation.State == store.DelegationOverridden {
				out.DelegationOverrides++
			}
		}
		switch {
		case q.Status == store.QuestionAnswered && q.Answer != nil:
			out.Answered++
			if clarified {
				out.AnsweredWithClarify++
			} else {
				out.AnsweredWithoutClarify++
			}
			d := q.Answer.At.Sub(q.BlockedSince).Seconds()
			if d < 0 {
				d = 0
			}
			blocked = append(blocked, d)
			out.TotalBlockedSeconds += d
		case q.Pending():
			out.Open++
			if d := now.Sub(q.BlockedSince).Seconds(); d > out.OldestOpenBlockedSeconds {
				out.OldestOpenBlockedSeconds = d
			}
		}
	}
	for _, r := range rejects {
		out.Rejected++
		agent(r.Agent).Rejected++
	}
	if len(blocked) > 0 {
		sort.Float64s(blocked)
		out.MedianBlockedSeconds = blocked[len(blocked)/2]
		out.P90BlockedSeconds = blocked[min(len(blocked)-1, len(blocked)*9/10)]
	}
	for _, a := range per {
		if a.Asked > 0 {
			a.UnclearPct = a.Unclear * 100 / a.Asked
		}
		out.Agents = append(out.Agents, *a)
	}
	sort.Slice(out.Agents, func(i, j int) bool {
		if out.Agents[i].UnclearPct != out.Agents[j].UnclearPct {
			return out.Agents[i].UnclearPct > out.Agents[j].UnclearPct
		}
		return out.Agents[i].Agent < out.Agents[j].Agent
	})
	return out, nil
}

func (s *Server) handleQuestionMetrics(w http.ResponseWriter, r *http.Request) {
	days := 7
	if n, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && n > 0 && n <= 365 {
		days = n
	}
	m, err := s.hub.questionMetrics(r.Context(), days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, m)
}
