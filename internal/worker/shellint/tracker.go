package shellint

import (
	"crypto/subtle"
	"sort"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
)

// HistoryTTL is how long syred keeps a full (unredacted) command line in memory.
const HistoryTTL = 30 * time.Minute

// maxHistory bounds the in-memory command history across all terminals.
const maxHistory = 2000

// Command is one command with its full, unredacted text. It never leaves the daemon's
// memory and is never written to disk.
type Command struct {
	TaskID    string
	CommandID string
	Command   string
	Cwd       string
	ExitCode  *int
	StartedAt time.Time
	EndedAt   *time.Time
}

// History keeps full command lines in memory for HistoryTTL.
type History struct {
	mu    sync.Mutex
	ttl   time.Duration
	now   func() time.Time
	items map[string]*histItem
	timer *time.Timer
}

type histItem struct {
	cmd     Command
	expires time.Time
}

// NewHistory returns an empty history with the default TTL.
func NewHistory() *History {
	return &History{ttl: HistoryTTL, now: time.Now, items: make(map[string]*histItem)}
}

// put records or updates a command and restarts its TTL.
func (h *History) put(c Command) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.pruneLocked(now)
	h.items[c.CommandID] = &histItem{cmd: c, expires: now.Add(h.ttl)}
	if len(h.items) > maxHistory {
		h.evictOldestLocked(len(h.items) - maxHistory)
	}
	if h.timer == nil {
		// Expire entries even when no new commands arrive.
		h.timer = time.AfterFunc(h.ttl, h.expire)
	}
}

func (h *History) expire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.timer = nil
	now := h.now()
	h.pruneLocked(now)
	if len(h.items) == 0 {
		return
	}
	next := h.ttl
	for _, it := range h.items {
		if d := it.expires.Sub(now); d < next {
			next = d
		}
	}
	h.timer = time.AfterFunc(max(next, time.Second), h.expire)
}

func (h *History) pruneLocked(now time.Time) {
	for id, it := range h.items {
		if !now.Before(it.expires) {
			delete(h.items, id)
		}
	}
}

func (h *History) evictOldestLocked(n int) {
	list := make([]*histItem, 0, len(h.items))
	for _, it := range h.items {
		list = append(list, it)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].expires.Before(list[j].expires) })
	for _, it := range list[:n] {
		delete(h.items, it.cmd.CommandID)
	}
}

// Get returns the full command for commandID if it has not expired.
func (h *History) Get(commandID string) (Command, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	it, ok := h.items[commandID]
	if !ok || !h.now().Before(it.expires) {
		return Command{}, false
	}
	return it.cmd, true
}

// Recent returns the unexpired commands of taskID ("" = all terminals), oldest first.
func (h *History) Recent(taskID string) []Command {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	var out []Command
	for _, it := range h.items {
		if now.Before(it.expires) && (taskID == "" || it.cmd.TaskID == taskID) {
			out = append(out, it.cmd)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// Tracker turns one terminal's marks into operator-command events: one when a command
// starts (no exit code) and one when it ends, with the same CommandID. The hub-bound
// Command is redacted; the full text goes to History only.
type Tracker struct {
	taskID  string
	nonce   string
	history *History
	emit    func(protocol.OperatorCommandPayload)
	now     func() time.Time

	mu            sync.Mutex
	translateMSYS bool
	cwd           string
	pending       *string  // command line from the last valid E mark, awaiting C
	running       *Command // started, not yet finished
}

// NewTracker returns a tracker for the terminal taskID whose shell was given nonce.
// history may be nil.
func NewTracker(taskID, nonce string, history *History, emit func(protocol.OperatorCommandPayload)) *Tracker {
	return &Tracker{taskID: taskID, nonce: nonce, history: history, emit: emit, now: time.Now}
}

// SetTranslateMSYS enables MSYS-to-Windows cwd translation (used for Git Bash on Windows).
func (t *Tracker) SetTranslateMSYS(enable bool) {
	t.mu.Lock()
	t.translateMSYS = enable
	t.mu.Unlock()
}

// Handle processes marks in stream order.
func (t *Tracker) Handle(marks []Mark) {
	for _, m := range marks {
		t.handle(m)
	}
}

func (t *Tracker) handle(m Mark) {
	t.mu.Lock()
	var events []Command
	switch m.Kind {
	case MarkCwd:
		if t.nonceOK(m.Nonce) {
			cwd := m.Value
			if t.translateMSYS {
				cwd = TranslateMSYSPath(cwd)
			}
			t.cwd = cwd
		}
	case MarkCommandLine:
		if t.nonceOK(m.Nonce) {
			v := m.Value
			t.pending = &v
		}
	case MarkPreExec:
		if t.pending == nil {
			break // no trusted command line (e.g. a program printing OSC 133;C)
		}
		if t.running != nil {
			events = append(events, t.endLocked(nil))
		}
		t.running = &Command{
			TaskID:    t.taskID,
			CommandID: ids.UUID(),
			Command:   *t.pending,
			Cwd:       t.cwd,
			StartedAt: t.now().UTC(),
		}
		t.pending = nil
		events = append(events, *t.running)
	case MarkCommandDone:
		if t.running != nil {
			var code *int
			if m.HasExit {
				c := m.ExitCode
				code = &c
			}
			events = append(events, t.endLocked(code))
		}
	case MarkPromptStart:
		// A prompt without D: the command's status is unknown.
		if t.running != nil {
			events = append(events, t.endLocked(nil))
		}
		t.pending = nil
	}
	t.mu.Unlock()
	for _, c := range events {
		t.publish(c)
	}
}

// Close ends a still-running command when the terminal exits (e.g. the user typed
// `exit 3`); its exit status is the shell's.
func (t *Tracker) Close(shellExit int) {
	t.mu.Lock()
	var ev *Command
	if t.running != nil {
		code := shellExit
		c := t.endLocked(&code)
		ev = &c
	}
	t.mu.Unlock()
	if ev != nil {
		t.publish(*ev)
	}
}

func (t *Tracker) endLocked(code *int) Command {
	c := *t.running
	t.running = nil
	end := t.now().UTC()
	c.EndedAt = &end
	c.ExitCode = code
	return c
}

func (t *Tracker) nonceOK(n string) bool {
	return t.nonce != "" && subtle.ConstantTimeCompare([]byte(n), []byte(t.nonce)) == 1
}

func (t *Tracker) publish(c Command) {
	if t.history != nil {
		t.history.put(c)
	}
	if t.emit != nil {
		t.emit(protocol.OperatorCommandPayload{
			TaskID:    c.TaskID,
			CommandID: c.CommandID,
			Command:   Redact(c.Command),
			Cwd:       c.Cwd,
			ExitCode:  c.ExitCode,
			StartedAt: c.StartedAt,
			EndedAt:   c.EndedAt,
		})
	}
}
