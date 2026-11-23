package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

// Open requests (PRD §5.8a): an agent asks the user to *do* something on a machine
// ("restart nginx; I don't have sudo") with request_user_action. The request is tracked,
// shown in the UI and told to the orchestrator; a matching user action (a terminal
// command on that machine and resource) resolves it instead of placing a hold, and the
// user can also mark it done or declined. The requesting agent's tool call returns the
// resolution, or — if it gave up waiting — it is told before its next turn.

// requestMatchWindow is how old an open request without a resource may be for a related
// command on its machine to resolve it.
const requestMatchWindow = 6 * time.Hour

// Request option ids of the chat elicitation (and resolve statuses).
const (
	requestOptionDone     = "done"
	requestOptionDeclined = "declined"
)

// AuditRequestResolve is the audit action for resolving a request in the UI.
const AuditRequestResolve = "request.resolve"

type requestUserActionInput struct {
	Machine     string `json:"machine,omitempty" jsonschema:"Machine where the user should act (id, sid m-… or name); default: the machine this chat runs on"`
	Resource    string `json:"resource,omitempty" jsonschema:"What to act on, e.g. 'service nginx', 'port 80', 'container web' or just 'nginx'"`
	Description string `json:"description" jsonschema:"What you need the user to do and why, e.g. 'Restart nginx so it picks up the new config (I don't have sudo)'"`
}

var portRe = regexp.MustCompile(`(?i)^(?:port\s+|:)?(\d{1,5})(?:/(?:tcp|udp))?$`)

// parseResource turns "service nginx", "port 80", ":8080", "container web" or "nginx"
// into a resource ("" kind matches any kind).
func parseResource(s string) *store.Resource {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if m := portRe.FindStringSubmatch(s); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && n < 65536 {
			return &store.Resource{Kind: "port", Port: n}
		}
	}
	kind, name, ok := strings.Cut(s, " ")
	if !ok {
		kind, name, ok = strings.Cut(s, ":")
	}
	if ok {
		switch k := strings.ToLower(strings.TrimSpace(kind)); k {
		case "service", "process", "container", "daemon", "unit", "path":
			if k == "unit" || k == "daemon" {
				k = "service"
			}
			return &store.Resource{Kind: k, Name: strings.TrimSuffix(strings.TrimSpace(name), ".service")}
		}
	}
	return &store.Resource{Name: strings.TrimSuffix(s, ".service")}
}

// requestUserAction creates an open request for chat chatID and waits (like ask_user)
// until it is resolved, declined, or the wait times out. It returns text for the agent.
func (h *Hub) requestUserAction(ctx context.Context, chatID string, in requestUserActionInput) (string, error) {
	desc := strings.TrimSpace(in.Description)
	if desc == "" {
		return "", errors.New("description is required")
	}
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil {
		return "", errors.New("chat not found")
	}
	workerID := cs.WorkerID
	if m := strings.TrimSpace(in.Machine); m != "" {
		workerID = h.resolveMachine(ctx, m)
	}
	if _, err := h.store.GetWorker(ctx, workerID); err != nil {
		return "", fmt.Errorf("unknown machine %q", in.Machine)
	}
	req, err := h.store.CreateUserRequest(context.WithoutCancel(ctx), store.UserRequest{
		ChatID: chatID, WorkerID: workerID, Resource: parseResource(in.Resource), Description: desc,
	})
	if err != nil {
		return "", err
	}
	cause := causeFrom(ctx)
	cause.RequestID = req.ID
	where := h.workerLabel(ctx, workerID)
	h.broadcastFleetUpdate("request", req)
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindRequest, Action: "created", Cause: cause, WorkerID: workerID, ChatID: chatID,
		Summary:  fmt.Sprintf("Chat %s (%s) asked the user to act on %s: %s (request %s)", cs.SID, cs.Title, where, desc, req.SID),
		MetaJSON: auditMeta(map[string]any{"request_sid": req.SID, "resource": req.Resource}),
	})

	// The request shows as a question in the chat too, answered by the request's
	// resolution from wherever it comes (this card, the requests list, a terminal command).
	askID := "req-" + req.ID
	ask := &pendingAsk{answer: make(chan askAnswer, 1)}
	rt := h.chatRT(chatID)
	rt.mu.Lock()
	rt.asks[askID] = ask
	rt.mu.Unlock()
	title := fmt.Sprintf("Please do this on %s: %s", where, desc)
	if req.Resource != nil {
		title += " (" + strings.TrimSpace(describeResource(req.Resource)) + ")"
	}
	opts := []protocol.AgentElicitationOption{
		{ID: requestOptionDone, Label: "Done", Kind: "choice"},
		{ID: requestOptionDeclined, Label: "I won't do it", Kind: "choice"},
	}
	h.emitChatEvent(ctx, chatID, protocol.AgentChatEvent{Kind: protocol.EventElicitation, Elicitation: &protocol.AgentElicitation{
		ID: askID, Kind: protocol.ElicitQuestion, Title: title, Options: opts, AllowText: true,
		Detail: "Konsert resolves this automatically when you do it in a Konsert terminal on " + where + ".",
	}}, emitOpts{})

	timeout := h.AskUserTimeout
	if timeout <= 0 {
		timeout = defaultAskUserTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var ans askAnswer
	select {
	case ans = <-ask.answer:
	case <-timer.C:
		ans = askAnswer{reason: "timeout"}
	case <-ctx.Done():
		ans = askAnswer{reason: "cancelled"}
	case <-h.ctx.Done():
		ans = askAnswer{reason: "shutdown"}
	}
	rt.mu.Lock()
	delete(rt.asks, askID)
	rt.mu.Unlock()
	h.emitChatEvent(ctx, chatID, protocol.AgentChatEvent{Kind: protocol.EventElicitationOK,
		Elicitation: &protocol.AgentElicitation{ID: askID, Kind: protocol.ElicitQuestion, Title: title}}, emitOpts{})

	if ans.reason == "" {
		// Answered on the chat card: resolve the request unless another path already did.
		status := store.RequestDone
		if ans.optionID == requestOptionDeclined {
			status = store.RequestDeclined
		}
		if cur, err := h.store.GetUserRequest(ctx, req.ID); err == nil && cur.Status == store.RequestOpen {
			_, _ = h.resolveRequest(withCause(ctx, store.Cause{Actor: store.ActorUser, Source: store.SourceUI, Confidence: store.ConfidenceCertain, RequestID: req.ID}),
				req.ID, status, strings.TrimSpace(ans.text), "")
		}
	}
	cur, err := h.store.GetUserRequest(ctx, req.ID)
	if err != nil {
		return "", err
	}
	return requestOutcomeText(cur, ans.reason), nil
}

