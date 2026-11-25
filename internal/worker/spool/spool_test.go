package spool

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)} }

// vitals records hold "v:<samples>"; merging sums the samples.
func mergeTest(_ time.Time, recs []Record) ([]byte, error) {
	n := 0
	for _, r := range recs {
		k, err := strconv.Atoi(strings.TrimPrefix(string(r.Data), "v:"))
		if err != nil {
			return nil, err
		}
		n += k
	}
	return []byte("v:" + strconv.Itoa(n)), nil
}

func open(t *testing.T, dir string, c *clock, mut func(*Options)) *Spool {
	t.Helper()
	o := Options{Dir: dir, SyncEvery: -1, Now: c.now, MergeVitals: mergeTest,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if mut != nil {
		mut(&o)
	}
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustAppend(t *testing.T, s *Spool, class Class, key, data string) uint64 {
	t.Helper()
	seq, err := s.Append(class, key, []byte(data))
	if err != nil {
		t.Fatalf("append %q: %v", data, err)
	}
	return seq
}

// readAll reads every record from from on.
func readAll(s *Spool, from uint64) []Record {
	var out []Record
	for {
		recs, next := s.Read(from, 7, 1<<20)
		out = append(out, recs...)
		if len(recs) == 0 {
			return out
		}
		from = next
	}
}

func datas(recs []Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = string(r.Data)
	}
	return out
}

func TestAppendReadAckReopen(t *testing.T) {
	dir, c := t.TempDir(), newClock()
	s := open(t, dir, c, nil)
	for i := 1; i <= 10; i++ {
		if seq := mustAppend(t, s, ClassChat, "", fmt.Sprintf("m%d", i)); seq != uint64(i) {
			t.Fatalf("seq %d, want %d", seq, i)
		}
		c.add(time.Second)
	}
	got := readAll(s, 1)
	if len(got) != 10 || got[0].Seq != 1 || got[9].Seq != 10 || string(got[4].Data) != "m5" || got[4].Class != ClassChat {
		t.Fatalf("read %v", datas(got))
	}
	if !got[1].At.Equal(got[0].At.Add(time.Second)) {
		t.Fatalf("timestamps not kept: %v %v", got[0].At, got[1].At)
	}
	s.Ack(5)
	if first, last := s.Bounds(); first != 6 || last != 10 {
		t.Fatalf("bounds %d..%d, want 6..10", first, last)
	}
	epoch := s.Epoch()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, dir, c, nil)
	defer s.Close()
	if s.Epoch() != epoch {
		t.Fatal("epoch changed across reopen")
	}
	got = readAll(s, 6)
	if len(got) != 5 || got[0].Seq != 6 || string(got[4].Data) != "m10" {
		t.Fatalf("after reopen read %v", datas(got))
	}
	// A seq is never reused after a restart (it may have been sent before a crash).
	if seq := mustAppend(t, s, ClassChat, "", "m11"); seq <= 10 {
		t.Fatalf("seq %d reused after reopen", seq)
	}
	if first, _ := s.Bounds(); first != 6 {
		t.Fatalf("first unacked %d, want 6", first)
	}
}

func TestRotationAndAckDeletesSegments(t *testing.T) {
	dir, c := t.TempDir(), newClock()
	s := open(t, dir, c, func(o *Options) { o.SegmentBytes = 300 })
	defer s.Close()
	for i := 1; i <= 60; i++ {
		mustAppend(t, s, ClassTaskEvent, "", fmt.Sprintf("event-%03d-%s", i, strings.Repeat("x", 20)))
	}
	segs, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	if len(segs) < 5 {
		t.Fatalf("expected rotation into several segments, got %d", len(segs))
	}
	if got := readAll(s, 1); len(got) != 60 {
		t.Fatalf("read %d across segments, want 60", len(got))
	}
	s.Ack(45)
	after, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	if len(after) >= len(segs) {
		t.Fatalf("acked segments not deleted: %d → %d", len(segs), len(after))
	}
	got := readAll(s, 46)
	if len(got) != 15 || got[0].Seq != 46 {
		t.Fatalf("after ack read %d from %d", len(got), got[0].Seq)
	}
	st := s.Status()
	if st.Records != 15 || st.AckedSeq != 45 || st.LastSeq != 60 {
		t.Fatalf("status %+v", st)
	}
}

