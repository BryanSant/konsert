package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ticketGetInput struct {
	TicketID string `json:"ticket_id,omitempty" jsonschema:"Optional ticket id or sid; if provided it must match this session's own ticket"`
}

type ticketCheckInput struct {
	TicketID string `json:"ticket_id,omitempty" jsonschema:"Optional ticket id or sid; if provided it must match this session's own ticket"`
	Index    *int   `json:"index,omitzero" jsonschema:"0-based index of the acceptance item to mark done"`
	Text     string `json:"text,omitempty" jsonschema:"Text (exact or unique case-insensitive substring) of the acceptance item to mark done"`
}

type ticketCommentInput struct {
	TicketID string `json:"ticket_id,omitempty" jsonschema:"Optional ticket id or sid; if provided it must match this session's own ticket"`
	Text     string `json:"text" jsonschema:"Short progress note or comment (at most 500 characters)"`
}

type ticketFollowupInput struct {
	TicketID string `json:"ticket_id,omitempty" jsonschema:"Optional ticket id or sid; if provided it must match this session's own ticket"`
	Title    string `json:"title" jsonschema:"Short title for the proposed follow-up ticket"`
	Why      string `json:"why,omitempty" jsonschema:"Why this follow-up is needed (evidence or risk)"`
	Scope    string `json:"scope,omitempty" jsonschema:"Affected repo, package, or component"`
}

// chatTicketID returns the ticket ID a chat session is working on (via cs.TaskID -> task.TicketID,
// or cs.TicketID directly), or "" when the session is not working on a ticket.
func (h *Hub) chatTicketID(ctx context.Context, cs store.ChatSession) string {
	if cs.TaskID != "" {
		if t, err := h.store.GetTask(ctx, cs.TaskID); err == nil && t.TicketID != "" {
			return t.TicketID
		}
	}
	return strings.TrimSpace(cs.TicketID)
}

// sessionTicketScope resolves the ticket and (if available) task owned by chatID.
// Rejects if the session is not working on a ticket or if requestedTicket names a different ticket.
func (h *Hub) sessionTicketScope(ctx context.Context, chatID, requestedTicket string) (store.ChatSession, store.Ticket, *store.Task, error) {
	cs, err := h.store.GetChatSession(ctx, chatID)
	if err != nil {
		return store.ChatSession{}, store.Ticket{}, nil, fmt.Errorf("session not found")
	}
	var task *store.Task
	ticketID := ""
	if cs.TaskID != "" {
		t, err := h.store.GetTask(ctx, cs.TaskID)
		if err != nil {
			return cs, store.Ticket{}, nil, fmt.Errorf("session task not found")
		}
		task = &t
		ticketID = strings.TrimSpace(t.TicketID)
	}
	if ticketID == "" {
		ticketID = strings.TrimSpace(cs.TicketID)
	}
	if ticketID == "" {
		return cs, store.Ticket{}, task, errors.New("session is not working on a ticket")
	}
	tk, err := h.store.GetTicket(ctx, ticketID)
	if err != nil {
		return cs, store.Ticket{}, task, fmt.Errorf("ticket not found: %w", err)
	}
	if req := strings.TrimSpace(requestedTicket); req != "" {
		if req != tk.ID && !strings.EqualFold(req, tk.SID) {
			return cs, store.Ticket{}, task, fmt.Errorf("access denied: session is scoped to ticket %s, cannot access %q", tk.SID, req)
		}
	}
	if task == nil && tk.Lease != nil && tk.Lease.TaskID != "" {
		if t, err := h.store.GetTask(ctx, tk.Lease.TaskID); err == nil {
			task = &t
		}
	}
	return cs, tk, task, nil
}

