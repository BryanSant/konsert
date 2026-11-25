package store

import (
	"context"
	"time"
)

// History retention (SYR-CSCWP; docs/design/retention.md). Live and recent work keeps
// every detail. Finished work older than the detail period is summarised: the task or
// chat record, its outcome, timing, summary and transcript stay, the bulky streamed
// events go. The fleet timeline keeps everything for a while and then only human
// actions and failures; per-call usage is folded into daily totals. Pinned ("Keep")
// work is never compacted.

// RetentionPolicy is how long each kind of history is kept in full.
type RetentionPolicy struct {
	// Detail: finished tasks and chat turns keep every event this long after they end.
	Detail time.Duration
	// Fleet: the fleet timeline is kept in full this long, then only human actions and
	// failures until FleetSummary, then deleted.
	Fleet        time.Duration
	FleetSummary time.Duration
	// UsageRaw: per-call usage is kept this long, then folded into daily totals, which
	// are kept UsageDaily.
	UsageRaw   time.Duration
	UsageDaily time.Duration
}

const day = 24 * time.Hour

// DefaultRetention is the policy unless the user changed it in Settings → Storage.
var DefaultRetention = RetentionPolicy{
	Detail:       30 * day,
	Fleet:        90 * day,
	FleetSummary: 365 * day,
	UsageRaw:     30 * day,
	UsageDaily:   365 * day,
}

// RetentionBound is the accepted range of one retention setting, in days.
type RetentionBound struct{ Min, Max int }

// RetentionBounds are the sane limits of each period (Settings → Storage).
var RetentionBounds = struct {
	Detail, Fleet, FleetSummary, UsageRaw, UsageDaily RetentionBound
}{
	Detail:       RetentionBound{7, 365},
	Fleet:        RetentionBound{14, 365},
	FleetSummary: RetentionBound{30, 3650},
	UsageRaw:     RetentionBound{7, 365},
	UsageDaily:   RetentionBound{30, 3650},
}

// TaskEventKeepKinds are the task_events kinds compaction keeps: milestones, the
// outcome, errors and every human intervention. Everything else (tool_call, file_edit,
// usage, quota and any other streamed telemetry) is dropped.
var TaskEventKeepKinds = []string{
	"milestone",     // agent progress milestones (turn completed, plan progress)
	"summary",       // the agent's own summary
	"exit",          // process exit (outcome)
	"error",         // failures
	"waiting_input", // the agent asked for the user
	"control",       // human interventions: steer, take control, input
	"steer",         // review "request changes" sent back to the agent
	"review",        // review decisions
	"delivery",      // delivery outcomes (merge, push, PR/MR)
	"command",       // operator commands run on the machine
	"syre_worktree", // where the work happened
	"git_worktree",
}

// ChatEventKeepKinds are the chat_events kinds compaction keeps: the transcript (the
// user's prompts, Syre's notes and the agent's complete replies), turn boundaries,
// errors and the agent's questions with their answers. Tool calls (with their inputs
// and outputs), reasoning, plans, usage, rate limits, sub-agent activity and status
// changes are dropped.
var ChatEventKeepKinds = []string{
	"user_message", // the user's prompt (hub-generated)
	"syre_note",    // fleet note prepended to a prompt (hub-generated)
	"message",      // the agent's complete reply
	"session_ready",
	"turn_end",
	"error",
	"elicitation", // permission requests and questions (human interventions)
	"elicitation_resolved",
}

// FleetEventKeepSQL is the SQL condition (over fleet_events columns) of the timeline
// events kept after the full period: human actions and failures.
const FleetEventKeepSQL = `(actor_kind = 'user' OR kind = 'veto' OR to_status = 'failed')`

// Compaction steps, run in this order by a pass.
const (
	CompactTasks = "tasks" // drop verbose task_events of old finished tasks
	CompactChats = "chats" // drop bulky chat_events of old chat turns
	CompactFleet = "fleet" // fleet timeline tiers
	CompactUsage = "usage" // fold per-call usage into daily totals, prune old totals
)

// CompactSteps lists every step in pass order.
var CompactSteps = []string{CompactTasks, CompactChats, CompactFleet, CompactUsage}

// CompactBatchResult is what one bounded compaction batch did.
type CompactBatchResult struct {
	// Rows is how many rows the batch deleted (or folded, for usage).
	Rows int64
	// Items is how many tasks or chats it marked compacted.
	Items int
	// Done: the step has nothing left to do as of now.
	Done bool
}

// StorageKind is one line of the storage breakdown.
type StorageKind struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Bytes int64  `json:"bytes"`
	Rows  int64  `json:"rows"`
}

// Storage breakdown kinds. Conversations are what people read back: commander
// transcripts (chat_messages) plus the agent-chat events compaction keeps
// (ChatEventKeepKinds). Session detail is every other chat event: tool calls with their
// output, reasoning, plans and streamed telemetry, the part compaction drops.
const (
	StorageConversations = "conversations"
	StorageSessionDetail = "session_detail"
	StorageTaskEvents    = "task_events"
	StorageFleet         = "fleet_timeline"
	StorageUsage         = "usage"
	StorageMachine       = "machine_history"
	StorageOther         = "other"
)

// StorageStats describes the database files and what uses the space.
type StorageStats struct {
	Path      string `json:"path"`
	FileBytes int64  `json:"file_bytes"`
	WALBytes  int64  `json:"wal_bytes"`
	// FreeBytes is space inside the file not used by any table (reclaimed by
	// incremental vacuum).
	FreeBytes int64 `json:"free_bytes"`
	PageSize  int64 `json:"page_size"`
	// Method is how the breakdown was measured: "dbstat" (exact page usage) or
	// "estimate" (row counts times typical row sizes).
	Method    string        `json:"method"`
	Breakdown []StorageKind `json:"breakdown"`
	// AutoVacuum is the database's auto_vacuum mode (incremental expected).
	AutoVacuum string    `json:"auto_vacuum"`
	MeasuredAt time.Time `json:"measured_at"`
}

// RetentionStore is the store side of history retention.
type RetentionStore interface {
	// CompactHistory runs one bounded batch of step as of now under policy p: at most
	// limit rows are deleted (or folded) in one short transaction. Repeating it until
	// Done finishes the step; every batch is atomic, so a pass interrupted anywhere
	// (restart) resumes correctly and never counts usage twice.
	CompactHistory(ctx context.Context, step string, now time.Time, p RetentionPolicy, limit int) (CompactBatchResult, error)
	// IncrementalVacuum returns up to pages free pages to the file system and reports
	// the bytes freed and how many free pages remain.
	IncrementalVacuum(ctx context.Context, pages int) (freed, remaining int64, err error)
	// CheckpointWAL folds the WAL into the database and truncates it.
	CheckpointWAL(ctx context.Context) error
	// StorageStats measures the database files and the space each kind of history uses.
	StorageStats(ctx context.Context) (StorageStats, error)
	// SetTaskKeep and SetChatKeep pin (or unpin) a task or chat: pinned work is never
	// compacted.
	SetTaskKeep(ctx context.Context, id string, keep bool) error
	SetChatKeep(ctx context.Context, id string, keep bool) error
}
