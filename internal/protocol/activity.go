package protocol

import "time"

// Session activity: what an agent session is doing right now (thinking, running a tool,
// writing its reply, waiting for the user, ...), for the live-work views. konsertd derives
// it from the engine stream (chat sessions) or the agent's session log (PTY tasks); the hub
// fills in what only it knows (queued, suspended) and what it learns from observed sessions.
// See docs/design/activity.md.

// MsgSessionActivity (worker->hub) carries one SessionActivity. konsertd rate-limits it
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
	ActivitySourceHook    = "hook"        // agent hooks of a session started outside Konsert
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

	// Since is when the current State (and Tool) began; At is when this was derived.
	Since time.Time `json:"since"`
	At    time.Time `json:"at"`
}

// Key identifies the session: "chat:<id>" or "task:<id>".
func (a SessionActivity) Key() string {
	if a.ChatID != "" {
		return "chat:" + a.ChatID
	}
	return "task:" + a.TaskID
}

// ActivityTerminal reports whether state ends the turn or task (done, error).
func ActivityTerminal(state string) bool {
	return state == ActivityDone || state == ActivityError
}
