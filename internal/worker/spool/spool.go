// Package spool is syred's durable outbound log (SYR-VZR9F, docs/design/spool.md).
//
// Every hub-bound message that matters is appended here before it is sent, with a
// per-daemon sequence number. The log is a directory of append-only segment files; each
// record carries its seq, the time it was written, its class, an optional collapse key and
// the encoded envelope, protected by a CRC. Writes are fsynced in batches (every
// SyncEvery) and at once for statuses and questions. The hub acknowledges what it applied
// (cumulatively); fully acknowledged segments are deleted. On (re)connect the sender
// resumes from the hub's last applied seq, so records written while the hub was away (or
// before a daemon restart) are replayed in order, followed by live ones.
//
// The spool is bounded (MaxBytes, MaxAge). Over budget, unsent vitals are compacted
// first (into 1-minute, then 1-hour means), then superseded activity is dropped, then
// compactable records past MaxAge. Transcripts, task events, statuses and questions are
// never dropped: if the budget is still exceeded the spool reports itself full and stops
// keeping new vitals and activity only.
package spool

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Class is what kind of message a record holds; it decides fsync urgency and what
// compaction may touch.
type Class uint8

// Record classes.
const (
	ClassOther       Class = iota // anything else that must not be lost
	ClassChat                     // agent chat event (transcript)
	ClassQuestion                 // agent chat elicitation (question or permission)
	ClassTaskEvent                // task event (including usage and quota reports)
	ClassStatus                   // non-terminal task status
	ClassFinalStatus              // terminal task status
	ClassReport                   // session reports: external sessions, operator commands
	ClassActivity                 // session activity (collapsible by key)
	ClassVitals                   // heartbeat vitals (compactable)
)

var classNames = [...]string{"other", "chat", "question", "task_event", "status", "final_status", "report", "activity", "vitals"}

func (c Class) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return "class" + strconv.Itoa(int(c))
}

// SyncNow reports whether a record of class c is fsynced as soon as it is written.
func (c Class) SyncNow() bool {
	return c == ClassQuestion || c == ClassStatus || c == ClassFinalStatus
}

// Compactable reports whether compaction may merge or drop records of class c.
func (c Class) Compactable() bool { return c == ClassActivity || c == ClassVitals }

// Record is one spooled message.
type Record struct {
	Seq   uint64
	At    time.Time
	Class Class
	// Key collapses records: a keyed record superseded by a newer one with the same key
	// is skipped when sent and dropped by compaction.
	Key  string
	Data []byte
}

// Defaults.
const (
	DefaultMaxBytes     = 256 << 20
	DefaultMaxAge       = 14 * 24 * time.Hour
	DefaultSegmentBytes = 4 << 20
	DefaultSyncEvery    = 100 * time.Millisecond
	// seqReserve seqs are reserved (in the meta file, fsynced) ahead of use, so a seq
	// that may have been sent before a crash is never reused after it.
	seqReserve = 4096
	// keepActiveBytes: a fully acknowledged active segment is only deleted once it is
	// this large (avoids creating a file per ack).
	keepActiveBytes = 1 << 20
	// MaxOrphanedArchives is the maximum number of archived spool directories kept next
	// to the active spool after hub identity changes.
	MaxOrphanedArchives = 2
	orphanedPrefix      = "spool-orphaned-"
)

// ErrFull is returned by Append for a compactable record while the spool is full.
var ErrFull = errors.New("spool full: vitals and activity are not kept")

// ErrClosed is returned after Close.
var ErrClosed = errors.New("spool closed")

// Options configure a Spool.
type Options struct {
	// Dir holds the spool (created 0700).
	Dir string
	// MaxBytes and MaxAge are the budget (DefaultMaxBytes, DefaultMaxAge when zero).
	MaxBytes int64
	MaxAge   time.Duration
	// SegmentBytes is the size at which the active segment is sealed.
	SegmentBytes int64
	// SyncEvery is the fsync batching interval (negative: no background goroutine;
	// tests call Sync and Compact).
	SyncEvery time.Duration
	// Now is the clock (time.Now when nil).
	Now    func() time.Time
	Logger *slog.Logger
	// MergeVitals merges vitals records (in seq order) into one record's data for the
	// period starting at start. Nil: vitals are never merged.
	MergeVitals func(start time.Time, recs []Record) ([]byte, error)
}

// entry indexes one record of a segment.
type entry struct {
	seq   uint64
	at    int64 // unix nanoseconds
	off   int64
	size  int64
	class Class
	key   string
}

