package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Project narration (SYR-H5BXK): the commander states each project's current goal (its
// headline) and, when useful, a one-line "now" for a task, with the set_narration tool.
// Gaps are filled from facts in the UI (task title, the live tool label, the ticket
// title), so nothing is ever blank. See docs/design/activity.md.

// Narration limits (runes).
const (
	maxHeadline  = 140
	maxNarration = 120
)

// SetNarrationInput is the set_narration MCP tool input.
type SetNarrationInput struct {
	Project  string `json:"project,omitempty" jsonschema:"Project id, sid (p-…) or name. Defaults to the task's project when task is given."`
	Headline string `json:"headline,omitempty" jsonschema:"The project's current goal in one line (max 140 chars), e.g. 'Ship the questions queue to mobile'. Set it when a plan starts or changes, not for every step."`
	Task     string `json:"task,omitempty" jsonschema:"Task id or sid (t-…) for the 'now' line."`
	Now      string `json:"now,omitempty" jsonschema:"One line (max 120 chars) on what the task is doing toward the goal, e.g. 'Porting the queue card to the phone layout'. Syre shows live tool activity by itself."`
}

// SetNarrationResult is the set_narration MCP tool output.
type SetNarrationResult struct {
	ProjectID  string `json:"project_id,omitempty"`
	ProjectSID string `json:"project_sid,omitempty"`
	Headline   string `json:"headline,omitempty"`
	TaskID     string `json:"task_id,omitempty"`
	TaskSID    string `json:"task_sid,omitempty"`
	Now        string `json:"now,omitempty"`
}

// NarrationBackend sets project headlines and task narration (FleetService).
type NarrationBackend interface {
	SetNarration(ctx context.Context, in SetNarrationInput) (SetNarrationResult, error)
}

const setNarrationDescription = "Set the headline of a project (its current goal) and/or a one-line 'now' for a task, shown in Syre's live work views. " +
	"Update the headline when a plan starts or changes — not on every step; Syre already shows each session's live activity (thinking, running a tool, asking). " +
	"Give a task a 'now' line when its purpose isn't obvious from its title. Plain, concrete words; one line each."

// narrationLine normalizes a narration to one line and checks its length.
func narrationLine(field, s string, max int) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if n := utf8.RuneCountInString(s); n > max {
		return "", fmt.Errorf("%s is %d characters; keep it to one line of at most %d", field, n, max)
	}
	return s, nil
}

// SetNarration implements NarrationBackend.
func (s *FleetService) SetNarration(ctx context.Context, in SetNarrationInput) (SetNarrationResult, error) {
	headline, err := narrationLine("headline", in.Headline, maxHeadline)
	if err != nil {
		return SetNarrationResult{}, err
	}
	now, err := narrationLine("now", in.Now, maxNarration)
	if err != nil {
		return SetNarrationResult{}, err
	}
	if headline == "" && now == "" {
		return SetNarrationResult{}, errors.New("give a headline (with project or task) and/or a now line (with task)")
	}
	if now != "" && strings.TrimSpace(in.Task) == "" {
		return SetNarrationResult{}, errors.New("a now line needs the task it describes (task)")
	}
	var res SetNarrationResult
	at := time.Now().UTC()
	var task *store.Task
	if ref := strings.TrimSpace(in.Task); ref != "" {
		t, err := s.store.GetTask(ctx, resolveRef(ctx, s.store, ids.Task, ref))
		if err != nil {
			return SetNarrationResult{}, fmt.Errorf("task %q not found", ref)
		}
		task = &t
	}
	if headline != "" {
		projID := ""
		if ref := strings.TrimSpace(in.Project); ref != "" {
			projID = s.resolveProjectRef(ctx, ref)
			if projID == "" {
				return SetNarrationResult{}, fmt.Errorf("project %q not found", ref)
			}
		} else if task != nil {
			projID = task.ProjectID
		}
		if projID == "" {
			return SetNarrationResult{}, errors.New("a headline needs its project (project) or a task in it (task)")
		}
		if err := s.store.SetProjectHeadline(ctx, projID, headline, at); err != nil {
			return SetNarrationResult{}, err
		}
		p, err := s.store.GetProject(ctx, projID)
		if err != nil {
			return SetNarrationResult{}, err
		}
		s.hub.broadcastFleetUpdate("project", p)
		res.ProjectID, res.ProjectSID, res.Headline = p.ID, p.SID, p.Headline
	}
	if now != "" && task != nil {
		if err := s.store.SetTaskNarration(ctx, task.ID, now, at); err != nil {
			return SetNarrationResult{}, err
		}
		t, err := s.store.GetTask(ctx, task.ID)
		if err != nil {
			return SetNarrationResult{}, err
		}
		s.hub.broadcastFleetUpdate("task", t)
		res.TaskID, res.TaskSID, res.Now = t.ID, t.SID, t.Narration
	}
	return res, nil
}

