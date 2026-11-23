package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/BryanSant/konsert/internal/worker"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

// --- validation -------------------------------------------------------------------------

func TestValidateAskRejectsUnderSpecifiedQuestions(t *testing.T) {
	good := askUserInput{
		Question: "Accept the shortened exit criteria on KON-2Q0CH?", Context: "KON-2Q0CH 'Questions and steering'", WhyNow: "Closing waits on it",
		Options:   []askOption{{Label: "Yes", Consequence: "Closes today", Recommended: true}, {Label: "No", Consequence: "Stays open"}},
		Rationale: "e2e covers it",
	}
	if err := validateAsk(normalizeAskInput(good)); err != nil {
		t.Fatalf("good question rejected: %v", err)
	}
	cases := []struct {
		name    string
		mutate  func(*askUserInput)
		missing []string
		says    string
	}{
		{"the user's example", func(in *askUserInput) {
			*in = askUserInput{Question: "Accept the shortened exit criteria on JK-ABC12?", Options: []askOption{{Label: "Yes (recommended)"}, {Label: "No keep the strict exit criteria"}}}
		}, []string{"context", "why_now", "consequence", "rationale"}, `why you recommend "Yes"`},
		{"no context", func(in *askUserInput) { in.Context = " " }, []string{"context"}, "ticket/PR/files with their titles"},
		{"no why_now", func(in *askUserInput) { in.WhyNow = "" }, []string{"why_now"}, "what is blocked"},
		{"missing consequence", func(in *askUserInput) { in.Options[1].Consequence = "" }, []string{"consequence"}, `option 2 ("No")`},
		{"recommendation without rationale", func(in *askUserInput) { in.Rationale = "" }, []string{"rationale"}, `why you recommend "Yes"`},
		{"unknown recommendation", func(in *askUserInput) {
			in.Options[0].Recommended = false
			in.Recommendation = "Maybe"
		}, []string{"recommendation"}, "not one of the option labels"},
		{"one option", func(in *askUserInput) { in.Options = in.Options[:1] }, []string{"options"}, "at least two"},
	}
	for _, c := range cases {
		in := good
		in.Options = append([]askOption(nil), good.Options...)
		c.mutate(&in)
		err := validateAsk(normalizeAskInput(in))
		if err == nil {
			t.Fatalf("%s: accepted", c.name)
		}
		if strings.Join(err.missing, ",") != strings.Join(c.missing, ",") || !strings.Contains(err.Error(), c.says) || !strings.Contains(err.Error(), "call ask_user again") {
			t.Errorf("%s: missing %v, error:\n%s", c.name, err.missing, err.Error())
		}
	}
	// Recommendation by number, and "(recommended)" decorations become the flag.
	n := normalizeAskInput(askUserInput{Options: []askOption{{Label: "A"}, {Label: "B (recommended)"}}, Recommendation: "1"})
	if !n.Options[0].Recommended || !n.Options[1].Recommended || n.Options[1].Label != "B" {
		t.Fatalf("normalize: %+v", n.Options)
	}
	// Free-form questions need no options.
	if err := validateAsk(normalizeAskInput(askUserInput{Question: "Which port?", Context: "Dev server", WhyNow: "Can't start it"})); err != nil {
		t.Fatalf("free-form rejected: %v", err)
	}
}

func TestParseDraft(t *testing.T) {
	q := store.Question{AllowText: true, Options: []store.QuestionOption{{ID: "option_1", Label: "Yes"}, {ID: "option_2", Label: "No, keep the strict exit criteria"}}}
	for in, want := range map[string]store.QuestionDraft{
		"It covers the flow.\nDRAFT ANSWER: Yes":                            {OptionID: "option_1", Label: "Yes"},
		"Because.\n**DRAFT ANSWER:** \"No, keep the strict exit criteria\"": {OptionID: "option_2", Label: "No, keep the strict exit criteria"},
		"x\nDRAFT ANSWER: option 2":                                         {OptionID: "option_2", Label: "No, keep the strict exit criteria"},
		"x\nDRAFT ANSWER: Yes (recommended)":                                {OptionID: "option_1", Label: "Yes"},
		"x\nDRAFT ANSWER: Yes — but do the phone run next week":             {OptionID: "option_1", Label: "Yes", Text: "but do the phone run next week"},
		"x\nDRAFT ANSWER: Schedule the phone run for Friday, then close":    {Text: "Schedule the phone run for Friday, then close"},
	} {
		display, d := parseDraft(in, q)
		if d == nil || *d != want || strings.Contains(display, "DRAFT ANSWER") {
			t.Errorf("parseDraft(%q) = %q, %+v; want %+v", in, display, d, want)
		}
	}
	if _, d := parseDraft("No draft here.", q); d != nil {
		t.Errorf("unexpected draft %+v", d)
	}
}

