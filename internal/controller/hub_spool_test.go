package controller

import (
	"context"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/coder/websocket"
)

// sendSeq sends a spooled record (Envelope.Seq set) as konsertd does.
func (fw *fakeWorker) sendSeq(seq uint64, msgType string, payload any) {
	fw.t.Helper()
	raw, err := protocol.NewEnvelope(msgType, "", payload)
	if err != nil {
		fw.t.Fatal(err)
	}
	if err := fw.conn.Write(fw.ctx, websocket.MessageText, protocol.WithSeq(raw, seq)); err != nil {
		fw.t.Fatalf("fake worker write %s: %v", msgType, err)
	}
}

// dialSpoolWorker connects a fake konsertd that spools and returns the hub's resume point.
func (th *testHub) dialSpoolWorker(t *testing.T, id, epoch string, last uint64, chats []protocol.ActiveChatInfo) (*fakeWorker, uint64) {
	t.Helper()
	fw := th.dialPeer(t, "/ws/worker", false)
	fw.send(protocol.MsgWorkerHello, protocol.WorkerHelloPayload{
		Worker:      testWorkerInfo(id, id),
		ActiveChats: chats,
		Spool:       &protocol.SpoolHello{Epoch: epoch, FirstSeq: 1, LastSeq: last},
	})
	env := fw.expect(protocol.MsgWorkerSpoolResume, 5*time.Second)
	p, err := protocol.DecodePayload[protocol.SpoolResume](env)
	if err != nil || p.Epoch != epoch {
		t.Fatalf("resume %+v: %v", p, err)
	}
	return fw, p.AppliedSeq
}

type spoolCounts struct {
	chatEvents, taskEvents, usageIn, vitalsN, rollupN, fleetEvents int
}

func countSpoolEffects(t *testing.T, th *testHub, chatID, taskID string) spoolCounts {
	t.Helper()
	ctx := context.Background()
	var c spoolCounts
	evs, err := th.st.ListChatEvents(ctx, chatID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	c.chatEvents = len(evs)
	tes, _ := th.st.ListEvents(ctx, taskID, 0, 1000)
	c.taskEvents = len(tes)
	sum, _ := th.st.GetUsageSummary(ctx)
	c.usageIn = int(sum.TotalInputTokens)
	th.h.flushVitals(ctx, true)
	vs, _ := th.st.ListVitals(ctx, time.Now().Add(-48*time.Hour), time.Now().Add(time.Hour))
	for _, v := range vs {
		c.vitalsN += v.N
	}
	// Hours past the raw tier only (a hub start rolls recent raw hours up as well).
	rs, _ := th.st.ListVitalsRollups(ctx, store.VitalsHourly, time.Now().Add(-60*24*time.Hour), time.Now().Add(-store.VitalsRawKeep-2*time.Hour))
	for _, r := range rs {
		c.rollupN += r.N
	}
	fes, _ := th.st.ListFleetEvents(ctx, store.FleetEventFilter{})
	c.fleetEvents = len(fes)
	return c
}

// sendAllRecordTypes sends one record of every spooled type, seqs 1..11.
func sendAllRecordTypes(fw *fakeWorker, chatID, taskID string, now time.Time) {
	fw.sendSeq(1, protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: chatID, Seq: 1, Kind: protocol.EventMessageDelta, Text: "hel"})
	fw.sendSeq(2, protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: chatID, Seq: 2, Kind: protocol.EventUsage,
		Usage: &protocol.AgentTokenUsage{InputTokens: 10, OutputTokens: 5}})
	fw.sendSeq(3, protocol.MsgTaskEvent, protocol.TaskEventPayload{TaskID: taskID, Kind: "log", Message: "step"})
	fw.sendSeq(4, protocol.MsgTaskEvent, protocol.TaskEventPayload{TaskID: taskID, Kind: "usage",
		Meta: map[string]any{"provider": "claude", "model": "haiku", "input_tokens": float64(100), "output_tokens": float64(7)}})
	fw.sendSeq(5, protocol.MsgTaskStatus, protocol.TaskStatusPayload{TaskID: taskID, Status: protocol.TaskStatusWaitingInput})
	fw.sendSeq(6, protocol.MsgExternalSession, protocol.ExternalSessionPayload{ExternalID: "pid-1", Agent: "claude", Status: "running"})
	fw.sendSeq(7, protocol.MsgOperatorCommand, protocol.OperatorCommandPayload{TaskID: taskID, CommandID: "cmd-1", Command: "ls"})
	fw.sendSeq(8, protocol.MsgSessionActivity, protocol.SessionActivity{ChatID: chatID, State: protocol.ActivityThinking, At: now})
	m := &protocol.WorkerMetrics{CPUPercent: 50, MemUsedMB: 1000, MemTotalMB: 8000, LoadAvg: 1}
	// A heartbeat from an hour ago (raw tier), one from a day ago (hourly roll-up), a live one.
	fw.sendSeq(9, protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{Metrics: m, At: now.Add(-time.Hour)})
	fw.sendSeq(10, protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{Metrics: m, At: now.Add(-25 * time.Hour), Samples: 4})
	fw.sendSeq(11, protocol.MsgWorkerHeartbeat, protocol.WorkerHeartbeatPayload{Metrics: m, At: now})
}