// resolveProjectRef resolves a project id, sid or (case-insensitive) name.
func (s *FleetService) resolveProjectRef(ctx context.Context, ref string) string {
	if id := resolveRef(ctx, s.store, ids.Project, ref); id != "" {
		if _, err := s.store.GetProject(ctx, id); err == nil {
			return id
		}
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return ""
	}
	for _, p := range projects {
		if strings.EqualFold(p.Name, ref) {
			return p.ID
		}
	}
	return ""
}

// addNarrationTool registers set_narration on srv when backend supports it.
func addNarrationTool(srv *mcp.Server, backend any) {
	nb, ok := backend.(NarrationBackend)
	if !ok {
		return
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "set_narration",
		Description: setNarrationDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SetNarrationInput) (*mcp.CallToolResult, any, error) {
		res, err := nb.SetNarration(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})
}

// setNarrationToolDef is set_narration for the API-key commander backend.
var setNarrationToolDef = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"project":  map[string]any{"type": "string", "description": "Project id, sid (p-…) or name; defaults to the task's project"},
		"headline": map[string]any{"type": "string", "description": "The project's current goal in one line (max 140 chars); set it when a plan starts or changes"},
		"task":     map[string]any{"type": "string", "description": "Task id or sid (t-…) for the now line"},
		"now":      map[string]any{"type": "string", "description": "One line (max 120 chars) on what the task is doing toward the goal"},
	},
}

// decodeSetNarration parses set_narration arguments of the API-key backend.
func decodeSetNarration(args []byte) (SetNarrationInput, error) {
	var in SetNarrationInput
	if err := json.Unmarshal(args, &in); err != nil {
		return in, fmt.Errorf("invalid set_narration args: %w", err)
	}
	return in, nil
}

// detectTicket returns the first ka ticket id in texts ("" when none).
func detectTicket(texts ...string) string {
	for _, t := range texts {
		if m := ticketRe.FindStringSubmatch(t); m != nil {
			return m[1]
		}
	}
	return ""
}

// resolveTaskTicket looks the task's ticket title up with ka (in the background) and
// broadcasts the task when it is found.
func (h *Hub) resolveTaskTicket(taskID, ticketID string) {
	if ticketID == "" {
		return
	}
	h.goBackground(func(ctx context.Context) {
		rctx, cancel := context.WithTimeout(ctx, refResolveTimeout)
		raw, err := h.kaShow(rctx, ticketID)
		cancel()
		if err != nil {
			return
		}
		var t struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(raw, &t) != nil || strings.TrimSpace(t.Title) == "" {
			return
		}
		if err := h.store.SetTaskTicket(ctx, taskID, ticketID, oneLine(t.Title, 160)); err != nil {
			return
		}
		if updated, err := h.store.GetTask(ctx, taskID); err == nil {
			h.broadcastFleetUpdate("task", updated)
		}
	})
}
