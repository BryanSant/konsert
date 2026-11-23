package drivers

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sessionLogPollInterval is how often drivers re-read their agent's session log.
const sessionLogPollInterval = 600 * time.Millisecond

// tailJSONL calls fn for each complete line appended to path since offset and returns the
// new offset. The offset only advances past a trailing '\n': a record that is still being
// written is re-read in full on the next poll instead of being lost or split. A file that
// shrank (truncated or replaced) is read again from the start.
func tailJSONL(path string, offset int64, fn func(line []byte)) int64 {
	f, err := os.Open(path)
	if err != nil {
		return offset
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() < offset {
		offset = 0
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return offset
		}
	}
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			offset += int64(len(line))
			fn(line)
		}
		if err != nil {
			return offset
		}
	}
}

// watchLoop drives a session-log watcher: onLine sees every PTY output line, and poll runs
// on a fixed interval plus once more when the task ends (context canceled or the PTY line
// channel closed), so records written just before exit are not missed.
func watchLoop(ctx context.Context, lines <-chan string, poll func(), onLine func(string)) {
	ticker := time.NewTicker(sessionLogPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			poll()
			return
		case line, ok := <-lines:
			if !ok {
				poll()
				return
			}
			onLine(line)
		case <-ticker.C:
			poll()
		}
	}
}

// ringContains reports whether the ANSI-stripped tail of the PTY output contains needle.
// TUI dialogs are drawn with cursor addressing and no newline, so they never reach
// OutputLines and have to be found in the ring instead.
func ringContains(snapshot func() []byte, needle string) bool {
	if snapshot == nil {
		return false
	}
	raw := snapshot()
	if len(raw) > 64*1024 {
		raw = raw[len(raw)-64*1024:]
	}
	return strings.Contains(StripANSI(string(raw)), needle)
}

// envValue returns the last value of key in env, then the daemon's own environment ("" if
// unset in both).
func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], prefix); ok && v != "" {
			return v
		}
	}
	return os.Getenv(key)
}

// agentHomeDir resolves an agent's state directory: the override variable from the task env
// or the daemon env, otherwise ~/<dotDir>.
func agentHomeDir(env []string, overrideKey, dotDir string) string {
	if v := envValue(env, overrideKey); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, dotDir)
}

// scanCLIOutput inspects one PTY output line for the few things session logs never record:
// CLI usage errors (printed before a session exists) and explicit y/n confirmation prompts.
func scanCLIOutput(line string) (TelemetryEvent, bool) {
	clean := strings.TrimSpace(StripANSI(line))
	if clean == "" {
		return TelemetryEvent{}, false
	}
	lower := strings.ToLower(clean)
	switch {
	case strings.HasPrefix(lower, "error:"):
		return TelemetryEvent{Kind: "error", Message: truncateRunes(clean, 300)}, true
	case strings.Contains(lower, "[y/n]"):
		return TelemetryEvent{Kind: "waiting_input", Message: truncateRunes(clean, 300)}, true
	}
	return TelemetryEvent{}, false
}
