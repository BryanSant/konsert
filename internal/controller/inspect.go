package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// inspectToolDescription is the shared MCP and commander description of inspect.
const inspectToolDescription = "Read one capped page of a ticket or task artifact. kind is digest, diff_stat, diff, stderr, log, transcript, or file. Pages default to 200 lines (max 2000) and 64 KB. When truncated is true, call inspect again with offset set to next_offset. Pass repo when the ticket has more than one binding. path is required for kind file (and optional for diff and diff_stat)."

const (
	transcriptBatch    = 5000
	transcriptEventCap = 20000
	// transcriptRenderCap bounds the rendered transcript the hub holds while paging.
	// Pages are taken from this text, so a later offset still sees the same lines.
	transcriptRenderCap = 8 << 20
)

// InspectRequest is one page of a ticket or task artifact.
type InspectRequest struct {
	Target   string `json:"target" jsonschema:"Ticket or task id or sid"`
	Kind     string `json:"kind" jsonschema:"digest, diff_stat, diff, stderr, log, transcript, or file"`
	Repo     string `json:"repo,omitempty" jsonschema:"Repo name when the ticket has more than one binding. Required for diff, diff_stat, and file in that case"`
	Path     string `json:"path,omitempty" jsonschema:"Relative path in the worktree. Required for kind file. Optional path filter for diff and diff_stat"`
	MaxLines int    `json:"max_lines,omitempty" jsonschema:"Lines per page (default 200, maximum 2000)"`
	Offset   int    `json:"offset,omitempty" jsonschema:"Line offset. Pass the previous response's next_offset to read the next page"`
}

// InspectResult is one capped page. NextOffset is the offset to pass for the next page.
type InspectResult struct {
	Target     string `json:"target"`
	Kind       string `json:"kind"`
	TaskID     string `json:"task_id,omitempty"`
	TaskSID    string `json:"task_sid,omitempty"`
	TicketID   string `json:"ticket_id,omitempty"`
	Repo       string `json:"repo,omitempty"`
	Path       string `json:"path,omitempty"`
	WorkerID   string `json:"worker_id,omitempty"`
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
	Offset     int    `json:"offset"`
	NextOffset int    `json:"next_offset,omitempty"`
	MaxLines   int    `json:"max_lines"`
	Note       string `json:"note,omitempty"`
}

// InspectBackend is the control-plane surface MCP registers inspect against.
type InspectBackend interface {
	Inspect(ctx context.Context, req InspectRequest) (InspectResult, error)
}

// inspectFailure is an inspect error with the HTTP status the REST handler should use.
type inspectFailure struct {
	Status int
	Msg    string
}

func (e *inspectFailure) Error() string {
	if e == nil {
		return ""
	}
	return e.Msg
}

