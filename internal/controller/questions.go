package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Questions (PRD §5.8b). Every question an agent asks — Syre's ask_user tool and the
// agents' own question and permission prompts — becomes a persistent hub object in one
// fleet-wide queue, sorted by blocking impact then age. A question stays open until it is
// answered: reloads, side conversations and abandoned tool calls don't dismiss it.
//
// ask_user is validated (context, why_now, per-option consequences, rationale for a
// recommendation); under-specified calls are sent back to the agent with what to add.
// The answer goes back to the agent's pending tool call; if the agent's MCP client gave
// up waiting, it is delivered as a "[Syre]" note before (or as) its next turn.

// questionState is the hub's in-memory question state.
type questionState struct {
	// mu serializes read-modify-write of question rows. Lock order: a chat's rt.mu may be
	// held when taking mu (hooks inside emitChatEvent), never the reverse.
	mu sync.Mutex
	// threadText: hidden side-conversation chat id -> the answerer's messages of the
	// running turn.
	threadText map[string][]threadMsg
	// summaryWaiters: hidden chat id -> waiter for the summary turn's text.
	summaryWaiters map[string]chan string
	// timers for decided delegations (question id -> timer).
	timers map[string]*time.Timer
	// closed is set by Hub.Shutdown so no new delegation timers are armed.
	closed bool
	// refCache caches resolved references briefly.
	refCache map[string]refCacheEntry
}

func (qs *questionState) init() {
	if qs.threadText == nil {
		qs.threadText = map[string][]threadMsg{}
		qs.summaryWaiters = map[string]chan string{}
		qs.timers = map[string]*time.Timer{}
		qs.refCache = map[string]refCacheEntry{}
	}
}

// questionElicitationID is the chat elicitation id showing question id inline.
func questionElicitationID(questionID string) string { return "q-" + questionID }

// --- ask_user input and validation ------------------------------------------------------