// --- queue ------------------------------------------------------------------------------

func TestQuestionQueueOrderAndDedupe(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	orch := store.ChatSession{Title: "Orchestrator", Backend: store.ChatBackendAgent, FleetTools: true, Agent: "claude"}
	worker := store.ChatSession{Title: "Fix e2e", Backend: store.ChatBackendAgent, Agent: "codex"}
	for _, c := range []*store.ChatSession{&orch, &worker} {
		if err := cs.st.CreateChatSession(ctx, *c); err != nil {
			t.Fatal(err)
		}
		all, _ := cs.st.ListChatSessions(ctx, 10)
		for _, x := range all {
			if x.Title == c.Title {
				*c = x
			}
		}
	}
	mk := func(chat store.ChatSession, prompt string, age time.Duration, kind string) store.Question {
		q, _, err := cs.h.createOrReuseQuestion(ctx, chat, store.Question{
			ChatID: chat.ID, Kind: kind, Source: store.QuestionSourceAskUser, Prompt: prompt,
			Options: []store.QuestionOption{{ID: "option_1", Label: "Yes"}, {ID: "option_2", Label: "No"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		q.BlockedSince = time.Now().Add(-age)
		_ = cs.st.UpdateQuestion(ctx, q)
		return q
	}
	old := mk(worker, "Old worker question?", 30*time.Minute, store.QuestionKindQuestion)
	young := mk(worker, "Young worker question?", time.Minute, store.QuestionKindQuestion)
	fromOrch := mk(orch, "Orchestrator question?", 2*time.Minute, store.QuestionKindQuestion)
	perm := mk(worker, "Allow rm?", 10*time.Second, store.QuestionKindPermission)

	// Re-asking the same question (any spacing/case) maps to the same card.
	again, reused, err := cs.h.createOrReuseQuestion(ctx, worker, store.Question{
		ChatID: worker.ID, Kind: store.QuestionKindQuestion, Source: store.QuestionSourceAskUser, Prompt: "  young WORKER question? ",
		Options: []store.QuestionOption{{ID: "option_1", Label: "yes"}, {ID: "option_2", Label: "no"}},
	})
	if err != nil || !reused || again.ID != young.ID || again.AskCount != 2 {
		t.Fatalf("re-ask: reused=%v id=%s (want %s) count=%d err=%v", reused, again.SID, young.SID, again.AskCount, err)
	}

	if _, err := cs.h.snoozeQuestion(ctx, old.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	views, err := cs.h.questionQueue(ctx, false, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range views {
		got = append(got, v.Prompt)
	}
	// Orchestrator first (impact 1 + 1 derived block + 2), then the permission prompt
	// (agent frozen), then by age; snoozed last.
	want := []string{fromOrch.Prompt, perm.Prompt, young.Prompt, old.Prompt}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("queue order:\n got %v\nwant %v", got, want)
	}
	if views[0].Impact <= views[2].Impact || !views[0].Orchestrator || views[0].GroupLabel != "Orchestrator" {
		t.Fatalf("views: %+v", views[0])
	}

	// The REST queue answers the same, and sids work in paths.
	code, raw := cs.api(t, http.MethodGet, "/api/questions", nil)
	if code != 200 || !strings.Contains(string(raw), fromOrch.SID) {
		t.Fatalf("GET /api/questions: %d %s", code, raw)
	}
	if code, _ := cs.api(t, http.MethodGet, "/api/questions/"+young.SID, nil); code != 200 {
		t.Fatalf("GET by sid: %d", code)
	}
	// Survives a "restart": a fresh hub on the same store sees the same queue.
	h2 := NewHub(cs.st, nil)
	if v2, _ := h2.questionQueue(ctx, false, ""); len(v2) != 4 {
		t.Fatalf("after restart: %d questions", len(v2))
	}
}

// --- references -------------------------------------------------------------------------

func TestDetectRefsAndEnrichWithFakeKa(t *testing.T) {
	refs := detectRefs("Accept the shortened exit criteria on JK-ABC12? See PR #123, internal/controller/questions.go:42 and t-7qk3m.", "c-w47xk")
	var kinds []string
	for _, r := range refs {
		kinds = append(kinds, r.Kind+":"+r.Ref)
	}
	want := "ticket:JK-ABC12 pr:#123 task:t-7qk3m chat:c-w47xk file:internal/controller/questions.go"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("refs = %v", kinds)
	}

	cs := newChatServer(t)
	ctx := context.Background()
	chat := store.ChatSession{Title: "c", Backend: store.ChatBackendAgent}
	_ = cs.st.CreateChatSession(ctx, chat)
	all, _ := cs.st.ListChatSessions(ctx, 1)
	chat = all[0]

	// A fake ka CLI on disk (the real resolution path), counting its calls.
	calls := filepath.Join(t.TempDir(), "calls")
	if runtime.GOOS != "windows" {
		ka := filepath.Join(t.TempDir(), "ka")
		script := "#!/bin/sh\necho \"$@\" >> " + calls + "\n" +
			`if [ "$2" = "JK-ABC12" ]; then printf '%s\n' '{"id":"JK-ABC12","title":"Exit criteria for the beta","status":"in_progress","body":"\n## Problem\n\nThe beta exits when 3 users finish onboarding.\n"}'; else exit 1; fi` + "\n"
		if err := os.WriteFile(ka, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		cs.h.KaPath = ka
	} else {
		cs.h.kaShowFn = func(_ context.Context, id string) ([]byte, error) {
			if id != "JK-ABC12" {
				return nil, errors.New("not found")
			}
			return []byte(`{"id":"JK-ABC12","title":"Exit criteria for the beta","status":"in_progress","body":"The beta exits when 3 users finish onboarding."}`), nil
		}
	}
	q, _, err := cs.h.createOrReuseQuestion(ctx, chat, store.Question{ChatID: chat.ID, Kind: store.QuestionKindQuestion, Source: store.QuestionSourceAskUser,
		Prompt: "Accept the shortened exit criteria on JK-ABC12?", Context: "Also mentions ZZ-00000."})
	if err != nil {
		t.Fatal(err)
	}
	if q.GroupKey != "ticket:JK-ABC12" {
		t.Fatalf("group key %q", q.GroupKey)
	}
	cs.h.enrichQuestion(ctx, q.ID)
	q, _ = cs.st.GetQuestion(ctx, q.ID)
	r := q.Refs[0]
	if !r.Resolved || r.Title != "Exit criteria for the beta" || r.Status != "in_progress" || r.Excerpt != "The beta exits when 3 users finish onboarding." {
		t.Fatalf("ticket card: %+v", r)
	}
	if q.Refs[1].Resolved {
		t.Fatalf("unknown ticket resolved: %+v", q.Refs[1])
	}
	// Cached: a second enrichment does not run ka again.
	cs.h.enrichQuestion(ctx, q.ID)
	if runtime.GOOS != "windows" {
		b, _ := os.ReadFile(calls)
		if n := strings.Count(string(b), "JK-ABC12"); n != 1 {
			t.Fatalf("ka ran %d times for JK-ABC12 (want 1, cached)", n)
		}
	}
	v := cs.h.questionView(ctx, cs.h.newViewContext(), q, false)
	if v.GroupLabel != "JK-ABC12 Exit criteria for the beta" {
		t.Fatalf("group label %q", v.GroupLabel)
	}
}

// --- end to end with a real konsertd and the mock agent ---------------------------------

// mockFleet runs a real konsertd with the mock engine against a test hub.
func mockFleet(t *testing.T) (*chatServer, string) {
	t.Helper()
	cs := newChatServer(t)
	cs.h.kaShowFn = func(_ context.Context, id string) ([]byte, error) {
		if id == "KON-2Q0CH" {
			return []byte(`{"id":"KON-2Q0CH","title":"Questions and steering","status":"in_progress","body":"Clear, context-rich questions."}`), nil
		}
		return nil, errors.New("not found")
	}
	ctx, cancel := context.WithCancel(context.Background())
	wCfg := config.DefaultWorkerConfig()
	wCfg.ControllerURL = cs.wsURL("/ws/worker")
	wCfg.WorkerID = "worker-q"
	wCfg.Name = "q-box"
	wCfg.HostGroup = "q"
	wCfg.EnableMockDriver = true
	daemon := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = daemon.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	var acct string
	waitFor(t, 10*time.Second, "mock account", func() bool {
		views, _ := cs.h.agentAccountViews(ctx, "worker-q")
		for _, v := range views {
			if v.Agent == "mock" && v.ChatSupported {
				acct = v.ID
				return true
			}
		}
		return false
	})
	return cs, acct
}

func (cs *chatServer) send(t *testing.T, chat store.ChatSession, text string) {
	t.Helper()
	if err := cs.h.sendAgentChat(withAuditActor(context.Background(), "tester", auditSourceWS), mustSession(t, cs.st, chat.ID), chatSendRequest{SessionID: chat.ID, Content: text}); err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
}

// waitReply waits for an assistant message in the chat containing all of subs.
func (cs *chatServer) waitReply(t *testing.T, chatID string, subs ...string) string {
	t.Helper()
	var found string
	waitFor(t, 20*time.Second, "reply containing "+strings.Join(subs, " & "), func() bool {
		evs, _ := cs.st.ListChatEvents(context.Background(), chatID, 0, 5000)
		for _, e := range evs {
			if e.Kind != protocol.EventMessage {
				continue
			}
			var ev protocol.AgentChatEvent
			_ = json.Unmarshal([]byte(e.JSON), &ev)
			ok := true
			for _, s := range subs {
				ok = ok && strings.Contains(ev.Text, s)
			}
			if ok {
				found = ev.Text
				return true
			}
		}
		return false
	})
	return found
}

func (cs *chatServer) waitQuestion(t *testing.T, chatID string, what string, pred func(store.Question) bool) store.Question {
	t.Helper()
	var out store.Question
	waitFor(t, 20*time.Second, what, func() bool {
		qs, _ := cs.st.ListQuestions(context.Background(), store.QuestionFilter{ChatID: chatID})
		for _, q := range qs {
			if pred(q) {
				out = q
				return true
			}
		}
		return false
	})
	return out
}

func (cs *chatServer) postQuestion(t *testing.T, q store.Question, action string, body any) QuestionView {
	t.Helper()
	code, raw := cs.api(t, http.MethodPost, "/api/questions/"+q.SID+"/"+action, body)
	if code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", action, q.SID, code, raw)
	}
	var v QuestionView
	_ = json.Unmarshal(raw, &v)
	return v
}

func isPending(q store.Question) bool { return q.Pending() }

func TestQuestionAnsweredInTurnAfterRejection(t *testing.T) {
	cs, acct := mockFleet(t)
	chat := cs.createChat(t, map[string]any{"worker_id": "worker-q", "agent": "mock", "account_id": acct})
	cs.send(t, chat, "vague question")
	q := cs.waitQuestion(t, chat.ID, "open question", isPending)
	if q.Rejections != 1 || q.Context == "" || q.WhyNow == "" || q.Recommendation != "option_1" || q.Rationale == "" || q.Options[1].Consequence == "" {
		t.Fatalf("question: %+v", q)
	}
	if rs, _ := cs.st.ListQuestionRejects(context.Background(), time.Now().Add(-time.Hour)); len(rs) != 1 || rs[0].Agent != "mock" || !strings.Contains(rs[0].Missing, "context") {
		t.Fatalf("rejects: %+v", rs)
	}
	// Enrichment resolves the ticket asynchronously.
	q = cs.waitQuestion(t, chat.ID, "ticket card", func(q store.Question) bool {
		return len(q.Refs) > 0 && q.Refs[0].Title == "Questions and steering"
	})
	if s := mustSession(t, cs.st, chat.ID); s.Status != protocol.ChatStatusNeedsYou {
		t.Fatalf("chat status while asking: %s", s.Status)
	}
	v := cs.postQuestion(t, q, "answer", map[string]any{"option_id": "option_2"})
	if v.Status != store.QuestionAnswered {
		t.Fatalf("after answer: %+v", v.Question)
	}
	cs.waitReply(t, chat.ID, "Rejected first", `The user chose: "No, keep the strict exit criteria".`)
	q, _ = cs.st.GetQuestion(context.Background(), q.ID)
	if q.Answer.Delivery != store.DeliveryTool || q.Answer.By.Actor != store.ActorUser {
		t.Fatalf("answer: %+v", q.Answer)
	}
	// Answering again is refused.
	if code, _ := cs.api(t, http.MethodPost, "/api/questions/"+q.SID+"/answer", map[string]any{"option_id": "option_1"}); code != http.StatusConflict {
		t.Fatalf("second answer: %d", code)
	}
	m, _ := cs.h.questionMetrics(context.Background(), 7)
	if m.Asked != 1 || m.Answered != 1 || m.AnsweredWithoutClarify != 1 || m.Rejected != 1 || len(m.Agents) != 1 || m.Agents[0].UnclearPct != 100 {
		t.Fatalf("metrics: %+v", m)
	}
}

func TestQuestionReaskAndAbandonedAnswerDeliveredAsNote(t *testing.T) {
	cs, acct := mockFleet(t)
	chat := cs.createChat(t, map[string]any{"worker_id": "worker-q", "agent": "mock", "account_id": acct})

	// The agent's tool call ends before the user answers (like an MCP client timeout).
	cs.h.AskUserTimeout = 300 * time.Millisecond
	cs.send(t, chat, "question")
	cs.waitReply(t, chat.ID, "Ask result: The question is still open")
	q := cs.waitQuestion(t, chat.ID, "abandoned question", func(q store.Question) bool { return q.Pending() && q.Abandoned })
	waitFor(t, 5*time.Second, "chat idle", func() bool { return chatIdle(mustSession(t, cs.st, chat.ID).Status) })

	// The agent asks the same question again: same card, now answered in-turn.
	cs.h.AskUserTimeout = 20 * time.Second
	cs.send(t, chat, "question")
	again := cs.waitQuestion(t, chat.ID, "re-ask", func(x store.Question) bool { return x.ID == q.ID && x.AskCount == 2 && !x.Abandoned })
	if !again.BlockedSince.Equal(q.BlockedSince) {
		t.Fatalf("re-ask reset blocked_since: %v vs %v", again.BlockedSince, q.BlockedSince)
	}
	time.Sleep(200 * time.Millisecond)
	cs.postQuestion(t, again, "answer", map[string]any{"text": "Do the phone run Friday"})
	cs.waitReply(t, chat.ID, "Ask result: The user answered: Do the phone run Friday")

	// A new question abandoned, answered later: delivered as a [Konsert] note turn.
	waitFor(t, 5*time.Second, "chat idle", func() bool { return chatIdle(mustSession(t, cs.st, chat.ID).Status) })
	cs.h.AskUserTimeout = 300 * time.Millisecond
	cs.send(t, chat, "question")
	q2 := cs.waitQuestion(t, chat.ID, "second abandoned question", func(x store.Question) bool { return x.ID != q.ID && x.Pending() && x.Abandoned })
	waitFor(t, 5*time.Second, "chat idle", func() bool { return chatIdle(mustSession(t, cs.st, chat.ID).Status) })
	cs.postQuestion(t, q2, "answer", map[string]any{"option_id": "option_1"})
	cs.waitReply(t, chat.ID, "[Konsert] Answer to your earlier question "+q2.SID, `The user chose: "Yes".`)
	q2, _ = cs.st.GetQuestion(context.Background(), q2.ID)
	if q2.Answer.Delivery != store.DeliveryNote || q2.Answer.DeliveredAt == nil {
		t.Fatalf("note delivery: %+v", q2.Answer)
	}
}

func TestSideConversationForkThenAnswerWithSummary(t *testing.T) {
	cs, acct := mockFleet(t)
	chat := cs.createChat(t, map[string]any{"worker_id": "worker-q", "agent": "mock", "account_id": acct})
	cs.send(t, chat, "question")
	q := cs.waitQuestion(t, chat.ID, "open question", isPending)
	asker := mustSession(t, cs.st, chat.ID)

	cs.postQuestion(t, q, "ask", map[string]any{"text": "What is KON-2Q0CH and why do you recommend Yes?"})
	q = cs.waitQuestion(t, chat.ID, "fork answered", func(x store.Question) bool {
		return x.Thread != nil && x.Thread.Status == "idle" && x.Thread.Draft != nil
	})
	if q.Thread.Answerer != store.AnswererFork || q.Thread.Draft.OptionID != "option_1" || q.Thread.WorkerID != "worker-q" || q.Thread.AccountID != acct {
		t.Fatalf("thread: %+v", q.Thread)
	}
	fork, err := cs.st.GetChatSession(context.Background(), q.Thread.ChatID)
	if err != nil || fork.ForkOf != chat.ID || fork.QuestionID != q.ID || fork.ApprovalMode != "plan" {
		t.Fatalf("fork chat: %+v %v", fork, err)
	}
	msgs, _ := cs.st.ListQuestionMessages(context.Background(), q.ID)
	if len(msgs) != 2 || msgs[1].Role != "answerer" || !strings.Contains(msgs[1].Text, "read-only fork of "+asker.AgentSessionID) ||
		!strings.Contains(msgs[1].Text, `the original was asked: "question"`) || strings.Contains(msgs[1].Text, "DRAFT ANSWER") {
		t.Fatalf("thread messages: %+v", msgs)
	}
	// The hidden chat is not listed; the original is still blocked.
	if _, raw := cs.api(t, http.MethodGet, "/api/chats", nil); strings.Contains(string(raw), fork.ID) {
		t.Fatal("side-conversation chat listed")
	}
	if s := mustSession(t, cs.st, chat.ID); s.Status != protocol.ChatStatusNeedsYou {
		t.Fatalf("original disturbed: %s", s.Status)
	}
	// A follow-up keeps the same fork.
	cs.postQuestion(t, q, "ask", map[string]any{"text": "And what if the phone run finds a bug?"})
	waitFor(t, 10*time.Second, "follow-up answered", func() bool {
		m, _ := cs.st.ListQuestionMessages(context.Background(), q.ID)
		return len(m) == 4
	})

	// Use the draft: the answer reaches the original with the fork's summary.
	cs.postQuestion(t, q, "answer", map[string]any{"option_id": q.Thread.Draft.OptionID})
	reply := cs.waitReply(t, chat.ID, `The user chose: "Yes".`, "Side conversation: The user asked what KON-2Q0CH is")
	if !strings.Contains(reply, `chose "Yes"`) {
		t.Fatalf("summary: %s", reply)
	}
	q = cs.waitQuestion(t, chat.ID, "thread discarded", func(x store.Question) bool { return x.ID == q.ID && x.Thread != nil && x.Thread.Discarded })
	if _, err := cs.st.GetChatSession(context.Background(), fork.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fork chat not deleted: %v", err)
	}
	if !strings.HasPrefix(q.Answer.Summary, "The user asked") || q.Answer.Preparing {
		t.Fatalf("answer: %+v", q.Answer)
	}
	m, _ := cs.h.questionMetrics(context.Background(), 7)
	if m.AnsweredWithClarify != 1 || m.Agents[0].Clarified != 1 {
		t.Fatalf("metrics: %+v", m)
	}
}

func TestSideConversationFallsBackToOrchestrator(t *testing.T) {
	cs, acct := mockFleet(t)
	chat := cs.createChat(t, map[string]any{"worker_id": "worker-q", "agent": "mock", "account_id": acct})
	cs.send(t, chat, "please remember the deploy freeze, then question")
	q := cs.waitQuestion(t, chat.ID, "open question", isPending)
	// This account can't fork (as for cursor or copilot).
	recs, _ := cs.st.ListAgentAccounts(context.Background(), "worker-q")
	for _, r := range recs {
		if a, ok := decodeAccount(r); ok && a.ID == acct {
			a.Fork = false
			b, _ := json.Marshal(a, protocol.JSONOptions)
			r.JSON = string(b)
			_ = cs.st.UpsertAgentAccount(context.Background(), r)
		}
	}
	cs.postQuestion(t, q, "ask", map[string]any{"text": "Why now?"})
	q = cs.waitQuestion(t, chat.ID, "orchestrator answered", func(x store.Question) bool { return x.Thread != nil && x.Thread.Status == "idle" })
	if q.Thread.Answerer != store.AnswererOrchestrator || !strings.Contains(q.Thread.Note, "can't fork") {
		t.Fatalf("thread: %+v", q.Thread)
	}
	msgs, _ := cs.st.ListQuestionMessages(context.Background(), q.ID)
	if !strings.Contains(msgs[len(msgs)-1].Text, "the orchestrator, answering from a context pack") {
		t.Fatalf("answer: %+v", msgs)
	}
	// The context pack carried the asking session's transcript.
	thread := mustSession(t, cs.st, q.Thread.ChatID)
	evs, _ := cs.st.ListChatEvents(context.Background(), thread.ID, 0, 100)
	var prompt string
	for _, e := range evs {
		if e.Kind == eventUserMessage {
			var ev protocol.AgentChatEvent
			_ = json.Unmarshal([]byte(e.JSON), &ev)
			prompt = ev.Text
		}
	}
	if !strings.Contains(prompt, "Recent transcript") || !strings.Contains(prompt, "deploy freeze") || thread.ForkOf != "" {
		t.Fatalf("context pack prompt: %s", prompt)
	}
	// Cancelling the turn cancels the question and discards the side conversation.
	if err := cs.h.controlChat(withAuditActor(context.Background(), "tester", auditSourceWS), chatControlRequest{SessionID: chat.ID, Cancel: true}); err != nil {
		t.Fatal(err)
	}
	q = cs.waitQuestion(t, chat.ID, "cancelled", func(x store.Question) bool { return x.ID == q.ID && x.Status == store.QuestionCancelled })
	waitFor(t, 5*time.Second, "thread discarded", func() bool {
		_, err := cs.st.GetChatSession(context.Background(), thread.ID)
		return errors.Is(err, store.ErrNotFound)
	})
}

func TestDelegationReceiptAndOverride(t *testing.T) {
	cs, acct := mockFleet(t)
	if code, raw := cs.api(t, http.MethodPut, "/api/questions/settings", map[string]any{"auto_delegate": []string{}, "delegation_grace_seconds": 1}); code != 200 {
		t.Fatalf("settings: %d %s", code, raw)
	}
	orch := cs.createChat(t, map[string]any{"worker_id": "worker-q", "agent": "mock", "account_id": acct, "fleet_tools": true})
	chat := cs.createChat(t, map[string]any{"worker_id": "worker-q", "agent": "mock", "account_id": acct})
	cs.send(t, chat, "question")
	q := cs.waitQuestion(t, chat.ID, "open question", isPending)

	v := cs.postQuestion(t, q, "delegate", nil)
	if v.Status != store.QuestionDelegated || v.Delegation == nil || v.Delegation.ChatID != orch.ID {
		t.Fatalf("delegated: %+v", v.Question)
	}
	q = cs.waitQuestion(t, chat.ID, "decided", func(x store.Question) bool {
		return x.Delegation != nil && x.Delegation.State == store.DelegationDecided
	})
	if q.Status != store.QuestionDelegated || q.Delegation.OptionID != "option_1" || !strings.HasPrefix(q.Delegation.Rationale, "Mock orchestrator") || q.Delegation.DeliverAt == nil {
		t.Fatalf("receipt: %+v", q.Delegation)
	}
	// After the grace period the decision reaches the agent's pending call.
	cs.waitReply(t, chat.ID, "The orchestrator answered on the user's behalf", `"Yes"`, "Mock orchestrator")
	q = cs.waitQuestion(t, chat.ID, "delivered", func(x store.Question) bool { return x.Status == store.QuestionAnswered && x.Answer.DeliveredAt != nil })
	if q.Answer.By.Actor != store.ActorOrchestrator || q.Delegation.State != store.DelegationDelivered {
		t.Fatalf("answer: %+v delegation %+v", q.Answer, q.Delegation)
	}
	// The user overrides afterwards: a correction note.
	waitFor(t, 5*time.Second, "chat idle", func() bool { return chatIdle(mustSession(t, cs.st, chat.ID).Status) })
	cs.postQuestion(t, q, "answer", map[string]any{"option_id": "option_2"})
	cs.waitReply(t, chat.ID, "[Konsert] Correction to your question "+q.SID, `The orchestrator had answered "Yes"`, `"No, keep the strict exit criteria"`)
	q, _ = cs.st.GetQuestion(context.Background(), q.ID)
	if q.Delegation.State != store.DelegationOverridden || !q.Answer.Correction || q.Answer.By.Actor != store.ActorUser {
		t.Fatalf("override: %+v %+v", q.Delegation, q.Answer)
	}

	// Auto-delegate rules (default off) match the question's category.
	if cs.h.autoDelegates(context.Background(), store.Question{Category: "permission"}) {
		t.Fatal("auto-delegation must be off by default")
	}
	_, _ = cs.h.saveQuestionSettings(context.Background(), QuestionSettings{AutoDelegate: []string{"Permission"}, DelegationGraceSeconds: 5})
	if !cs.h.autoDelegates(context.Background(), store.Question{Category: "permission"}) {
		t.Fatal("rule not applied")
	}
}
