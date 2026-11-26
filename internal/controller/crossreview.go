package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Cross-agent review (docs/design/autopilot.md "Verification gate and self-repair"):
// with autonomy.cross_review on, a ticket whose verify passed isn't moved to review yet.
// A reviewer chat on a different agent (or model) gets the acceptance items and the diff,
// read-only, and answers APPROVE or CHANGES. Approval completes the run; changes go back
// to the author session like a verify failure, up to retry_limit rounds.

const crossReviewDiffBudget = 48 * 1024 // bytes of diff in the review prompt

type crossReview struct {
	ticketID   string
	authorChat string
	taskID     string
	reviewer   string // reviewer chat ID
	agent      string
	rounds     int
	dig        *protocol.RunDigest
	summary    string
}

type crossReviews struct {
	mu         sync.Mutex
	byReviewer map[string]*crossReview
	byTicket   map[string]*crossReview
}

func (c *crossReviews) forReviewer(chatID string) (*crossReview, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byReviewer[chatID]
	return r, ok
}

// ticketCrossReview resolves autonomy.cross_review: ticket, then project, then off.
func (h *Hub) ticketCrossReview(ctx context.Context, tk store.Ticket) bool {
	if tk.Autonomy.CrossReview != nil {
		return *tk.Autonomy.CrossReview
	}
	if p, err := h.store.GetProject(ctx, tk.ProjectID); err == nil && p.Autonomy.CrossReview != nil {
		return *p.Autonomy.CrossReview
	}
	return false
}

// maybeStartCrossReview starts a review round for tk when cross-review applies and
// returns true; false means the run should complete normally. The round itself runs in
// the background: this is called on the worker connection's read loop, and fetching the
// diff is a request to that same worker.
func (h *Hub) maybeStartCrossReview(ctx context.Context, tk store.Ticket, author store.ChatSession, task *store.Task, dig *protocol.RunDigest, summary string) bool {
	if task == nil || !h.ticketCrossReview(ctx, tk) {
		return false
	}
	t := *task
	started := h.goBackground(func(bctx context.Context) {
		if !h.startCrossReview(bctx, tk, author, &t, dig, summary) {
			h.completeTicketRun(bctx, tk, author, &t, dig, summary)
		}
	})
	return started
}

// startCrossReview opens the reviewer chat; false means it couldn't (complete instead).
func (h *Hub) startCrossReview(ctx context.Context, tk store.Ticket, author store.ChatSession, task *store.Task, dig *protocol.RunDigest, summary string) bool {
	h.cross.mu.Lock()
	if h.cross.byTicket == nil {
		h.cross.byTicket, h.cross.byReviewer = map[string]*crossReview{}, map[string]*crossReview{}
	}
	st := h.cross.byTicket[tk.ID]
	if st == nil {
		st = &crossReview{ticketID: tk.ID}
		h.cross.byTicket[tk.ID] = st
	}
	st.authorChat, st.taskID, st.dig, st.summary = author.ID, task.ID, dig, summary
	h.cross.mu.Unlock()

	agent, model := h.pickReviewer(ctx, author)
	prompt := h.crossReviewPrompt(ctx, tk, *task)
	now := time.Now().UTC()
	cs := store.ChatSession{
		ID: ids.UUID(), Title: "Review " + tk.SID + ": " + tk.Title, Backend: store.ChatBackendAgent,
		WorkerID: author.WorkerID, Agent: agent, Model: model, Cwd: author.Cwd, FleetTools: false,
		Status: protocol.ChatStatusStarting, CreatedAt: now, UpdatedAt: now,
	}
	if sid, err := h.store.NewSID(ctx, ids.Chat); err == nil {
		cs.SID = sid
	}
	h.resolveChatTarget(ctx, &cs)
	cs.ApprovalMode = h.readOnlyApprovalMode(ctx, cs)
	h.initNotesCursor(ctx, &cs)
	if err := h.store.CreateChatSession(ctx, cs); err != nil {
		h.logger.Warn("cross-review: create reviewer chat failed; completing without review", "ticket_id", tk.ID, "err", err)
		return false
	}
	h.cross.mu.Lock()
	st.reviewer, st.agent = cs.ID, cs.Agent
	h.cross.byReviewer[cs.ID] = st
	h.cross.mu.Unlock()
	h.broadcastFleetUpdate("chat_session", cs)
	if err := h.sendAgentChat(ctx, cs, chatSendRequest{SessionID: cs.ID, Content: prompt}); err != nil {
		h.logger.Warn("cross-review: reviewer dispatch failed; completing without review", "ticket_id", tk.ID, "err", err)
		h.cross.mu.Lock()
		delete(h.cross.byReviewer, cs.ID)
		delete(h.cross.byTicket, tk.ID)
		h.cross.mu.Unlock()
		h.closeChat(ctx, cs.ID)
		return false
	}
	return true
}