// requestOutcomeText tells the agent how its request ended.
func requestOutcomeText(r store.UserRequest, waitEnd string) string {
	switch r.Status {
	case store.RequestDone:
		msg := "The user did it"
		if r.Resolution != "" {
			msg += ": " + r.Resolution
		}
		return msg + ". Continue with your task; there is no need to investigate the change."
	case store.RequestDeclined:
		msg := "The user declined the request"
		if r.Resolution != "" {
			msg += ": " + r.Resolution
		}
		return msg + ". Don't do it another way; ask the user (ask_user) how to proceed."
	case store.RequestCanceled:
		return "The request was canceled."
	}
	switch waitEnd {
	case "timeout":
		return fmt.Sprintf("The request is still open (%s): the user hasn't done it yet. Konsert will tell you when it is resolved (a note before your next turn); you can check with recent_events. Don't do it yourself another way.", r.SID)
	case "shutdown":
		return fmt.Sprintf("The Konsert hub is shutting down; request %s stays open.", r.SID)
	}
	return fmt.Sprintf("Stopped waiting; request %s stays open and you'll be told when it is resolved.", r.SID)
}

// resolveRequest closes an open request with the cause in ctx, records it in the
// timeline (which delivers it to the requesting chat and the orchestrator) and answers
// the requesting chat's pending tool call.
func (h *Hub) resolveRequest(ctx context.Context, id, status, resolution, commandID string) (store.UserRequest, error) {
	if status != store.RequestDone && status != store.RequestDeclined && status != store.RequestCanceled {
		return store.UserRequest{}, fmt.Errorf("status must be done or declined")
	}
	req, err := h.store.GetUserRequest(ctx, id)
	if err != nil {
		return req, err
	}
	if req.Status != store.RequestOpen {
		return req, nil
	}
	cause := causeFrom(ctx)
	cause.RequestID = req.ID
	if commandID != "" {
		cause.CommandID = commandID
	}
	by := describeActor(cause)
	if isUserCause(cause) && cause.ActorID != "" {
		by = cause.ActorID
	}
	ok, err := h.store.ResolveUserRequest(context.WithoutCancel(ctx), req.ID, status, by, resolution, commandID, time.Now().UTC())
	if err != nil {
		return req, err
	}
	if cur, err := h.store.GetUserRequest(ctx, req.ID); err == nil {
		req = cur
	}
	if !ok {
		return req, nil
	}
	h.broadcastFleetUpdate("request", req)
	verb := map[string]string{store.RequestDone: "done", store.RequestDeclined: "declined", store.RequestCanceled: "canceled"}[status]
	msg := fmt.Sprintf("Request %s (%s) marked %s by %s", req.SID, req.Description, verb, describeActor(cause))
	if resolution != "" {
		msg += ": " + resolution
	}
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind: store.EventKindRequest, Action: status, Cause: cause, WorkerID: req.WorkerID, ChatID: req.ChatID,
		Summary: msg, MetaJSON: auditMeta(map[string]any{"request_sid": req.SID}),
	})
	// Answer the requesting chat's waiting tool call, if it is still waiting.
	if req.ChatID != "" {
		rt := h.chatRT(req.ChatID)
		rt.mu.Lock()
		ask := rt.asks["req-"+req.ID]
		delete(rt.asks, "req-"+req.ID)
		rt.mu.Unlock()
		if ask != nil {
			ask.answer <- askAnswer{optionID: status, text: resolution}
		}
	}
	return req, nil
}

