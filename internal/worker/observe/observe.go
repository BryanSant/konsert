// Package observe makes agent sessions visible whoever started them (docs/plan.md,
// Phase 5). Agents already write their sessions to disk: claude under
// ~/.claude/projects, codex under ~/.codex/sessions, and so on. A Source per agent
// finds those sessions and turns their records into the normalized
// protocol.AgentChatEvent stream the chat engines emit, so an observed session renders
// with the same components as a managed one.
//
// The Registry polls the sources, reads each session incrementally from a byte offset
// persisted in the daemon's state dir (a restart resumes, it doesn't replay), and never
// reports a session Syre manages itself: those already reach the hub as chats or tasks.
package observe

import (
	"context"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// Session is one agent session found in an agent's own session store.
type Session struct {
	Agent string // "claude", "codex", ...
	// Account names the agent account whose store holds it ("claude2" for
	// CLAUDE_CONFIG_DIR=~/.claude2); "" for the default.
	Account string
	ID      string // the agent's own session id
	Path    string // the transcript file the source reads
	Cwd     string
	Model   string
	// Size and ModTime are the transcript's, from discovery: the registry reads a
	// session again only when either changed.
	Size    int64
	ModTime time.Time
	// PID is the agent process, when the source can tell (0 unknown). Ancestors are
	// its parent chain, nearest first, when the source read the process table: a session
	// under a process Syre manages is managed.
	PID       int
	Ancestors []int
	// Status is the session's live state as the agent itself publishes it (claude's
	// sessions/<pid>.json): a protocol.ChatStatus* value, "" when the agent publishes
	// none (status then comes from the records) and ChatStatusDone once the process that
	// published it is gone, or when the agent publishes status but no process does for
	// this session. Waiting says what a needs_you session waits for
	// ("approve Bash"). A change of either is reported even when the transcript did not
	// grow.
	Status  string
	Waiting string
	// Alive marks a session a running agent process holds although the agent publishes
	// no status (codex): its status still comes from the records, but it doesn't age out
	// while the process runs.
	Alive bool
}

// Live reports whether the session has a running agent process: one publishing its
// status, or one holding the session (Alive).
func (s Session) Live() bool {
	return s.Alive && s.Status == "" || s.Status != "" && s.Status != protocol.ChatStatusDone
}

// Key identifies a session across sources and restarts.
func (s Session) Key() string { return s.Agent + "/" + s.Account + "/" + s.ID }

// Source reads one agent's session store. Implementations must be cheap to call
// repeatedly: Discover runs every few seconds.
type Source interface {
	// Agent names the agent ("claude").
	Agent() string
	// Discover lists sessions whose transcript changed at or after since.
	Discover(ctx context.Context, since time.Time) ([]Session, error)
	// Read parses complete records of s's transcript from offset and returns their
	// events and the offset after the last complete record. A record still being
	// written is left for the next call. Read may return fewer records than are
	// available (a read limit); the registry calls again on the next poll.
	Read(ctx context.Context, s Session, offset int64) ([]protocol.AgentChatEvent, int64, error)
}

// UpdateKind says what an Update reports.
type UpdateKind int

const (
	// UpdateEvents carries new events from a session (possibly none, on first sight).
	UpdateEvents UpdateKind = iota
	// UpdateRetract withdraws a session reported earlier that turned out to be managed
	// by Syre: the hub drops its observed copy.
	UpdateRetract
)

// Update is what the registry reports for one session.
type Update struct {
	Kind    UpdateKind
	Session Session
	Events  []protocol.AgentChatEvent
}
