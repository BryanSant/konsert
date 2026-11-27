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
	// PID is the agent process, when the source can tell (0 unknown).
	PID int
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