// Inspect returns one capped page of a ticket or task artifact. digest and transcript
// come from the hub. diff, diff_stat, file, stderr, and log are read from the daemon
// that ran the work, which enforces the byte cap.
func (s *FleetService) Inspect(ctx context.Context, req InspectRequest) (InspectResult, error) {
	req.Target = strings.TrimSpace(req.Target)
	req.Kind = strings.ToLower(strings.TrimSpace(req.Kind))
	req.Repo = strings.TrimSpace(req.Repo)
	req.Path = strings.TrimSpace(req.Path)
	if req.Offset < 0 {
		req.Offset = 0
	}
	if req.Target == "" {
		return InspectResult{}, &inspectFailure{Status: http.StatusBadRequest, Msg: "target is required"}
	}
	if !validInspectKind(req.Kind) {
		return InspectResult{}, &inspectFailure{Status: http.StatusBadRequest, Msg: fmt.Sprintf("unknown inspect kind %q", req.Kind)}
	}
	task, ticketID, err := s.resolveInspectTarget(ctx, req.Target)
	if err != nil {
		return InspectResult{}, err
	}
	res := InspectResult{
		Target:   req.Target,
		Kind:     req.Kind,
		TaskID:   task.ID,
		TaskSID:  task.SID,
		TicketID: ticketID,
		WorkerID: task.WorkerID,
		Repo:     req.Repo,
		Path:     req.Path,
	}
	switch req.Kind {
	case protocol.InspectKindDigest:
		return s.inspectDigest(req, res, task), nil
	case protocol.InspectKindTranscript:
		if task.ID == "" && ticketID == "" {
			return InspectResult{}, &inspectFailure{Status: http.StatusNotFound, Msg: "no transcript for this session"}
		}
		return s.inspectTranscript(ctx, req, res, task, ticketID)
	case protocol.InspectKindDiff, protocol.InspectKindDiffStat, protocol.InspectKindFile:
		if task.ID == "" {
			return InspectResult{}, &inspectFailure{Status: http.StatusNotFound, Msg: "no run recorded for this ticket"}
		}
		if task.WorktreesCleanedAt != nil {
			msg := "worktree was cleaned up"
			if req.Repo != "" {
				msg = "worktree for " + req.Repo + " was cleaned up"
			}
			return InspectResult{}, &inspectFailure{Status: http.StatusConflict, Msg: msg}
		}
		wt, wErr := pickWorktree(task.Worktrees, req.Repo)
		if wErr != nil {
			return InspectResult{}, wErr
		}
		res.Repo = wt.RepoName
		if req.Kind == protocol.InspectKindFile && req.Path == "" {
			return InspectResult{}, &inspectFailure{Status: http.StatusBadRequest, Msg: "path is required for kind file"}
		}
		if strings.TrimSpace(wt.WorktreePath) == "" {
			return InspectResult{}, &inspectFailure{Status: http.StatusBadRequest, Msg: "no worktree recorded"}
		}
		return s.fetchEdge(ctx, res, task.WorkerID, "", wt.WorktreePath, req)
	case protocol.InspectKindStderr:
		if task.ID == "" {
			return InspectResult{}, &inspectFailure{Status: http.StatusNotFound, Msg: "no run recorded for this ticket"}
		}
		cs, ok, cErr := s.inspectChat(ctx, task, ticketID)
		if cErr != nil {
			return InspectResult{}, cErr
		}
		if !ok || cs.ID == "" {
			return InspectResult{}, &inspectFailure{Status: http.StatusNotFound, Msg: "no retained stderr for this session"}
		}
		workerID := task.WorkerID
		if cs.WorkerID != "" {
			workerID = cs.WorkerID
		}
		res.WorkerID = workerID
		return s.fetchEdge(ctx, res, workerID, cs.ID, "", req)
	case protocol.InspectKindLog:
		if task.ID == "" {
			return InspectResult{}, &inspectFailure{Status: http.StatusNotFound, Msg: "no run recorded for this ticket"}
		}
		return s.fetchEdge(ctx, res, task.WorkerID, "", "", req)
	default:
		return InspectResult{}, &inspectFailure{Status: http.StatusBadRequest, Msg: fmt.Sprintf("unknown inspect kind %q", req.Kind)}
	}
}

func validInspectKind(kind string) bool {
	switch kind {
	case protocol.InspectKindDigest, protocol.InspectKindDiffStat, protocol.InspectKindDiff,
		protocol.InspectKindStderr, protocol.InspectKindLog, protocol.InspectKindTranscript, protocol.InspectKindFile:
		return true
	default:
		return false
	}
}

func (s *FleetService) resolveInspectTarget(ctx context.Context, target string) (store.Task, string, error) {
	taskID := resolveRef(ctx, s.store, ids.Task, target)
	task, err := s.store.GetTask(ctx, taskID)
	if err == nil {
		return task, task.TicketID, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Task{}, "", err
	}
	ticketID := resolveRef(ctx, s.store, ids.Ticket, target)
	tk, err := s.store.GetTicket(ctx, ticketID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Task{}, "", store.ErrNotFound
		}
		return store.Task{}, "", err
	}
	tasks, err := s.store.ListTasksByTicket(ctx, tk.ID)
	if err != nil {
		return store.Task{}, "", err
	}
	if len(tasks) == 0 {
		return store.Task{}, tk.ID, nil
	}
	return tasks[0], tk.ID, nil
}

func (s *FleetService) inspectDigest(req InspectRequest, res InspectResult, task store.Task) InspectResult {
	text := ""
	if strings.TrimSpace(task.DigestJSON) != "" {
		var d protocol.RunDigest
		if json.Unmarshal([]byte(task.DigestJSON), &d, protocol.JSONOptions) == nil {
			text = d.Text()
		}
	}
	if strings.TrimSpace(text) == "" {
		res.MaxLines = protocol.ClampInspectMaxLines(req.MaxLines)
		res.Offset = req.Offset
		res.Note = "no digest yet"
		return res
	}
	applyPage(&res, protocol.PageText(text, req.MaxLines, req.Offset))
	return res
}

