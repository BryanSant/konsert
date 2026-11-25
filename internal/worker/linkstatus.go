package worker

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/install"
	"github.com/BryanSant/syre/internal/protocol"
)

// Link state (SYR-G8WA3). The running daemon keeps <stateDir>/run/<worker id>.link.json
// (0600, in the user-only run directory next to the local socket) up to date with its
// link to the hub; `syred status` reads it. A file rather than a query on the local
// socket: an outbound daemon has no socket, and a stopped daemon's last state is still
// worth showing (the pid tells whether it is still running).

// Link states.
const (
	LinkConnecting   = "connecting"
	LinkConnected    = "connected"
	LinkDisconnected = "disconnected"
	// LinkWaiting: a local-socket daemon with no hub attached.
	LinkWaiting = "waiting for the hub"
)

// LinkStatus is the daemon's link to the hub as `syred status` shows it.
type LinkStatus struct {
	WorkerID string `json:"worker_id"`
	Name     string `json:"name,omitempty"`
	PID      int    `json:"pid"`
	State    string `json:"state"`
	// Transport is outbound (syred dials the hub), ssh or local (the hub attaches).
	Transport string `json:"transport"`
	Peer      string `json:"peer,omitempty"`
	// Since is when State began.
	Since       time.Time `json:"since"`
	LastError   string    `json:"last_error,omitempty"`
	NextAttempt time.Time `json:"next_attempt,omitzero"`
	// Attempts counts failed attempts since the link was last up.
	Attempts int `json:"attempts,omitzero"`
	// Spool is the outbound spool's state (nil without a spool).
	Spool     *protocol.SpoolStatus `json:"spool,omitzero"`
	UpdatedAt time.Time             `json:"updated_at"`
}

// LinkStatusPath is where the daemon with workerID keeps its link state ("" without a
// state directory or a valid id).
func LinkStatusPath(stateDir, workerID string) string {
	sock := LocalSocketPath(stateDir, workerID)
	if sock == "" {
		return ""
	}
	return strings.TrimSuffix(sock, ".sock") + ".link.json"
}

// ReadLinkStatus reads the link state written by the daemon with workerID.
func ReadLinkStatus(stateDir, workerID string) (LinkStatus, error) {
	var st LinkStatus
	path := LinkStatusPath(stateDir, workerID)
	if path == "" {
		return st, errors.New("no state directory")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(raw, &st)
	return st, err
}

// summaryEvery is the most often "still disconnected" is logged during an outage.
const summaryEvery = time.Minute

// linkTracker keeps the link state file current and keeps reconnect logging quiet: the
// first failure, then at most one summary a minute, then one line when the link is back.
type linkTracker struct {
	logger *slog.Logger
	path   string
	now    func() time.Time

	mu          sync.Mutex
	st          LinkStatus
	downSince   time.Time // zero while connected or before the first attempt failed
	lastSummary time.Time
	wasUp       bool
}

func newLinkTracker(logger *slog.Logger, path, workerID, name, transport string) *linkTracker {
	t := &linkTracker{logger: logger, path: path, now: time.Now}
	n := t.now()
	t.st = LinkStatus{WorkerID: workerID, Name: name, PID: os.Getpid(), State: LinkConnecting, Transport: transport, Since: n}
	return t
}

// status returns the current link state.
func (t *linkTracker) status() LinkStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st
}

// up records an established link and logs it: "connected" the first time, "reconnected
// after …" after an outage.
func (t *linkTracker) up(transport, peer string) {
	t.mu.Lock()
	n := t.now()
	attempts, down := t.st.Attempts, t.downSince
	t.st.State, t.st.Transport, t.st.Peer, t.st.Since = LinkConnected, transport, peer, n
	t.st.NextAttempt, t.st.Attempts, t.st.LastError = time.Time{}, 0, ""
	t.downSince, t.lastSummary, t.wasUp = time.Time{}, time.Time{}, true
	t.writeLocked(n)
	t.mu.Unlock()
	switch {
	case !down.IsZero():
		t.logger.Info(fmt.Sprintf("reconnected to the hub after %s (%s)", roundDur(n.Sub(down)), plural(attempts, "attempt")), "via", peer)
	default:
		t.logger.Info("connected to the hub", "via", peer)
	}
}

