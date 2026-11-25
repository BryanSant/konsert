package protocol

import "time"

// Session activity: what an agent session is doing right now (thinking, running a tool,
// writing its reply, waiting for the user, ...), for the live-work views. syred derives
// it from the engine stream (chat sessions) or the agent's session log (PTY tasks); the hub
// fills in what only it knows (queued, suspended) and what it learns from observed sessions.
// See docs/design/activity.md.

// MsgSessionActivity (worker->hub) carries one SessionActivity. syred rate-limits it
// per session (see internal/worker/activity); the hub fans it out to browsers as a
// fleet.update of kind "activity".
const MsgSessionActivity = "session.activity"

// Activity states (SessionActivity.State).
const (
	ActivityThinking = "thinking" // the model is reasoning, or a turn waits on the model
	ActivityWorking  = "working"  // a tool call is running (Tool, Label)
	ActivitySpeaking = "speaking" // the model streams its reply (TokensPerSec)
	ActivityAsking   = "asking"   // waiting for the user: question, permission, ask_user
	ActivityBlocked  = "blocked"  // suspended, paused or held: it can't continue on its own
	ActivityQueued   = "queued"   // accepted but not running yet (no machine yet, engine starting)
	ActivityDone     = "done"     // the turn or task ended normally
	ActivityError    = "error"    // the turn or task failed (Label says why)
)

// Activity sources (SessionActivity.Source), from the most to the least precise.
const (
	ActivitySourceStream  = "stream"      // the engine's structured event stream (chat sessions)
	ActivitySourceLog     = "session_log" // the agent's session log and PTY output (PTY tasks)
	ActivitySourceHook    = "hook"        // agent hooks of a session started outside Syre
	ActivitySourcePassive = "passive"     // process/transcript inspection of an observed session
	ActivitySourceHub     = "hub"         // derived by the hub from task/chat status
)

// SessionActivity is the live activity of one session: a chat (ChatID) or a task (TaskID).
type SessionActivity struct {
	ChatID string `json:"chat_id,omitempty"`
	TaskID string `json:"task_id,omitempty"`
	// WorkerID is set by the hub (the reporting worker).
	WorkerID string `json:"worker_id,omitempty"`

	State string `json:"state"`
	// Tool is the running tool's name (working) and Label a short human line: the tool
	// call's title ("Run: go test ./..."), the question being asked, the error.
	// Tool calls running at the same time are joined in Label with nowline.Separator.
	Tool  string `json:"tool,omitempty"`
	Label string `json:"label,omitempty"`
	// Summary is the readable "now" line for a working session ("running tests",
	// "editing wc.py"), set by the hub from Tool and Label with paths shortened against
	// the session's checkout (internal/nowline). Label stays the raw line.
	Summary string `json:"summary,omitempty"`
	// TokensPerSec is the reply's output rate while speaking (estimated from the streamed
	// text, about 4 characters per token).
	TokensPerSec float64 `json:"tokens_per_sec,omitzero"`
	// Coarse marks a best-effort state from a weak signal (session log, hooks, process
	// inspection): transitions may be late or skipped.
	Coarse bool   `json:"coarse,omitzero"`
	Source string `json:"source,omitempty"`

	// Waiting marks a parent that only waits on its sub-agents (every running tool call
	// is a sub-agent's): the views draw a plain grey ring and let the sub-agent dots move.
	Waiting bool `json:"waiting,omitzero"`

	// Shells counts the shell processes running under the session's agent (from its
	// process tree, so background shells count), and ShellsBusy those running a command.
	// Shells left running after the session ended still count until they exit.
	// ShellList details at most MaxShellList of them (Shells stays the exact count).
	Shells     int         `json:"shells,omitzero"`
	ShellsBusy int         `json:"shells_busy,omitzero"`
	ShellList  []ShellInfo `json:"shell_list,omitempty"`

	// Subagents are the session's running sub-agents (claude Task/Agent calls), each with
	// its own state. State above stays the parent loop's state.
	Subagents []SubagentActivity `json:"subagents,omitempty"`

	// Since is when the current State (and Tool) began; At is when this was derived.
	Since time.Time `json:"since"`
	At    time.Time `json:"at"`
}