// pickReviewer prefers a different agent on the author's machine; otherwise the same
// agent with a different model when one is known.
func (h *Hub) pickReviewer(ctx context.Context, author store.ChatSession) (agent, model string) {
	if w, err := h.store.GetWorker(ctx, author.WorkerID); err == nil {
		for _, a := range w.Agents {
			if a == author.Agent || a == "shell" {
				continue
			}
			if acct, ok := h.lookupAccount(ctx, w.ID, a, ""); ok && acct.ChatSupported {
				return a, ""
			}
		}
	}
	if acct, ok := h.lookupAccount(ctx, author.WorkerID, author.Agent, author.AccountID); ok {
		for _, m := range acct.Models {
			if m.ID != author.Model {
				return author.Agent, m.ID
			}
		}
	}
	return author.Agent, author.Model
}

// readOnlyApprovalMode picks the reviewer's lowest-risk mode (plan/read-only).
func (h *Hub) readOnlyApprovalMode(ctx context.Context, cs store.ChatSession) string {
	acct, ok := h.lookupAccount(ctx, cs.WorkerID, cs.Agent, cs.AccountID)
	if !ok || len(acct.Approvals) == 0 {
		return cs.ApprovalMode
	}
	best, bestRisk := "", 1<<30
	for _, m := range acct.Approvals {
		if m.Disabled == "" && m.Risk < bestRisk {
			best, bestRisk = m.ID, m.Risk
		}
	}
	return best
}

// crossReviewPrompt gives the reviewer the acceptance items and each repo's diff against
// its base, fetched from the worker (capped), so it needs no tools.
func (h *Hub) crossReviewPrompt(ctx context.Context, tk store.Ticket, task store.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are reviewing someone else's change for ticket %s: %s.\n", tk.SID, tk.Title)
	if len(tk.Acceptance) > 0 {
		b.WriteString("Acceptance criteria:\n")
		for _, a := range tk.Acceptance {
			b.WriteString("- " + a.Text + "\n")
		}
	}
	budget := crossReviewDiffBudget
	for _, wt := range task.Worktrees {
		if budget <= 0 {
			b.WriteString("\n(diff truncated)\n")
			break
		}
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		env, err := h.RequestWorker(rctx, task.WorkerID, protocol.MsgArtifactFetch, protocol.ArtifactFetchRequest{
			TaskID: task.ID, Kind: protocol.InspectKindDiff, Workdir: wt.WorktreePath, Base: wt.BaseSHA, MaxLines: 2000,
		})
		cancel()
		text := ""
		if err == nil {
			if res, err := protocol.DecodePayload[protocol.ArtifactFetchResult](env); err == nil {
				text = res.Text
			}
		}
		if len(text) > budget {
			text = text[:budget] + "\n(diff truncated)"
		}
		budget -= len(text)
		fmt.Fprintf(&b, "\nDiff of %s:\n```diff\n%s\n```\n", wt.RepoName, strings.TrimRight(text, "\n"))
	}
	b.WriteString("\nCheck the change against every acceptance criterion and for correctness. Don't edit files.\n" +
		"Reply with the first line exactly APPROVE if it is ready, or CHANGES: followed by a short list of what must change.")
	return b.String()
}

// onCrossReviewEvent handles the reviewer chat's turn end or failure.
func (h *Hub) onCrossReviewEvent(ctx context.Context, cs store.ChatSession, ev protocol.AgentChatEvent) {
	st, ok := h.cross.forReviewer(cs.ID)
	if !ok {
		return
	}
	switch ev.Kind {
	case protocol.EventTurnEnd:
		verdict := h.lastAssistantText(ctx, cs.ID)
		approve, notes := parseReviewVerdict(verdict)
		h.endCrossReview(ctx, st, approve || notes == "", notes, verdict)
	case protocol.EventError:
		h.logger.Warn("cross-review: reviewer failed; completing without review", "ticket_id", st.ticketID)
		h.endCrossReview(ctx, st, true, "", "")
	}
}