// failed records a lost link or a failed attempt, with the next attempt in next (0: at
// once), and logs the first failure of an outage and then a summary at most once a
// minute.
func (t *linkTracker) failed(err error, next time.Duration) {
	t.mu.Lock()
	n := t.now()
	msg := "link closed"
	if err != nil {
		msg = err.Error()
	}
	first := t.downSince.IsZero()
	if first {
		t.downSince, t.lastSummary = n, n
		t.st.Since = n
	}
	t.st.State, t.st.LastError = LinkDisconnected, msg
	t.st.Attempts++
	t.st.NextAttempt = n.Add(next)
	attempts, down, wasUp := t.st.Attempts, t.downSince, t.wasUp
	summary := !first && n.Sub(t.lastSummary) >= summaryEvery
	if summary {
		t.lastSummary = n
	}
	t.writeLocked(n)
	t.mu.Unlock()
	switch {
	case first && wasUp:
		t.logger.Warn("hub link lost: " + msg + "; reconnecting")
	case first:
		t.logger.Warn("cannot reach the hub: " + msg + "; retrying")
	case summary:
		t.logger.Warn(fmt.Sprintf("still disconnected after %s: %s, last error: %s", roundDur(n.Sub(down)), plural(attempts, "attempt"), msg))
	}
}

// waiting records a local-socket daemon with no hub attached.
func (t *linkTracker) waiting(lastErr string) {
	t.mu.Lock()
	n := t.now()
	t.st.State, t.st.Peer, t.st.Since, t.st.LastError = LinkWaiting, "", n, lastErr
	t.st.NextAttempt, t.st.Attempts = time.Time{}, 0
	t.writeLocked(n)
	t.mu.Unlock()
}

// stopped records that the daemon is exiting.
func (t *linkTracker) stopped() {
	t.mu.Lock()
	n := t.now()
	t.st.State, t.st.Since, t.st.NextAttempt = LinkDisconnected, n, time.Time{}
	if t.st.LastError == "" {
		t.st.LastError = "syred stopped"
	}
	t.writeLocked(n)
	t.mu.Unlock()
}

// setSpool records the spool's state (written at most every 2 s unless it changed
// shape: replaying, full).
func (t *linkTracker) setSpool(st *protocol.SpoolStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.now()
	prev := t.st.Spool
	t.st.Spool = st
	if prev != nil && st != nil && prev.Replaying == st.Replaying && prev.Full == st.Full && n.Sub(t.st.UpdatedAt) < 2*time.Second {
		return
	}
	t.writeLocked(n)
}

// forget stops writing the state file (the daemon is being uninstalled; its state goes
// with it).
func (t *linkTracker) forget() {
	t.mu.Lock()
	t.path = ""
	t.mu.Unlock()
}

// writeLocked writes the state file atomically (best effort).
func (t *linkTracker) writeLocked(n time.Time) {
	if t.path == "" {
		return
	}
	t.st.UpdatedAt = n
	raw, err := json.Marshal(t.st)
	if err != nil {
		return
	}
	dir := filepath.Dir(t.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = install.RestrictDir(dir)
	tmp, err := os.CreateTemp(dir, ".link-*.tmp")
	if err != nil {
		return
	}
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Chmod(tmp.Name(), 0o600) != nil || os.Rename(tmp.Name(), t.path) != nil {
		_ = os.Remove(tmp.Name())
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// roundDur rounds d for log lines: tenths of a second under a minute, else seconds.
func roundDur(d time.Duration) time.Duration {
	if d >= time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(100 * time.Millisecond)
}