func (s *FleetService) inspectTranscript(ctx context.Context, req InspectRequest, res InspectResult, task store.Task, ticketID string) (InspectResult, error) {
	cs, ok, err := s.inspectChat(ctx, task, ticketID)
	if err != nil {
		return InspectResult{}, err
	}
	if !ok || cs.ID == "" {
		return InspectResult{}, &inspectFailure{Status: http.StatusNotFound, Msg: "no transcript for this session"}
	}
	text, err := s.renderTranscript(ctx, cs.ID)
	if err != nil {
		return InspectResult{}, err
	}
	applyPage(&res, protocol.PageText(text, req.MaxLines, req.Offset))
	return res, nil
}

func (s *FleetService) inspectChat(ctx context.Context, task store.Task, ticketID string) (store.ChatSession, bool, error) {
	if task.ID != "" {
		cs, err := s.store.GetChatSessionByTaskID(ctx, task.ID)
		if err == nil {
			return cs, true, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return store.ChatSession{}, false, err
		}
	}
	if ticketID != "" {
		list, err := s.store.ListChatSessionsByTicket(ctx, ticketID)
		if err != nil {
			return store.ChatSession{}, false, err
		}
		if len(list) > 0 {
			return list[0], true, nil
		}
	}
	if task.ParentChatID != "" {
		cs, err := s.store.GetChatSession(ctx, resolveRef(ctx, s.store, ids.Chat, task.ParentChatID))
		if err == nil {
			return cs, true, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return store.ChatSession{}, false, err
		}
	}
	return store.ChatSession{}, false, nil
}