func TestTornTailIsTruncated(t *testing.T) {
	dir, c := t.TempDir(), newClock()
	s := open(t, dir, c, nil)
	for i := 1; i <= 3; i++ {
		mustAppend(t, s, ClassChat, "", fmt.Sprintf("m%d", i))
	}
	s.Close()
	segs, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	if len(segs) != 1 {
		t.Fatalf("segments %v", segs)
	}
	// A crash in the middle of the third record: cut it short and add junk.
	raw, _ := os.ReadFile(segs[0])
	raw = append(raw[:len(raw)-4], 0xde, 0xad)
	if err := os.WriteFile(segs[0], raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, c, nil)
	got := readAll(s, 1)
	if len(got) != 2 || string(got[1].Data) != "m2" {
		t.Fatalf("after torn tail read %v", datas(got))
	}
	seq := mustAppend(t, s, ClassChat, "", "m4")
	s.Close()

	// A flipped bit in a record is caught by its CRC: the rest of that segment is
	// dropped, earlier records (and later segments: m4 went to a new one) survive.
	s = open(t, dir, c, nil)
	if got := readAll(s, 1); len(got) != 3 || got[2].Seq != seq {
		t.Fatalf("after recovery+append read %v", datas(got))
	}
	s.Close()
	segs, _ = filepath.Glob(filepath.Join(dir, "*.seg"))
	raw, _ = os.ReadFile(segs[0])
	i := bytes.Index(raw, []byte("m2"))
	raw[i] ^= 0xff
	if err := os.WriteFile(segs[0], raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, c, nil)
	defer s.Close()
	if got := datas(readAll(s, 1)); strings.Join(got, ",") != "m1,m4" {
		t.Fatalf("after CRC failure read %v", got)
	}
}

// A daemon restarts in the middle of a replay: the next connection resumes from what the
// hub applied, not from what was sent.
func TestRestartMidReplay(t *testing.T) {
	dir, c := t.TempDir(), newClock()
	s := open(t, dir, c, nil)
	for i := 1; i <= 100; i++ {
		mustAppend(t, s, ClassChat, "", fmt.Sprintf("m%d", i))
	}
	recs, _ := s.Read(1, 40, 1<<20)
	s.MarkSent(recs[len(recs)-1].Seq)
	s.Close()

	s = open(t, dir, c, nil)
	defer s.Close()
	s.Resumed(30) // the hub applied 30 of the 40 sent before the restart
	got := readAll(s, 31)
	if len(got) != 70 || got[0].Seq != 31 || got[69].Seq != 100 {
		t.Fatalf("replay after restart: %d records from %d", len(got), got[0].Seq)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq != got[i-1].Seq+1 {
			t.Fatalf("gap or reorder at %d: %d after %d", i, got[i].Seq, got[i-1].Seq)
		}
	}
	if first, _ := s.Bounds(); first != 31 {
		t.Fatalf("first unacked %d, want 31", first)
	}
}

