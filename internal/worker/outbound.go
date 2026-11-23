package worker

import (
	"encoding/json/v2"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/coder/websocket"
)

// ptyQueueSize bounds buffered live PTY data frames per connection. PTY data is the only
// traffic that may be dropped under backpressure; the terminal keeps a ring buffer and a
// re-subscribe replays a snapshot.
const ptyQueueSize = 512

type outboundMsg struct {
	msgType websocket.MessageType
	data    []byte
	// droppable marks live PTY data frames, the only messages that may be discarded.
	droppable bool
}

// connQueues holds the outbound queues of one hub connection.
//
// Priority: control messages (JSON envelopes and PTY snapshot frames) are never dropped and
// are written first; then pending final task statuses from the outbox; then PTY data frames,
// which are dropped when their bounded queue is full.
type connQueues struct {
	mu      sync.Mutex
	control []outboundMsg
	signal  chan struct{} // capacity 1: "control queue is non-empty"
	pty     chan outboundMsg
}

func newConnQueues() *connQueues {
	return &connQueues{
		signal: make(chan struct{}, 1),
		pty:    make(chan outboundMsg, ptyQueueSize),
	}
}

// push enqueues msg. It never blocks: control messages are appended to an unbounded
// queue (bounded in practice by the 10 s write timeout, after which the connection is torn
// down and the queue discarded); PTY data frames are dropped if their queue is full.
// It reports whether the message was queued.
func (q *connQueues) push(msg outboundMsg) bool {
	if msg.droppable {
		select {
		case q.pty <- msg:
			return true
		default:
			return false
		}
	}
	q.mu.Lock()
	q.control = append(q.control, msg)
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
	return true
}

// popControl removes the oldest control message, if any.
func (q *connQueues) popControl() (outboundMsg, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.control) == 0 {
		return outboundMsg{}, false
	}
	msg := q.control[0]
	q.control[0] = outboundMsg{}
	q.control = q.control[1:]
	return msg, true
}

func (q *connQueues) controlLen() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.control)
}

// statusOutbox holds final (terminal) task statuses until they have been written to a hub
// connection. It is persisted to disk when a path is set, so a status produced while
// disconnected (or just before a daemon restart) is re-sent after the next worker.hello.
type statusOutbox struct {
	mu      sync.Mutex
	path    string
	logger  *slog.Logger
	entries []outboxEntry
	seq     uint64
	signal  chan struct{} // capacity 1: "outbox has entries"
}

type outboxEntry struct {
	Seq    uint64                     `json:"seq"`
	Status protocol.TaskStatusPayload `json:"status"`
}

func newStatusOutbox(path string, logger *slog.Logger) *statusOutbox {
	o := &statusOutbox{path: path, logger: logger, signal: make(chan struct{}, 1)}
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			if err := json.Unmarshal(raw, &o.entries); err != nil {
				logger.Warn("ignoring unreadable task status outbox", "path", path, "err", err)
				o.entries = nil
			}
			for _, e := range o.entries {
				if e.Seq > o.seq {
					o.seq = e.Seq
				}
			}
		}
	}
	if len(o.entries) > 0 {
		o.notify()
	}
	return o
}

func (o *statusOutbox) notify() {
	select {
	case o.signal <- struct{}{}:
	default:
	}
}

// add records a final status, replacing any older pending status for the same task.
func (o *statusOutbox) add(p protocol.TaskStatusPayload) {
	o.mu.Lock()
	o.seq++
	kept := o.entries[:0]
	for _, e := range o.entries {
		if e.Status.TaskID != p.TaskID {
			kept = append(kept, e)
		}
	}
	o.entries = append(kept, outboxEntry{Seq: o.seq, Status: p})
	o.persistLocked()
	o.mu.Unlock()
	o.notify()
}

// pending returns a copy of the queued entries in insertion order.
func (o *statusOutbox) pending() []outboxEntry {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]outboxEntry(nil), o.entries...)
}

// remove deletes the entry with seq once it has been written; a newer status for the same
// task (different seq) is kept.
func (o *statusOutbox) remove(seq uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, e := range o.entries {
		if e.Seq == seq {
			o.entries = append(o.entries[:i], o.entries[i+1:]...)
			o.persistLocked()
			return
		}
	}
}

func (o *statusOutbox) persistLocked() {
	if o.path == "" {
		return
	}
	if len(o.entries) == 0 {
		if err := os.Remove(o.path); err != nil && !os.IsNotExist(err) {
			o.logger.Warn("remove task status outbox", "path", o.path, "err", err)
		}
		return
	}
	raw, err := json.Marshal(o.entries, protocol.JSONOptions)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(o.path), 0o700); err != nil {
		o.logger.Warn("create task status outbox dir", "path", o.path, "err", err)
		return
	}
	tmp := o.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		o.logger.Warn("write task status outbox", "path", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, o.path); err != nil {
		o.logger.Warn("persist task status outbox", "path", o.path, "err", err)
	}
}

// outboxPath returns <stateDir>/outbox-<workerID>.json, or "" if stateDir is empty.
func outboxPath(stateDir, workerID string) string {
	if stateDir == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range workerID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" {
		name = "worker"
	}
	return filepath.Join(stateDir, "outbox-"+name+".json")
}

// StateDirEnv overrides konsertd's state directory (worker ID, machine ID, status
// outbox). Tests and side-by-side daemons use it to stay out of ~/.konsert.
const StateDirEnv = "KONSERT_STATE_DIR"

// defaultStateDir is konsertd's per-user state directory: $KONSERT_STATE_DIR, else ~/.konsert.
func defaultStateDir() string {
	if dir := strings.TrimSpace(os.Getenv(StateDirEnv)); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".konsert")
}
