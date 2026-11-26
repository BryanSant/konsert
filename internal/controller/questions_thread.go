package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Side conversations ("Ask about this", PRD §5.8b). The user asks clarifying questions
// before answering. The answerer is a read-only fork of the asking agent's session on the
// same machine and account (same conversation context), or — when the agent can't fork —
// the commander's agent with a context pack. Either way it runs as a hidden agent
// chat through the normal chat machinery. The asking session stays blocked and
// undisturbed. The answerer can propose an answer ("DRAFT ANSWER: …"), and when the user
// answers it writes the summary delivered with the answer. The hidden chat is discarded
// once the question is answered or cancelled.

// sideConversationSystemPrompt is the answerer's standing instruction.
const sideConversationSystemPrompt = `You are answering a side conversation in Syre. The user is deciding how to answer a question an agent asked and wants to understand it first.
Explain plainly and concretely: what things are (name tickets, PRs and files with their titles), why the decision matters now, what each option leads to, and why the recommendation was made.
You are read-only: you may read files and run read-only commands to answer, but never change anything and never act on the decision yourself.
Keep answers short (a few sentences or bullets). When you can propose an answer, end with one line exactly like:
DRAFT ANSWER: <the exact option label, or a short free-text answer>`

// summaryWait bounds how long delivery waits for the answerer's summary.
var summaryWait = 90 * time.Second

// errThreadBusy is returned when the answerer is still replying.
var errThreadBusy = errors.New("the side conversation is still answering; wait for its reply")

// threadHasUserMessages reports whether the user asked anything in the side conversation.
func (h *Hub) threadHasUserMessages(ctx context.Context, questionID string) bool {
	msgs, err := h.store.ListQuestionMessages(ctx, questionID)
	if err != nil {
		return false
	}
	for _, m := range msgs {
		if m.Role == "user" {
			return true
		}
	}
	return false
}

// askAboutQuestion sends the user's message to the question's side conversation,
// starting it (fork or commander) on the first message.
func (h *Hub) askAboutQuestion(ctx context.Context, id, text string) (store.Question, error) {
	ctx = context.WithoutCancel(ctx)
	text = strings.TrimSpace(text)
	if text == "" {
		return store.Question{}, errors.New("type what you want to know")
	}
	h.questions.mu.Lock()
	q, err := h.store.GetQuestion(ctx, id)
	if err != nil {
		h.questions.mu.Unlock()
		return q, err
	}
	if !q.Pending() {
		h.questions.mu.Unlock()
		return q, errQuestionClosed
	}
	first := q.Thread == nil || q.Thread.Discarded
	if !first && q.Thread.Status == "working" {
		h.questions.mu.Unlock()
		return q, errThreadBusy
	}
	var answerer store.ChatSession
	if first {
		answerer, q.Thread, err = h.newThreadChat(ctx, q)
		if err != nil {
			h.questions.mu.Unlock()
			return q, err
		}
	} else if answerer, err = h.store.GetChatSession(ctx, q.Thread.ChatID); err != nil {
		h.questions.mu.Unlock()
		return q, err
	}
	q.Thread.Status = "working"
	if err := h.store.UpdateQuestion(ctx, q); err != nil {
		h.questions.mu.Unlock()
		return q, err
	}
	_, _ = h.store.AppendQuestionMessage(ctx, store.QuestionMessage{QuestionID: q.ID, Role: "user", Text: text})
	h.questions.mu.Unlock()
	h.broadcastQuestion(ctx, q)

	prompt := h.threadPrompt(ctx, q, text, first)
	if err := h.sendAgentChat(ctx, answerer, chatSendRequest{SessionID: answerer.ID, Content: prompt}); err != nil {
		h.threadFailed(ctx, q.ID, err.Error())
		return h.questionByID(ctx, q.ID), err
	}
	return h.questionByID(ctx, q.ID), nil
}

