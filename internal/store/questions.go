package store

import "time"

// Questions (PRD §5.8b): every question an agent asks the user — ask_user calls, the
// agents' own question and permission prompts, and open requests — is a persistent hub
// object in one fleet-wide queue. It stays open until it is answered (reloads, side
// conversations and abandoned tool calls don't dismiss it).

// Question statuses.
const (
	QuestionOpen      = "open"
	QuestionAnswered  = "answered"
	QuestionDelegated = "delegated" // waiting for the commander to decide
	QuestionExpired   = "expired"   // the agent no longer needs an answer (its prompt went away)
	QuestionCancelled = "cancelled" // the user stopped the turn, the chat was deleted, ...
)

// Question kinds.
const (
	QuestionKindQuestion   = "question"   // a decision or information
	QuestionKindPermission = "permission" // a tool approval from the agent itself
)

// Question sources.
const (
	QuestionSourceAskUser     = "ask_user"    // Syre's ask_user MCP tool (validated)
	QuestionSourceElicitation = "elicitation" // the agent's own prompt (permission, AskUserQuestion, ACP)
)

// Answer delivery paths.
const (
	DeliveryTool   = "tool"   // returned to the agent's pending tool call
	DeliveryWorker = "worker" // forwarded to the agent's own pending prompt
	DeliveryNote   = "note"   // the tool call was abandoned: told before the agent's next turn
)

// QuestionOption is one choice with what happens if the user picks it.
type QuestionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Consequence string `json:"consequence,omitempty"`
	Recommended bool   `json:"recommended,omitzero"`
	// Kind hints the UI for permission prompts: allow_once, allow_always, reject_once, ...
	Kind string `json:"kind,omitempty"`
}

// QuestionRef is a reference found in a question (ticket, PR, file, Syre sid),
// resolved into a card when possible.
type QuestionRef struct {
	Kind     string `json:"kind"` // ticket | pr | file | task | chat | machine | question
	Ref      string `json:"ref"`  // as written: SYR-2Q0CH, #123, internal/x.go, t-7qk3m
	Title    string `json:"title,omitempty"`
	Status   string `json:"status,omitempty"`
	Excerpt  string `json:"excerpt,omitempty"`
	URL      string `json:"url,omitempty"`
	Resolved bool   `json:"resolved"`
}

// QuestionAnswer is how a question was answered and delivered.
type QuestionAnswer struct {
	OptionID string `json:"option_id,omitempty"`
	Label    string `json:"label,omitempty"` // the option's label (empty for free text)
	Text     string `json:"text,omitempty"`  // free text (or a note added to an option)
	// Summary of the side conversation, delivered with the answer.
	Summary string    `json:"summary,omitempty"`
	By      Cause     `json:"by"`
	At      time.Time `json:"at"`
	// Delivery is how it reached the agent (tool, worker, note); DeliveredAt is when.
	Delivery    string     `json:"delivery,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitzero"`
	// Correction is set when the answer replaced an earlier (delegated) one that the
	// agent had already received: it is delivered as a correction note.
	Correction bool `json:"correction,omitzero"`
	// Preparing is set while the side-conversation summary is being written; the answer
	// is delivered once it is ready.
	Preparing bool `json:"preparing,omitzero"`
}

// Delegation states.
const (
	DelegationPending    = "pending"    // waiting for the commander
	DelegationDecided    = "decided"    // the commander chose; held briefly so the user can override
	DelegationDelivered  = "delivered"  // sent to the agent
	DelegationOverridden = "overridden" // the user replaced the commander's choice
	DelegationFailed     = "failed"     // no commander could decide; back in the user's queue
)

// QuestionDelegation is the receipt of "Let the commander decide".
type QuestionDelegation struct {
	State       string    `json:"state"`
	RequestedAt time.Time `json:"requested_at"`
	RequestedBy Cause     `json:"requested_by"`
	Auto        bool      `json:"auto,omitzero"` // by an auto-delegate rule
	ChatID      string    `json:"chat_id,omitempty"`
	// SentAt is when the question was handed to the commander (a turn started).
	SentAt    *time.Time `json:"sent_at,omitzero"`
	OptionID  string     `json:"option_id,omitempty"`
	Label     string     `json:"label,omitempty"`
	Text      string     `json:"text,omitempty"`
	Rationale string     `json:"rationale,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitzero"`
	// DeliverAt is when a decided answer goes to the agent unless the user overrides it.
	DeliverAt *time.Time `json:"deliver_at,omitzero"`
	Note      string     `json:"note,omitempty"`
}

// Thread answerers.
const (
	AnswererFork      = "fork"      // a read-only fork of the asking agent's session
	AnswererCommander = "commander" // the commander's agent with a context pack
)