// Live records written during a replay are sent after it, in seq order, without gaps.
func TestReplayThenLiveOrdering(t *testing.T) {
	dir, c := t.TempDir(), newClock()
	s := open(t, dir, c, func(o *Options) { o.SegmentBytes = 2048 })
	defer s.Close()
	for i := 1; i <= 500; i++ {
		mustAppend(t, s, ClassChat, "", fmt.Sprintf("old%d", i))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= 500; i++ {
			if _, err := s.Append(ClassChat, "", []byte(fmt.Sprintf("live%d", i))); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	var got []Record
	from := uint64(1)
	deadline := time.Now().Add(10 * time.Second)
	for len(got) < 1000 && time.Now().Before(deadline) {
		ch := s.Changed()
		recs, next := s.Read(from, 16, 1<<20)
		got = append(got, recs...)
		from = next
		if len(recs) == 0 {
			select {
			case <-ch:
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	<-done
	if len(got) != 1000 {
		t.Fatalf("got %d records, want 1000", len(got))
	}
	for i, r := range got {
		if r.Seq != uint64(i+1) {
			t.Fatalf("record %d has seq %d", i, r.Seq)
		}
		want := fmt.Sprintf("old%d", i+1)
		if i >= 500 {
			want = fmt.Sprintf("live%d", i-499)
		}
		if string(r.Data) != want {
			t.Fatalf("record %d is %q, want %q", i, r.Data, want)
		}
	}
}

// Keyed (activity) records superseded by a newer one with the same key are skipped.
func TestCollapseKeyedRecords(t *testing.T) {
	dir, c := t.TempDir(), newClock()
	s := open(t, dir, c, nil)
	defer s.Close()
	mustAppend(t, s, ClassActivity, "a", "a1")
	mustAppend(t, s, ClassActivity, "b", "b1")
	mustAppend(t, s, ClassChat, "", "chat")
	mustAppend(t, s, ClassActivity, "a", "a2")
	recs, next := s.Read(1, 100, 1<<20)
	if got := strings.Join(datas(recs), ","); got != "b1,chat,a2" || next != 5 {
		t.Fatalf("read %s next %d", got, next)
	}
	// The newest state is only ever sent once it is the newest.
	recs, next = s.Read(1, 1, 1<<20)
	if len(recs) != 1 || string(recs[0].Data) != "b1" || next != 3 {
		t.Fatalf("first record %v next %d", datas(recs), next)
	}
}

// Over budget, unsent vitals are compacted first (1 min, then 1 h), then superseded
// activity; transcripts and statuses are never dropped, and a spool still over budget
// says it is full and stops keeping vitals and activity only.
func TestCompactionOrder(t *testing.T) {
	dir, c := t.TempDir(), newClock()
	s := open(t, dir, c, func(o *Options) { o.SegmentBytes = 4096; o.MaxBytes = 1 << 30 })
	defer s.Close()
	// Two hours of 15-second vitals, activity ticks and a few transcript events.
	var chats []string
	for i := 0; i < 480; i++ {
		mustAppend(t, s, ClassVitals, "", "v:1")
		if i%10 == 0 {
			mustAppend(t, s, ClassActivity, "activity:chat:c", fmt.Sprintf("act%d", i))
		}
		if i%40 == 0 {
			d := fmt.Sprintf("chat%d", i)
			chats = append(chats, d)
			mustAppend(t, s, ClassChat, "", d)
			mustAppend(t, s, ClassFinalStatus, "", "status"+d)
		}
		c.add(15 * time.Second)
	}
	total := s.Status().Bytes

	count := func() (vitals, samples, activity, chat, status int) {
		for _, g := range s.segs {
			for _, e := range g.entries {
				switch e.class {
				case ClassVitals:
					vitals++
				case ClassActivity:
					activity++
				case ClassChat:
					chat++
				case ClassFinalStatus:
					status++
				}
			}
		}
		// Read without collapse: raw records.
		for _, g := range s.segs {
			for _, e := range g.entries {
				if e.class == ClassVitals {
					buf := make([]byte, e.size)
					_, _ = g.f.ReadAt(buf, e.off)
					r, _ := decode(buf)
					n, _ := strconv.Atoi(strings.TrimPrefix(string(r.Data), "v:"))
					samples += n
				}
			}
		}
		return
	}

	// Within budget: nothing happens.
	if res := s.Compact(); res.Level != 0 || res.Merged != 0 {
		t.Fatalf("compacted within budget: %+v", res)
	}

	// Slightly over budget: one-minute vitals are enough.
	s.opts.MaxBytes = total - 1000
	res := s.Compact()
	v, n, a, ch, st := count()
	if res.Level != levelVitalsMinute || res.Full != "" || a != 48 || ch != len(chats) || st != len(chats) || n != 480 {
		t.Fatalf("level-1 compaction: %+v vitals=%d samples=%d activity=%d chat=%d status=%d", res, v, n, a, ch, st)
	}
	if v >= 480 || v < 100 {
		t.Fatalf("vitals not merged to minutes: %d records", v)
	}

	// Room for little more than the transcript and statuses: hourly vitals, then the
	// superseded activity goes.
	var keep int64
	for _, g := range s.segs {
		for _, e := range g.entries {
			if e.class == ClassChat || e.class == ClassFinalStatus {
				keep += e.size
			}
		}
	}
	s.opts.MaxBytes = keep + 2000
	res = s.Compact()
	v, n, a, ch, st = count()
	if n != 480 || ch != len(chats) || st != len(chats) {
		t.Fatalf("compaction lost samples or transcript: %+v samples=%d chat=%d status=%d", res, n, ch, st)
	}
	// Merging stays within a segment: at most one record per hour per segment.
	if v > 2*len(s.segs) {
		t.Fatalf("vitals not merged to hours: %d records in %d segments", v, len(s.segs))
	}
	// Activity is only dropped (oldest segments first) once vitals could not make room,
	// and only as much as needed.
	if res.Level < levelActivity || a >= 48 || res.Full != "" {
		t.Fatalf("superseded activity not dropped: level %d, %d left", res.Level, a)
	}
	// The newest activity state survives.
	var last string
	for _, r := range readAll(s, 1) {
		if r.Class == ClassActivity {
			last = string(r.Data)
		}
	}
	if last != "act470" {
		t.Fatalf("latest activity %q, want act470", last)
	}

	// Impossible budget: full. Transcripts and statuses are still kept; vitals are not.
	s.opts.MaxBytes = 100
	res = s.Compact()
	if res.Full == "" || s.Status().Full == "" {
		t.Fatalf("spool not full: %+v", res)
	}
	_, _, _, ch, st = count()
	if ch != len(chats) || st != len(chats) {
		t.Fatalf("full spool dropped transcript or statuses: chat=%d status=%d", ch, st)
	}
	if _, err := s.Append(ClassVitals, "", []byte("v:1")); err != ErrFull {
		t.Fatalf("vitals appended to a full spool: %v", err)
	}
	mustAppend(t, s, ClassChat, "", "still kept")
	if s.Status().Dropped == 0 {
		t.Fatal("dropped vitals not counted")
	}
	// Acknowledging frees room: the spool is no longer full.
	s.opts.MaxBytes = 1 << 30
	s.Ack(s.Status().LastSeq)
	if s.Status().Full != "" {
		t.Fatal("spool still full after everything was acknowledged")
	}
	mustAppend(t, s, ClassVitals, "", "v:1")
}

// Compaction never touches a record already handed to a hub: it may have been applied.
func TestCompactionSkipsSentRecords(t *testing.T) {
	dir, c := t.TempDir(), newClock()
	s := open(t, dir, c, func(o *Options) { o.SegmentBytes = 1 << 20 })
	defer s.Close()
	for i := 0; i < 40; i++ {
		mustAppend(t, s, ClassVitals, "", "v:1")
		c.add(15 * time.Second)
	}
	s.Resumed(0)
	s.MarkSent(40)
	s.opts.MaxBytes = 10
	s.Compact()
	if got := readAll(s, 1); len(got) != 40 {
		t.Fatalf("sent records were compacted: %d left", len(got))
	}
	// Once the hub says it applied only 20, the 20 newer ones may be compacted.
	s.Resumed(20)
	s.Compact()
	got := readAll(s, 21)
	if len(got) >= 20 || len(got) == 0 {
		t.Fatalf("unsent vitals not compacted: %d", len(got))
	}
	n := 0
	for _, r := range got {
		k, _ := strconv.Atoi(strings.TrimPrefix(string(r.Data), "v:"))
		n += k
	}
	if n != 20 || got[0].Seq < 21 {
		t.Fatalf("compacted records %v", datas(got))
	}
}

// Expired vitals go at the last level; an old transcript keeps the spool full.
func TestCompactionAgeBudget(t *testing.T) {
	dir, c := t.TempDir(), newClock()
	s := open(t, dir, c, func(o *Options) { o.MaxAge = time.Hour; o.SegmentBytes = 512 })
	defer s.Close()
	for i := 0; i < 20; i++ {
		mustAppend(t, s, ClassVitals, "", "v:1")
		c.add(time.Minute)
	}
	c.add(2 * time.Hour)
	mustAppend(t, s, ClassChat, "", "recent")
	res := s.Compact()
	if res.Full != "" {
		t.Fatalf("expired vitals not dropped: %+v", res)
	}
	if got := datas(readAll(s, 1)); len(got) != 1 || got[0] != "recent" {
		t.Fatalf("left %v", got)
	}
	mustAppend(t, s, ClassVitals, "", "v:1")
	s2dir := t.TempDir()
	s2 := open(t, s2dir, c, func(o *Options) { o.MaxAge = time.Hour })
	defer s2.Close()
	mustAppend(t, s2, ClassChat, "", "old transcript")
	c.add(2 * time.Hour)
	if res := s2.Compact(); res.Full == "" {
		t.Fatal("an old transcript must keep the spool full (never dropped)")
	}
	if got := readAll(s2, 1); len(got) != 1 {
		t.Fatal("old transcript dropped")
	}
}

func TestBackgroundSync(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Options{Dir: dir, SyncEvery: 10 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, s, ClassChat, "", "x")
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		dirty := s.dirty
		s.mu.Unlock()
		if !dirty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background sync never ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ClassChat, "", []byte("y")); err != ErrClosed {
		t.Fatalf("append after close: %v", err)
	}
}

func TestBindHubMatchAndReopen(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "w1")
	c := newClock()
	s := open(t, dir, c, nil)
	if s.HubID() != "" {
		t.Fatalf("fresh spool hub id %q, want empty", s.HubID())
	}
	epoch := s.Epoch()
	res, err := s.BindHub("hub-A")
	if err != nil || res.Archived || s.HubID() != "hub-A" || s.Epoch() != epoch {
		t.Fatalf("first bind: res=%+v err=%v hub=%q epoch=%q", res, err, s.HubID(), s.Epoch())
	}
	// Records written while disconnected stay tagged with hub-A across restarts.
	mustAppend(t, s, ClassChat, "", "m1")
	mustAppend(t, s, ClassChat, "", "m2")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := open(t, dir, c, nil)
	defer s2.Close()
	if s2.HubID() != "hub-A" || s2.Epoch() != epoch {
		t.Fatalf("after reopen: hub=%q epoch=%q", s2.HubID(), s2.Epoch())
	}
	res, err = s2.BindHub("hub-A")
	if err != nil || res.Archived || s2.Epoch() != epoch {
		t.Fatalf("matching bind: res=%+v err=%v epoch=%q", res, err, s2.Epoch())
	}
	if got := datas(readAll(s2, 1)); len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Fatalf("replayed records %v, want [m1 m2]", got)
	}
}

func TestBindHubMismatchArchivesAndStartsFreshEpoch(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "w1")
	c := newClock()
	s := open(t, dir, c, nil)
	if _, err := s.BindHub("hub-A"); err != nil {
		t.Fatal(err)
	}
	oldEpoch := s.Epoch()
	mustAppend(t, s, ClassChat, "", "from-A-1")
	mustAppend(t, s, ClassChat, "", "from-A-2")

	c.add(time.Minute)
	res, err := s.BindHub("hub-B")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Archived || res.OldHubID != "hub-A" || res.NewEpoch == "" || res.NewEpoch == oldEpoch {
		t.Fatalf("mismatch bind result: %+v (oldEpoch=%q)", res, oldEpoch)
	}
	if s.HubID() != "hub-B" || s.Epoch() != res.NewEpoch {
		t.Fatalf("post-mismatch spool: hub=%q epoch=%q", s.HubID(), s.Epoch())
	}
	if got := readAll(s, 1); len(got) != 0 {
		t.Fatalf("active spool must be empty after archive, got %v", datas(got))
	}
	if seq := mustAppend(t, s, ClassChat, "", "from-B-1"); seq != 1 {
		t.Fatalf("first seq in new epoch = %d, want 1", seq)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Archive directory exists with the expected prefix and holds the old records.
	base := filepath.Base(res.ArchiveDir)
	if !strings.HasPrefix(base, "spool-orphaned-hub-A-") {
		t.Fatalf("archive name %q, want prefix spool-orphaned-hub-A-", base)
	}
	archived := open(t, res.ArchiveDir, c, nil)
	defer archived.Close()
	if archived.HubID() != "hub-A" {
		t.Fatalf("archived hub id %q, want hub-A", archived.HubID())
	}
	if got := datas(readAll(archived, 1)); len(got) != 2 || got[0] != "from-A-1" || got[1] != "from-A-2" {
		t.Fatalf("archived records %v, want [from-A-1 from-A-2]", got)
	}

	// Reopening the active spool keeps hub-B and its new record.
	s2 := open(t, dir, c, nil)
	defer s2.Close()
	if s2.HubID() != "hub-B" || s2.Epoch() != res.NewEpoch {
		t.Fatalf("reopened active spool: hub=%q epoch=%q", s2.HubID(), s2.Epoch())
	}
	if got := datas(readAll(s2, 1)); len(got) != 1 || got[0] != "from-B-1" {
		t.Fatalf("reopened active records %v, want [from-B-1]", got)
	}
}

func TestBindHubUnknownWithUnackedRecordsArchives(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "w1")
	c := newClock()
	s := open(t, dir, c, nil)
	defer s.Close()
	mustAppend(t, s, ClassChat, "", "legacy-unacked")

	res, err := s.BindHub("hub-A")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Archived || res.OldHubID != "" || !strings.HasPrefix(filepath.Base(res.ArchiveDir), "spool-orphaned-unknown-") {
		t.Fatalf("unknown archive result: %+v", res)
	}
	if s.HubID() != "hub-A" || len(readAll(s, 1)) != 0 {
		t.Fatalf("post-archive state: hub=%q recs=%d", s.HubID(), len(readAll(s, 1)))
	}
}

func TestBindHubArchiveRotation(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "w1")
	c := newClock()
	s := open(t, dir, c, nil)
	defer s.Close()

	// Use non-alphabetical hub IDs so rotation must sort by timestamp, not hub ID.
	hubs := []string{"hub-z", "hub-a", "hub-m", "hub-b"}
	if _, err := s.BindHub(hubs[0]); err != nil {
		t.Fatal(err)
	}
	for _, nextHub := range hubs[1:] {
		mustAppend(t, s, ClassChat, "", "for-"+s.HubID())
		c.add(time.Minute)
		res, err := s.BindHub(nextHub)
		if err != nil || !res.Archived {
			t.Fatalf("bind %s: res=%+v err=%v", nextHub, res, err)
		}
	}

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var archives []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), orphanedPrefix) {
			archives = append(archives, e.Name())
		}
	}
	if len(archives) != MaxOrphanedArchives {
		t.Fatalf("kept %d archives (%v), want %d", len(archives), archives, MaxOrphanedArchives)
	}
	// hub-z was oldest and must be pruned; hub-a and hub-m must remain.
	joined := strings.Join(archives, ",")
	if strings.Contains(joined, "hub-z") || !strings.Contains(joined, "hub-a") || !strings.Contains(joined, "hub-m") {
		t.Fatalf("unexpected archives kept: %v", archives)
	}
}