// newThreadChat creates the hidden answerer chat: a fork of the asking session when its
// agent can fork, else the commander's agent. Caller holds h.questions.mu.
func (h *Hub) newThreadChat(ctx context.Context, q store.Question) (store.ChatSession, *store.QuestionThread, error) {
	asker, err := h.store.GetChatSession(ctx, q.ChatID)
	if err != nil {
		return store.ChatSession{}, nil, err
	}
	if ok, why := h.canFork(ctx, asker); ok {
		return h.createThreadChat(ctx, q, asker, store.AnswererFork, "")
	} else {
		return h.createThreadChat(ctx, q, asker, store.AnswererCommander, why)
	}
}

// canFork reports whether the asking chat's session can be forked, or why not.
func (h *Hub) canFork(ctx context.Context, cs store.ChatSession) (bool, string) {
	label := agentLabel(cs)
	if cs.AgentSessionID == "" {
		return false, label + " has no session to copy yet"
	}
	if h.workerConnFor(cs.WorkerID) == nil {
		return false, h.workerLabel(ctx, cs.WorkerID) + " is not connected"
	}
	a, ok := h.lookupAccount(ctx, cs.WorkerID, cs.Agent, cs.AccountID)
	if !ok || !a.Fork {
		return false, label + " can't fork a session"
	}
	return true, ""
}

// readOnlyMode returns the account's read-only (risk 0) approval mode, if any.
func readOnlyMode(a protocol.AgentAccount) string {
	for _, m := range a.Approvals {
		if m.Risk == 0 && m.Disabled == "" {
			return m.ID
		}
	}
	return ""
}

func (h *Hub) createThreadChat(ctx context.Context, q store.Question, asker store.ChatSession, answerer, note string) (store.ChatSession, *store.QuestionThread, error) {
	sid, err := h.store.NewSID(ctx, ids.Chat)
	if err != nil {
		return store.ChatSession{}, nil, err
	}
	cs := store.ChatSession{
		ID: ids.UUID(), SID: sid, Backend: store.ChatBackendAgent, Status: protocol.ChatStatusIdle,
		Title: "Side conversation about " + q.SID, QuestionID: q.ID,
	}
	if answerer == store.AnswererFork {
		cs.ForkOf = asker.ID
		cs.WorkerID, cs.Agent, cs.AccountID = asker.WorkerID, asker.Agent, asker.AccountID
		cs.Model, cs.Effort, cs.Cwd = asker.Model, asker.Effort, asker.Cwd
	} else {
		// The commander's agent (its chat's target), else the default agent.
		if orch, ok := h.commanderChat(ctx); ok && orch.ID != asker.ID {
			cs.WorkerID, cs.Agent, cs.AccountID, cs.Model = orch.WorkerID, orch.Agent, orch.AccountID, orch.Model
		} else if asker.FleetTools {
			cs.WorkerID, cs.Agent, cs.AccountID, cs.Model = asker.WorkerID, asker.Agent, asker.AccountID, asker.Model
		}
		h.resolveChatTarget(ctx, &cs)
		cs.Cwd = "~"
		if cs.WorkerID == asker.WorkerID && asker.Cwd != "" {
			cs.Cwd = asker.Cwd
		}
	}
	if a, ok := h.lookupAccount(ctx, cs.WorkerID, cs.Agent, cs.AccountID); ok {
		cs.ApprovalMode = readOnlyMode(a)
	}
	if cs.WorkerID == "" || cs.Agent == "" {
		return cs, nil, errors.New("no connected agent can answer a side conversation")
	}
	h.initNotesCursor(ctx, &cs)
	if err := h.store.CreateChatSession(ctx, cs); err != nil {
		return cs, nil, err
	}
	return cs, &store.QuestionThread{
		ChatID: cs.ID, Answerer: answerer, Agent: cs.Agent, AccountID: cs.AccountID, WorkerID: cs.WorkerID,
		Status: "idle", Note: note, StartedAt: time.Now().UTC(),
	}, nil
}

