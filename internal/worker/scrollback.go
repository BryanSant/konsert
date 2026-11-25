package worker

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Terminal scrollback on disk (SYR-VZR9F). Raw PTY bytes are not spooled (they are
// large and only the screen matters), so each PTY task's output is also kept in a bounded
// file: a re-attached terminal redraws from it even when the task has ended or syred
// restarted (the in-memory ring buffer covers a running task). Each task keeps two
// halves, <task>.log and <task>.log.1, of at most scrollbackHalf bytes each.

const (
	scrollbackHalf = 512 << 10
	// scrollbackKeep is how long a finished task's scrollback is kept.
	scrollbackKeep = 14 * 24 * time.Hour
)

// scrollbackDir is <state dir>/scrollback/<worker id> ("" without a state directory).
func scrollbackDir(stateDir, workerID string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "scrollback", safeFileName(workerID))
}

// scrollback is one task's on-disk terminal output.
type scrollback struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func scrollbackPath(dir, taskID string) string {
	return filepath.Join(dir, safeFileName(taskID)+".log")
}

// openScrollback starts (truncating) taskID's scrollback in dir; nil when dir is empty
// or the file cannot be created (best effort).
func openScrollback(dir, taskID string) *scrollback {
	if dir == "" || taskID == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	path := scrollbackPath(dir, taskID)
	_ = os.Remove(path + ".1")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil
	}
	return &scrollback{path: path, f: f}
}

// Write appends terminal output, rotating halves at scrollbackHalf.
func (s *scrollback) Write(p []byte) {
	if s == nil || len(p) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return
	}
	if len(p) > scrollbackHalf {
		p = p[len(p)-scrollbackHalf:]
	}
	if s.size+int64(len(p)) > scrollbackHalf {
		s.f.Close()
		_ = os.Rename(s.path, s.path+".1")
		f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			s.f = nil
			return
		}
		s.f, s.size = f, 0
	}
	if n, err := s.f.Write(p); err == nil {
		s.size += int64(n)
	}
}

// Close closes the file (the scrollback stays on disk).
func (s *scrollback) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
}

// readScrollback returns the last max bytes of taskID's scrollback in dir (nil when
// there is none).
func readScrollback(dir, taskID string, maxBytes int) []byte {
	if dir == "" || taskID == "" {
		return nil
	}
	path := scrollbackPath(dir, taskID)
	var out []byte
	for _, p := range []string{path + ".1", path} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(f)
		f.Close()
		out = append(out, b...)
	}
	if len(out) > maxBytes {
		out = out[len(out)-maxBytes:]
	}
	return out
}

// pruneScrollback removes scrollback files not written for keep.
func pruneScrollback(dir string, keep time.Duration) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-keep)
	for _, e := range entries {
		if e.IsDir() || !(strings.HasSuffix(e.Name(), ".log") || strings.HasSuffix(e.Name(), ".log.1")) {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
