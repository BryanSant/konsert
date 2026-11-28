package controller

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Observed transcripts (SYR-XHAHC, SYR-XAMF6). The transcript of a session started
// outside Syre stays in the agent's own files on its machine. When a browser opens the
// session's tab the hub fetches it from the daemon, keeps it in memory, and relays the
// daemon's live events to browsers while someone keeps viewing it. Nothing of it is
// written to the database: an entry not viewed for the TTL (30 minutes by default,
// setting observed_transcript_ttl_minutes) is purged.

const (
	observedTranscriptTTLSetting = "observed_transcript_ttl_minutes"
	defaultObservedTranscriptTTL = 30 * time.Minute
	// transcriptRenewEvery renews the daemon's watch while a tab stays open (well within
	// protocol.ObservedWatchTTL).
	transcriptRenewEvery   = 2 * time.Minute
	transcriptFetchTimeout = 15 * time.Second
)

type transcriptEntry struct {
	workerID, extID, taskID string
	res                     protocol.ObservedTranscript
	viewedAt, renewedAt     time.Time
}

type transcriptCache struct {
	mu     sync.Mutex
	byTask map[string]*transcriptEntry
}

var errNotObserved = errors.New("not an observed session: its transcript is the session's own view")

// ObservedTranscript returns an observed task's transcript, fetching it from its machine
// when not cached (or renewing the machine's watch while it is viewed).
func (h *Hub) ObservedTranscript(ctx context.Context, taskID string) (protocol.ObservedTranscript, error) {
	t, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		return protocol.ObservedTranscript{}, err
	}
	if t.Origin != protocol.TaskOriginExternal || t.ExternalID == "" {
		return protocol.ObservedTranscript{}, errNotObserved
	}
	now := time.Now()
	h.transcripts.mu.Lock()
	e := h.transcripts.byTask[t.ID]
	if e != nil {
		e.viewedAt = now
	}
	fresh := e != nil && e.res.Available && now.Sub(e.renewedAt) < transcriptRenewEvery
	var cached protocol.ObservedTranscript
	if e != nil {
		cached = e.res
	}
	h.transcripts.mu.Unlock()
	if fresh {
		return cached, nil
	}
	res, err := h.fetchObservedTranscript(ctx, t)
	if err != nil {
		if e != nil && cached.Available {
			return cached, nil // keep showing what we have; the next view retries
		}
		return protocol.ObservedTranscript{ExternalID: t.ExternalID, Reason: err.Error()}, nil
	}
	h.transcripts.mu.Lock()
	if h.transcripts.byTask == nil {
		h.transcripts.byTask = map[string]*transcriptEntry{}
	}
	h.transcripts.byTask[t.ID] = &transcriptEntry{workerID: t.WorkerID, extID: t.ExternalID, taskID: t.ID, res: res, viewedAt: now, renewedAt: now}
	h.transcripts.mu.Unlock()
	return res, nil
}

func (h *Hub) fetchObservedTranscript(ctx context.Context, t store.Task) (protocol.ObservedTranscript, error) {
	if !h.IsWorkerConnected(t.WorkerID) {
		return protocol.ObservedTranscript{}, errors.New(h.workerLabel(ctx, t.WorkerID) + " isn't connected, so its sessions' transcripts can't be read now")
	}
	rctx, cancel := context.WithTimeout(ctx, transcriptFetchTimeout)
	defer cancel()
	env, err := h.RequestWorker(rctx, t.WorkerID, protocol.MsgObservedTranscript, protocol.ObservedTranscriptRequest{ExternalID: t.ExternalID, Watch: true})
	if err != nil {
		return protocol.ObservedTranscript{}, err
	}
	return protocol.DecodePayload[protocol.ObservedTranscript](env)
}

// onObservedEvents relays a watched session's new events to browsers and appends them
// to the cached transcript.
func (h *Hub) onObservedEvents(wc *workerConn, env protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.ObservedEvents](env)
	if err != nil || len(p.Events) == 0 {
		return
	}
	h.transcripts.mu.Lock()
	var taskID string
	for _, e := range h.transcripts.byTask {
		if e.workerID != wc.id || e.extID != p.ExternalID {
			continue
		}
		taskID = e.taskID
		last := int64(0)
		if n := len(e.res.Events); n > 0 {
			last = e.res.Events[n-1].Seq
		}
		for _, ev := range p.Events {
			if ev.Seq > last {
				e.res.Events = append(e.res.Events, ev)
			}
		}
		if n := len(e.res.Events); n > 2*observedEventsCap {
			e.res.Events, e.res.Truncated = e.res.Events[n-observedEventsCap:], true
		}
	}
	h.transcripts.mu.Unlock()
	if taskID == "" {
		return // nobody views it (any more)
	}
	p.TaskID = taskID
	h.broadcastJSON(protocol.MsgObservedEvents, p)
}

// observedEventsCap bounds a cached transcript in memory.
const observedEventsCap = 5000

// purgeTranscripts forgets transcripts not viewed within the TTL.
func (h *Hub) purgeTranscripts(ctx context.Context, now time.Time) int {
	ttl := defaultObservedTranscriptTTL
	if kv, err := h.store.GetAllSettings(ctx); err == nil {
		if m, err := strconv.Atoi(kv[observedTranscriptTTLSetting]); err == nil && m >= 1 && m <= 24*60 {
			ttl = time.Duration(m) * time.Minute
		}
	}
	h.transcripts.mu.Lock()
	defer h.transcripts.mu.Unlock()
	n := 0
	for id, e := range h.transcripts.byTask {
		if now.Sub(e.viewedAt) > ttl {
			delete(h.transcripts.byTask, id)
			n++
		}
	}
	return n
}

// handleObservedTranscript serves GET /api/tasks/{id}/transcript.
func (s *Server) handleObservedTranscript(w http.ResponseWriter, r *http.Request) {
	res, err := s.hub.ObservedTranscript(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, res)
	}
}
