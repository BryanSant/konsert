package worker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/drivers"
	"github.com/BryanSant/syre/internal/worker/spool"
	"github.com/coder/websocket"
)

func taskEventMsg(id string) func(protocol.Envelope) bool {
	return func(e protocol.Envelope) bool {
		if e.Type != protocol.MsgTaskEvent {
			return false
		}
		p, err := protocol.DecodePayload[protocol.TaskEventPayload](e)
		return err == nil && p.TaskID == id
	}
}

// collectTaskEvents reads the task events of task id from c until n arrived.
func collectTaskEvents(t *testing.T, c *hubConn, id string, n int) []protocol.Envelope {
	t.Helper()
	var out []protocol.Envelope
	for len(out) < n {
		out = append(out, c.waitFor(t, 10*time.Second, fmt.Sprintf("task event %d", len(out)+1), taskEventMsg(id)))
	}
	return out
}

// Messages produced while the hub is unreachable are spooled and replayed on the next
// connection, after what the hub already applied, in order and exactly once.
func TestDaemonReplaysSpoolAfterReconnect(t *testing.T) {
	hub := newFakeHub(t)
	d, _ := startTestDaemon(t, hub, nil)
	c1 := hub.accept(t)
	hello := helloOf(t, c1)
	if hello.Spool == nil || hello.Spool.Epoch == "" {
		t.Fatalf("hello has no spool section: %+v", hello.Spool)
	}
	for i := 1; i <= 3; i++ {
		d.sendJSON(protocol.MsgTaskEvent, protocol.TaskEventPayload{TaskID: "t1", Kind: "log", Message: fmt.Sprintf("live %d", i)})
	}
	live := collectTaskEvents(t, c1, "t1", 3)
	for i := 1; i < len(live); i++ {
		if live[i].Seq <= live[i-1].Seq {
			t.Fatalf("seqs not increasing: %d after %d", live[i].Seq, live[i-1].Seq)
		}
	}

	// The hub goes away; the daemon keeps working.
	hub.reject.Store(true)
	_ = c1.conn.Close(websocket.StatusGoingAway, "hub asleep")
	waitUntil(t, 10*time.Second, "disconnect", func() bool { return d.link.status().State != LinkConnected })
	for i := 1; i <= 5; i++ {
		d.sendJSON(protocol.MsgTaskEvent, protocol.TaskEventPayload{TaskID: "t1", Kind: "log", Message: fmt.Sprintf("offline %d", i)})
	}
	st := d.spoolStatus(nil)
	if st.Records < 5 || st.OldestUnsentAt.IsZero() {
		t.Fatalf("spool status while disconnected: %+v", st)
	}
	hub.reject.Store(false)

	c2 := hub.accept(t)
	hello2 := helloOf(t, c2)
	if hello2.Spool.LastSeq < live[2].Seq+5 {
		t.Fatalf("hello last seq %d, want at least %d", hello2.Spool.LastSeq, live[2].Seq+5)
	}
	replayed := collectTaskEvents(t, c2, "t1", 5)
	for i, e := range replayed {
		p, _ := protocol.DecodePayload[protocol.TaskEventPayload](e)
		if want := fmt.Sprintf("offline %d", i+1); p.Message != want {
			t.Fatalf("replayed event %d is %q, want %q (no duplicates of what the hub applied)", i, p.Message, want)
		}
		if e.Seq <= live[2].Seq {
			t.Fatalf("replayed seq %d at or below applied %d", e.Seq, live[2].Seq)
		}
	}
	c2.waitFor(t, 10*time.Second, "caught_up", isType(protocol.MsgWorkerSpoolCaughtUp))
	// Live messages follow the replay on the same stream.
	d.sendJSON(protocol.MsgTaskEvent, protocol.TaskEventPayload{TaskID: "t1", Kind: "log", Message: "after"})
	after := collectTaskEvents(t, c2, "t1", 1)[0]
	if after.Seq <= replayed[4].Seq {
		t.Fatalf("live seq %d not after replay %d", after.Seq, replayed[4].Seq)
	}
	// Acknowledged records leave the spool.
	c2.send(t, protocol.MsgWorkerSpoolAck, "", protocol.SpoolAck{Seq: after.Seq})
	waitUntil(t, 5*time.Second, "ack applied", func() bool { return d.spool.Status().AckedSeq >= after.Seq })
}

