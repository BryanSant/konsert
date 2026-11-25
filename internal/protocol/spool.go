package protocol

import (
	"strconv"
	"time"
)

// Durable outbound spool (SYR-VZR9F, docs/design/spool.md). syred appends every
// hub-bound message that matters (chat events, task events and statuses, session
// reports, activity, vitals) to an on-disk log before sending it, stamped with a
// per-daemon sequence number (Envelope.Seq). The hub applies each seq at most once,
// acknowledges what it applied, and on every (re)connect tells the daemon where to
// resume, so nothing is lost while the hub sleeps and nothing is applied twice.
const (
	// MsgWorkerSpoolResume (hub → worker), the reply to worker.hello: the hub applied
	// every record up to AppliedSeq of Epoch; the daemon replays from AppliedSeq+1.
	MsgWorkerSpoolResume = "worker.spool.resume"
	// MsgWorkerSpoolAck (hub → worker): the hub durably applied every record up to Seq
	// (cumulative). The daemon deletes acknowledged segments.
	MsgWorkerSpoolAck = "worker.spool.ack"
	// MsgWorkerSpoolCaughtUp (worker → hub, in stream order): the replay reached Seq,
	// the daemon's last record when the hub resumed it. Records up to Seq that were not
	// sent were superseded (activity collapsed to its latest state) or compacted.
	MsgWorkerSpoolCaughtUp = "worker.spool.caught_up"
	// MsgWorkerSpoolStatus (worker → hub, not spooled): the spool's size, oldest unsent
	// record and replay progress, for Fleet.
	MsgWorkerSpoolStatus = "worker.spool.status"
)

// SpoolHello is the spool section of worker.hello.
type SpoolHello struct {
	// Epoch identifies the spool: a new one (state directory wiped) restarts seqs at 1,
	// so the hub forgets what it applied for an older epoch.
	Epoch string `json:"epoch"`
	// FirstSeq is the oldest record still held (not yet acknowledged) and LastSeq the
	// newest record written (0 when none ever was).
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
}

// SpoolResume is the payload of worker.spool.resume.
type SpoolResume struct {
	// HubID is the hub's stable identity (settings hub_id). A daemon whose spool is
	// bound to a different hub archives the old spool and starts a fresh epoch instead
	// of replaying records from another hub (or a reset database).
	HubID      string `json:"hub_id,omitempty"`
	Epoch      string `json:"epoch"`
	AppliedSeq uint64 `json:"applied_seq"`
}

// SpoolAck is the payload of worker.spool.ack and worker.spool.caught_up.
type SpoolAck struct {
	Seq uint64 `json:"seq"`
	// Epoch is set on worker.spool.caught_up when the daemon archived an orphaned spool
	// and switched to a fresh epoch for this hub.
	Epoch string `json:"epoch,omitempty"`
}

// SpoolStatus describes a daemon's spool (worker.spool.status, `syred status`,
// Fleet).
type SpoolStatus struct {
	// Bytes is the spool's size on disk and BudgetBytes its budget.
	Bytes       int64 `json:"bytes"`
	BudgetBytes int64 `json:"budget_bytes"`
	// Records counts records not yet acknowledged by the hub.
	Records int `json:"records"`
	// OldestUnsentAt is when the oldest record not yet sent was written (zero: all sent).
	OldestUnsentAt time.Time `json:"oldest_unsent_at,omitzero"`
	LastSeq        uint64    `json:"last_seq"`
	AckedSeq       uint64    `json:"acked_seq"`
	// Replaying is set while records written before this connection are being sent;
	// ReplaySent of ReplayTotal are done.
	Replaying   bool   `json:"replaying,omitzero"`
	ReplaySent  uint64 `json:"replay_sent,omitzero"`
	ReplayTotal uint64 `json:"replay_total,omitzero"`
	// Full is set when the spool is over its budget even after compaction: new vitals
	// and activity are no longer kept (Dropped counts them); transcripts, task events,
	// statuses and questions still are. FullReason says which limit.
	Full       bool   `json:"full,omitzero"`
	FullReason string `json:"full_reason,omitempty"`
	Dropped    int64  `json:"dropped,omitzero"`
	// Error is the last write error (the spool fell back to sending without keeping).
	Error string `json:"error,omitempty"`
}

// ActiveChatInfo is an agent chat open on the daemon (worker.hello), so the hub does not
// fail a turn that is still running there while its events replay.
type ActiveChatInfo struct {
	ChatID     string `json:"chat_id"`
	TurnActive bool   `json:"turn_active,omitzero"`
	LastSeq    int64  `json:"last_seq"`
}

// WithSeq returns the encoded envelope env with "seq" set to seq (env must be a JSON
// object as NewEnvelope produces, without a seq).
func WithSeq(env []byte, seq uint64) []byte {
	if len(env) < 2 || env[0] != '{' {
		return env
	}
	out := make([]byte, 0, len(env)+24)
	out = append(out, `{"seq":`...)
	out = strconv.AppendUint(out, seq, 10)
	if env[1] != '}' {
		out = append(out, ',')
	}
	return append(out, env[1:]...)
}
