package controller

import (
	"context"
	"fmt"

	"github.com/BryanSant/syre/internal/protocol"
)

// Per-project concurrency caps (SYR-WZGZ6): a project with MaxConcurrent > 0 runs at most
// that many of its tasks and ticket runs at once across the fleet. Extra work waits in
// the queue (FIFO, by priority for tickets); nothing running is preempted. A person
// starting work by hand is not held back.

// projectCaps is a pass's view of the capped projects: their cap and live work.
type projectCaps struct {
	caps    map[string]int
	running map[string]int
	names   map[string]string
}

// liveTaskStatuses are the statuses that hold one of a project's slots.
var liveTaskStatuses = []string{protocol.TaskStatusAssigned, protocol.TaskStatusRunning, protocol.TaskStatusWaitingInput, protocol.TaskStatusIdle, protocol.TaskStatusSuspended}

// loadProjectCaps reads the capped projects and counts their live work; nil when no
// project has a cap.
func (h *Hub) loadProjectCaps(ctx context.Context) *projectCaps {
	projects, err := h.store.ListProjects(ctx)
	if err != nil {
		return nil
	}
	pc := &projectCaps{caps: map[string]int{}, running: map[string]int{}, names: map[string]string{}}
	for _, p := range projects {
		if p.MaxConcurrent > 0 {
			pc.caps[p.ID] = p.MaxConcurrent
			pc.names[p.ID] = p.Name
		}
	}
	if len(pc.caps) == 0 {
		return nil
	}
	for _, st := range liveTaskStatuses {
		tasks, err := h.store.ListTasks(ctx, st, "", 5000)
		if err != nil {
			continue
		}
		for _, t := range tasks {
			if t.Origin == protocol.TaskOriginExternal {
				continue
			}
			if _, capped := pc.caps[t.ProjectID]; capped {
				pc.running[t.ProjectID]++
			}
		}
	}
	return pc
}

// full reports whether project is at its cap, with why.
func (pc *projectCaps) full(project string) (string, bool) {
	if pc == nil {
		return "", false
	}
	limit, capped := pc.caps[project]
	if !capped || pc.running[project] < limit {
		return "", false
	}
	return fmt.Sprintf("project %s is at its limit of %d running at once", pc.names[project], limit), true
}

// started counts a placement against the project's cap for the rest of the pass.
func (pc *projectCaps) started(project string) {
	if pc == nil {
		return
	}
	if _, capped := pc.caps[project]; capped {
		pc.running[project]++
	}
}

// projectFull is the cap check for one dispatch outside a scheduler pass.
func (h *Hub) projectFull(ctx context.Context, project string) (string, bool) {
	if project == "" {
		return "", false
	}
	p, err := h.store.GetProject(ctx, project)
	if err != nil || p.MaxConcurrent <= 0 {
		return "", false
	}
	return h.loadProjectCaps(ctx).full(project)
}