// A final task status produced while disconnected survives a daemon restart and is
// replayed by the next daemon process.
func TestDaemonRestartReplaysSpool(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(StateDirEnv, t.TempDir())
	cfg := config.WorkerConfig{WorkerID: "restart-worker", Name: "restart-worker", EnableMockDriver: true}
	d := NewDaemon(cfg, drivers.DefaultRegistry, nil)
	code := 0
	d.sendJSON(protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: "t9", Status: protocol.TaskStatusCompleted, ExitCode: &code})
	d.sendJSON(protocol.MsgTaskEvent, protocol.TaskEventPayload{TaskID: "t9", Kind: "log", Message: "before restart"})
	d.shutdown()

	hub := newFakeHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg.ControllerURL = hub.wsURL()
	d2 := NewDaemon(cfg, drivers.DefaultRegistry, nil)
	d2.HeartbeatInterval, d2.FactsInterval = time.Hour, time.Hour
	done := make(chan struct{})
	go func() { defer close(done); _ = d2.Run(ctx) }()
	defer func() { cancel(); <-done }()
	c := hub.accept(t)
	helloOf(t, c)
	env := c.waitFor(t, 10*time.Second, "final status after restart", isType(protocol.MsgTaskStatus))
	if p, _ := protocol.DecodePayload[protocol.TaskStatusPayload](env); p.TaskID != "t9" || p.Status != protocol.TaskStatusCompleted || env.Seq == 0 {
		t.Fatalf("replayed status %+v seq %d", p, env.Seq)
	}
	c.waitFor(t, 10*time.Second, "event after restart", taskEventMsg("t9"))
}

// Heartbeats carry their sample time, so a replayed one is stored where it belongs.
func TestHeartbeatCarriesSampleTime(t *testing.T) {
	hub := newFakeHub(t)
	startTestDaemon(t, hub, nil)
	c := hub.accept(t)
	helloOf(t, c)
	env := c.waitFor(t, 10*time.Second, "heartbeat", isType(protocol.MsgWorkerHeartbeat))
	p, _ := protocol.DecodePayload[protocol.WorkerHeartbeatPayload](env)
	if p.At.IsZero() || time.Since(p.At) > time.Minute || env.Seq == 0 {
		t.Fatalf("heartbeat at %v seq %d", p.At, env.Seq)
	}
}

func TestMergeVitalsAverages(t *testing.T) {
	g := 40.0
	hb := func(cpu float64, mem uint64, samples int, gpu *float64) spool.Record {
		raw, err := protocol.NewEnvelope(protocol.MsgWorkerHeartbeat, "", protocol.WorkerHeartbeatPayload{
			ActiveTaskIDs: []string{"a"}, Samples: samples,
			Metrics: &protocol.WorkerMetrics{CPUPercent: cpu, MemUsedMB: mem, MemTotalMB: 1000, LoadAvg: cpu / 10, GPUPercent: gpu},
		})
		if err != nil {
			t.Fatal(err)
		}
		return spool.Record{Class: spool.ClassVitals, Data: raw}
	}
	start := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	raw, err := mergeVitals(start, []spool.Record{hb(10, 100, 1, nil), hb(40, 400, 3, &g)})
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.DecodeEnvelope(raw)
	if err != nil || env.Type != protocol.MsgWorkerHeartbeat {
		t.Fatalf("merged envelope %s: %v", raw, err)
	}
	p, _ := protocol.DecodePayload[protocol.WorkerHeartbeatPayload](env)
	if p.Samples != 4 || !p.At.Equal(start) || p.Metrics == nil {
		t.Fatalf("merged %+v", p)
	}
	if p.Metrics.CPUPercent != 32.5 || p.Metrics.MemUsedMB != 325 || p.Metrics.GPUPercent == nil || *p.Metrics.GPUPercent != 40 {
		t.Fatalf("merged metrics %+v", p.Metrics)
	}
}