// parseReviewVerdict reads APPROVE or CHANGES: from the reviewer's reply. Anything else
// is treated as changes requested with the whole reply as notes.
func parseReviewVerdict(text string) (approve bool, notes string) {
	t := strings.TrimSpace(text)
	first, rest, _ := strings.Cut(t, "\n")
	first = strings.TrimSpace(strings.Trim(first, "*`_ "))
	switch {
	case strings.HasPrefix(strings.ToUpper(first), "APPROVE"):
		return true, ""
	case strings.HasPrefix(strings.ToUpper(first), "CHANGES"):
		n := strings.TrimSpace(strings.TrimLeft(first[len("CHANGES"):], ": "))
		if r := strings.TrimSpace(rest); r != "" {
			n = strings.TrimSpace(n + "\n" + r)
		}
		return false, n
	}
	return false, t
}

func (h *Hub) lastAssistantText(ctx context.Context, chatID string) string {
	evs, err := h.store.ListChatEvents(ctx, chatID, 0, 5000)
	if err != nil {
		return ""
	}
	var deltas strings.Builder
	last := ""
	for _, e := range evs {
		if e.Kind == eventUserMessage {
			deltas.Reset()
			last = ""
			continue
		}
		var ev protocol.AgentChatEvent
		if err := json.Unmarshal([]byte(e.JSON), &ev, protocol.JSONOptions); err != nil {
			continue
		}
		switch ev.Kind {
		case eventUserMessage:
			deltas.Reset()
			last = ""
		case protocol.EventMessage:
			last = ev.Text
			deltas.Reset()
		case protocol.EventMessageDelta:
			deltas.WriteString(ev.Text)
		}
	}
	if last == "" {
		last = deltas.String()
	}
	return last
}

// endCrossReview closes the reviewer and either completes the run (approved) or sends
// the notes back to the author session, escalating to needs_you past retry_limit.
func (h *Hub) endCrossReview(ctx context.Context, st *crossReview, approve bool, notesAndVerdict ...string) {
	h.cross.mu.Lock()
	delete(h.cross.byReviewer, st.reviewer)
	reviewer, agent := st.reviewer, st.agent
	h.cross.mu.Unlock()
	if reviewer != "" {
		h.closeChat(ctx, reviewer)
	}
	tk, err := h.store.GetTicket(ctx, st.ticketID)
	if err != nil {
		return
	}
	notes := ""
	if len(notesAndVerdict) > 0 {
		notes = notesAndVerdict[0]
	}
	verdictWord := "approved"
	if !approve {
		verdictWord = "requested changes"
	}
	if agent != "" {
		body := "Cross-review (" + agent + ") " + verdictWord + "."
		if notes != "" {
			body += "\n" + notes
		}
		_, _ = h.store.CreateTicketComment(ctx, store.TicketComment{TicketID: tk.ID, Author: "review:" + agent, Body: clipRunes(body, 500)})
	}
	author, aErr := h.store.GetChatSession(ctx, st.authorChat)
	var task *store.Task
	if t, err := h.store.GetTask(ctx, st.taskID); err == nil {
		task = &t
	}
	if approve || aErr != nil {
		h.cross.mu.Lock()
		delete(h.cross.byTicket, st.ticketID)
		h.cross.mu.Unlock()
		h.completeTicketRun(ctx, tk, author, task, st.dig, st.summary)
		return
	}
	retryLimit, _ := h.ticketAutonomy(ctx, tk)
	h.cross.mu.Lock()
	st.rounds++
	rounds := st.rounds
	h.cross.mu.Unlock()
	if rounds >= retryLimit {
		h.cross.mu.Lock()
		delete(h.cross.byTicket, st.ticketID)
		h.cross.mu.Unlock()
		if updated, changed, err := h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusNeedsYou); err == nil && changed {
			h.broadcastFleetUpdate("ticket", updated)
			h.pulseTicketTransition(ctx, updated, task, st.dig, "")
		}
		return
	}
	prompt := fmt.Sprintf("A reviewer (%s) requested changes (round %d/%d). Address them, then end your turn.\n%s", agent, rounds, retryLimit, notes)
	if err := h.SendTicketTurn(ctx, tk.ID, prompt); err != nil {
		h.logger.Warn("cross-review: sending changes to the author failed; completing", "ticket_id", tk.ID, "err", err)
		h.completeTicketRun(ctx, tk, author, task, st.dig, st.summary)
	}
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