type segment struct {
	first   uint64 // seq in the file name
	path    string
	f       *os.File
	size    int64
	entries []entry
	sealed  bool
	level   int // highest compaction level applied
}

func (g *segment) last() uint64 {
	if len(g.entries) == 0 {
		// Empty (compacted away): keeps the segments ordered by last().
		return g.first - 1
	}
	return g.entries[len(g.entries)-1].seq
}

type meta struct {
	Epoch    string `json:"epoch"`
	HubID    string `json:"hub_id,omitempty"`
	Acked    uint64 `json:"acked"`
	Reserved uint64 `json:"reserved"`
}

// Spool is a durable, segmented, append-only outbound log. It is safe for concurrent use.
type Spool struct {
	opts Options
	log  *slog.Logger

	mu        sync.Mutex
	meta      meta
	metaDirty bool
	segs      []*segment
	next      uint64
	last      uint64 // highest seq written (or acked across restarts); 0 when none
	bytes     int64
	latest    map[string]uint64 // collapse key → newest seq
	dirty     bool              // unsynced writes in the active segment
	changed   chan struct{}
	sent      uint64 // highest seq handed to a hub (compaction only touches newer records)
	full      string // why the spool is full ("" when not)
	dropped   int64
	writeErr  error
	closed    bool
	compactCh chan struct{}
	stop      chan struct{}
	done      chan struct{}
}

// Open opens (or creates) the spool in opts.Dir, recovering from a crash: a torn record
// at the end of a segment is truncated away.
func Open(opts Options) (*Spool, error) {
	if opts.Dir == "" {
		return nil, errors.New("spool: no directory")
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = DefaultMaxAge
	}
	if opts.SegmentBytes <= 0 {
		opts.SegmentBytes = DefaultSegmentBytes
	}
	if opts.SyncEvery == 0 {
		opts.SyncEvery = DefaultSyncEvery
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, err
	}
	s := &Spool{
		opts:      opts,
		log:       opts.Logger,
		latest:    map[string]uint64{},
		changed:   make(chan struct{}),
		compactCh: make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	if err := s.loadMeta(); err != nil {
		return nil, err
	}
	if err := s.recover(); err != nil {
		s.closeFiles()
		return nil, err
	}
	// Everything recovered may have been sent before the restart: until a hub says what
	// it applied (Resumed), compaction leaves it alone.
	s.sent = s.last
	if s.next > s.meta.Reserved {
		s.meta.Reserved = s.next - 1
	}
	if err := s.reserveLocked(); err != nil {
		s.closeFiles()
		return nil, err
	}
	if opts.SyncEvery > 0 {
		go s.loop()
	} else {
		close(s.done)
	}
	return s, nil
}

func (s *Spool) metaPath() string { return filepath.Join(s.opts.Dir, "spool.json") }

func (s *Spool) loadMeta() error {
	raw, err := os.ReadFile(s.metaPath())
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.meta); err != nil {
			s.log.Warn("spool: unreadable meta file; starting a new epoch", "path", s.metaPath(), "err", err)
			s.meta = meta{}
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if s.meta.Epoch == "" {
		s.meta = meta{Epoch: randomEpoch()}
		// A new epoch: segments of an unknown one cannot be resumed.
		s.removeSegmentFiles()
	}
	return nil
}

func randomEpoch() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Spool) removeSegmentFiles() {
	names, _ := filepath.Glob(filepath.Join(s.opts.Dir, "*.seg"))
	for _, n := range names {
		_ = os.Remove(n)
	}
}

// writeMetaLocked writes the meta file atomically and durably.
func (s *Spool) writeMetaLocked() error {
	raw, err := json.Marshal(s.meta)
	if err != nil {
		return err
	}
	tmp := s.metaPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.metaPath()); err != nil {
		return err
	}
	syncDir(s.opts.Dir)
	s.metaDirty = false
	return nil
}

// reserveLocked makes sure seq s.next is covered by a durable reservation.
func (s *Spool) reserveLocked() error {
	if s.next <= s.meta.Reserved {
		return nil
	}
	old := s.meta.Reserved
	s.meta.Reserved = s.next + seqReserve
	if err := s.writeMetaLocked(); err != nil {
		s.meta.Reserved = old
		return err
	}
	return nil
}

func segName(first uint64) string { return fmt.Sprintf("%020d.seg", first) }