// QuestionThread is the side conversation ("Ask about this") attached to a question.
type QuestionThread struct {
	// ChatID is the hidden chat that answers (its events stay out of the chat list).
	ChatID   string `json:"chat_id"`
	Answerer string `json:"answerer"`
	// Agent/AccountID/WorkerID: where the answerer runs (shown in the UI).
	Agent     string `json:"agent,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	WorkerID  string `json:"worker_id,omitempty"`
	// Status: working while the answerer is replying, idle otherwise, failed.
	Status string `json:"status"`
	// Note explains a fallback ("claude can't fork this session: …").
	Note string `json:"note,omitempty"`
	// Draft is the answer the answerer proposes ("Use this answer").
	Draft     *QuestionDraft `json:"draft,omitzero"`
	StartedAt time.Time      `json:"started_at"`
	// Discarded is set once the answerer chat was closed after the question ended.
	Discarded bool `json:"discarded,omitzero"`
}

// QuestionDraft is a proposed answer from the side conversation.
type QuestionDraft struct {
	OptionID string `json:"option_id,omitempty"`
	Label    string `json:"label,omitempty"`
	Text     string `json:"text,omitempty"`
}

// QuestionMessage is one message of a side conversation.
type QuestionMessage struct {
	QuestionID string    `json:"question_id"`
	Seq        int64     `json:"seq"`
	Role       string    `json:"role"` // user | answerer | system
	Text       string    `json:"text"`
	TurnID     string    `json:"turn_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Question is one question an agent asked the user.
type Question struct {
	ID       string `json:"id"`
	SID      string `json:"sid"`
	ChatID   string `json:"chat_id"`
	TaskID   string `json:"task_id,omitempty"`
	WorkerID string `json:"worker_id,omitempty"`
	Agent    string `json:"agent,omitempty"`
	// AccountID is the worker-local account of the asking session.
	AccountID string `json:"account_id,omitempty"`
	Kind      string `json:"kind"`
	Source    string `json:"source"`
	// ElicitationID is the chat elicitation showing this question inline.
	ElicitationID  string           `json:"elicitation_id,omitempty"`
	Prompt         string           `json:"prompt"`
	Context        string           `json:"context,omitempty"`
	WhyNow         string           `json:"why_now,omitempty"`
	Options        []QuestionOption `json:"options,omitempty"`
	AllowText      bool             `json:"allow_text"`
	Recommendation string           `json:"recommendation,omitempty"` // the recommended option's id
	Rationale      string           `json:"rationale,omitempty"`
	Category       string           `json:"category,omitempty"`
	// Blocks lists what waits on the answer (declared by the agent and derived).
	Blocks   []string      `json:"blocks,omitempty"`
	GroupKey string        `json:"group_key,omitempty"`
	Refs     []QuestionRef `json:"refs,omitempty"`
	Status   string        `json:"status"`
	// BlockedSince is when the agent first asked (re-asks keep it).
	BlockedSince time.Time  `json:"blocked_since"`
	SnoozedUntil *time.Time `json:"snoozed_until,omitzero"`
	// Abandoned: the agent's tool call ended without an answer; the answer is delivered
	// as a note before its next turn.
	Abandoned bool `json:"abandoned,omitzero"`
	// AskCount counts identical re-asks mapped to this card; Rejections counts
	// under-specified attempts rejected before it was accepted.
	AskCount   int                 `json:"ask_count"`
	Rejections int                 `json:"rejections,omitzero"`
	Answer     *QuestionAnswer     `json:"answer,omitzero"`
	Delegation *QuestionDelegation `json:"delegation,omitzero"`
	Thread     *QuestionThread     `json:"thread,omitzero"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
	ClosedAt   *time.Time          `json:"closed_at,omitzero"`
}

// Pending reports whether the question still waits for a decision (open or delegated).
func (q Question) Pending() bool {
	return q.Status == QuestionOpen || q.Status == QuestionDelegated
}

// QuestionFilter selects questions, newest first. Zero values mean no filter.
type QuestionFilter struct {
	// Statuses limits to these statuses ("" entries ignored).
	Statuses []string
	ChatID   string
	// UndeliveredOnly keeps answered questions whose answer has not reached the agent.
	UndeliveredOnly bool
	Since           time.Time
	Limit           int
}

// QuestionReject records an under-specified ask_user call that was sent back to the agent.
type QuestionReject struct {
	ID        int64     `json:"id"`
	ChatID    string    `json:"chat_id"`
	Agent     string    `json:"agent,omitempty"`
	Missing   string    `json:"missing"` // comma-separated fields
	CreatedAt time.Time `json:"created_at"`
}