func TestSpoolClassification(t *testing.T) {
	cases := []struct {
		typ     string
		payload any
		class   spool.Class
		key     string
		spooled bool
	}{
		{protocol.MsgAgentChatEvent, protocol.AgentChatEvent{Kind: protocol.EventMessageDelta}, spool.ClassChat, "", true},
		{protocol.MsgAgentChatEvent, protocol.AgentChatEvent{Kind: protocol.EventElicitation}, spool.ClassQuestion, "", true},
		{protocol.MsgTaskStatus, protocol.TaskStatusPayload{Status: protocol.TaskStatusRunning}, spool.ClassStatus, "", true},
		{protocol.MsgTaskStatus, protocol.TaskStatusPayload{Status: protocol.TaskStatusFailed}, spool.ClassFinalStatus, "", true},
		{protocol.MsgSessionActivity, protocol.SessionActivity{ChatID: "c"}, spool.ClassActivity, "activity:chat:c", true},
		{protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{}, spool.ClassVitals, "", true},
		{protocol.MsgExternalSession, protocol.ExternalSessionPayload{}, spool.ClassReport, "", true},
		{protocol.MsgAgentCapabilities, protocol.AgentCapabilitiesReport{}, 0, "", false},
		{protocol.MsgWorkerPorts, protocol.WorkerPortsPayload{}, 0, "", false},
		{protocol.MsgHostWakeLockState, protocol.HostWakeLockState{}, 0, "", false},
	}
	for _, c := range cases {
		class, key, ok := spoolClass(c.typ, c.payload)
		if ok != c.spooled || class != c.class || key != c.key {
			t.Errorf("%s: got (%v, %q, %v), want (%v, %q, %v)", c.typ, class, key, ok, c.class, c.key, c.spooled)
		}
	}
}

func TestScrollbackRotatesAndRedraws(t *testing.T) {
	dir := t.TempDir()
	sb := openScrollback(dir, "task-1")
	if sb == nil {
		t.Fatal("no scrollback")
	}
	chunk := bytes.Repeat([]byte("x"), 100<<10)
	for i := 0; i < 12; i++ {
		sb.Write(chunk)
	}
	sb.Write([]byte("THE END"))
	sb.Close()
	got := readScrollback(dir, "task-1", DefaultRingBufferSize)
	if len(got) != DefaultRingBufferSize || !bytes.HasSuffix(got, []byte("THE END")) {
		t.Fatalf("scrollback %d bytes, suffix %q", len(got), got[max(0, len(got)-7):])
	}
	var total int64
	for _, n := range []string{"task-1.log", "task-1.log.1"} {
		if fi, err := os.Stat(filepath.Join(dir, n)); err == nil {
			total += fi.Size()
		}
	}
	if total > 2*scrollbackHalf {
		t.Fatalf("scrollback grew to %d bytes", total)
	}

	// A subscribe for a task that is gone redraws from disk.
	var frames []byte
	r := NewRunner(nil, false, nil, RunnerCallbacks{OnPtyFrame: func(ft byte, id string, data []byte) {
		if ft == protocol.FramePtySnapshot && id == "task-1" {
			frames = data
		}
	}}, discardLogger())
	r.SetScrollbackDir(dir)
	if !r.SubscribePty("task-1") || !bytes.HasSuffix(frames, []byte("THE END")) {
		t.Fatalf("no redraw from scrollback (%d bytes)", len(frames))
	}
	if r.SubscribePty("unknown") {
		t.Fatal("subscribe to an unknown task without scrollback succeeded")
	}
}

