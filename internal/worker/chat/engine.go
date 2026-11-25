// Package chat runs agent chat sessions on syred: one long-lived engine process per
// chat, speaking the agent CLI's structured headless protocol (claude stream-json, ACP,
// codex app-server), normalized into protocol.AgentChatEvent. See docs/design/agent-chat.md.
package chat

import (
	"context"
	"strings"

	"github.com/BryanSant/syre/internal/protocol"
)

// Account is a resolved agent account: which CLI binary to run and the environment that
// selects its subscription (e.g. CLAUDE_CONFIG_DIR + CLAUDE_CODE_OAUTH_TOKEN for a second
// Claude account). Env values may contain secrets and must never be logged or sent to the hub.
type Account struct {
	ID     string
	Agent  string
	Label  string
	Binary string            // resolved executable path
	Env    map[string]string // overrides applied on top of the daemon's environment
	// ArgsPrefix is prepended to every invocation (rarely needed; e.g. a wrapper's fixed flags).
	ArgsPrefix []string
	IsDefault  bool
}

// Emit delivers one normalized event. Engines set Kind and payload fields; the session
// manager stamps ChatID, Seq and At.
type Emit func(protocol.AgentChatEvent)

// Engine knows how to talk to one agent CLI family.
type Engine interface {
	// Agent is the driver name this engine serves (claude, grok, ...). One engine type may
	// serve several agents (the ACP engine serves grok, agy, copilot, cursor).
	Agent() string

	// Probe reports the account's capabilities without sending a model prompt: login
	// state, models with effort levels, approval modes, and usage when available.
	Probe(ctx context.Context, acct Account) protocol.AgentAccount

	// Open starts (or resumes, when opts.AgentSessionID is set) a session. The returned
	// Session must emit EventSessionReady once usable, or EventError (e.g. ErrKindAuth)
	// and fail. emit stays valid until Close returns.
	Open(ctx context.Context, acct Account, opts protocol.AgentChatOpen, emit Emit) (Session, error)
}

// Session is one live engine process bound to a chat.
type Session interface {
	// Prompt starts a turn. It returns once the turn is accepted; progress, elicitations
	// and the final EventTurnEnd (or EventError) arrive through emit. Only one turn runs
	// at a time; a Prompt during a running turn returns an error.
	Prompt(ctx context.Context, p protocol.AgentChatPrompt) error

	// Respond answers a pending elicitation.
	Respond(ctx context.Context, r protocol.AgentChatRespond) error

	// Control changes model / effort / approval mode, or cancels the running turn.
	// Engines that can't change something live apply it to the next turn (by restarting
	// with the new flags and resuming the agent session) and say so via EventStatus Text.
	Control(ctx context.Context, c protocol.AgentChatControl) error

	// AgentSessionID is the CLI's own session id, for resuming after a restart.
	AgentSessionID() string

	// Close terminates the engine process (and its process group) and waits for it.
	Close() error
}

// syreToolRunning reports whether one of Syre's own MCP tools (ask_user,
// request_user_action) is still running in a turn: it waits for the user, which is not
// a stalled turn, so the watchdog stays quiet.
func syreToolRunning(tools map[string]*protocol.AgentToolCall) bool {
	for _, tc := range tools {
		if tc == nil || (tc.Status != "pending" && tc.Status != "running" && tc.Status != "") {
			continue
		}
		name := strings.ToLower(tc.Name + " " + tc.Title)
		if strings.Contains(name, "syre") && (strings.Contains(name, "ask_user") || strings.Contains(name, "request_user_action")) {
			return true
		}
	}
	return false
}