// matchOpenRequest finds the open request on workerID that a user command answers: same
// resource, or (for requests without a resource) a recent one whose description shares
// a significant word with the command.
func (h *Hub) matchOpenRequest(ctx context.Context, workerID string, eff commandEffect, command string, at time.Time) (store.UserRequest, bool) {
	reqs, err := h.store.ListUserRequests(ctx, store.UserRequestFilter{Status: store.RequestOpen, WorkerID: workerID})
	if err != nil {
		return store.UserRequest{}, false
	}
	// Oldest first: the request waiting longest is answered first.
	for i := len(reqs) - 1; i >= 0; i-- {
		r := reqs[i]
		if r.Resource != nil && eff.resource != nil {
			if sameResource(r.Resource, eff.resource) {
				return r, true
			}
			continue
		}
		if r.Resource != nil && textMentionsResource(command, r.Resource) {
			return r, true
		}
		if r.Resource == nil && at.Sub(r.CreatedAt) <= requestMatchWindow && sharesSignificantWord(r.Description, command) {
			return r, true
		}
	}
	return store.UserRequest{}, false
}

var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "please": true, "can": true, "you": true, "sudo": true, "with": true,
	"that": true, "this": true, "from": true, "into": true, "have": true, "need": true, "don't": true, "dont": true,
	"systemctl": true, "service": true, "docker": true, "start": true, "stop": true, "restart": true, "kill": true,
	"reload": true, "run": true, "machine": true, "host": true,
}

func sharesSignificantWord(a, b string) bool {
	words := func(s string) map[string]bool {
		out := map[string]bool{}
		for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
		}) {
			w = strings.Trim(w, ".-_")
			if len(w) >= 3 && !stopWords[w] {
				out[w] = true
			}
		}
		return out
	}
	wa := words(a)
	for w := range words(b) {
		if wa[w] {
			return true
		}
	}
	return false
}

// --- REST -------------------------------------------------------------------------------

func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.UserRequestFilter{
		Status:   strings.TrimSpace(q.Get("status")),
		WorkerID: resolveRef(r.Context(), s.store, ids.Machine, strings.TrimSpace(q.Get("worker_id"))),
		ChatID:   resolveRef(r.Context(), s.store, ids.Chat, strings.TrimSpace(q.Get("chat_id"))),
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		f.Limit = n
	}
	reqs, err := s.store.ListUserRequests(r.Context(), f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if reqs == nil {
		reqs = []store.UserRequest{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": reqs})
}

func (s *Server) handleResolveRequest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	status := strings.TrimSpace(body.Status)
	if status != store.RequestDone && status != store.RequestDeclined {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `status must be "done" or "declined"`})
		return
	}
	req, err := s.hub.resolveRequest(r.Context(), r.PathValue("id"), status, strings.TrimSpace(body.Note), "")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "request not found"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.hub.recordAudit(r.Context(), store.AuditEvent{Action: AuditRequestResolve, WorkerID: req.WorkerID,
		Message:  fmt.Sprintf("Marked request %s %s", req.SID, status),
		MetaJSON: auditMeta(map[string]any{"request_id": req.ID, "status": status})})
	writeJSON(w, http.StatusOK, req)
}

// handleListFleetEvents serves GET /api/events (newest first).
func (s *Server) handleListFleetEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.FleetEventFilter{
		WorkerID: resolveRef(r.Context(), s.store, ids.Machine, strings.TrimSpace(q.Get("worker_id"))),
		TaskID:   resolveRef(r.Context(), s.store, ids.Task, strings.TrimSpace(q.Get("task_id"))),
		ChatID:   resolveRef(r.Context(), s.store, ids.Chat, strings.TrimSpace(q.Get("chat_id"))),
		Kind:     strings.TrimSpace(q.Get("kind")),
		Actor:    strings.TrimSpace(q.Get("actor")),
		Notable:  q.Get("notable") == "1" || q.Get("notable") == "true",
		Limit:    100,
	}
	if n, err := strconv.ParseInt(q.Get("before_id"), 10, 64); err == nil {
		f.BeforeID = n
	}
	if n, err := strconv.ParseInt(q.Get("after_id"), 10, 64); err == nil {
		f.AfterID = n
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		f.Limit = min(n, 500)
	}
	evs, err := s.store.ListFleetEvents(r.Context(), f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if evs == nil {
		evs = []store.FleetEvent{}
	}
	resp := map[string]any{"events": evs}
	if len(evs) == f.Limit {
		resp["next_cursor"] = evs[len(evs)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}
