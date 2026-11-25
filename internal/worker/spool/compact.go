package spool

import (
	"fmt"
	"os"
	"time"
)

// Compaction levels, applied in order while the spool is over budget. Only records
// newer than the last one handed to a hub are touched (a record the hub may already have
// applied is never merged or dropped).
const (
	levelVitalsMinute = 1 + iota // vitals → 1-minute means
	levelVitalsHour              // vitals → 1-hour means
	levelActivity                // superseded activity dropped
	levelExpired                 // vitals and activity older than MaxAge dropped
	maxLevel          = levelExpired
)

// CompactResult says what one Compact run did.
type CompactResult struct {
	BytesBefore, BytesAfter int64
	Merged, Dropped         int
	// Level is the highest level that ran (0: the spool was within budget).
	Level int
	// Full is the reason the spool is still over budget ("" when it is not).
	Full string
}

// overBudgetLocked returns why the spool is over budget ("" when it is not).
func (s *Spool) overBudgetLocked() string {
	if s.bytes > s.opts.MaxBytes {
		return fmt.Sprintf("over %d MB", s.opts.MaxBytes>>20)
	}
	if i, j, ok := s.findLocked(s.meta.Acked + 1); ok {
		if age := s.opts.Now().Sub(time.Unix(0, s.segs[i].entries[j].at)); age > s.opts.MaxAge {
			return fmt.Sprintf("records older than %s", s.opts.MaxAge)
		}
	}
	return ""
}

// Compact brings the spool back within budget if it can (see the package comment) and
// sets or clears the full state.
func (s *Spool) Compact() CompactResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := CompactResult{BytesBefore: s.bytes}
	defer func() { res.BytesAfter = s.bytes }()
	if s.closed {
		return res
	}
	why := s.overBudgetLocked()
	if why == "" {
		s.full = ""
		return res
	}
	// Seal the active segment so everything unsent can be compacted.
	if n := len(s.segs); n > 0 && !s.segs[n-1].sealed && len(s.segs[n-1].entries) > 0 {
		s.sealLocked(s.segs[n-1])
	}
	for level := levelVitalsMinute; level <= maxLevel && why != ""; level++ {
		res.Level = level
		for _, g := range s.segs {
			if g.level >= level || !g.sealed || g.last() <= s.sent {
				continue
			}
			merged, dropped, err := s.rewriteLocked(g, level)
			if err != nil {
				s.log.Warn("spool: compaction failed", "segment", g.path, "level", level, "err", err)
				continue
			}
			res.Merged += merged
			res.Dropped += dropped
			if why = s.overBudgetLocked(); why == "" {
				break
			}
		}
	}
	if why != "" && s.full == "" {
		s.log.Warn("spool full: no longer keeping vitals and activity; transcripts, task events, statuses and questions are still kept", "reason", why, "bytes", s.bytes)
	}
	s.full = why
	res.Full = why
	s.notifyLocked()
	return res
}

// rewriteLocked compacts one sealed segment to level, rewriting it atomically.
func (s *Spool) rewriteLocked(g *segment, level int) (merged, dropped int, err error) {
	recs := make([]Record, 0, len(g.entries))
	for _, e := range g.entries {
		buf := make([]byte, e.size)
		if _, err := g.f.ReadAt(buf, e.off); err != nil {
			return 0, 0, err
		}
		r, ok := decode(buf)
		if !ok {
			return 0, 0, fmt.Errorf("corrupt record %d", e.seq)
		}
		recs = append(recs, r)
	}
	var out []Record
	switch level {
	case levelVitalsMinute, levelVitalsHour:
		period := time.Minute
		if level == levelVitalsHour {
			period = time.Hour
		}
		out, merged = s.mergeVitals(recs, period)
	case levelActivity:
		lastByKey := map[string]uint64{}
		for _, r := range recs {
			if r.Key != "" {
				lastByKey[r.Key] = r.Seq
			}
		}
		for _, r := range recs {
			if r.Seq > s.sent && r.Key != "" && (lastByKey[r.Key] > r.Seq || s.latest[r.Key] > r.Seq) {
				dropped++
				continue
			}
			out = append(out, r)
		}
	case levelExpired:
		cutoff := s.opts.Now().Add(-s.opts.MaxAge)
		for _, r := range recs {
			if r.Seq > s.sent && r.Class.Compactable() && r.At.Before(cutoff) {
				dropped++
				continue
			}
			out = append(out, r)
		}
	}
	g.level = level
	if merged == 0 && dropped == 0 {
		return 0, 0, nil
	}
	s.dropped += int64(dropped)
	return merged, dropped, s.replaceLocked(g, out)
}

// mergeVitals merges unsent vitals records within the same period into one record (at
// the position and seq of the group's first record, stamped with the period start).
func (s *Spool) mergeVitals(recs []Record, period time.Duration) ([]Record, int) {
	if s.opts.MergeVitals == nil {
		return recs, 0
	}
	type group struct {
		start time.Time
		idx   []int
	}
	groups := map[int64]*group{}
	var order []int64
	for i, r := range recs {
		if r.Class != ClassVitals || r.Seq <= s.sent {
			continue
		}
		start := r.At.Truncate(period)
		k := start.UnixNano()
		gr := groups[k]
		if gr == nil {
			gr = &group{start: start}
			groups[k] = gr
			order = append(order, k)
		}
		gr.idx = append(gr.idx, i)
	}
	drop := map[int]bool{}
	replace := map[int]Record{}
	merged := 0
	for _, k := range order {
		gr := groups[k]
		if len(gr.idx) < 2 {
			continue
		}
		members := make([]Record, len(gr.idx))
		for n, i := range gr.idx {
			members[n] = recs[i]
		}
		data, err := s.opts.MergeVitals(gr.start, members)
		if err != nil {
			s.log.Warn("spool: merging vitals failed", "err", err)
			continue
		}
		first := members[0]
		replace[gr.idx[0]] = Record{Seq: first.Seq, At: gr.start, Class: ClassVitals, Key: first.Key, Data: data}
		for _, i := range gr.idx[1:] {
			drop[i] = true
		}
		merged += len(gr.idx) - 1
	}
	if merged == 0 {
		return recs, 0
	}
	out := make([]Record, 0, len(recs)-merged)
	for i, r := range recs {
		if drop[i] {
			continue
		}
		if m, ok := replace[i]; ok {
			r = m
		}
		out = append(out, r)
	}
	return out, merged
}

// replaceLocked atomically replaces segment g's records with recs.
func (s *Spool) replaceLocked(g *segment, recs []Record) error {
	tmp := g.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	var entries []entry
	var off int64
	for _, r := range recs {
		buf := encode(r)
		if _, err := f.WriteAt(buf, off); err != nil {
			f.Close()
			_ = os.Remove(tmp)
			return err
		}
		entries = append(entries, entry{seq: r.Seq, at: r.At.UnixNano(), off: off, size: int64(len(buf)), class: r.Class, key: r.Key})
		off += int64(len(buf))
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// Windows cannot rename over an open file.
	g.f.Close()
	if err := os.Rename(tmp, g.path); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		if old, oerr := os.OpenFile(g.path, os.O_RDWR, 0o600); oerr == nil {
			g.f = old
		}
		return err
	}
	syncDir(s.opts.Dir)
	s.bytes += off - g.size
	// An emptied segment stays (its name orders the segments) until acknowledged.
	g.f, g.size, g.entries = f, off, entries
	return nil
}