func (s *FleetService) renderTranscript(ctx context.Context, chatID string) (string, error) {
	var b strings.Builder
	var after int64
	loaded := 0
	for loaded < transcriptEventCap && b.Len() <= transcriptRenderCap {
		batch, err := s.store.ListChatEvents(ctx, chatID, after, transcriptBatch)
		if err != nil {
			return "", err
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			after = e.Seq
			loaded++
			line := renderTranscriptEvent(e)
			if line == "" {
				if loaded >= transcriptEventCap {
					break
				}
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
			if loaded >= transcriptEventCap || b.Len() > transcriptRenderCap {
				break
			}
		}
		if len(batch) < transcriptBatch {
			break
		}
	}
	return b.String(), nil
}

func renderTranscriptEvent(e store.ChatEvent) string {
	var ev protocol.AgentChatEvent
	if e.JSON != "" {
		if json.Unmarshal([]byte(e.JSON), &ev, protocol.JSONOptions) != nil {
			ev = protocol.AgentChatEvent{Kind: e.Kind, Text: e.JSON}
		}
	}
	if ev.Kind == "" {
		ev.Kind = e.Kind
	}
	return compactEventLine(ev)
}

func compactEventLine(ev protocol.AgentChatEvent) string {
	switch ev.Kind {
	case eventUserMessage:
		return labeledLine("user", ev.Text)
	case eventSyreNote:
		return labeledLine("note", ev.Text)
	case protocol.EventMessage:
		return labeledLine("assistant", ev.Text)
	case protocol.EventToolCall:
		if ev.ToolCall == nil {
			return ""
		}
		switch ev.ToolCall.Status {
		case "", "completed", "failed":
		default:
			return ""
		}
		name := flattenLine(ev.ToolCall.Name)
		title := flattenLine(ev.ToolCall.Title)
		if name == "" && title == "" {
			return ""
		}
		if title != "" {
			return "tool: " + strings.TrimSpace(name+" "+title)
		}
		return "tool: " + name
	case protocol.EventError:
		msg := ev.Text
		if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		return labeledLine("error", msg)
	case protocol.EventElicitation:
		msg := ev.Text
		if ev.Elicitation != nil {
			msg = strings.TrimSpace(ev.Elicitation.Title + " " + ev.Elicitation.Detail)
			if strings.TrimSpace(msg) == "" {
				msg = ev.Text
			}
		}
		return labeledLine("elicitation", msg)
	case protocol.EventTurnEnd:
		if strings.TrimSpace(ev.StopReason) == "" {
			return "turn_end"
		}
		return "turn_end: " + flattenLine(ev.StopReason)
	default:
		return ""
	}
}

func labeledLine(label, text string) string {
	text = flattenLine(text)
	if text == "" {
		return ""
	}
	return label + ": " + text
}

func flattenLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func pickWorktree(wts []store.TaskWorktree, repo string) (store.TaskWorktree, error) {
	if len(wts) == 0 {
		return store.TaskWorktree{}, &inspectFailure{Status: http.StatusBadRequest, Msg: "no worktree recorded"}
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		if len(wts) == 1 {
			return wts[0], nil
		}
		names := make([]string, 0, len(wts))
		for _, w := range wts {
			names = append(names, w.RepoName)
		}
		return store.TaskWorktree{}, &inspectFailure{Status: http.StatusBadRequest, Msg: "repo is required when several bindings exist: " + strings.Join(names, ", ")}
	}
	for _, w := range wts {
		if strings.EqualFold(w.RepoName, repo) {
			return w, nil
		}
	}
	return store.TaskWorktree{}, &inspectFailure{Status: http.StatusBadRequest, Msg: "no worktree recorded for repo " + repo}
}

func (s *FleetService) fetchEdge(ctx context.Context, res InspectResult, workerID, chatID, workdir string, req InspectRequest) (InspectResult, error) {
	if s.hub == nil || strings.TrimSpace(workerID) == "" || !s.hub.IsWorkerConnected(workerID) {
		return InspectResult{}, offlineFailure(ctx, s.hub, workerID)
	}
	rpcCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	env, err := s.hub.RequestWorker(rpcCtx, workerID, protocol.MsgArtifactFetch, protocol.ArtifactFetchRequest{
		TaskID:   res.TaskID,
		ChatID:   chatID,
		Kind:     req.Kind,
		Workdir:  workdir,
		Path:     req.Path,
		MaxLines: protocol.ClampInspectMaxLines(req.MaxLines),
		Offset:   req.Offset,
	})
	if err != nil {
		if strings.Contains(err.Error(), "not connected") {
			return InspectResult{}, offlineFailure(ctx, s.hub, workerID)
		}
		return InspectResult{}, err
	}
	page, err := protocol.DecodePayload[protocol.ArtifactFetchResult](env)
	if err != nil {
		return InspectResult{}, err
	}
	if page.Error != "" {
		status := http.StatusBadRequest
		if strings.Contains(page.Error, "cleaned up") {
			status = http.StatusConflict
		}
		return InspectResult{}, &inspectFailure{Status: status, Msg: page.Error}
	}
	res.Text = page.Text
	res.Truncated = page.Truncated
	res.Offset = page.Offset
	res.NextOffset = page.NextOffset
	res.MaxLines = page.MaxLines
	res.Note = page.Note
	if res.MaxLines == 0 {
		res.MaxLines = protocol.ClampInspectMaxLines(req.MaxLines)
	}
	return res, nil
}

func offlineFailure(ctx context.Context, h *Hub, workerID string) error {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" || h == nil {
		return &inspectFailure{Status: http.StatusServiceUnavailable, Msg: "the machine that ran this is offline"}
	}
	label := h.workerLabel(ctx, workerID)
	if label == "" || label == "(none)" {
		return &inspectFailure{Status: http.StatusServiceUnavailable, Msg: "the machine that ran this is offline"}
	}
	return &inspectFailure{Status: http.StatusServiceUnavailable, Msg: "machine " + label + " is offline"}
}

func applyPage(res *InspectResult, page protocol.TextPage) {
	res.Text = page.Text
	res.Truncated = page.Truncated
	res.Offset = page.Offset
	res.NextOffset = page.NextOffset
	res.MaxLines = page.MaxLines
	res.Note = page.Note
}

func (s *Server) handleGetTicketInspect(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	maxLines, _ := strconv.Atoi(q.Get("max_lines"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	res, err := s.service.Inspect(r.Context(), InspectRequest{
		Target:   r.PathValue("id"),
		Kind:     q.Get("kind"),
		Repo:     q.Get("repo"),
		Path:     q.Get("path"),
		MaxLines: maxLines,
		Offset:   offset,
	})
	if err != nil {
		var fail *inspectFailure
		if errors.As(err, &fail) && fail.Status != 0 {
			writeJSON(w, fail.Status, map[string]string{"error": fail.Msg})
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