// askOption is one ask_user option. A bare string is accepted (as the label) so the
// validation can say exactly what is missing instead of failing on the schema.
type askOption struct {
	Label       string `json:"label"`
	Consequence string `json:"consequence,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}

func (o *askOption) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*o = askOption{Label: s}
		return nil
	}
	type plain askOption
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*o = askOption(p)
	return nil
}

type askUserInput struct {
	Question       string      `json:"question"`
	Context        string      `json:"context,omitempty"`
	WhyNow         string      `json:"why_now,omitempty"`
	Options        []askOption `json:"options,omitempty"`
	Recommendation string      `json:"recommendation,omitempty"`
	Rationale      string      `json:"rationale,omitempty"`
	AllowText      *bool       `json:"allow_text,omitzero"`
	Category       string      `json:"category,omitempty"`
	Blocks         []string    `json:"blocks,omitempty"`
}

// askUserSchema is ask_user's input schema. context, why_now and the per-option
// consequence are required by Syre's own validation (with a precise error), not by
// the schema, so an agent always learns exactly what to add.
var askUserSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"question": map[string]any{"type": "string", "description": "ONE decision, phrased so it can be answered without other context. Name tickets/PRs with their titles, not just ids (\"Shorten the exit criteria of SYR-2Q0CH 'Questions and steering'?\")."},
		"context":  map[string]any{"type": "string", "description": "REQUIRED. What this is about: the ticket/PR/files with their titles, what you are doing and what you found, so a user who wasn't watching understands the question."},
		"why_now":  map[string]any{"type": "string", "description": "REQUIRED. What is blocked until the user answers and what you will do next with each answer."},
		"options": map[string]any{
			"type":        "array",
			"description": "Choices (prefer two: yes/no with a recommendation). Each needs a consequence. Omit for a free-form answer.",
			"items": map[string]any{"anyOf": []any{
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"label":       map[string]any{"type": "string", "description": "Short choice text"},
						"consequence": map[string]any{"type": "string", "description": "REQUIRED. What happens if the user picks it"},
						"recommended": map[string]any{"type": "boolean", "description": "Mark the option you recommend (then give rationale)"},
					},
					"required": []any{"label"},
				},
				map[string]any{"type": "string"},
			}},
		},
		"recommendation": map[string]any{"type": "string", "description": "Label of the option you recommend (or mark it recommended)"},
		"rationale":      map[string]any{"type": "string", "description": "REQUIRED with a recommendation: why you recommend it"},
		"allow_text":     map[string]any{"type": "boolean", "description": "Also accept a free-form answer (always on when there are no options)"},
		"category":       map[string]any{"type": "string", "description": "Optional kind of decision, e.g. routine, design, scope, risk (users can let the commander decide some categories)"},
		"blocks": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
			"description": "Optional: what else waits on the answer (e.g. 't-7qk3m deploy', 'the release')"},
	},
	"required": []any{"question"},
}

const askUserDescription = "Ask the user ONE decision in Syre and wait for the answer. The question shows in the user's fleet-wide question queue (desktop and phone) " +
	"and stays open until answered. Required: question, context (what this is about, with ticket/PR titles), why_now (what is blocked and what you'll do next), " +
	"and for each option a consequence (what happens if picked). Prefer yes/no with a recommendation: mark the recommended option and give rationale (why). " +
	"Returns the user's answer (and a summary if they asked clarifying questions first). If it returns that the question is still open, don't ask again: " +
	"Syre delivers the answer before your next turn."

// recommendedSuffixes are label decorations agents use for a recommendation.
var recommendedSuffixes = []string{"(recommended)", "[recommended]", "- recommended", "(recommend)"}

// normalizeAskInput trims fields, strips "(recommended)" decorations into the option's
// Recommended flag, and resolves recommendation (a label or 1-based number).
func normalizeAskInput(in askUserInput) askUserInput {
	in.Question = strings.TrimSpace(in.Question)
	in.Context = strings.TrimSpace(in.Context)
	in.WhyNow = strings.TrimSpace(in.WhyNow)
	in.Rationale = strings.TrimSpace(in.Rationale)
	in.Category = strings.ToLower(strings.TrimSpace(in.Category))
	in.Recommendation = strings.TrimSpace(in.Recommendation)
	opts := make([]askOption, 0, len(in.Options))
	for _, o := range in.Options {
		o.Label = strings.TrimSpace(o.Label)
		o.Consequence = strings.TrimSpace(o.Consequence)
		low := strings.ToLower(o.Label)
		for _, sfx := range recommendedSuffixes {
			if strings.HasSuffix(low, sfx) {
				o.Label = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(o.Label[:len(o.Label)-len(sfx)]), "-–—:"))
				o.Recommended = true
				break
			}
		}
		if o.Label != "" {
			opts = append(opts, o)
		}
	}
	if r := in.Recommendation; r != "" {
		idx := -1
		if n, err := strconv.Atoi(r); err == nil && n >= 1 && n <= len(opts) {
			idx = n - 1
		}
		for i, o := range opts {
			if strings.EqualFold(o.Label, r) {
				idx = i
			}
		}
		if idx >= 0 {
			opts[idx].Recommended = true
			in.Recommendation = opts[idx].Label
		}
	}
	in.Options = opts
	var blocks []string
	for _, b := range in.Blocks {
		if b = strings.TrimSpace(b); b != "" {
			blocks = append(blocks, b)
		}
	}
	in.Blocks = blocks
	return in
}

// askValidationError lists what an under-specified question is missing.
type askValidationError struct {
	missing []string // field names (for metrics)
	lines   []string // what to add
}

func (e *askValidationError) Error() string {
	var b strings.Builder
	b.WriteString("Question not shown to the user: it needs more so they can answer without guessing. Add:\n")
	for _, l := range e.lines {
		b.WriteString("- " + l + "\n")
	}
	b.WriteString("Then call ask_user again. Ask one decision; say what you would do and why; name tickets with their titles, not just ids.")
	return b.String()
}

// validateAsk checks a normalized ask_user input. nil means it may be shown.
func validateAsk(in askUserInput) *askValidationError {
	e := &askValidationError{}
	add := func(field, line string) {
		e.missing = append(e.missing, field)
		e.lines = append(e.lines, line)
	}
	if in.Question == "" {
		add("question", "question: the one decision you need, as a question")
	}
	if in.Context == "" {
		add("context", "context: what this is about — the ticket/PR/files with their titles and what you're doing (the user may not have watched this session)")
	}
	if in.WhyNow == "" {
		add("why_now", "why_now: what is blocked until the user answers, and what you'll do next with each answer")
	}
	if len(in.Options) == 1 {
		add("options", "options: give at least two choices (e.g. yes/no), or none for a free-form answer")
	}
	var noCons []string
	recommended := ""
	nRec := 0
	for i, o := range in.Options {
		if o.Consequence == "" {
			noCons = append(noCons, fmt.Sprintf("%d (%q)", i+1, o.Label))
		}
		if o.Recommended {
			nRec++
			recommended = o.Label
		}
	}
	if len(noCons) > 0 {
		add("consequence", "consequence for option "+strings.Join(noCons, ", ")+": what happens if the user picks it")
	}
	if in.Recommendation != "" && nRec == 0 {
		add("recommendation", fmt.Sprintf("recommendation: %q is not one of the option labels; use an exact label or mark the option recommended", in.Recommendation))
	}
	if nRec > 1 {
		add("recommendation", "recommendation: recommend at most one option")
	}
	if nRec == 1 && in.Rationale == "" {
		add("rationale", fmt.Sprintf("rationale: why you recommend %q", recommended))
	}
	if len(e.lines) == 0 {
		return nil
	}
	return e
}

// --- formatting -------------------------------------------------------------------------

// questionDetail renders a question's context block for the inline chat card.
func questionDetail(q store.Question) string {
	var parts []string
	if q.Context != "" {
		parts = append(parts, "**Context:** "+q.Context)
	}
	if q.WhyNow != "" {
		parts = append(parts, "**Why now:** "+q.WhyNow)
	}
	if rec := recommendedOption(q); rec != nil && q.Rationale != "" {
		parts = append(parts, fmt.Sprintf("**Recommended:** %s — %s", rec.Label, q.Rationale))
	}
	return strings.Join(parts, "\n\n")
}

func elicitationOptions(q store.Question) []protocol.AgentElicitationOption {
	out := make([]protocol.AgentElicitationOption, 0, len(q.Options))
	for _, o := range q.Options {
		kind := o.Kind
		if kind == "" {
			kind = "choice"
		}
		out = append(out, protocol.AgentElicitationOption{ID: o.ID, Label: o.Label, Kind: kind, Consequence: o.Consequence, Recommended: o.Recommended})
	}
	return out
}

// questionText renders a question for an agent (side conversations, delegation).
func questionText(q store.Question) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Question %s: %s\n", q.SID, q.Prompt)
	if q.Context != "" {
		fmt.Fprintf(&b, "Context: %s\n", q.Context)
	}
	if q.WhyNow != "" {
		fmt.Fprintf(&b, "Why now: %s\n", q.WhyNow)
	}
	for i, o := range q.Options {
		fmt.Fprintf(&b, "Option %d: %s", i+1, o.Label)
		if o.Recommended {
			b.WriteString(" (recommended)")
		}
		if o.Consequence != "" {
			fmt.Fprintf(&b, " — %s", o.Consequence)
		}
		b.WriteString("\n")
	}
	if q.Rationale != "" {
		fmt.Fprintf(&b, "Rationale for the recommendation: %s\n", q.Rationale)
	}
	if q.AllowText {
		b.WriteString("A free-form answer is also accepted.\n")
	}
	for _, r := range q.Refs {
		if r.Resolved && r.Title != "" {
			fmt.Fprintf(&b, "Reference %s: %s", r.Ref, r.Title)
			if r.Status != "" {
				fmt.Fprintf(&b, " [%s]", r.Status)
			}
			if r.Excerpt != "" {
				fmt.Fprintf(&b, " — %s", oneLine(r.Excerpt, 300))
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// recommendedOption returns the question's recommended option, if any.
func recommendedOption(q store.Question) *store.QuestionOption {
	for i := range q.Options {
		if q.Options[i].Recommended || (q.Recommendation != "" && q.Options[i].ID == q.Recommendation) {
			return &q.Options[i]
		}
	}
	return nil
}

// answerChoiceText describes an answer in one line ("No, keep the strict criteria" or the text).
func answerChoiceText(a *store.QuestionAnswer) string {
	switch {
	case a == nil:
		return ""
	case a.Label != "" && a.Text != "":
		return fmt.Sprintf("%q (note: %s)", a.Label, a.Text)
	case a.Label != "":
		return fmt.Sprintf("%q", a.Label)
	default:
		return fmt.Sprintf("%q", a.Text)
	}
}

// answerResultText is the tool result an agent receives for an answered question.
func answerResultText(q store.Question) string {
	a := q.Answer
	var b strings.Builder
	if a.By.Actor == store.ActorCommander {
		fmt.Fprintf(&b, "The commander answered on the user's behalf (the user delegated this question): %s.", answerChoiceText(a))
		if q.Delegation != nil && q.Delegation.Rationale != "" {
			fmt.Fprintf(&b, " Its rationale: %s", q.Delegation.Rationale)
		}
		b.WriteString(" The user may still override it; Syre will tell you if they do.")
	} else {
		if a.Label != "" {
			fmt.Fprintf(&b, "The user chose: %q.", a.Label)
			if a.Text != "" {
				fmt.Fprintf(&b, "\nTheir note: %s", a.Text)
			}
		} else {
			fmt.Fprintf(&b, "The user answered: %s", a.Text)
		}
	}
	if a.Summary != "" {
		fmt.Fprintf(&b, "\nSide conversation: %s", a.Summary)
	}
	return b.String()
}

// answerNoteText is the "[Syre]" note for an answer the agent's tool call no longer waits for.
func answerNoteText(q store.Question) string {
	a := q.Answer
	if a.Correction {
		prev := ""
		if q.Delegation != nil {
			prev = fmt.Sprintf(" The commander had answered %q on the user's behalf;", firstNonEmpty(q.Delegation.Label, q.Delegation.Text))
		}
		s := fmt.Sprintf("Correction to your question %s (%q):%s the user overrides it with %s.", q.SID, oneLine(q.Prompt, 200), prev, answerChoiceText(a))
		if a.Summary != "" {
			s += " Side conversation: " + a.Summary
		}
		return s + " Undo anything you did based on the earlier answer if needed, then continue with this one."
	}
	return fmt.Sprintf("Answer to your earlier question %s (%q): %s Continue with this answer; don't ask it again.",
		q.SID, oneLine(q.Prompt, 200), answerResultText(q))
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > max {
		r := []rune(s)
		return string(r[:max-1]) + "…"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// normalizedPrompt is the dedupe key of a question: prompt and option labels, folded.
func normalizedPrompt(prompt string, labels []string) string {
	f := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	parts := []string{f(prompt)}
	for _, l := range labels {
		parts = append(parts, f(l))
	}
	return strings.Join(parts, "\x1f")
}

func optionLabels(opts []store.QuestionOption) []string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = o.Label
	}
	return out
}

// --- ask_user ---------------------------------------------------------------------------

// askUser is the ask_user MCP tool: validate, create (or re-attach to) the question,
// show it in the chat and the queue, and wait for the answer. The returned error is a
// tool error the agent sees (under-specified question).
func (h *Hub) askUser(ctx context.Context, chatID string, raw askUserInput) (string, error) {
	text, _, err := h.askUserAnswer(ctx, chatID, raw)
	return text, err
}

// askUserAnswer is askUser that also returns the answer when the call got one.
func (h *Hub) askUserAnswer(ctx context.Context, chatID string, raw askUserInput) (string, *store.QuestionAnswer, error) {
	text, qid, err := h.askUserWait(ctx, chatID, raw)
	if err != nil || qid == "" {
		return text, nil, err
	}
	if q, err := h.store.GetQuestion(context.WithoutCancel(ctx), qid); err == nil && q.Status == store.QuestionAnswered && q.Answer != nil && q.Answer.DeliveredAt != nil && q.Answer.Delivery == store.DeliveryTool {
		return text, q.Answer, nil
	}
	return text, nil, nil
}

// askUserWait runs ask_user and returns the tool result text and, when the call ended
// with an answer, the question id.
func (h *Hub) askUserWait(ctx context.Context, chatID string, raw askUserInput) (string, string, error) {
	in := normalizeAskInput(raw)
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil {
		return "", "", errors.New("chat not found")
	}
	if verr := validateAsk(in); verr != nil {
		if err := h.store.AddQuestionReject(context.WithoutCancel(ctx), store.QuestionReject{
			ChatID: chatID, Agent: cs.Agent, Missing: strings.Join(verr.missing, ","),
		}); err != nil {
			h.logger.Warn("record question reject failed", "chat_id", chatID, "err", err)
		}
		return "", "", verr
	}
	allowText := len(in.Options) == 0
	if in.AllowText != nil {
		allowText = *in.AllowText || len(in.Options) == 0
	}
	opts := make([]store.QuestionOption, 0, len(in.Options))
	recID := ""
	for i, o := range in.Options {
		id := fmt.Sprintf("option_%d", i+1)
		opts = append(opts, store.QuestionOption{ID: id, Label: o.Label, Consequence: o.Consequence, Recommended: o.Recommended})
		if o.Recommended {
			recID = id
		}
	}
	q := store.Question{
		ChatID: chatID, WorkerID: cs.WorkerID, Agent: cs.Agent, AccountID: cs.AccountID,
		Kind: store.QuestionKindQuestion, Source: store.QuestionSourceAskUser,
		Prompt: in.Question, Context: in.Context, WhyNow: in.WhyNow, Options: opts, AllowText: allowText,
		Recommendation: recID, Rationale: in.Rationale, Category: in.Category, Blocks: in.Blocks,
	}
	q, reused, err := h.createOrReuseQuestion(ctx, cs, q)
	if err != nil {
		return "", "", err
	}
	// An identical re-ask after the answer arrived but before it was delivered gets it now.
	if reused && q.Status == store.QuestionAnswered {
		if cur, ok := h.claimDelivery(ctx, q.ID, store.DeliveryTool); ok {
			return answerResultText(cur), q.ID, nil
		}
	}

	elID := questionElicitationID(q.ID)
	ask := &pendingAsk{answer: make(chan askAnswer, 1), questionID: q.ID}
	rt := h.chatRT(chatID)
	rt.mu.Lock()
	if old := rt.asks[elID]; old != nil {
		// A previous call for the same card is superseded by this one.
		old.answer <- askAnswer{reason: "superseded"}
	}
	rt.asks[elID] = ask
	rt.mu.Unlock()
	h.showQuestionInChat(ctx, q)
	// The hub knows best that the agent now waits for the user (its tool call shows as
	// running to the worker).
	h.setActivity(protocol.SessionActivity{ChatID: chatID, WorkerID: cs.WorkerID, State: protocol.ActivityAsking,
		Tool: "ask_user", Label: oneLine(q.Prompt, 160), Source: protocol.ActivitySourceHub})
	if !reused {
		h.afterQuestionCreated(q)
	}

	timer := time.NewTimer(h.askWait(cs.Agent))
	defer timer.Stop()
	var ans askAnswer
	select {
	case ans = <-ask.answer:
	case <-timer.C:
		ans = askAnswer{reason: "timeout"}
	case <-ctx.Done():
		ans = askAnswer{reason: "abandoned"}
	case <-h.ctx.Done():
		ans = askAnswer{reason: "shutdown"}
	}
	rt.mu.Lock()
	if cur := rt.asks[elID]; cur == ask {
		delete(rt.asks, elID)
	} else if ans.reason != "" {
		// Someone took this waiter out to answer it: the answer is (about to be) in the channel.
		select {
		case a := <-ask.answer:
			ans = a
		case <-time.After(2 * time.Second):
		}
	}
	rt.mu.Unlock()
	if ans.final {
		return ans.text, q.ID, nil
	}
	switch ans.reason {
	case "superseded":
		return "This call was replaced by a newer identical question; the answer goes there.", "", nil
	case "cancelled":
		return "no answer: " + firstNonEmpty(ans.text, "the question was cancelled"), "", nil
	}
	// The agent stops waiting (timeout, its MCP client gave up, hub shutdown): the
	// question stays open, and an answer that arrived in the meantime is returned now.
	if cur, ok := h.claimDelivery(context.WithoutCancel(ctx), q.ID, store.DeliveryTool); ok {
		return answerResultText(cur), q.ID, nil
	}
	h.markAbandoned(context.WithoutCancel(ctx), q.ID)
	return fmt.Sprintf("The question is still open in Syre (%s): the user hasn't answered yet. Don't ask it again and don't guess. "+
		"Continue with work that doesn't depend on the answer, or end your turn; Syre will give you the answer in a [Syre] note before your next turn.", q.SID), "", nil
}

// agentMCPCallLimits are MCP tool-call limits agents impose that Syre can't raise
// from the session (cursor-agent: the MCP SDK's fixed 60 s; Antigravity: 180 s unless
// its user config says otherwise). ask_user returns "still open" just before them, so
// the agent gets a clear answer instead of a timeout error.
var agentMCPCallLimits = map[string]time.Duration{
	"cursor": 55 * time.Second,
	"agy":    170 * time.Second,
}

// askWait is how long ask_user waits for the user in a chat of agent.
func (h *Hub) askWait(agent string) time.Duration {
	t := h.askTimeout()
	if lim, ok := agentMCPCallLimits[agent]; ok && lim < t {
		return lim
	}
	return t
}

func (h *Hub) askTimeout() time.Duration {
	if h.AskUserTimeout > 0 {
		return h.AskUserTimeout
	}
	return defaultAskUserTimeout
}

// mcpToolTimeoutSeconds is the MCP tool-call timeout handed to managed agents: longer
// than the hub's own ask_user wait, so the hub always answers first.
func (h *Hub) mcpToolTimeoutSeconds() int {
	return int((h.askTimeout() + 10*time.Minute) / time.Second)
}

// createOrReuseQuestion stores q, or returns the open question of the same chat with the
// same prompt and options (re-asks map to the same card; blocked time keeps running).
func (h *Hub) createOrReuseQuestion(ctx context.Context, cs store.ChatSession, q store.Question) (store.Question, bool, error) {
	ctx = context.WithoutCancel(ctx)
	h.questions.mu.Lock()
	defer h.questions.mu.Unlock()
	key := normalizedPrompt(q.Prompt, optionLabels(q.Options))
	existing, err := h.store.ListQuestions(ctx, store.QuestionFilter{ChatID: cs.ID, Limit: 50})
	if err != nil {
		return q, false, err
	}
	for _, e := range existing {
		if e.Source != q.Source || normalizedPrompt(e.Prompt, optionLabels(e.Options)) != key {
			continue
		}
		pending := e.Pending()
		undelivered := e.Status == store.QuestionAnswered && e.Answer != nil && e.Answer.DeliveredAt == nil
		if !pending && !undelivered {
			continue
		}
		e.AskCount++
		e.Abandoned = false
		// Refresh what the agent may have improved in its re-ask.
		if pending {
			e.Context, e.WhyNow, e.Rationale = q.Context, q.WhyNow, q.Rationale
			e.Options, e.Recommendation, e.AllowText = q.Options, q.Recommendation, q.AllowText
		}
		if err := h.store.UpdateQuestion(ctx, e); err != nil {
			return e, false, err
		}
		h.broadcastQuestion(ctx, e)
		return e, true, nil
	}
	q.Rejections = h.recentRejects(ctx, cs.ID)
	q.Blocks = append(q.Blocks, h.derivedBlocks(ctx, cs)...)
	q.Refs = detectRefs(q.Prompt, q.Context, q.WhyNow, q.Rationale, strings.Join(optionLabels(q.Options), " "))
	q.GroupKey = questionGroupKey(q, cs)
	created, err := h.store.CreateQuestion(ctx, q)
	if err != nil {
		return q, false, err
	}
	return created, false, nil
}

// recentRejects counts this chat's rejected attempts since its last question (15 min max).
func (h *Hub) recentRejects(ctx context.Context, chatID string) int {
	since := time.Now().Add(-15 * time.Minute)
	if qs, err := h.store.ListQuestions(ctx, store.QuestionFilter{ChatID: chatID, Limit: 1}); err == nil && len(qs) > 0 && qs[0].CreatedAt.After(since) {
		since = qs[0].CreatedAt
	}
	rs, err := h.store.ListQuestionRejects(ctx, since)
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range rs {
		if r.ChatID == chatID {
			n++
		}
	}
	return n
}

// derivedBlocks lists what waits on a chat's answer beyond the chat itself.
func (h *Hub) derivedBlocks(ctx context.Context, cs store.ChatSession) []string {
	var out []string
	if cs.FleetTools {
		out = append(out, "the commander (fleet coordination)")
	}
	if n := h.liveTasksOf(ctx, cs.ID); n > 0 {
		out = append(out, fmt.Sprintf("%d live task(s) dispatched by %s", n, cs.SID))
	}
	return out
}

func (h *Hub) liveTasksOf(ctx context.Context, chatID string) int {
	tasks, err := h.store.ListTasks(ctx, "", "", 500)
	if err != nil {
		return 0
	}
	n := 0
	for _, t := range tasks {
		if t.ParentChatID == chatID && !protocol.IsTerminalTaskStatus(t.Status) {
			n++
		}
	}
	return n
}

// questionGroupKey groups a question with others about the same ticket, else the same chat.
func questionGroupKey(q store.Question, cs store.ChatSession) string {
	for _, r := range q.Refs {
		if r.Kind == "ticket" {
			return "ticket:" + r.Ref
		}
	}
	if q.TaskID != "" {
		return "task:" + q.TaskID
	}
	return "chat:" + cs.ID
}

// showQuestionInChat emits the question's elicitation in its chat (inline card).
func (h *Hub) showQuestionInChat(ctx context.Context, q store.Question) {
	h.emitChatEvent(ctx, q.ChatID, protocol.AgentChatEvent{
		Kind: protocol.EventElicitation,
		Elicitation: &protocol.AgentElicitation{
			ID: questionElicitationID(q.ID), Kind: protocol.ElicitQuestion, Title: q.Prompt, Detail: questionDetail(q),
			Options: elicitationOptions(q), AllowText: q.AllowText, QuestionID: q.ID,
		},
	}, emitOpts{})
}

// afterQuestionCreated runs the asynchronous follow-ups of a new question: reference
// enrichment and auto-delegation rules. It never delays the question itself.
func (h *Hub) afterQuestionCreated(q store.Question) {
	ctx := context.WithoutCancel(h.ctx)
	if cur, err := h.store.GetQuestion(ctx, q.ID); err == nil {
		h.broadcastQuestion(ctx, cur)
	}
	h.goBackground(func(ctx context.Context) {
		h.enrichQuestion(ctx, q.ID)
	})
	if h.autoDelegates(ctx, q) {
		h.goBackground(func(ctx context.Context) {
			if _, err := h.delegateQuestion(withCause(ctx, store.Cause{Actor: store.ActorSystem, Source: "auto-delegate", Confidence: store.ConfidenceCertain}), q.ID, true); err != nil {
				h.logger.Info("auto-delegation skipped", "question", q.SID, "err", err)
			}
		})
	}
}

// markAbandoned records that the agent's tool call stopped waiting for a question.
func (h *Hub) markAbandoned(ctx context.Context, id string) {
	h.questions.mu.Lock()
	q, err := h.store.GetQuestion(ctx, id)
	if err == nil && q.Pending() && !q.Abandoned {
		q.Abandoned = true
		err = h.store.UpdateQuestion(ctx, q)
	}
	h.questions.mu.Unlock()
	if err == nil {
		h.broadcastQuestion(ctx, q)
	}
}

// claimDelivery marks an answered question's answer as delivered by `how`; false when
// there is no ready, undelivered answer (another path delivered it, or the summary is
// still being written).
func (h *Hub) claimDelivery(ctx context.Context, id, how string) (store.Question, bool) {
	h.questions.mu.Lock()
	defer h.questions.mu.Unlock()
	q, err := h.store.GetQuestion(ctx, id)
	if err != nil || q.Status != store.QuestionAnswered || q.Answer == nil || q.Answer.DeliveredAt != nil || q.Answer.Preparing {
		return q, false
	}
	now := time.Now().UTC()
	q.Answer.Delivery, q.Answer.DeliveredAt = how, &now
	if q.Delegation != nil && q.Delegation.State == store.DelegationDecided && q.Answer.By.Actor == store.ActorCommander {
		q.Delegation.State = store.DelegationDelivered
	}
	if err := h.store.UpdateQuestion(ctx, q); err != nil {
		return q, false
	}
	return q, true
}

// --- answering --------------------------------------------------------------------------

// errQuestionClosed is returned when answering a question that no longer waits.
var errQuestionClosed = errors.New("this question is no longer open")

// questionAnswerInput is an answer from the UI, an operator, or the commander.
type questionAnswerInput struct {
	OptionID string `json:"option_id,omitempty"`
	Text     string `json:"text,omitempty"`
}

// answerQuestion records an answer with the cause in ctx and delivers it (after writing
// the side-conversation summary, when there was one). Answering a question the
// commander already answered on the user's behalf is an override: a correction note.
func (h *Hub) answerQuestion(ctx context.Context, id string, in questionAnswerInput) (store.Question, error) {
	ctx = context.WithoutCancel(ctx)
	cause := causeFrom(ctx)
	h.questions.mu.Lock()
	q, err := h.store.GetQuestion(ctx, id)
	if err != nil {
		h.questions.mu.Unlock()
		return q, err
	}
	correction := false
	switch {
	case q.Pending():
	case q.Status == store.QuestionAnswered && q.Answer != nil && q.Answer.By.Actor == store.ActorCommander && cause.Actor != store.ActorCommander &&
		(q.Source == store.QuestionSourceAskUser || q.Answer.DeliveredAt == nil):
		// Overriding the commander's decision: before delivery it is simply replaced;
		// after, the agent gets a correction note.
		correction = q.Answer.DeliveredAt != nil
	default:
		h.questions.mu.Unlock()
		return q, errQuestionClosed
	}
	a := &store.QuestionAnswer{By: cause, At: time.Now().UTC(), Text: strings.TrimSpace(in.Text), Correction: correction}
	if in.OptionID != "" {
		found := false
		for _, o := range q.Options {
			if o.ID == in.OptionID || strings.EqualFold(o.Label, in.OptionID) {
				a.OptionID, a.Label, found = o.ID, o.Label, true
				break
			}
		}
		if !found {
			h.questions.mu.Unlock()
			return q, fmt.Errorf("unknown option %q", in.OptionID)
		}
	}
	if a.OptionID == "" && a.Text == "" {
		h.questions.mu.Unlock()
		return q, errors.New("choose an option or type an answer")
	}
	if a.OptionID == "" && !q.AllowText && len(q.Options) > 0 {
		h.questions.mu.Unlock()
		return q, errors.New("this question needs one of its options")
	}
	if q.Delegation != nil && cause.Actor != store.ActorCommander {
		switch q.Delegation.State {
		case store.DelegationPending, store.DelegationDecided, store.DelegationDelivered:
			q.Delegation.State = store.DelegationOverridden
		}
		h.stopDelegationTimer(q.ID)
	}
	needSummary := q.Thread != nil && !q.Thread.Discarded && h.threadHasUserMessages(ctx, q.ID)
	a.Preparing = needSummary
	q.Answer = a
	q.Status = store.QuestionAnswered
	q.SnoozedUntil = nil
	now := time.Now().UTC()
	q.ClosedAt = &now
	if err := h.store.UpdateQuestion(ctx, q); err != nil {
		h.questions.mu.Unlock()
		return q, err
	}
	h.questions.mu.Unlock()
	h.broadcastQuestion(ctx, q)
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindQuestion, Action: "answered", Cause: cause, WorkerID: q.WorkerID, ChatID: q.ChatID,
		Summary:  fmt.Sprintf("%s answered question %s (%s): %s", capitalize(describeActor(cause)), q.SID, oneLine(q.Prompt, 120), answerChoiceText(a)),
		MetaJSON: auditMeta(map[string]any{"question_sid": q.SID, "option_id": a.OptionID, "clarified": needSummary}),
	})
	if needSummary {
		h.goBackground(func(bctx context.Context) {
			summary := h.threadSummary(bctx, q)
			h.questions.mu.Lock()
			cur, err := h.store.GetQuestion(bctx, q.ID)
			if err == nil && cur.Answer != nil {
				cur.Answer.Summary, cur.Answer.Preparing = summary, false
				err = h.store.UpdateQuestion(bctx, cur)
			}
			h.questions.mu.Unlock()
			if err == nil {
				h.broadcastQuestion(bctx, cur)
				h.deliverAnswer(bctx, cur)
			}
		})
		return q, nil
	}
	h.deliverAnswer(ctx, q)
	return h.questionByID(ctx, q.ID), nil
}

func (h *Hub) questionByID(ctx context.Context, id string) store.Question {
	q, _ := h.store.GetQuestion(ctx, id)
	return q
}

// deliverAnswer sends an answered question's answer to the agent: its own pending prompt
// (worker elicitations), its pending ask_user call, or a note before its next turn.
func (h *Hub) deliverAnswer(ctx context.Context, q store.Question) {
	ctx = context.WithoutCancel(ctx)
	defer h.discardThread(ctx, q.ID)
	if q.Answer == nil || q.Answer.Correction {
		h.scheduleAnswerNotes(ctx, q.ChatID)
		return
	}
	switch q.Source {
	case store.QuestionSourceElicitation:
		cur, ok := h.claimDelivery(ctx, q.ID, store.DeliveryWorker)
		if !ok {
			return
		}
		cs, err := h.store.GetChatSession(ctx, cur.ChatID)
		if err != nil {
			return
		}
		wc := h.workerConnFor(cs.WorkerID)
		if wc == nil || !h.sendWorkerJSON(wc, protocol.MsgAgentChatRespond, protocol.AgentChatRespond{
			ChatID: cs.ID, ElicitationID: cur.ElicitationID, OptionID: cur.Answer.OptionID, Text: cur.Answer.Text,
		}) {
			h.logger.Warn("could not forward an answer to the worker", "question", cur.SID)
		}
		return
	}
	elID := questionElicitationID(q.ID)
	rt := h.chatRT(q.ChatID)
	rt.mu.Lock()
	ask := rt.asks[elID]
	if ask != nil {
		delete(rt.asks, elID)
	}
	rt.mu.Unlock()
	if ask != nil {
		if cur, ok := h.claimDelivery(ctx, q.ID, store.DeliveryTool); ok {
			ask.answer <- askAnswer{final: true, text: answerResultText(cur)}
		} else {
			ask.answer <- askAnswer{reason: "cancelled", text: "the question was answered elsewhere"}
		}
		h.resolveQuestionCard(ctx, q)
		return
	}
	// Nobody waits: the agent's MCP client gave up. Deliver as a note.
	h.resolveQuestionCard(ctx, q)
	h.markAbandonedAnswered(ctx, q.ID)
	h.scheduleAnswerNotes(ctx, q.ChatID)
}

func (h *Hub) markAbandonedAnswered(ctx context.Context, id string) {
	h.questions.mu.Lock()
	defer h.questions.mu.Unlock()
	if q, err := h.store.GetQuestion(ctx, id); err == nil && !q.Abandoned {
		q.Abandoned = true
		_ = h.store.UpdateQuestion(ctx, q)
	}
}

// resolveQuestionCard closes the question's inline card in its chat.
func (h *Hub) resolveQuestionCard(ctx context.Context, q store.Question) {
	if q.Source != store.QuestionSourceAskUser {
		return
	}
	h.emitChatEvent(ctx, q.ChatID, protocol.AgentChatEvent{
		Kind:        protocol.EventElicitationOK,
		Elicitation: &protocol.AgentElicitation{ID: questionElicitationID(q.ID), Kind: protocol.ElicitQuestion, Title: q.Prompt, QuestionID: q.ID},
	}, emitOpts{})
}

// scheduleAnswerNotes delivers a chat's undelivered answers now if it is idle (as a
// turn); otherwise they go with its next prompt or when its turn ends.
func (h *Hub) scheduleAnswerNotes(ctx context.Context, chatID string) {
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil || cs.QuestionID != "" || !chatIdle(cs.Status) || h.workerConnFor(cs.WorkerID) == nil {
		return
	}
	h.goBackground(func(bctx context.Context) { h.deliverAnswerNotesTurn(bctx, chatID) })
}

// deliverAnswerNotesTurn starts a turn in an idle chat carrying its undelivered answers.
func (h *Hub) deliverAnswerNotesTurn(ctx context.Context, chatID string) {
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil || !chatIdle(cs.Status) {
		return
	}
	note := h.takeAnswerNotes(ctx, chatID)
	if note == "" {
		return
	}
	dctx := withCause(ctx, store.Cause{Actor: store.ActorSystem, Source: "question", Confidence: store.ConfidenceCertain})
	if err := h.sendAgentChat(dctx, cs, chatSendRequest{SessionID: cs.ID, Content: note}); err != nil {
		h.logger.Warn("answer note delivery failed", "chat_id", chatID, "err", err)
	}
}

// takeAnswerNotes claims every undelivered answer of a chat and renders them as one
// "[Syre]" note ("" when there is none).
func (h *Hub) takeAnswerNotes(ctx context.Context, chatID string) string {
	qs, err := h.store.ListQuestions(ctx, store.QuestionFilter{ChatID: chatID, UndeliveredOnly: true, Statuses: []string{store.QuestionAnswered}, Limit: 20})
	if err != nil || len(qs) == 0 {
		return ""
	}
	sort.Slice(qs, func(i, j int) bool { return qs[i].CreatedAt.Before(qs[j].CreatedAt) })
	var lines []string
	for _, q := range qs {
		if q.Source != store.QuestionSourceAskUser {
			continue
		}
		cur, ok := h.claimDelivery(ctx, q.ID, store.DeliveryNote)
		if !ok {
			continue
		}
		lines = append(lines, answerNoteText(cur))
		h.broadcastQuestion(ctx, cur)
	}
	if len(lines) == 0 {
		return ""
	}
	return eventNotePrefix + " " + strings.Join(lines, "\n"+eventNotePrefix+" ")
}

// --- cancel / expire --------------------------------------------------------------------

// closeQuestion ends a pending question without an answer (cancelled or expired).
func (h *Hub) closeQuestion(ctx context.Context, id, status, why string) {
	ctx = context.WithoutCancel(ctx)
	h.questions.mu.Lock()
	q, err := h.store.GetQuestion(ctx, id)
	if err != nil || !q.Pending() {
		h.questions.mu.Unlock()
		return
	}
	q.Status = status
	now := time.Now().UTC()
	q.ClosedAt = &now
	if q.Delegation != nil && q.Delegation.State == store.DelegationPending {
		q.Delegation.State, q.Delegation.Note = store.DelegationFailed, why
	}
	h.stopDelegationTimer(q.ID)
	err = h.store.UpdateQuestion(ctx, q)
	h.questions.mu.Unlock()
	if err != nil {
		return
	}
	h.broadcastQuestion(ctx, q)
	h.resolveQuestionCard(ctx, q)
	h.discardThread(ctx, q.ID)
}

// endChatQuestions handles a chat's pending questions when its pending tool calls are
// ended: cancelled (the user stopped the turn, the chat was deleted) or kept open as
// abandoned (the engine restarted), with the answer delivered as a note later.
func (h *Hub) endChatQuestions(ctx context.Context, chatID string, cancel bool, why string) {
	qs, err := h.store.ListQuestions(ctx, store.QuestionFilter{ChatID: chatID, Statuses: []string{store.QuestionOpen, store.QuestionDelegated}})
	if err != nil {
		return
	}
	for _, q := range qs {
		if cancel || q.Source == store.QuestionSourceElicitation {
			status := store.QuestionCancelled
			if !cancel {
				status = store.QuestionExpired
			}
			h.closeQuestion(ctx, q.ID, status, why)
			continue
		}
		h.markAbandoned(ctx, q.ID)
	}
}

// --- worker elicitations ----------------------------------------------------------------

// questionForElicitation mirrors an agent's own question or permission prompt as a
// question (so it is in the fleet queue and shown through the same card) and returns
// its id. Replays of the same elicitation reuse the question.
func (h *Hub) questionForElicitation(ctx context.Context, cs store.ChatSession, el *protocol.AgentElicitation) string {
	if el == nil || el.ID == "" || (el.Kind != protocol.ElicitQuestion && el.Kind != protocol.ElicitPermission) {
		return ""
	}
	ctx = context.WithoutCancel(ctx)
	h.questions.mu.Lock()
	existing, _ := h.store.ListQuestions(ctx, store.QuestionFilter{ChatID: cs.ID, Statuses: []string{store.QuestionOpen, store.QuestionDelegated}, Limit: 50})
	for _, e := range existing {
		if e.ElicitationID == el.ID {
			h.questions.mu.Unlock()
			return e.ID
		}
	}
	q := store.Question{
		ChatID: cs.ID, WorkerID: cs.WorkerID, Agent: cs.Agent, AccountID: cs.AccountID,
		Source: store.QuestionSourceElicitation, ElicitationID: el.ID, Prompt: strings.TrimSpace(el.Title),
		AllowText: el.AllowText, Kind: store.QuestionKindQuestion,
	}
	if q.Prompt == "" {
		q.Prompt = "The agent needs a decision"
	}
	if el.Kind == protocol.ElicitPermission {
		q.Kind, q.Category = store.QuestionKindPermission, "permission"
		label := agentLabel(cs)
		q.Context = firstNonEmpty(el.Detail, permissionContext(el))
		q.WhyNow = label + " is paused until you decide; it continues with your answer."
	} else {
		q.Context = el.Detail
	}
	for _, o := range el.Options {
		q.Options = append(q.Options, store.QuestionOption{ID: o.ID, Label: o.Label, Kind: o.Kind, Consequence: firstNonEmpty(o.Consequence, permissionConsequence(o))})
	}
	q.Blocks = h.derivedBlocks(ctx, cs)
	q.Refs = detectRefs(q.Prompt, q.Context)
	q.GroupKey = questionGroupKey(q, cs)
	created, err := h.store.CreateQuestion(ctx, q)
	h.questions.mu.Unlock()
	if err != nil {
		h.logger.Warn("store question failed", "chat_id", cs.ID, "err", err)
		return ""
	}
	h.afterQuestionCreated(created)
	return created.ID
}

func agentLabel(cs store.ChatSession) string {
	if cs.Agent != "" {
		return capitalize(cs.Agent)
	}
	return "The agent"
}

func permissionContext(el *protocol.AgentElicitation) string {
	if el.ToolCall == nil {
		return ""
	}
	tc := el.ToolCall
	s := firstNonEmpty(tc.Title, tc.Name)
	if tc.Input != "" {
		s += "\n\n```\n" + oneLine(tc.Input, 600) + "\n```"
	}
	return s
}

// permissionConsequence describes what a permission option does.
func permissionConsequence(o protocol.AgentElicitationOption) string {
	switch k := o.Kind; {
	case k == "allow_once":
		return "Runs it this time; asks again next time."
	case k == "allow_always":
		return "Runs it and won't ask again for this kind of action in this chat."
	case strings.HasPrefix(k, "reject"):
		return "Doesn't run it; the agent tries another way or asks you."
	}
	return ""
}

// onElicitationResolved closes the question of a worker elicitation that ended without
// a Syre answer (answered in the agent, or its turn ended).
func (h *Hub) onElicitationResolved(ctx context.Context, chatID, elicitationID string) {
	qs, err := h.store.ListQuestions(ctx, store.QuestionFilter{ChatID: chatID, Statuses: []string{store.QuestionOpen, store.QuestionDelegated}, Limit: 50})
	if err != nil {
		return
	}
	for _, q := range qs {
		if q.Source == store.QuestionSourceElicitation && q.ElicitationID == elicitationID {
			h.closeQuestion(ctx, q.ID, store.QuestionExpired, "the agent no longer needs an answer")
		}
	}
}

// expireElicitationQuestions closes a chat's pending worker-elicitation questions when its
// turn ended (the agent's prompt is gone).
func (h *Hub) expireElicitationQuestions(ctx context.Context, chatID string) {
	qs, err := h.store.ListQuestions(ctx, store.QuestionFilter{ChatID: chatID, Statuses: []string{store.QuestionOpen, store.QuestionDelegated}, Limit: 50})
	if err != nil {
		return
	}
	for _, q := range qs {
		if q.Source == store.QuestionSourceElicitation {
			h.closeQuestion(ctx, q.ID, store.QuestionExpired, "the agent's turn ended")
		}
	}
}

// onChatIdle runs when a chat's turn ended: undelivered answers go to it as a turn, and
// for commander chats, delegated questions are handed over (or handed back).
func (h *Hub) onChatIdle(cs store.ChatSession) {
	if cs.QuestionID != "" || cs.Backend != store.ChatBackendAgent {
		return
	}
	h.goBackground(func(ctx context.Context) {
		h.expireElicitationQuestions(ctx, cs.ID)
		if cs.FleetTools {
			h.commanderIdle(ctx, cs)
		}
		h.deliverAnswerNotesTurn(ctx, cs.ID)
	})
}

// --- snooze -----------------------------------------------------------------------------

// snoozeQuestion moves a question down the queue until a time (it stays open).
func (h *Hub) snoozeQuestion(ctx context.Context, id string, d time.Duration) (store.Question, error) {
	ctx = context.WithoutCancel(ctx)
	h.questions.mu.Lock()
	q, err := h.store.GetQuestion(ctx, id)
	if err == nil && !q.Pending() {
		err = errQuestionClosed
	}
	if err == nil {
		if d <= 0 {
			q.SnoozedUntil = nil
		} else {
			until := time.Now().UTC().Add(d)
			q.SnoozedUntil = &until
		}
		err = h.store.UpdateQuestion(ctx, q)
	}
	h.questions.mu.Unlock()
	if err == nil {
		h.broadcastQuestion(ctx, q)
	}
	return q, err
}

// --- views and queue order --------------------------------------------------------------

// QuestionView is a question with what the UI shows around it.
type QuestionView struct {
	store.Question
	ChatSID      string                  `json:"chat_sid,omitempty"`
	ChatTitle    string                  `json:"chat_title,omitempty"`
	MachineName  string                  `json:"machine_name,omitempty"`
	AccountLabel string                  `json:"account_label,omitempty"`
	Commander    bool                    `json:"commander,omitzero"` // asked by a commander chat
	Impact       int                     `json:"impact"`
	GroupLabel   string                  `json:"group_label,omitempty"`
	Messages     []store.QuestionMessage `json:"messages"`
	// ThreadAccountLabel/ThreadMachineName say where the side conversation runs.
	ThreadAccountLabel string `json:"thread_account_label,omitempty"`
	ThreadMachineName  string `json:"thread_machine_name,omitempty"`
}

// viewContext caches lookups while building many views.
type viewContext struct {
	h        *Hub
	chats    map[string]*store.ChatSession
	workers  map[string]string
	accounts map[string]string // worker/account -> label
	tasks    []store.Task
	loaded   bool
}

func (h *Hub) newViewContext() *viewContext {
	return &viewContext{h: h, chats: map[string]*store.ChatSession{}, workers: map[string]string{}, accounts: map[string]string{}}
}

func (v *viewContext) load(ctx context.Context) {
	if v.loaded {
		return
	}
	v.loaded = true
	if ws, err := v.h.store.ListWorkers(ctx); err == nil {
		for _, w := range ws {
			v.workers[w.ID] = w.Name
		}
	}
	if recs, err := v.h.store.ListAgentAccounts(ctx, ""); err == nil {
		for _, r := range recs {
			if a, ok := decodeAccount(r); ok {
				v.accounts[r.WorkerID+"/"+a.ID] = firstNonEmpty(a.Label, a.ID)
			}
		}
	}
	v.tasks, _ = v.h.store.ListTasks(ctx, "", "", 500)
}

func (v *viewContext) chat(ctx context.Context, id string) *store.ChatSession {
	if cs, ok := v.chats[id]; ok {
		return cs
	}
	var out *store.ChatSession
	if cs, err := v.h.store.GetChatSession(ctx, id); err == nil {
		out = &cs
	}
	v.chats[id] = out
	return out
}

func (v *viewContext) accountLabel(workerID, accountID, agent string) string {
	if l := v.accounts[workerID+"/"+accountID]; l != "" {
		return l
	}
	return firstNonEmpty(accountID, agent)
}

func (h *Hub) questionView(ctx context.Context, v *viewContext, q store.Question, withMessages bool) QuestionView {
	v.load(ctx)
	out := QuestionView{Question: q, MachineName: v.workers[q.WorkerID], Messages: []store.QuestionMessage{}}
	out.AccountLabel = v.accountLabel(q.WorkerID, q.AccountID, q.Agent)
	impact := 1 + len(q.Blocks)
	if cs := v.chat(ctx, q.ChatID); cs != nil {
		out.ChatSID, out.ChatTitle, out.Commander = cs.SID, cs.Title, cs.FleetTools
		if cs.FleetTools {
			impact += 2
		}
	}
	if q.Kind == store.QuestionKindPermission {
		impact++ // the agent is frozen mid-action
	}
	out.Impact = impact
	out.GroupLabel = out.ChatTitle
	for _, r := range q.Refs {
		if r.Kind == "ticket" && "ticket:"+r.Ref == q.GroupKey {
			out.GroupLabel = r.Ref
			if r.Title != "" {
				out.GroupLabel += " " + r.Title
			}
		}
	}
	if q.Thread != nil {
		out.ThreadAccountLabel = v.accountLabel(q.Thread.WorkerID, q.Thread.AccountID, q.Thread.Agent)
		out.ThreadMachineName = v.workers[q.Thread.WorkerID]
	}
	if withMessages {
		if msgs, err := h.store.ListQuestionMessages(ctx, q.ID); err == nil && msgs != nil {
			out.Messages = msgs
		}
	}
	return out
}

// sortQueue orders pending questions by blocking impact, then age (oldest first);
// snoozed questions go last; closed ones after that, newest first.
func sortQueue(views []QuestionView, now time.Time) {
	rank := func(q QuestionView) int {
		switch {
		case !q.Pending():
			return 2
		case q.SnoozedUntil != nil && q.SnoozedUntil.After(now):
			return 1
		}
		return 0
	}
	sort.SliceStable(views, func(i, j int) bool {
		a, b := views[i], views[j]
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra < rb
		}
		if rank(a) == 2 {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		if a.Impact != b.Impact {
			return a.Impact > b.Impact
		}
		if !a.BlockedSince.Equal(b.BlockedSince) {
			return a.BlockedSince.Before(b.BlockedSince)
		}
		return a.ID < b.ID
	})
}

// broadcastQuestion pushes a question (with its side conversation) to every browser.
func (h *Hub) broadcastQuestion(ctx context.Context, q store.Question) {
	h.broadcastFleetUpdate("question", h.questionView(ctx, h.newViewContext(), q, true))
}

// questionForChatElicitation returns the question shown by a chat elicitation ("" if none).
func (h *Hub) questionForChatElicitation(ctx context.Context, chatID, elicitationID string) string {
	if id, ok := strings.CutPrefix(elicitationID, "q-"); ok {
		if q, err := h.store.GetQuestion(ctx, id); err == nil && q.ChatID == chatID {
			return q.ID
		}
	}
	qs, err := h.store.ListQuestions(ctx, store.QuestionFilter{ChatID: chatID, Limit: 50})
	if err != nil {
		return ""
	}
	for _, q := range qs {
		if q.ElicitationID == elicitationID && q.ElicitationID != "" {
			return q.ID
		}
	}
	return ""
}

// pendingQuestions returns the open or delegated questions of a chat.
func (h *Hub) pendingQuestions(ctx context.Context, chatID string) []store.Question {
	qs, _ := h.store.ListQuestions(ctx, store.QuestionFilter{ChatID: chatID, Statuses: []string{store.QuestionOpen, store.QuestionDelegated}})
	return qs
}