// MaxShellList caps SessionActivity.ShellList (activity frames go to every browser).
const MaxShellList = 12

// MaxSubagents caps SessionActivity.Subagents.
const MaxSubagents = 16

// ShellInfo is one shell process of a session.
type ShellInfo struct {
	PID int `json:"pid"`
	// Command is the shell's command line, cleaned of agent wrappers (claude's snapshot
	// sourcing, eval quoting) and capped; Summary is its readable line ("running tests"),
	// set by the hub with internal/nowline.
	Command string `json:"command"`
	Summary string `json:"summary,omitempty"`
	// StartedAt is the process start time (with PID it identifies the process).
	StartedAt time.Time `json:"started_at"`
	// Busy: the shell runs a command (a non-shell child process, or CPU use).
	Busy bool `json:"busy,omitzero"`
	// Since is when Busy last changed (age of the current busy / idle spell).
	Since time.Time `json:"since"`
	// Dangling: the shell is no longer under the session's agent (the session ended, or
	// the agent let go of it) but still runs.
	Dangling bool `json:"dangling,omitzero"`
}

// SubagentActivity is the live state of one sub-agent.
type SubagentActivity struct {
	// ID is the parent's tool call id that started the sub-agent.
	ID string `json:"id"`
	// Title is the sub-agent's description ("Docs for cache flags").
	Title string `json:"title,omitempty"`
	// State is thinking, working, speaking or asking; Label the running tool call or
	// the question.
	State string    `json:"state"`
	Label string    `json:"label,omitempty"`
	Since time.Time `json:"since"`
}

// SameDetail reports whether a and b show the same shells and sub-agents (ignoring
// timestamps).
func (a SessionActivity) SameDetail(b SessionActivity) bool {
	if a.Waiting != b.Waiting || a.Shells != b.Shells || a.ShellsBusy != b.ShellsBusy ||
		len(a.ShellList) != len(b.ShellList) || len(a.Subagents) != len(b.Subagents) {
		return false
	}
	for i := range a.ShellList {
		x, y := a.ShellList[i], b.ShellList[i]
		if x.PID != y.PID || x.Busy != y.Busy || x.Dangling != y.Dangling || x.Command != y.Command {
			return false
		}
	}
	for i := range a.Subagents {
		x, y := a.Subagents[i], b.Subagents[i]
		if x.ID != y.ID || x.State != y.State || x.Label != y.Label || x.Title != y.Title {
			return false
		}
	}
	return true
}

// Key identifies the session: "chat:<id>" or "task:<id>".
func (a SessionActivity) Key() string {
	if a.ChatID != "" {
		return "chat:" + a.ChatID
	}
	return "task:" + a.TaskID
}

// MsgSessionShellKill (hub → worker) terminates shells of a session (the user's Kill /
// Clean up all in Fleet). The worker kills only processes it still attributes to that
// session (same pid and start time), each with its whole process tree, and reports the
// new shell count through session.activity.
const MsgSessionShellKill = "session.shell.kill"

// SessionShellKill names the session and the shells to kill (empty Shells: all of them).
type SessionShellKill struct {
	ChatID string       `json:"chat_id,omitempty"`
	TaskID string       `json:"task_id,omitempty"`
	Shells []ShellIdent `json:"shells,omitempty"`
}

// ShellIdent identifies one shell process: pid plus start time (guards against pid reuse).
type ShellIdent struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// Key identifies the session ("chat:<id>" / "task:<id>").
func (k SessionShellKill) Key() string {
	if k.ChatID != "" {
		return "chat:" + k.ChatID
	}
	return "task:" + k.TaskID
}

// ActivityTerminal reports whether state ends the turn or task (done, error).
func ActivityTerminal(state string) bool {
	return state == ActivityDone || state == ActivityError
}