// recover loads every segment, truncating a torn tail, and drops fully acknowledged ones.
func (s *Spool) recover() error {
	names, err := filepath.Glob(filepath.Join(s.opts.Dir, "*.seg"))
	if err != nil {
		return err
	}
	sort.Strings(names)
	var last uint64
	for _, name := range names {
		first, err := strconv.ParseUint(strings.TrimSuffix(filepath.Base(name), ".seg"), 10, 64)
		if err != nil {
			continue
		}
		g, err := s.loadSegment(name, first)
		if err != nil {
			return err
		}
		if len(g.entries) == 0 || g.last() <= s.meta.Acked || g.last() <= last {
			g.f.Close()
			_ = os.Remove(name)
			continue
		}
		g.sealed = true
		last = g.last()
		s.segs = append(s.segs, g)
		s.bytes += g.size
		for _, e := range g.entries {
			if e.key != "" && e.seq > s.meta.Acked {
				s.latest[e.key] = e.seq
			}
		}
	}
	s.last = max(last, s.meta.Acked)
	s.next = max(last, s.meta.Reserved, s.meta.Acked) + 1
	return nil
}

// Record layout: len u32 | crc32c u32 | body. Body: seq u64 | at i64 | class u8 |
// keylen u16 | key | data. len is the body length; the CRC covers the body.
const headerLen = 8

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func encode(r Record) []byte {
	bodyLen := 8 + 8 + 1 + 2 + len(r.Key) + len(r.Data)
	buf := make([]byte, headerLen+bodyLen)
	body := buf[headerLen:]
	binary.LittleEndian.PutUint64(body[0:], r.Seq)
	binary.LittleEndian.PutUint64(body[8:], uint64(r.At.UnixNano()))
	body[16] = byte(r.Class)
	binary.LittleEndian.PutUint16(body[17:], uint16(len(r.Key)))
	copy(body[19:], r.Key)
	copy(body[19+len(r.Key):], r.Data)
	binary.LittleEndian.PutUint32(buf[0:], uint32(bodyLen))
	binary.LittleEndian.PutUint32(buf[4:], crc32.Checksum(body, castagnoli))
	return buf
}

// decode parses one record from buf (exactly one record). ok is false on a CRC or
// layout error.
func decode(buf []byte) (Record, bool) {
	if len(buf) < headerLen+19 {
		return Record{}, false
	}
	n := binary.LittleEndian.Uint32(buf[0:])
	if int(n) != len(buf)-headerLen {
		return Record{}, false
	}
	body := buf[headerLen:]
	if crc32.Checksum(body, castagnoli) != binary.LittleEndian.Uint32(buf[4:]) {
		return Record{}, false
	}
	kl := int(binary.LittleEndian.Uint16(body[17:]))
	if 19+kl > len(body) {
		return Record{}, false
	}
	return Record{
		Seq:   binary.LittleEndian.Uint64(body[0:]),
		At:    time.Unix(0, int64(binary.LittleEndian.Uint64(body[8:]))),
		Class: Class(body[16]),
		Key:   string(body[19 : 19+kl]),
		Data:  append([]byte(nil), body[19+kl:]...),
	}, true
}

// maxRecord bounds a record's body (a corrupt length beyond it is a torn tail).
const maxRecord = 64 << 20

// loadSegment opens and indexes a segment file, truncating it after the last good record.
func (s *Spool) loadSegment(path string, first uint64) (*segment, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	g := &segment{first: first, path: path, f: f}
	var off int64
	var prev uint64
	hdr := make([]byte, headerLen)
	for off < st.Size() {
		if _, err := f.ReadAt(hdr, off); err != nil {
			break
		}
		n := int64(binary.LittleEndian.Uint32(hdr[0:]))
		if n < 19 || n > maxRecord || off+headerLen+n > st.Size() {
			break
		}
		buf := make([]byte, headerLen+n)
		if _, err := f.ReadAt(buf, off); err != nil {
			break
		}
		r, ok := decode(buf)
		if !ok || r.Seq <= prev {
			break
		}
		prev = r.Seq
		g.entries = append(g.entries, entry{seq: r.Seq, at: r.At.UnixNano(), off: off, size: headerLen + n, class: r.Class, key: r.Key})
		off += headerLen + n
	}
	if off < st.Size() {
		s.log.Warn("spool: truncating a torn or corrupt tail", "segment", filepath.Base(path), "kept_bytes", off, "dropped_bytes", st.Size()-off)
		if err := f.Truncate(off); err != nil {
			f.Close()
			return nil, err
		}
		_ = f.Sync()
	}
	g.size = off
	return g, nil
}

