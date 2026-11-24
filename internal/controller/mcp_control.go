package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP tools for human control: recent_events everywhere; intervention tools the
// orchestrator uses when the user tells it to (suspend, veto, drain, pause); and, on the
// per-chat endpoint, request_user_action and release_hold. Undoing what the user did
// (resume, resume everything, undrain, release a hold) needs the user's confirmation:
// per-chat endpoints ask in the chat; the external MCP server refuses and points to the UI.

// HumanControlBackend is implemented by MCP backends that expose human-control tools.
type HumanControlBackend interface {
	RecentEvents(ctx context.Context, in RecentEventsInput) (RecentEventsResult, error)
	SuspendSession(ctx context.Context, in SessionRefInput) (any, error)
	ResumeSession(ctx context.Context, in SessionRefInput) (any, error)
	VetoTask(ctx context.Context, in VetoInput) (ActionResult, error)
	DrainMachine(ctx context.Context, in DrainInput) (WorkerControlResult, error)
	PauseEverything(ctx context.Context) (FleetPauseState, error)
	ResumeEverything(ctx context.Context) (FleetPauseState, error)
}

// SessionRefInput names a task or an agent chat.
type SessionRefInput struct {
	TaskID string `json:"task_id,omitempty" jsonschema:"Task id or sid (t-…)"`
	ChatID string `json:"chat_id,omitempty" jsonschema:"Agent chat id or sid (c-…)"`
}

// VetoInput is the veto_task tool input.
type VetoInput struct {
	TaskID string `json:"task_id" jsonschema:"Task id or sid (t-…) to veto (it is canceled)"`
	Reason string `json:"reason" jsonschema:"wrong_approach, wrong_machine, not_now, dangerous or other"`
	Note   string `json:"note,omitempty" jsonschema:"The user's words: why, and what to do instead"`
}

// DrainInput is the drain_machine tool input.
type DrainInput struct {
	Machine string `json:"machine" jsonschema:"Machine id, sid (m-…) or name"`
	Drained *bool  `json:"drained,omitzero" jsonschema:"true to drain (default), false to undrain"`
}

type releaseHoldInput struct {
	HoldID string `json:"hold_id" jsonschema:"Hold id or sid (h-…)"`
	Note   string `json:"note,omitempty" jsonschema:"What the user said"`
}

// --- FleetService (local backend) --------------------------------------------------------

func (s *FleetService) RecentEvents(ctx context.Context, in RecentEventsInput) (RecentEventsResult, error) {
	return s.hub.RecentEvents(ctx, in, nil)
}

func (s *FleetService) SuspendSession(ctx context.Context, in SessionRefInput) (any, error) {
	switch {
	case in.TaskID != "":
		t, err := s.store.GetTask(ctx, resolveRef(ctx, s.store, ids.Task, in.TaskID))
		if err != nil {
			return nil, err
		}
		return s.hub.suspendTask(ctx, t, store.SuspendedByUser)
	case in.ChatID != "":
		cs, err := s.store.GetChatSession(ctx, resolveRef(ctx, s.store, ids.Chat, in.ChatID))
		if err != nil {
			return nil, err
		}
		cs, err = s.hub.suspendChat(ctx, cs, store.SuspendedByUser)
		return chatActionResult(cs), err
	}
	return nil, errors.New("task_id or chat_id is required")
}

func (s *FleetService) ResumeSession(ctx context.Context, in SessionRefInput) (any, error) {
	switch {
	case in.TaskID != "":
		t, err := s.store.GetTask(ctx, resolveRef(ctx, s.store, ids.Task, in.TaskID))
		if err != nil {
			return nil, err
		}
		return s.hub.resumeTask(ctx, t)
	case in.ChatID != "":
		cs, err := s.store.GetChatSession(ctx, resolveRef(ctx, s.store, ids.Chat, in.ChatID))
		if err != nil {
			return nil, err
		}
		cs, err = s.hub.resumeChat(ctx, cs)
		return chatActionResult(cs), err
	}
	return nil, errors.New("task_id or chat_id is required")
}