// commanderChat returns the default commander chat, if one exists.
func (h *Hub) commanderChat(ctx context.Context) (store.ChatSession, bool) {
	saved, _ := h.store.GetAllSettings(ctx)
	if id := saved[commanderChatSetting]; id != "" {
		if cs, err := h.store.GetChatSession(ctx, id); err == nil && cs.Backend == store.ChatBackendAgent && cs.FleetTools {
			return cs, true
		}
	}
	for _, cs := range h.commanderChats(ctx) {
		return cs, true
	}
	return store.ChatSession{}, false
}

// threadPrompt is the message sent to the answerer for one user message.
func (h *Hub) threadPrompt(ctx context.Context, q store.Question, text string, first bool) string {
	reminder := "(Read-only. If you can propose an answer, end with a line: DRAFT ANSWER: <exact option label or short text>.)"
	if !first {
		return "The user asks: " + text + "\n" + reminder
	}
	var b strings.Builder
	if q.Thread != nil && q.Thread.Answerer == store.AnswererFork {
		b.WriteString("[Syre side conversation] You are a read-only copy of this session. The original session is paused, waiting for the user's answer to the question you asked (below). The user asks about it before answering: answer from your context, and don't change anything.\n\n")
		b.WriteString(questionText(q))
	} else {
		b.WriteString("[Syre side conversation] An agent asked the user the question below and can't answer follow-up questions itself, so you answer for it from this context pack. Don't change anything.\n\n")
		b.WriteString(questionText(q))
		b.WriteString("\n\n")
		b.WriteString(h.contextPack(ctx, q))
	}
	b.WriteString("\n\nThe user asks: " + text + "\n" + reminder)
	return b.String()
}

