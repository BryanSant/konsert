package worker

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

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
// Priority: control messages (JSON envelopes not spooled, and PTY snapshot frames) are never
// dropped and are written first; then spooled records (daemon_spool.go); then PTY data
// frames, which are dropped when their bounded queue is full.
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

// safeFileName maps a worker id to a file name (letters, digits, - and _ kept; anything
// else becomes _; "worker" when empty).
func safeFileName(workerID string) string {
	var b strings.Builder
	for _, r := range workerID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "worker"
	}
	return b.String()
}

// StateDirEnv overrides syred's state directory (worker ID, machine ID, outbound
// spool). Tests and side-by-side daemons use it to stay out of ~/.syre.
const StateDirEnv = "SYRE_STATE_DIR"

// defaultStateDir is syred's per-user state directory: $SYRE_STATE_DIR, else ~/.syre.
func defaultStateDir() string {
	if dir := strings.TrimSpace(os.Getenv(StateDirEnv)); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".syre")
}
