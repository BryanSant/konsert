package observe

import (
	"encoding/json/v2"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// offsetRec is how far one session's transcript has been read.
type offsetRec struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
}

// offsetStore persists read offsets as one small JSON file, written atomically (temp
// file + rename) when something changed, at most once per poll.
type offsetStore struct {
	path   string
	logger *slog.Logger

	mu    sync.Mutex
	m     map[string]offsetRec
	dirty bool
}

// maxOffsets bounds the file: the oldest entries are of sessions long finished, and
// a re-read of one from the start is harmless.
const maxOffsets = 4096

func loadOffsets(path string, logger *slog.Logger) *offsetStore {
	st := &offsetStore{path: path, logger: logger, m: map[string]offsetRec{}}
	if path == "" {
		return st
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	if err := json.Unmarshal(raw, &st.m); err != nil {
		logger.Warn("observe: ignoring unreadable offsets file", "path", path, "err", err)
		st.m = map[string]offsetRec{}
	}
	return st
}

func (st *offsetStore) get(key string) (offsetRec, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	o, ok := st.m[key]
	return o, ok
}

func (st *offsetStore) set(key string, o offsetRec) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.m[key]; !ok && len(st.m) >= maxOffsets {
		clear(st.m) // see maxOffsets
	}
	st.m[key] = o
	st.dirty = true
}

func (st *offsetStore) drop(key string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.m[key]; ok {
		delete(st.m, key)
		st.dirty = true
	}
}

func (st *offsetStore) flush() {
	st.mu.Lock()
	if !st.dirty || st.path == "" {
		st.mu.Unlock()
		return
	}
	raw, err := json.Marshal(st.m, json.Deterministic(true))
	st.dirty = false
	st.mu.Unlock()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o700); err != nil {
		st.logger.Warn("observe: saving offsets failed", "err", err)
		return
	}
	tmp := st.path + ".tmp"
	err = os.WriteFile(tmp, raw, 0o600)
	if err == nil {
		err = os.Rename(tmp, st.path)
	}
	if err != nil {
		st.logger.Warn("observe: saving offsets failed", "err", err)
		st.mu.Lock()
		st.dirty = true // try again next poll
		st.mu.Unlock()
	}
}
