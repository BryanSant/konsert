package activity

import (
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// Task derives the coarse activity of a PTY task from its driver telemetry (the agent's
// session log, tailed every ~600 ms, and PTY output lines) and its status. Session logs
// record whole messages, not streamed deltas, so "speaking" is never reported and short
// states can be missed: every activity is marked Coarse. Not safe for concurrent use.
type Task struct {
	taskID             string
	state, tool, label string
	since              time.Time
}

// NewTask returns a deriver for taskID.
func NewTask(taskID string) *Task { return &Task{taskID: taskID} }

// waitingForPrompt matches drivers' "waiting for your next prompt" notes: the turn ended.
func waitingForPrompt(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "waiting for your next prompt")
}

// ObserveEvent folds one telemetry event in and reports whether the activity changed.
func (t *Task) ObserveEvent(ev protocol.TaskEventPayload, now time.Time) bool {
	switch ev.Kind {
	case "tool_call", "file_edit":
		tool, _ := ev.Meta["tool"].(string)
		if tool == "" && ev.Kind == "file_edit" {
			tool = "Edit"
		}
		if tool == "" {
			tool, _, _ = strings.Cut(ev.Message, ":")
		}
		if IsUserTool(tool) {
			return t.set(protocol.ActivityAsking, tool, "", now)
		}
		return t.set(protocol.ActivityWorking, tool, ev.Message, now)
	case "waiting_input":
		if waitingForPrompt(ev.Message) {
			return t.set(protocol.ActivityDone, "", "", now)
		}
		return t.set(protocol.ActivityAsking, "", ev.Message, now)
	case "milestone", "usage", "summary":
		// The model produced something: whatever ran before is over.
		if st, _ := ev.Meta["stop_reason"].(string); st != "" {
			return t.set(protocol.ActivityDone, "", "", now)
		}
		if t.state == protocol.ActivityWorking || t.state == protocol.ActivityAsking || t.state == "" {
			return t.set(protocol.ActivityThinking, "", "", now)
		}
	case "steer", "input":
		return t.set(protocol.ActivityThinking, "", "", now)
	case "error":
		return t.set(protocol.ActivityError, "", ev.Message, now)
	}
	return false
}

// ObserveStatus folds a task status in and reports whether the activity changed.
func (t *Task) ObserveStatus(p protocol.TaskStatusPayload, now time.Time) bool {
	switch p.Status {
	case protocol.TaskStatusRunning:
		if t.state == "" || t.state == protocol.ActivityQueued || t.state == protocol.ActivityBlocked || t.state == protocol.ActivityAsking {
			return t.set(protocol.ActivityThinking, "", "", now)
		}
	case protocol.TaskStatusWaitingInput:
		// The event that caused it already set asking (or done: "waiting for your next prompt").
		if t.state != protocol.ActivityAsking && t.state != protocol.ActivityDone {
			return t.set(protocol.ActivityAsking, "", "Waiting for input", now)
		}
	case protocol.TaskStatusAssigned, protocol.TaskStatusQueued:
		return t.set(protocol.ActivityQueued, "", "Starting", now)
	case protocol.TaskStatusIdle:
		return t.set(protocol.ActivityDone, "", "", now)
	case protocol.TaskStatusSuspended:
		return t.set(protocol.ActivityBlocked, "", "Suspended", now)
	case protocol.TaskStatusCompleted:
		return t.set(protocol.ActivityDone, "", "", now)
	case protocol.TaskStatusCanceled:
		return t.set(protocol.ActivityDone, "", "Cancelled", now)
	case protocol.TaskStatusFailed:
		label := "Failed"
		if p.ExitCode != nil && *p.ExitCode != 0 {
			label = "Exited with an error"
		}
		return t.set(protocol.ActivityError, "", label, now)
	}
	return false
}

func (t *Task) set(state, tool, label string, now time.Time) bool {
	label = oneLine(label)
	if state == t.state && tool == t.tool && label == t.label {
		return false
	}
	t.state, t.tool, t.label, t.since = state, tool, label, now
	return true
}

// Activity returns the current activity.
func (t *Task) Activity(now time.Time) protocol.SessionActivity {
	return protocol.SessionActivity{
		TaskID: t.taskID,
		State:  t.state,
		Tool:   t.tool,
		Label:  t.label,
		Coarse: true,
		Source: protocol.ActivitySourceLog,
		Since:  t.since.UTC(),
		At:     now.UTC(),
	}
}

// Tracker keeps the Task derivers of a worker's PTY tasks and feeds a Publisher.
type Tracker struct {
	pub *Publisher
	now func() time.Time

	tasks map[string]*Task
}

// NewTracker returns a tracker publishing through pub. Callers serialize access (the
// daemon calls it from the runner callbacks under its own lock).
func NewTracker(pub *Publisher, now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	return &Tracker{pub: pub, now: now, tasks: map[string]*Task{}}
}

func (tr *Tracker) task(id string) *Task {
	t := tr.tasks[id]
	if t == nil {
		t = NewTask(id)
		tr.tasks[id] = t
	}
	return t
}

// Event folds a task event in.
func (tr *Tracker) Event(ev protocol.TaskEventPayload) {
	if ev.TaskID == "" {
		return
	}
	now := tr.now()
	t := tr.task(ev.TaskID)
	if ev.Kind == "exit" {
		return // the final task.status follows
	}
	if t.ObserveEvent(ev, now) {
		tr.pub.Update(t.Activity(now))
	}
}

// Status folds a task status in; a final status is published right away and the task
// forgotten.
func (tr *Tracker) Status(p protocol.TaskStatusPayload) {
	if p.TaskID == "" {
		return
	}
	now := tr.now()
	t := tr.task(p.TaskID)
	if t.ObserveStatus(p, now) {
		tr.pub.Update(t.Activity(now))
	}
	if protocol.IsTerminalTaskStatus(p.Status) {
		tr.pub.Flush("task:" + p.TaskID)
		delete(tr.tasks, p.TaskID)
	}
}