// activeLocked returns the segment to append to, sealing a full one.
func (s *Spool) activeLocked() (*segment, error) {
	if n := len(s.segs); n > 0 {
		g := s.segs[n-1]
		if !g.sealed && g.size < s.opts.SegmentBytes {
			return g, nil
		}
		if !g.sealed {
			s.sealLocked(g)
		}
	}
	path := filepath.Join(s.opts.Dir, segName(s.next))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	syncDir(s.opts.Dir)
	g := &segment{first: s.next, path: path, f: f}
	s.segs = append(s.segs, g)
	return g, nil
}

func (s *Spool) sealLocked(g *segment) {
	if g.sealed {
		return
	}
	if s.dirty {
		_ = g.f.Sync()
		s.dirty = false
	}
	g.sealed = true
}

// Append writes a record and returns its seq. Records of a class that SyncNow are on
// disk when Append returns; others within SyncEvery.
func (s *Spool) Append(class Class, key string, data []byte) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	if s.full != "" && class.Compactable() {
		s.dropped++
		return 0, ErrFull
	}
	if err := s.reserveLocked(); err != nil {
		s.writeErr = err
		return 0, err
	}
	g, err := s.activeLocked()
	if err != nil {
		s.writeErr = err
		return 0, err
	}
	r := Record{Seq: s.next, At: s.opts.Now(), Class: class, Key: key, Data: data}
	buf := encode(r)
	if _, err := g.f.WriteAt(buf, g.size); err != nil {
		_ = g.f.Truncate(g.size)
		s.writeErr = err
		return 0, err
	}
	g.entries = append(g.entries, entry{seq: r.Seq, at: r.At.UnixNano(), off: g.size, size: int64(len(buf)), class: class, key: key})
	g.size += int64(len(buf))
	s.bytes += int64(len(buf))
	s.last = r.Seq
	s.next++
	if key != "" {
		s.latest[key] = r.Seq
	}
	s.dirty = true
	if class.SyncNow() {
		if err := g.f.Sync(); err != nil {
			s.writeErr = err
		}
		s.dirty = false
	}
	s.writeErr = nil
	s.notifyLocked()
	if s.overBudgetLocked() != "" {
		select {
		case s.compactCh <- struct{}{}:
		default:
		}
	}
	return r.Seq, nil
}

func (s *Spool) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Changed returns a channel closed at the next Append or compaction.
func (s *Spool) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// findLocked returns the segment index and entry index of the first record with
// seq >= from (ok false when there is none).
func (s *Spool) findLocked(from uint64) (int, int, bool) {
	i := sort.Search(len(s.segs), func(i int) bool { return s.segs[i].last() >= from })
	for ; i < len(s.segs); i++ {
		es := s.segs[i].entries
		j := sort.Search(len(es), func(j int) bool { return es[j].seq >= from })
		if j < len(es) {
			return i, j, true
		}
	}
	return 0, 0, false
}

// Read returns up to maxRecords records with seq >= from (at most about maxBytes),
// skipping records superseded by a newer one with the same key, and the seq to read from
// next (past every record examined, including skipped ones).
func (s *Spool) Read(from uint64, maxRecords int, maxBytes int64) ([]Record, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := from
	i, j, ok := s.findLocked(from)
	if !ok {
		return nil, max(from, s.next)
	}
	var out []Record
	var size int64
	for ; i < len(s.segs); i, j = i+1, 0 {
		g := s.segs[i]
		for ; j < len(g.entries); j++ {
			if len(out) >= maxRecords || (len(out) > 0 && size >= maxBytes) {
				return out, next
			}
			e := g.entries[j]
			next = e.seq + 1
			if e.key != "" && s.latest[e.key] > e.seq {
				continue // superseded: only the latest state matters
			}
			buf := make([]byte, e.size)
			if _, err := g.f.ReadAt(buf, e.off); err != nil && !errors.Is(err, io.EOF) {
				s.log.Warn("spool: read failed; skipping record", "seq", e.seq, "err", err)
				continue
			}
			r, ok := decode(buf)
			if !ok {
				s.log.Warn("spool: corrupt record; skipping it", "seq", e.seq)
				continue
			}
			out = append(out, r)
			size += e.size
		}
	}
	return out, max(next, s.next)
}