// Deterministic (no timing) test for hub-id mismatch archive + cursor reset:
//  1. Even if a connection's spoolConn already holds a stale high cursor from an old epoch
//     (or an in-flight writeSpool was reading at a high seq), archiving via BindHub resets
//     the cursor to 1, kicks a live heartbeat in the new epoch, emits caught_up with the
//     new epoch, and streams live records starting at seq 1.
func TestSpoolConnResetAfterArchiveResetsStaleCursorAndKicksHeartbeat(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "w-reset")
	sp, err := spool.Open(spool.Options{Dir: dir, SyncEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.BindHub("hub-A"); err != nil {
		t.Fatal(err)
	}
	oldEv, err := protocol.NewEnvelope(protocol.MsgTaskEvent, "", protocol.TaskEventPayload{TaskID: "tA", Kind: "log", Message: "from-A"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Append(spool.ClassTaskEvent, "", oldEv); err != nil {
		t.Fatal(err)
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen so the next seq jumps past the 4096-seq reservation, then append another record.
	sp, err = spool.Open(spool.Options{Dir: dir, SyncEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	highSeq, err := sp.Append(spool.ClassTaskEvent, "", oldEv)
	if err != nil {
		t.Fatal(err)
	}
	if highSeq <= 4096 {
		t.Fatalf("expected highSeq > 4096 after reopen, got %d", highSeq)
	}
	oldEpoch := sp.Epoch()

	d := &Daemon{
		spool:     sp,
		logger:    discardLogger(),
		spoolKick: make(chan struct{}, 1),
		hbKick:    make(chan struct{}, 1),
	}
	sc := newSpoolConn()
	// Simulate a stale cursor already positioned past highSeq on the old epoch.
	if !sc.resume(highSeq, highSeq) {
		t.Fatal("initial resume failed")
	}

	// Hub B sends worker.spool.resume with a different HubID.
	resumeRaw, err := protocol.NewEnvelope(protocol.MsgWorkerSpoolResume, "", protocol.SpoolResume{
		HubID:      "hub-B",
		Epoch:      oldEpoch,
		AppliedSeq: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	resumeEnv, err := protocol.DecodeEnvelope(resumeRaw)
	if err != nil {
		t.Fatal(err)
	}
	if !d.handleSpoolMessage(sc, resumeEnv) {
		t.Fatal("handleSpoolMessage did not handle worker.spool.resume")
	}
	newEpoch := sp.Epoch()
	if newEpoch == "" || newEpoch == oldEpoch {
		t.Fatalf("epoch did not change after archive: %q", newEpoch)
	}

	// The cursor must be reset to 1 (not stuck at highSeq+1) and hbKick must be signaled.
	cursor, start, target, caught := sc.position()
	if cursor != 1 || start != 1 || target != 0 || caught {
		t.Fatalf("post-reset position = (cursor=%d, start=%d, target=%d, caught=%v), want (1, 1, 0, false)", cursor, start, target, caught)
	}
	select {
	case <-d.hbKick:
	default:
		t.Fatal("expected hbKick to be signaled after spool archive so the new hub gets a live heartbeat")
	}

	// First writeSpool emits worker.spool.caught_up with the new epoch and seq 0.
	var sent []protocol.Envelope
	write := func(m outboundMsg) error {
		env, err := protocol.DecodeEnvelope(m.data)
		if err != nil {
			return err
		}
		sent = append(sent, env)
		return nil
	}
	more, err := d.writeSpool(sc, write)
	if err != nil || !more {
		t.Fatalf("writeSpool(caught_up): more=%v err=%v", more, err)
	}
	if len(sent) != 1 || sent[0].Type != protocol.MsgWorkerSpoolCaughtUp {
		t.Fatalf("expected caught_up first, got %+v", sent)
	}
	ack, err := protocol.DecodePayload[protocol.SpoolAck](sent[0])
	if err != nil || ack.Epoch != newEpoch || ack.Seq != 0 {
		t.Fatalf("caught_up payload = %+v (err=%v), want epoch=%q seq=0", ack, err, newEpoch)
	}

	// Append a live record in the new epoch (seq 1) and verify writeSpool sends it.
	liveRaw, err := protocol.NewEnvelope(protocol.MsgWorkerHeartbeat, "", protocol.WorkerHeartbeatPayload{LoadAvg: 1.5})
	if err != nil {
		t.Fatal(err)
	}
	liveSeq, err := sp.Append(spool.ClassVitals, "", liveRaw)
	if err != nil || liveSeq != 1 {
		t.Fatalf("live append seq=%d err=%v, want seq=1", liveSeq, err)
	}
	more, err = d.writeSpool(sc, write)
	if err != nil || !more {
		t.Fatalf("writeSpool(live): more=%v err=%v", more, err)
	}
	if len(sent) != 2 || sent[1].Type != protocol.MsgWorkerHeartbeat || sent[1].Seq != 1 {
		t.Fatalf("expected live heartbeat seq=1, got %+v", sent)
	}
}