func (s *FleetService) VetoTask(ctx context.Context, in VetoInput) (ActionResult, error) {
	return s.hub.VetoTask(ctx, resolveRef(ctx, s.store, ids.Task, in.TaskID), in.Reason, in.Note)
}

func (s *FleetService) DrainMachine(ctx context.Context, in DrainInput) (WorkerControlResult, error) {
	drained := in.Drained == nil || *in.Drained
	return s.hub.DrainWorker(ctx, s.hub.resolveMachine(ctx, in.Machine), drained)
}

func (s *FleetService) PauseEverything(ctx context.Context) (FleetPauseState, error) {
	return s.hub.PauseEverything(ctx)
}

func (s *FleetService) ResumeEverything(ctx context.Context) (FleetPauseState, error) {
	return s.hub.ResumeEverything(ctx)
}

// --- RemoteFleetClient (REST) --------------------------------------------------------------

func (c *RemoteFleetClient) RecentEvents(ctx context.Context, in RecentEventsInput) (RecentEventsResult, error) {
	q := url.Values{}
	if in.Machine != "" {
		q.Set("machine", in.Machine)
	}
	if in.TaskID != "" {
		q.Set("task_id", in.TaskID)
	}
	if in.SinceMinutes > 0 {
		q.Set("since_minutes", fmt.Sprint(in.SinceMinutes))
	}
	if in.Limit > 0 {
		q.Set("limit", fmt.Sprint(in.Limit))
	}
	if in.All {
		q.Set("all", "1")
	}
	var res RecentEventsResult
	err := c.doJSON(ctx, http.MethodGet, "/api/events/recent?"+q.Encode(), nil, &res)
	return res, err
}

func (c *RemoteFleetClient) SuspendSession(ctx context.Context, in SessionRefInput) (any, error) {
	var out map[string]any
	if in.TaskID != "" {
		return out, c.doJSON(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(in.TaskID)+"/suspend", nil, &out)
	}
	if in.ChatID != "" {
		return out, c.doJSON(ctx, http.MethodPost, "/api/chats/"+url.PathEscape(in.ChatID)+"/suspend", nil, &out)
	}
	return nil, errors.New("task_id or chat_id is required")
}

func (c *RemoteFleetClient) ResumeSession(ctx context.Context, in SessionRefInput) (any, error) {
	var out map[string]any
	if in.TaskID != "" {
		return out, c.doJSON(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(in.TaskID)+"/resume", nil, &out)
	}
	if in.ChatID != "" {
		return out, c.doJSON(ctx, http.MethodPost, "/api/chats/"+url.PathEscape(in.ChatID)+"/resume", nil, &out)
	}
	return nil, errors.New("task_id or chat_id is required")
}

func (c *RemoteFleetClient) VetoTask(ctx context.Context, in VetoInput) (ActionResult, error) {
	var res ActionResult
	err := c.doJSON(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(in.TaskID)+"/veto", map[string]string{"reason": in.Reason, "note": in.Note}, &res)
	return res, err
}

func (c *RemoteFleetClient) DrainMachine(ctx context.Context, in DrainInput) (WorkerControlResult, error) {
	var res WorkerControlResult
	drained := in.Drained == nil || *in.Drained
	err := c.doJSON(ctx, http.MethodPost, "/api/workers/"+url.PathEscape(in.Machine)+"/drain", map[string]bool{"drained": drained}, &res)
	return res, err
}

func (c *RemoteFleetClient) PauseEverything(ctx context.Context) (FleetPauseState, error) {
	var res FleetPauseState
	err := c.doJSON(ctx, http.MethodPost, "/api/fleet/pause", nil, &res)
	return res, err
}

func (c *RemoteFleetClient) ResumeEverything(ctx context.Context) (FleetPauseState, error) {
	var res FleetPauseState
	err := c.doJSON(ctx, http.MethodPost, "/api/fleet/resume", nil, &res)
	return res, err
}

// --- tool registration -----------------------------------------------------------------------

// confirmFunc asks the user to confirm an action that undoes one of theirs; it returns
// nil when confirmed. A nil confirmFunc refuses (no way to ask).
type confirmFunc func(ctx context.Context, question string) error