// MarkSent records that every record up to seq was handed to a hub.
func (s *Spool) MarkSent(seq uint64) {
	s.mu.Lock()
	s.sent = max(s.sent, seq)
	s.mu.Unlock()
}

// Resumed records that a hub applied every record up to applied: newer records were
// never applied, so compaction may touch those not sent again since.
func (s *Spool) Resumed(applied uint64) {
	s.mu.Lock()
	s.sent = applied
	s.mu.Unlock()
	s.Ack(applied)
}

// Ack records that the hub applied every record up to seq and deletes fully
// acknowledged segments.
func (s *Spool) Ack(seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || seq <= s.meta.Acked {
		return
	}
	seq = min(seq, s.last)
	if seq <= s.meta.Acked {
		return
	}
	s.meta.Acked = seq
	s.metaDirty = true
	kept := s.segs[:0]
	for i, g := range s.segs {
		active := i == len(s.segs)-1 && !g.sealed
		if g.last() <= seq && (len(g.entries) == 0 && g.sealed || len(g.entries) > 0 && (!active || g.size >= keepActiveBytes)) {
			s.bytes -= g.size
			g.f.Close()
			_ = os.Remove(g.path)
			continue
		}
		kept = append(kept, g)
	}
	for i := len(kept); i < len(s.segs); i++ {
		s.segs[i] = nil
	}
	s.segs = kept
	for k, v := range s.latest {
		if v <= seq {
			delete(s.latest, k)
		}
	}
	if s.full != "" && s.overBudgetLocked() == "" {
		s.full = ""
	}
}

// Sync flushes unsynced writes and the meta file.
func (s *Spool) Sync() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()
}

func (s *Spool) syncLocked() {
	if s.dirty && len(s.segs) > 0 {
		if err := s.segs[len(s.segs)-1].f.Sync(); err != nil {
			s.writeErr = err
		}
		s.dirty = false
	}
	if s.metaDirty {
		if err := s.writeMetaLocked(); err != nil {
			s.writeErr = err
		}
	}
}

func (s *Spool) loop() {
	defer close(s.done)
	t := time.NewTicker(s.opts.SyncEvery)
	defer t.Stop()
	// Compaction runs when an append finds the spool over budget, at most this often.
	const compactEvery = 5 * time.Second
	var lastCompact time.Time
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.Sync()
		case <-s.compactCh:
			if time.Since(lastCompact) < compactEvery {
				go func() {
					time.Sleep(compactEvery)
					select {
					case s.compactCh <- struct{}{}:
					default:
					}
				}()
				continue
			}
			lastCompact = time.Now()
			s.Compact()
		}
	}
}

// Close syncs and closes the spool.
func (s *Spool) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.syncLocked()
	s.closed = true
	s.closeFiles()
	s.mu.Unlock()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
	return nil
}

func (s *Spool) closeFiles() {
	for _, g := range s.segs {
		if g.f != nil {
			g.f.Close()
			g.f = nil
		}
	}
}

// Epoch identifies this spool (see protocol.SpoolHello).
func (s *Spool) Epoch() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta.Epoch
}

// HubID returns the hub identity this spool is bound to ("" when never bound).
func (s *Spool) HubID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta.HubID
}

// BindHubResult describes what BindHub did.
type BindHubResult struct {
	// Archived is true when the existing spool belonged to a different hub and was
	// archived in favor of a fresh epoch.
	Archived   bool
	OldHubID   string
	ArchiveDir string
	NewEpoch   string
}

