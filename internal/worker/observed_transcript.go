package worker

import (
	"context"
	"os"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/observe"
)

// Observed transcripts (SYR-XHAHC). When a browser opens an observed session's tab the
// hub asks for its transcript: the daemon reads the agent's own session file from the
// start (no copy is kept here) and, while the hub keeps renewing its watch, sends the
// session's new events as they are written.

const (
	// transcriptMaxEvents bounds a transcript reply: the latest events are kept.
	transcriptMaxEvents = 5000
	// knownSessionsMax bounds the sessions remembered for transcript requests.
	knownSessionsMax = 512
)

type transcriptWatch struct {
	s       observe.Session
	offset  int64
	seq     int64
	turn    string
	renewed time.Time
}

// remember keeps a session's location for transcript requests, also after it ends.
func (o *observedSessions) remember(s observe.Session) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.known == nil || len(o.known) >= knownSessionsMax {
		o.known = map[string]observe.Session{}
	}
	o.known[s.ID] = s
}

func (o *observedSessions) source(agent string) observe.Source {
	for _, src := range o.sources {
		if src.Agent() == agent {
			return src
		}
	}
	return nil
}

// readFrom reads s's transcript from offset to its end, numbering events after seq and
// giving each the turn of the prompt before it.
func (o *observedSessions) readFrom(ctx context.Context, s observe.Session, offset, seq int64, turn string) ([]protocol.AgentChatEvent, int64, int64, string, error) {
	src := o.source(s.Agent)
	if src == nil {
		return nil, offset, seq, turn, os.ErrNotExist
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		return nil, offset, seq, turn, err
	}
	s.Size = info.Size()
	var out []protocol.AgentChatEvent
	for offset < s.Size && ctx.Err() == nil {
		evs, next, err := src.Read(ctx, s, offset)
		if err != nil {
			return out, offset, seq, turn, err
		}
		for _, ev := range evs {
			seq++
			if ev.Kind == protocol.EventUserMessage {
				turn = ev.MessageID
			}
			ev.Seq, ev.TurnID = seq, turn
			out = append(out, ev)
		}
		if next <= offset {
			break // a record still being written: the rest next time
		}
		offset = next
	}
	return out, offset, seq, turn, nil
}

// Transcript reads an observed session's transcript and, with watch, starts streaming
// its new events.
func (o *observedSessions) Transcript(ctx context.Context, req protocol.ObservedTranscriptRequest) protocol.ObservedTranscript {
	res := protocol.ObservedTranscript{ExternalID: req.ExternalID}
	o.mu.Lock()
	s, ok := o.known[req.ExternalID]
	o.mu.Unlock()
	if !ok {
		res.Reason = "this machine doesn't know that session's transcript (it isn't a session syred reads, or syred restarted since it ended)"
		return res
	}
	evs, offset, seq, turn, err := o.readFrom(ctx, s, 0, 0, "")
	if err != nil && len(evs) == 0 {
		res.Reason = "the session's transcript can't be read: " + err.Error()
		return res
	}
	res.Available = true
	if len(evs) > transcriptMaxEvents {
		evs, res.Truncated = evs[len(evs)-transcriptMaxEvents:], true
	}
	res.Events = evs
	if req.Watch {
		o.mu.Lock()
		if o.watches == nil {
			o.watches = map[string]*transcriptWatch{}
		}
		o.watches[req.ExternalID] = &transcriptWatch{s: s, offset: offset, seq: seq, turn: turn, renewed: o.now()}
		o.mu.Unlock()
	}
	return res
}

// pollWatches sends the new events of watched sessions and drops lapsed watches.
func (o *observedSessions) pollWatches(ctx context.Context, send func(protocol.ObservedEvents)) {
	now := o.now()
	o.mu.Lock()
	var list []*transcriptWatch
	var ids []string
	for id, w := range o.watches {
		if now.Sub(w.renewed) > protocol.ObservedWatchTTL {
			delete(o.watches, id)
			continue
		}
		list = append(list, w)
		ids = append(ids, id)
	}
	o.mu.Unlock()
	for i, w := range list {
		evs, offset, seq, turn, _ := o.readFrom(ctx, w.s, w.offset, w.seq, w.turn)
		o.mu.Lock()
		w.offset, w.seq, w.turn = offset, seq, turn
		o.mu.Unlock()
		if len(evs) > 0 && send != nil {
			send(protocol.ObservedEvents{ExternalID: ids[i], Events: evs})
		}
	}
}