func appliedSeq(th *testHub, id string) uint64 {
	th.h.spoolMu.Lock()
	ws := th.h.spools[id]
	th.h.spoolMu.Unlock()
	if ws == nil {
		return 0
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.applied
}

// A replay of records the hub already applied changes nothing, for every record type:
// on the same hub (a reconnect) and after a hub restart (applied seqs come from SQLite).
func TestSpoolReplayIsIdempotent(t *testing.T) {
	dbPath := t.TempDir() + "/hub.db"
	th := newTestHubAt(t, dbPath)
	ctx := context.Background()
	const wid, chatID, taskID = "w-spool", "chat-spool", "t-spool"

	fw, applied := th.dialSpoolWorker(t, wid, "epoch-1", 0, nil)
	if applied != 0 {
		t.Fatalf("fresh machine resumes at %d", applied)
	}
	waitFor(t, 5*time.Second, "registration", func() bool { return th.h.IsWorkerConnected(wid) })
	seedRunningTask(t, th, taskID, wid, protocol.OrphanPolicyFail)
	if err := th.st.CreateChatSession(ctx, store.ChatSession{ID: chatID, Title: "c", Backend: store.ChatBackendAgent, WorkerID: wid, Agent: "claude", Status: protocol.ChatStatusWorking}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	sendAllRecordTypes(fw, chatID, taskID, now)
	waitFor(t, 5*time.Second, "all records applied", func() bool { return appliedSeq(th, wid) == 11 })
	ack := fw.expect(protocol.MsgWorkerSpoolAck, 3*time.Second)
	if p, _ := protocol.DecodePayload[protocol.SpoolAck](ack); p.Seq != 11 {
		t.Fatalf("ack %d, want 11", p.Seq)
	}
	want := countSpoolEffects(t, th, chatID, taskID)
	if want.chatEvents != 2 || want.taskEvents < 1 || want.usageIn != 100 || want.vitalsN != 2 || want.rollupN != 4 {
		t.Fatalf("first application: %+v", want)
	}
	if got := mustTask(t, th.st, taskID); got.Status != protocol.TaskStatusWaitingInput {
		t.Fatalf("status not applied: %s", got.Status)
	}

	// Reconnect: the daemon never got the ack and replays everything.
	fw.close()
	waitFor(t, 3*time.Second, "disconnect", func() bool { return !th.h.IsWorkerConnected(wid) })
	alive := []protocol.ActiveChatInfo{{ChatID: chatID, TurnActive: true}}
	fw2, applied := th.dialSpoolWorker(t, wid, "epoch-1", 11, alive)
	if applied != 11 {
		t.Fatalf("reconnect resumes at %d, want 11", applied)
	}
	sendAllRecordTypes(fw2, chatID, taskID, now)
	fw2.sendSeq(12, protocol.MsgTaskEvent, protocol.TaskEventPayload{TaskID: taskID, Kind: "log", Message: "new"})
	waitFor(t, 5*time.Second, "new record applied", func() bool { return appliedSeq(th, wid) == 12 })
	got := countSpoolEffects(t, th, chatID, taskID)
	want.taskEvents++ // only seq 12
	// The hello (not the replay) ends the external session reported before the reconnect.
	want.fleetEvents++
	if got != want {
		t.Fatalf("replay changed state:\n got %+v\nwant %+v", got, want)
	}

	// Hub restart: the applied seq comes back from SQLite.
	fw2.close()
	th.stop()
	th2 := newTestHubAt(t, dbPath)
	fw3, applied := th2.dialSpoolWorker(t, wid, "epoch-1", 12, alive)
	if applied != 12 {
		t.Fatalf("after hub restart resumes at %d, want 12", applied)
	}
	sendAllRecordTypes(fw3, chatID, taskID, now)
	fw3.sendSeq(12, protocol.MsgTaskEvent, protocol.TaskEventPayload{TaskID: taskID, Kind: "log", Message: "new"})
	fw3.sendSeq(13, protocol.MsgWorkerSpoolCaughtUp, protocol.SpoolAck{Seq: 12})
	time.Sleep(300 * time.Millisecond)
	if got := countSpoolEffects(t, th2, chatID, taskID); got != want {
		t.Fatalf("replay after hub restart changed state:\n got %+v\nwant %+v", got, want)
	}

	// A new epoch (the daemon's state was wiped) starts over at 0.
	fw3.close()
	waitFor(t, 3*time.Second, "disconnect", func() bool { return !th2.h.IsWorkerConnected(wid) })
	_, applied = th2.dialSpoolWorker(t, wid, "epoch-2", 3, nil)
	if applied != 0 {
		t.Fatalf("new epoch resumes at %d, want 0", applied)
	}
}

// The in-transaction guard: an append-style write of a record whose seq is already stored
// as applied is refused, even when the hub's memory lost track (a crash).
func TestSpoolGuardRefusesAppliedRecords(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	th.connectWorker(t, testWorkerInfo("w-g", "g"), nil, "w-g").close()
	seedRunningTask(t, th, "t-g", "w-g", protocol.OrphanPolicyFail)
	if err := th.st.ResetSpoolEpoch(ctx, "w-g", "e"); err != nil {
		t.Fatal(err)
	}
	mark := func(seq uint64) context.Context {
		return store.WithSpoolMark(ctx, &store.SpoolMark{WorkerID: "w-g", Seq: seq})
	}
	if _, err := th.st.AppendEvent(mark(5), store.TaskEvent{TaskID: "t-g", Kind: "log", Message: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := th.st.AppendEvent(mark(5), store.TaskEvent{TaskID: "t-g", Kind: "log", Message: "a"}); err != store.ErrSpoolDuplicate {
		t.Fatalf("duplicate task event: %v", err)
	}
	if err := th.st.RecordUsage(mark(4), store.UsageEvent{Provider: "p", InputTokens: 1}); err != store.ErrSpoolDuplicate {
		t.Fatalf("older usage record: %v", err)
	}
	if err := th.st.RecordUsage(mark(6), store.UsageEvent{Provider: "p", InputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := th.st.AppendChatEvent(mark(6), store.ChatEvent{ChatID: "x", Seq: 1, Kind: "k", JSON: "{}"}); err != store.ErrSpoolDuplicate {
		t.Fatalf("duplicate chat event: %v", err)
	}
	if err := th.st.UpsertVitals(mark(6), []store.VitalsSample{{WorkerID: "w-g", At: time.Now(), N: 1}}); err != store.ErrSpoolDuplicate {
		t.Fatalf("duplicate vitals: %v", err)
	}
	// One record may append several rows: only the first write claims it.
	m := &store.SpoolMark{WorkerID: "w-g", Seq: 7}
	mctx := store.WithSpoolMark(ctx, m)
	for i := 0; i < 2; i++ {
		if _, err := th.st.AppendEvent(mctx, store.TaskEvent{TaskID: "t-g", Kind: "log", Message: "b"}); err != nil {
			t.Fatalf("write %d of one record: %v", i, err)
		}
	}
	st, _ := th.st.GetSpoolState(ctx, "w-g")
	if st.AppliedSeq != 7 || st.Epoch != "e" {
		t.Fatalf("spool state %+v", st)
	}
	// Advancing never lowers.
	_ = th.st.AdvanceSpoolApplied(ctx, "w-g", 3)
	if st, _ := th.st.GetSpoolState(ctx, "w-g"); st.AppliedSeq != 7 {
		t.Fatalf("applied lowered to %d", st.AppliedSeq)
	}
}

// A chat whose turn still runs on the daemon is not failed on reconnect; one whose turn
// did not survive is failed only once the replay delivered every event it had.
func TestReconnectKeepsRunningChatsAndFailsAfterReplay(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	const wid = "w-chat"
	th.connectWorker(t, testWorkerInfo(wid, wid), nil, wid).close()
	waitFor(t, 3*time.Second, "disconnect", func() bool { return !th.h.IsWorkerConnected(wid) })
	for _, id := range []string{"chat-alive", "chat-dead"} {
		if err := th.st.CreateChatSession(ctx, store.ChatSession{ID: id, Title: id, Backend: store.ChatBackendAgent, WorkerID: wid, Agent: "claude", Status: protocol.ChatStatusWorking}); err != nil {
			t.Fatal(err)
		}
	}
	fw, _ := th.dialSpoolWorker(t, wid, "e", 2, []protocol.ActiveChatInfo{{ChatID: "chat-alive", TurnActive: true, LastSeq: 9}})
	time.Sleep(200 * time.Millisecond)
	for _, id := range []string{"chat-alive", "chat-dead"} {
		if cs, _ := th.st.GetChatSession(ctx, id); cs.Status != protocol.ChatStatusWorking {
			t.Fatalf("%s failed before the replay finished: %s", id, cs.Status)
		}
	}
	// The dead chat's last events arrive in the replay; then the replay ends.
	fw.sendSeq(1, protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: "chat-dead", Seq: 1, Kind: protocol.EventMessageDelta, Text: "partial"})
	fw.sendSeq(2, protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: "chat-alive", Seq: 1, Kind: protocol.EventMessageDelta, Text: "still going"})
	fw.send(protocol.MsgWorkerSpoolCaughtUp, protocol.SpoolAck{Seq: 2})
	waitFor(t, 3*time.Second, "interrupted chat failed after replay", func() bool {
		cs, _ := th.st.GetChatSession(ctx, "chat-dead")
		return cs.Status == protocol.ChatStatusFailed
	})
	if cs, _ := th.st.GetChatSession(ctx, "chat-alive"); cs.Status != protocol.ChatStatusWorking {
		t.Fatalf("running chat was failed: %s", cs.Status)
	}
	evs, _ := th.st.ListChatEvents(ctx, "chat-dead", 0, 100)
	if len(evs) < 2 || evs[0].Kind != protocol.EventMessageDelta {
		t.Fatalf("replayed event must precede the failure: %+v", evs)
	}
}

// Vitals sampled while the hub slept are stored at their original time, in the tier
// that holds that time: raw while the raw tier still covers it, else hourly / daily.
func TestLateVitalsTierPlacement(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	th.connectWorker(t, testWorkerInfo("w-v", "v"), nil, "w-v").close()
	now := time.Now().UTC()
	at := func(d time.Duration) time.Time { return now.Add(-d) }
	sample := func(t time.Time, cpu float64, n int) store.VitalsSample {
		return store.VitalsSample{WorkerID: "w-v", At: t, N: n, CPU: cpu, Mem: 100, MemTotal: 1000, Heat: cpu / 100, HeatMax: cpu / 100, Cause: "cpu"}
	}
	rawT, hourlyT, dailyT := at(2*time.Hour), at(26*time.Hour), at(40*24*time.Hour)
	// An hourly row already exists for hourlyT's hour (from samples before the sleep).
	hourStart := hourlyT.Truncate(time.Hour)
	if _, err := th.st.RollupVitals(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := th.st.StoreLateVitals(ctx, []store.VitalsSample{sample(hourStart.Add(time.Minute), 10, 1)}, now); err != nil {
		t.Fatal(err)
	}
	if err := th.st.StoreLateVitals(ctx, []store.VitalsSample{sample(rawT, 50, 1), sample(hourlyT, 40, 3), sample(dailyT, 70, 2)}, now); err != nil {
		t.Fatal(err)
	}
	raw, _ := th.st.ListVitals(ctx, at(3*time.Hour), now)
	if len(raw) != 1 || !raw[0].At.Equal(rawT.Truncate(store.VitalsBucket)) || raw[0].CPU != 50 {
		t.Fatalf("raw tier %+v", raw)
	}
	hourly, _ := th.st.ListVitalsRollups(ctx, store.VitalsHourly, hourStart, hourStart.Add(time.Hour))
	if len(hourly) != 1 || hourly[0].N != 4 || hourly[0].CPU.Avg != 32.5 || hourly[0].CPU.Min != 10 || hourly[0].CPU.Max != 40 {
		t.Fatalf("hourly tier %+v", hourly)
	}
	// The day holding hourlyT is complete (recomputed from hours) unless it is today.
	if day := hourlyT.Truncate(24 * time.Hour); day.Before(now.Truncate(24 * time.Hour)) {
		daily, _ := th.st.ListVitalsRollups(ctx, store.VitalsDaily, day, day.Add(24*time.Hour))
		if len(daily) != 1 || daily[0].N < 4 {
			t.Fatalf("daily roll-up of the late hour %+v", daily)
		}
	}
	dayStart := dailyT.Truncate(24 * time.Hour)
	daily, _ := th.st.ListVitalsRollups(ctx, store.VitalsDaily, dayStart, dayStart.Add(24*time.Hour))
	if len(daily) != 1 || daily[0].N != 2 || daily[0].CPU.Avg != 70 {
		t.Fatalf("daily tier %+v", daily)
	}
	// The raw sample is rolled up with its hour like any other.
	if _, err := th.st.RollupVitals(ctx, now); err != nil {
		t.Fatal(err)
	}
	h, _ := th.st.ListVitalsRollups(ctx, store.VitalsHourly, rawT.Truncate(time.Hour), rawT.Truncate(time.Hour).Add(time.Hour))
	if len(h) != 1 || h[0].CPU.Avg != 50 {
		t.Fatalf("rolled-up raw sample %+v", h)
	}
}