// BindHub binds the spool to hubID. If the spool is already bound to hubID, nothing
// changes. If it has never been bound, it is tagged in place and keeps its records for
// replay. Otherwise (it belonged to a different hub), all open segment files are closed,
// the spool directory is renamed to spool-orphaned-<oldHubID>-<timestamp> (keeping at most
// MaxOrphanedArchives), and a fresh epoch bound to hubID is started.
func (s *Spool) BindHub(hubID string) (BindHubResult, error) {
	hubID = strings.TrimSpace(hubID)
	if hubID == "" {
		return BindHubResult{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return BindHubResult{}, ErrClosed
	}
	if s.meta.HubID == hubID {
		return BindHubResult{}, nil
	}
	// A spool never bound to any hub (a fresh daemon whose first records were written
	// before it learned the hub's id) belongs to the first hub it meets: adopt it and keep
	// the records for replay. Only a spool bound to a different hub is archived.
	if s.meta.HubID == "" {
		s.meta.HubID = hubID
		if err := s.writeMetaLocked(); err != nil {
			s.meta.HubID = ""
			return BindHubResult{}, err
		}
		return BindHubResult{}, nil
	}

	oldHub := s.meta.HubID
	s.syncLocked()
	// Windows cannot rename a directory containing open files: close every segment first.
	s.closeFiles()

	parent := filepath.Dir(s.opts.Dir)
	label := sanitizeHubLabel(oldHub)
	ts := s.opts.Now().UTC().Format("20060102T150405.000000000Z")
	archiveDir := uniqueArchivePath(parent, fmt.Sprintf("%s%s-%s", orphanedPrefix, label, ts))
	if err := os.Rename(s.opts.Dir, archiveDir); err != nil {
		// Reopen segments so the spool stays usable on failure.
		s.segs = nil
		s.bytes = 0
		s.latest = map[string]uint64{}
		_ = s.recover()
		return BindHubResult{}, err
	}
	if err := os.MkdirAll(s.opts.Dir, 0o700); err != nil {
		return BindHubResult{}, err
	}

	newEpoch := randomEpoch()
	s.meta = meta{Epoch: newEpoch, HubID: hubID}
	s.metaDirty = false
	s.segs = nil
	s.next = 1
	s.last = 0
	s.bytes = 0
	s.latest = map[string]uint64{}
	s.dirty = false
	s.sent = 0
	s.full = ""
	s.dropped = 0
	s.writeErr = nil
	if err := s.reserveLocked(); err != nil {
		return BindHubResult{}, err
	}
	pruneOrphanedArchives(parent, MaxOrphanedArchives)
	syncDir(parent)
	s.notifyLocked()
	return BindHubResult{
		Archived:   true,
		OldHubID:   oldHub,
		ArchiveDir: archiveDir,
		NewEpoch:   newEpoch,
	}, nil
}

func sanitizeHubLabel(hubID string) string {
	if hubID == "" {
		return "unknown"
	}
	var b strings.Builder
	b.Grow(len(hubID))
	for i := 0; i < len(hubID); i++ {
		c := hubID[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func uniqueArchivePath(parent, base string) string {
	p := filepath.Join(parent, base)
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return p
	}
	for i := 1; ; i++ {
		cand := filepath.Join(parent, fmt.Sprintf("%s.%d", base, i))
		if _, err := os.Stat(cand); errors.Is(err, os.ErrNotExist) {
			return cand
		}
	}
}

func archiveSortKey(name string) string {
	if idx := strings.LastIndexByte(name, '-'); idx >= 0 && idx+1 < len(name) {
		return name[idx+1:]
	}
	return name
}

func pruneOrphanedArchives(parent string, keep int) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	var archives []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), orphanedPrefix) {
			archives = append(archives, e.Name())
		}
	}
	if len(archives) <= keep {
		return
	}
	sort.Slice(archives, func(i, j int) bool {
		ki, kj := archiveSortKey(archives[i]), archiveSortKey(archives[j])
		if ki != kj {
			return ki < kj
		}
		return archives[i] < archives[j]
	})
	for _, name := range archives[:len(archives)-keep] {
		_ = os.RemoveAll(filepath.Join(parent, name))
	}
}

// Bounds returns the oldest record not yet acknowledged (the next seq when none) and the
// newest record written (0 when none ever was).
func (s *Spool) Bounds() (first, last uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	first = s.next
	if i, j, ok := s.findLocked(s.meta.Acked + 1); ok {
		first = s.segs[i].entries[j].seq
	}
	return first, s.last
}

// Status is the spool's state (replay fields are the sender's).
type Status struct {
	Bytes          int64
	BudgetBytes    int64
	Records        int
	OldestUnsentAt time.Time
	LastSeq        uint64
	AckedSeq       uint64
	Full           string
	Dropped        int64
	Err            error
}

// Status returns the spool's current state.
func (s *Spool) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Bytes: s.bytes, BudgetBytes: s.opts.MaxBytes, LastSeq: s.last, AckedSeq: s.meta.Acked,
		Full: s.full, Dropped: s.dropped, Err: s.writeErr}
	for _, g := range s.segs {
		for _, e := range g.entries {
			if e.seq > s.meta.Acked {
				st.Records++
			}
		}
	}
	if i, j, ok := s.findLocked(s.sent + 1); ok {
		st.OldestUnsentAt = time.Unix(0, s.segs[i].entries[j].at)
	}
	return st
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