// addChatTicketTools registers the agent-facing ticket tools (ticket_get, ticket_check,
// ticket_comment, ticket_followup) scoped strictly to cs's own ticket.
func (h *Hub) addChatTicketTools(srv *mcp.Server, cs store.ChatSession) {
	chatID := cs.ID

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ticket_get",
		Description: "Return the compiled dispatch packet for this session's ticket (objective, unchecked acceptance items, contracts, repos, verify command).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ticketGetInput) (*mcp.CallToolResult, any, error) {
		curCS, tk, task, err := h.sessionTicketScope(ctx, chatID, in.TicketID)
		if err != nil {
			return nil, nil, err
		}
		opts := PacketOptions{WorkerID: curCS.WorkerID}
		if task != nil {
			if task.WorkerID != "" {
				opts.WorkerID = task.WorkerID
			}
			opts.Worktrees = task.Worktrees
		}
		pkt, err := h.CompileDispatchPacket(ctx, tk.ID, opts)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: pkt.Text()}},
		}, nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ticket_check",
		Description: "Mark an acceptance item on this session's ticket as done, selected by 0-based index or matching text.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ticketCheckInput) (*mcp.CallToolResult, any, error) {
		_, tk, _, err := h.sessionTicketScope(ctx, chatID, in.TicketID)
		if err != nil {
			return nil, nil, err
		}
		idx, err := matchAcceptanceIndex(tk.Acceptance, in.Index, in.Text)
		if err != nil {
			return nil, nil, err
		}
		tk.Acceptance[idx].Done = true
		updated, err := h.store.UpdateTicket(ctx, tk)
		if err != nil {
			return nil, nil, err
		}
		h.broadcastFleetUpdate("ticket", updated)
		remaining := 0
		for _, item := range updated.Acceptance {
			if !item.Done {
				remaining++
			}
		}
		return jsonToolResult(map[string]any{
			"ok":         true,
			"ticket_sid": updated.SID,
			"index":      idx,
			"text":       updated.Acceptance[idx].Text,
			"done":       true,
			"remaining":  remaining,
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ticket_comment",
		Description: "Post a short progress note or comment (<= 500 characters) on this session's ticket.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ticketCommentInput) (*mcp.CallToolResult, any, error) {
		curCS, tk, _, err := h.sessionTicketScope(ctx, chatID, in.TicketID)
		if err != nil {
			return nil, nil, err
		}
		body := strings.TrimSpace(in.Text)
		if body == "" {
			return nil, nil, errors.New("text is required")
		}
		if utf8.RuneCountInString(body) > store.MaxTicketCommentChars {
			return nil, nil, fmt.Errorf("comment text exceeds %d characters", store.MaxTicketCommentChars)
		}
		created, err := h.store.CreateTicketComment(ctx, store.TicketComment{
			TicketID:  tk.ID,
			Author:    curCS.SID,
			Body:      body,
			CreatedAt: time.Now().UTC(),
		})
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(map[string]any{
			"ok":         true,
			"ticket_sid": tk.SID,
			"comment":    created,
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ticket_followup",
		Description: "Record an out-of-scope finding as a proposed follow-up ticket (title, why, scope). It is merged into this task's post-run digest followups.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ticketFollowupInput) (*mcp.CallToolResult, any, error) {
		_, tk, task, err := h.sessionTicketScope(ctx, chatID, in.TicketID)
		if err != nil {
			return nil, nil, err
		}
		title := strings.TrimSpace(in.Title)
		if title == "" {
			return nil, nil, errors.New("title is required")
		}
		if task == nil {
			if tasks, lErr := h.store.ListTasksByTicket(ctx, tk.ID); lErr == nil && len(tasks) > 0 {
				t := tasks[0]
				task = &t
			}
		}
		if task == nil {
			return nil, nil, errors.New("no task associated with this session to store followup")
		}
		f := store.TaskFollowup{
			Title:     title,
			Why:       strings.TrimSpace(in.Why),
			Scope:     strings.TrimSpace(in.Scope),
			CreatedAt: time.Now().UTC(),
		}
		all, err := h.store.AppendTaskFollowup(ctx, task.ID, f)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(map[string]any{
			"ok":         true,
			"ticket_sid": tk.SID,
			"task_sid":   task.SID,
			"followup":   f,
			"count":      len(all),
		})
	})
}

func matchAcceptanceIndex(items []store.AcceptanceItem, index *int, text string) (int, error) {
	if len(items) == 0 {
		return -1, errors.New("ticket has no acceptance items")
	}
	if index != nil {
		idx := *index
		if idx < 0 || idx >= len(items) {
			return -1, fmt.Errorf("acceptance index %d out of range (0..%d)", idx, len(items)-1)
		}
		return idx, nil
	}
	needle := strings.ToLower(strings.TrimSpace(text))
	if needle == "" {
		return -1, errors.New("either index or text is required")
	}
	// 1. Exact case-insensitive match.
	for i, it := range items {
		if strings.ToLower(strings.TrimSpace(it.Text)) == needle {
			return i, nil
		}
	}
	// 2. Substring match (prefer unchecked items when disambiguating).
	var uncheckedMatches, allMatches []int
	for i, it := range items {
		if strings.Contains(strings.ToLower(it.Text), needle) {
			allMatches = append(allMatches, i)
			if !it.Done {
				uncheckedMatches = append(uncheckedMatches, i)
			}
		}
	}
	if len(uncheckedMatches) == 1 {
		return uncheckedMatches[0], nil
	}
	if len(allMatches) == 1 {
		return allMatches[0], nil
	}
	if len(allMatches) > 1 {
		return -1, fmt.Errorf("text %q matched %d acceptance items; specify a more specific text or an index", text, len(allMatches))
	}
	return -1, fmt.Errorf("no acceptance item matches %q", text)
}

func formatTaskFollowup(f store.TaskFollowup) string {
	title := singleLine(f.Title)
	why := singleLine(f.Why)
	scope := singleLine(f.Scope)
	var b strings.Builder
	b.WriteString(title)
	if why != "" {
		if b.Len() > 0 {
			b.WriteString(" — ")
		}
		b.WriteString(why)
	}
	if scope != "" {
		b.WriteString(" [")
		b.WriteString(scope)
		b.WriteByte(']')
	}
	return b.String()
}

// mergeTaskFollowupsIntoDigest appends any ticket_followup entries recorded on the task
// into d.Followups (deduplicating against entries already present).
func mergeTaskFollowupsIntoDigest(d *protocol.RunDigest, followups []store.TaskFollowup) {
	if d == nil || len(followups) == 0 {
		return
	}
	merged := append([]string(nil), d.Followups...)
	for _, f := range followups {
		entry := formatTaskFollowup(f)
		if entry == "" || slices.Contains(merged, entry) {
			continue
		}
		merged = append(merged, entry)
	}
	d.Followups = merged
}