var errNeedsUI = errors.New("this undoes something the user did: only the user can do it, in the Konsert UI (or by telling an orchestrator chat, which asks them to confirm)")

const recentEventsDescription = "Recent fleet events with attribution: what the user did (Konsert UI, CLI, Konsert terminals), holds, open requests, vetoes, pauses and failures — who caused each change and how sure Konsert is. " +
	"Call it BEFORE investigating anything that changed unexpectedly (a service stopped, a session ended, a port closed): if the user caused it, stop and ask the user instead of investigating or undoing it. Also lists active holds and open requests."

// addHumanControlTools registers recent_events and the intervention tools on srv.
// confirm asks the user before undoing their actions (nil: refuse).
func addHumanControlTools(srv *mcp.Server, b HumanControlBackend, recent func(context.Context, RecentEventsInput) (RecentEventsResult, error), confirm confirmFunc) {
	if recent == nil {
		recent = b.RecentEvents
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "recent_events", Description: recentEventsDescription,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RecentEventsInput) (*mcp.CallToolResult, any, error) {
		res, err := recent(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "suspend_session",
		Description: "Freeze a session's process tree (task_id or chat_id) when the user asks you to. Work is kept; it resumes intact.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SessionRefInput) (*mcp.CallToolResult, any, error) {
		res, err := b.SuspendSession(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "resume_session",
		Description: "Resume a suspended session (task_id or chat_id). The user confirms first.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SessionRefInput) (*mcp.CallToolResult, any, error) {
		if confirm == nil {
			return nil, nil, errNeedsUI
		}
		if err := confirm(ctx, "Resume suspended session "+strings.TrimSpace(in.TaskID+" "+in.ChatID)+"?"); err != nil {
			return nil, nil, err
		}
		res, err := b.ResumeSession(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "veto_task",
		Description: "Veto a task for the user: cancel it and record why (reason wrong_approach, wrong_machine, not_now, dangerous or other, plus the user's note) in orchestrator memory so the approach isn't retried.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in VetoInput) (*mcp.CallToolResult, any, error) {
		res, err := b.VetoTask(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "drain_machine",
		Description: "Drain a machine when the user asks (it finishes current work and accepts nothing new); drained=false undrains it after the user confirms.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in DrainInput) (*mcp.CallToolResult, any, error) {
		if in.Drained != nil && !*in.Drained {
			if confirm == nil {
				return nil, nil, errNeedsUI
			}
			if err := confirm(ctx, "Undrain "+in.Machine+" so it accepts new work again?"); err != nil {
				return nil, nil, err
			}
		}
		res, err := b.DrainMachine(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "pause_everything",
		Description: "Pause everything when the user asks: stop all dispatching and suspend every running managed session.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		res, err := b.PauseEverything(ctx)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "resume_everything",
		Description: "Resume after Pause everything (restores what the pause suspended). The user confirms first.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		if confirm == nil {
			return nil, nil, errNeedsUI
		}
		if err := confirm(ctx, "Resume everything (restart dispatching and resume what Pause everything suspended)?"); err != nil {
			return nil, nil, err
		}
		res, err := b.ResumeEverything(ctx)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})
}

// chatConfirm asks the chat's user to confirm with a yes/no question elicitation.
func (h *Hub) chatConfirm(chatID string) confirmFunc {
	return func(ctx context.Context, question string) error {
		f := false
		text, ans, err := h.askUserAnswer(ctx, chatID, askUserInput{
			Question: question,
			Context:  "The orchestrator wants to do this on your behalf.",
			WhyNow:   "It waits for your confirmation before acting.",
			Options: []askOption{
				{Label: "Yes, do it", Consequence: "The orchestrator does it now."},
				{Label: "No", Consequence: "Nothing happens; the orchestrator asks you what you want instead."},
			},
			AllowText: &f,
		})
		if err != nil {
			return err
		}
		if ans == nil || ans.Label != "Yes, do it" {
			return fmt.Errorf("the user did not confirm (%s); don't do it — ask the user what they want instead", text)
		}
		return nil
	}
}

// addChatControlTools registers the per-chat human-control tools: request_user_action,
// release_hold, recent_events (scoped to the chat) and, for orchestrator chats, the
// intervention tools.
func (h *Hub) addChatControlTools(srv *mcp.Server, cs store.ChatSession, fleet HumanControlBackend) {
	chatID := cs.ID
	recent := func(ctx context.Context, in RecentEventsInput) (RecentEventsResult, error) {
		cur, err := h.store.GetChatSession(ctx, chatID)
		if err != nil {
			return RecentEventsResult{}, err
		}
		return h.RecentEvents(ctx, in, &cur)
	}
	if fleet != nil {
		addHumanControlTools(srv, fleet, recent, h.chatConfirm(chatID))
		mcp.AddTool(srv, &mcp.Tool{
			Name: "request_override",
			Description: "Break glass: ask the user for a time-boxed exception to a limit or hold when the work truly needs it — e.g. " +
				"scope=machine_slots machine=mac extra_slots=1 duration_minutes=30 to allow a 2nd session on mac for 30m, or scope=hold hold_id=h-… to lift a hold for a while. " +
				"The request shows in Konsert's Needs you with your reason and what approving allows; it returns at once. Nothing changes until the user approves: " +
				"you are told the decision (and later the expiry) before your next turn, or a turn starts if you are idle. Konsert enforces an approved override until it expires. " +
				"Never use it to undo something the user did on purpose without saying so in the reason.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in requestOverrideInput) (*mcp.CallToolResult, any, error) {
			o, err := h.createOverride(ctx, chatID, in)
			if err != nil {
				return nil, nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: requestOverrideReply(o)}}}, nil, nil
		})
	} else {
		mcp.AddTool(srv, &mcp.Tool{
			Name: "recent_events", Description: recentEventsDescription,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in RecentEventsInput) (*mcp.CallToolResult, any, error) {
			res, err := recent(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return jsonToolResult(res)
		})
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "request_user_action",
		Description: "Ask the user to DO something on a machine that you cannot or must not do yourself (e.g. restart nginx — you have no sudo; plug in a device; approve a deploy). " +
			"Creates a tracked request shown in Konsert and waits until the user does it (Konsert notices when they do it in a Konsert terminal) or marks it done/declined. " +
			"Returns the outcome; continue your task when it says the user did it. Use ask_user for questions; use this for actions.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in requestUserActionInput) (*mcp.CallToolResult, any, error) {
		out, err := h.requestUserAction(ctx, chatID, in)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name: "release_hold",
		Description: "Release a hold (h-…) ONLY when the user has told you to (e.g. 'go ahead and restart it'). Konsert asks the user to confirm in the chat before releasing. " +
			"Never call it on your own initiative to get around a hold.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in releaseHoldInput) (*mcp.CallToolResult, any, error) {
		id := resolveRef(ctx, h.store, ids.Hold, in.HoldID)
		hd, err := h.store.GetHold(ctx, id)
		if err != nil {
			return nil, nil, fmt.Errorf("hold %q not found", in.HoldID)
		}
		if !hd.Active() {
			return jsonToolResult(hd)
		}
		q := fmt.Sprintf("Release hold %s on %s (%s)? Automatic actions there will be allowed again.", hd.SID, h.holdScopeLabel(ctx, hd), hd.Summary)
		if err := h.chatConfirm(chatID)(ctx, q); err != nil {
			return nil, nil, err
		}
		// The release is the user's decision, carried out by the agent.
		ucause := store.Cause{Actor: store.ActorUser, Source: "chat:" + chatID, Confidence: store.ConfidenceHigh}
		res, err := h.releaseHold(withCause(ctx, ucause), hd.ID, strings.TrimSpace("confirmed in chat "+cs.SID+". "+in.Note))
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})
}

// handleRecentEvents serves GET /api/events/recent (the recent_events view for
// `konsert mcp`).
func (s *Server) handleRecentEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	in := RecentEventsInput{Machine: q.Get("machine"), TaskID: q.Get("task_id"), All: q.Get("all") == "1"}
	fmt.Sscan(q.Get("since_minutes"), &in.SinceMinutes)
	fmt.Sscan(q.Get("limit"), &in.Limit)
	res, err := s.hub.RecentEvents(r.Context(), in, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