// contextPack describes the asking session for the commander fallback: who asked,
// where, its goal, a recent transcript excerpt and the files it touched.
func (h *Hub) contextPack(ctx context.Context, q store.Question) string {
	cs, err := h.store.GetChatSession(ctx, q.ChatID)
	if err != nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Asked by: %s in chat %s (%q) on %s, working directory %s.\n", agentLabel(cs), cs.SID, cs.Title, h.workerLabel(ctx, cs.WorkerID), cs.Cwd)
	evs, err := h.store.ListChatEvents(ctx, cs.ID, 0, 5000)
	if err != nil {
		return b.String()
	}
	var goal string
	var lines []string
	files := map[string]bool{}
	var fileOrder []string
	for _, e := range evs {
		var ev protocol.AgentChatEvent
		if json.Unmarshal([]byte(e.JSON), &ev) != nil {
			continue
		}
		switch ev.Kind {
		case eventUserMessage:
			t := ev.Text
			if strings.HasPrefix(t, eventNotePrefix) {
				continue
			}
			if goal == "" {
				goal = t
			}
			lines = append(lines, "User: "+oneLine(t, 400))
		case protocol.EventMessage:
			lines = append(lines, "Agent: "+oneLine(ev.Text, 600))
		case protocol.EventToolCall:
			if ev.ToolCall != nil && (ev.ToolCall.Status == "completed" || ev.ToolCall.Status == "failed") {
				lines = append(lines, "Tool: "+oneLine(firstNonEmpty(ev.ToolCall.Title, ev.ToolCall.Name), 160))
				for _, p := range ev.ToolCall.Paths {
					if !files[p] {
						files[p] = true
						fileOrder = append(fileOrder, p)
					}
				}
			}
		}
	}
	if goal != "" {
		fmt.Fprintf(&b, "Its goal (first message of that chat): %s\n", oneLine(goal, 800))
	}
	if len(fileOrder) > 0 {
		if len(fileOrder) > 20 {
			fileOrder = fileOrder[len(fileOrder)-20:]
		}
		fmt.Fprintf(&b, "Files it touched: %s\n", strings.Join(fileOrder, ", "))
	}
	// The most recent transcript lines, within a budget.
	budget, start := 6000, len(lines)
	for start > 0 && budget-len(lines[start-1]) > 0 {
		budget -= len(lines[start-1])
		start--
	}
	if start < len(lines) {
		b.WriteString("Recent transcript (oldest first):\n")
		for _, l := range lines[start:] {
			b.WriteString(l + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// threadMsg is one answerer message of the running turn (EventMessage replaces by id).
type threadMsg struct{ id, text string }

// onThreadChatEvent follows a hidden answerer chat: collects its reply, records it in
// the side conversation when the turn ends (or hands it to a summary waiter), and falls
// back to the commander if a fork can't start. Runs under the hidden chat's lock.
func (h *Hub) onThreadChatEvent(ctx context.Context, cs store.ChatSession, ev protocol.AgentChatEvent) {
	switch ev.Kind {
	case protocol.EventMessage:
		h.questions.mu.Lock()
		h.questions.init()
		msgs := h.questions.threadText[cs.ID]
		replaced := false
		for i := range msgs {
			if ev.MessageID != "" && msgs[i].id == ev.MessageID {
				msgs[i].text, replaced = ev.Text, true
			}
		}
		if !replaced {
			msgs = append(msgs, threadMsg{id: ev.MessageID, text: ev.Text})
		}
		h.questions.threadText[cs.ID] = msgs
		h.questions.mu.Unlock()
	case protocol.EventTurnEnd, protocol.EventError:
		if ev.Kind == protocol.EventError && ev.Error != nil && ev.Error.Kind == protocol.ErrKindTimeout {
			return // the answerer is slow, not failed
		}
		h.questions.mu.Lock()
		h.questions.init()
		var parts []string
		for _, m := range h.questions.threadText[cs.ID] {
			if t := strings.TrimSpace(m.text); t != "" {
				parts = append(parts, t)
			}
		}
		text := strings.Join(parts, "\n\n")
		delete(h.questions.threadText, cs.ID)
		if w := h.questions.summaryWaiters[cs.ID]; w != nil {
			delete(h.questions.summaryWaiters, cs.ID)
			h.questions.mu.Unlock()
			w <- text
			return
		}
		q, err := h.store.GetQuestion(ctx, cs.QuestionID)
		if err != nil || q.Thread == nil || q.Thread.ChatID != cs.ID {
			h.questions.mu.Unlock()
			return
		}
		failed := ev.Kind == protocol.EventError && text == ""
		if failed && q.Thread.Answerer == store.AnswererFork && !h.hasAnswererMessage(ctx, q.ID) && q.Pending() {
			// The fork couldn't start (or answer): let the commander answer instead.
			reason := "the fork failed"
			if ev.Error != nil {
				reason = "the fork failed: " + oneLine(ev.Error.Message, 200)
			}
			h.questions.mu.Unlock()
			h.goBackground(func(bctx context.Context) { h.fallbackToCommander(bctx, q.ID, reason) })
			return
		}
		display, draft := parseDraft(text, q)
		if display != "" {
			_, _ = h.store.AppendQuestionMessage(ctx, store.QuestionMessage{QuestionID: q.ID, Role: "answerer", Text: display, TurnID: ev.TurnID})
		}
		q.Thread.Status = "idle"
		if failed {
			q.Thread.Status = "failed"
			if ev.Error != nil {
				q.Thread.Note = oneLine(ev.Error.Message, 300)
			}
		}
		if draft != nil {
			q.Thread.Draft = draft
		}
		err = h.store.UpdateQuestion(ctx, q)
		h.questions.mu.Unlock()
		if err == nil {
			h.broadcastQuestion(ctx, q)
		}
	}
}

func (h *Hub) hasAnswererMessage(ctx context.Context, questionID string) bool {
	msgs, _ := h.store.ListQuestionMessages(ctx, questionID)
	for _, m := range msgs {
		if m.Role == "answerer" {
			return true
		}
	}
	return false
}

// threadFailed marks the side conversation failed (the answerer could not be reached).
func (h *Hub) threadFailed(ctx context.Context, id, why string) {
	h.questions.mu.Lock()
	q, err := h.store.GetQuestion(ctx, id)
	if err == nil && q.Thread != nil {
		q.Thread.Status, q.Thread.Note = "failed", oneLine(why, 300)
		err = h.store.UpdateQuestion(ctx, q)
	}
	h.questions.mu.Unlock()
	if err == nil {
		h.broadcastQuestion(ctx, q)
	}
}

// fallbackToCommander replaces a failed fork with the commander and re-asks the
// user's messages so far.
func (h *Hub) fallbackToCommander(ctx context.Context, id, reason string) {
	h.questions.mu.Lock()
	q, err := h.store.GetQuestion(ctx, id)
	if err != nil || !q.Pending() || q.Thread == nil {
		h.questions.mu.Unlock()
		return
	}
	asker, err := h.store.GetChatSession(ctx, q.ChatID)
	if err != nil {
		h.questions.mu.Unlock()
		return
	}
	oldChat := q.Thread.ChatID
	cs, thread, err := h.createThreadChat(ctx, q, asker, store.AnswererCommander, capitalize(reason)+"; the commander answers instead.")
	if err != nil {
		h.questions.mu.Unlock()
		h.threadFailed(ctx, id, reason+"; "+err.Error())
		return
	}
	thread.Status = "working"
	q.Thread = thread
	err = h.store.UpdateQuestion(ctx, q)
	h.questions.mu.Unlock()
	if err != nil {
		return
	}
	h.dropThreadChat(ctx, oldChat, true)
	h.broadcastQuestion(ctx, q)
	var asked []string
	if msgs, err := h.store.ListQuestionMessages(ctx, q.ID); err == nil {
		for _, m := range msgs {
			if m.Role == "user" {
				asked = append(asked, m.Text)
			}
		}
	}
	text := strings.Join(asked, "\n")
	if err := h.sendAgentChat(ctx, cs, chatSendRequest{SessionID: cs.ID, Content: h.threadPrompt(ctx, q, text, true)}); err != nil {
		h.threadFailed(ctx, id, err.Error())
	}
}

var draftRe = regexp.MustCompile(`(?im)^[ \t>*_]*DRAFT ANSWER[ \t]*[:：][ \t*_]*(.+?)[ \t*_]*$`)

// parseDraft splits the answerer's reply into the text shown and its proposed answer.
func parseDraft(text string, q store.Question) (string, *store.QuestionDraft) {
	ms := draftRe.FindAllStringSubmatchIndex(text, -1)
	if len(ms) == 0 {
		return text, nil
	}
	last := ms[len(ms)-1]
	raw := strings.TrimSpace(text[last[2]:last[3]])
	display := strings.TrimSpace(text[:last[0]] + text[last[1]:])
	raw = strings.Trim(raw, "\"'“”‘’`")
	if raw == "" {
		return display, nil
	}
	d := &store.QuestionDraft{}
	low := strings.ToLower(raw)
	for i, o := range q.Options {
		n := strconv.Itoa(i + 1)
		if strings.EqualFold(o.Label, raw) || low == n || low == "option "+n || strings.HasPrefix(low, strings.ToLower(o.Label)+" ") || strings.HasPrefix(low, strings.ToLower(o.Label)+" —") {
			d.OptionID, d.Label = o.ID, o.Label
			if rest := strings.TrimSpace(raw[min(len(raw), len(o.Label)):]); rest != "" && strings.HasPrefix(low, strings.ToLower(o.Label)) {
				rest = strings.TrimSpace(strings.TrimLeft(rest, " -—:,"))
				for _, sfx := range recommendedSuffixes {
					if strings.EqualFold(rest, sfx) || strings.EqualFold(rest, strings.Trim(sfx, "()[]- ")) {
						rest = ""
					}
				}
				d.Text = rest
			}
			return display, d
		}
	}
	d.Text = raw
	return display, d
}

// threadSummary asks the answerer for the note delivered with the answer ("The user
// asked …; after learning …, chose … because …"). It falls back to a plain summary when
// the answerer can't write it in time.
func (h *Hub) threadSummary(ctx context.Context, q store.Question) string {
	fallback := h.plainSummary(ctx, q)
	if q.Thread == nil {
		return fallback
	}
	cs, err := h.store.GetChatSession(ctx, q.Thread.ChatID)
	if err != nil || !chatIdle(cs.Status) {
		return fallback
	}
	w := make(chan string, 1)
	h.questions.mu.Lock()
	h.questions.init()
	h.questions.summaryWaiters[cs.ID] = w
	h.questions.mu.Unlock()
	prompt := fmt.Sprintf("[Syre] The user has now answered the original question with %s. Write the note the original agent will receive with the answer: "+
		"one or two sentences, in the form \"The user asked <what they asked>; after learning <the key fact>, chose <the answer> because <their reason, if given or implied>.\" "+
		"Reply with only that note.", answerChoiceText(q.Answer))
	if err := h.sendAgentChat(ctx, cs, chatSendRequest{SessionID: cs.ID, Content: prompt}); err != nil {
		h.dropSummaryWaiter(cs.ID)
		return fallback
	}
	t := time.NewTimer(summaryWait)
	defer t.Stop()
	select {
	case s := <-w:
		s, _ = parseDraft(strings.TrimSpace(s), q)
		if s = oneLine(s, 600); s != "" {
			return s
		}
	case <-t.C:
		h.dropSummaryWaiter(cs.ID)
	case <-ctx.Done():
		h.dropSummaryWaiter(cs.ID)
	}
	return fallback
}

func (h *Hub) dropSummaryWaiter(chatID string) {
	h.questions.mu.Lock()
	delete(h.questions.summaryWaiters, chatID)
	h.questions.mu.Unlock()
}

// plainSummary summarizes a side conversation without the answerer.
func (h *Hub) plainSummary(ctx context.Context, q store.Question) string {
	msgs, _ := h.store.ListQuestionMessages(ctx, q.ID)
	var asked []string
	for _, m := range msgs {
		if m.Role == "user" {
			asked = append(asked, m.Text)
		}
	}
	if len(asked) == 0 {
		return ""
	}
	s := fmt.Sprintf("The user asked %q", oneLine(asked[0], 200))
	if len(asked) > 1 {
		s += fmt.Sprintf(" and %d follow-up question(s)", len(asked)-1)
	}
	return s + " before choosing " + answerChoiceText(q.Answer) + "."
}

// discardThread closes and deletes a question's answerer chat (and, for a fork, what
// the agent stored for it) once the question no longer needs it.
func (h *Hub) discardThread(ctx context.Context, id string) {
	h.questions.mu.Lock()
	q, err := h.store.GetQuestion(ctx, id)
	if err != nil || q.Thread == nil || q.Thread.Discarded {
		h.questions.mu.Unlock()
		return
	}
	chatID := q.Thread.ChatID
	q.Thread.Discarded = true
	if q.Thread.Status == "working" {
		q.Thread.Status = "idle"
	}
	err = h.store.UpdateQuestion(ctx, q)
	h.questions.mu.Unlock()
	if err != nil {
		return
	}
	h.dropThreadChat(ctx, chatID, true)
	h.broadcastQuestion(ctx, q)
}

// dropThreadChat ends a hidden answerer chat's engine (discarding its session) and
// deletes the chat.
func (h *Hub) dropThreadChat(ctx context.Context, chatID string, discard bool) {
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil || cs.QuestionID == "" {
		return
	}
	if wc := h.workerConnFor(cs.WorkerID); wc != nil {
		h.sendWorkerJSON(wc, protocol.MsgAgentChatClose, protocol.AgentChatClose{ChatID: chatID, Discard: discard})
	}
	h.questions.mu.Lock()
	delete(h.questions.threadText, chatID)
	h.questions.mu.Unlock()
	h.mu.Lock()
	rt := h.agentChats[chatID]
	delete(h.agentChats, chatID)
	h.mu.Unlock()
	if rt != nil {
		rt.mu.Lock()
		srv := rt.mcpSrv
		rt.mcpSrv = nil
		rt.mu.Unlock()
		closeMCPServerSessions(srv)
	}
	if err := h.store.DeleteChatSession(ctx, chatID); err != nil {
		h.logger.Warn("delete side-conversation chat failed", "chat_id", chatID, "err", err)
	}
}
