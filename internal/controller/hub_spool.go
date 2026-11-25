package controller

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

// Idempotent ingestion of a daemon's durable spool (KON-VZR9F, docs/design/spool.md).
//
// Every spooled record carries the daemon's seq (Envelope.Seq). Per machine the hub keeps
// the highest seq it applied (in memory, and in worker_spool): a record at or below it is
// a replay of something already applied and is skipped. Records that append rows (chat
// events, task events, usage, late vitals) advance the stored seq in their own
// transaction (store.SpoolMark); task statuses and session reports advance it right after
// they are applied; everything else (activity, live vitals) is persisted with the next
// acknowledgement, at most a second later. The hub acknowledges what it applied at least
// once a second while records arrive.

// spoolAckEvery is how often the hub acknowledges applied records.
const spoolAckEvery = time.Second

// lateVitalsAfter: a heartbeat sampled longer ago than this is stored at its own time
// (a replay), not folded into the machine's live bucket and heat.
const lateVitalsAfter = 30 * time.Second

// workerSpool is one machine's applied spool state. mu also serializes applying the
// machine's records (an old connection may still be draining while a new one replays).
type workerSpool struct {
	mu        sync.Mutex
	epoch     string
	applied   uint64 // highest seq applied
	persisted uint64 // highest seq known to be stored in worker_spool
	acked     uint64 // highest seq acknowledged to the daemon
	dups      uint64 // replayed records skipped as already applied
	// status is the daemon's last spool report (Fleet). Not under mu: fleet views are
	// built while a record is being applied.
	status atomic.Pointer[protocol.SpoolStatus]
}

// workerSpoolFor returns workerID's spool state for epoch, loading it from the store the
// first time and starting over when the daemon's spool is a new epoch.
func (h *Hub) workerSpoolFor(ctx context.Context, workerID, epoch string) *workerSpool {
	h.spoolMu.Lock()
	if h.spools == nil {
		h.spools = map[string]*workerSpool{}
	}
	ws := h.spools[workerID]
	loaded := ws != nil
	if ws == nil {
		ws = &workerSpool{}
		h.spools[workerID] = ws
	}
	h.spoolMu.Unlock()

	ws.mu.Lock()
	defer ws.mu.Unlock()
	if !loaded {
		if st, err := h.store.GetSpoolState(ctx, workerID); err == nil {
			ws.epoch, ws.applied, ws.persisted = st.Epoch, st.AppliedSeq, st.AppliedSeq
		} else {
			h.logger.Warn("load spool state failed", "worker_id", workerID, "err", err)
		}
	}
	if ws.epoch != epoch {
		if ws.epoch != "" {
			h.logger.Info("machine's spool is a new epoch; nothing of it applied yet", "worker_id", workerID, "epoch", epoch)
		}
		if err := h.store.ResetSpoolEpoch(ctx, workerID, epoch); err != nil {
			h.logger.Warn("reset spool epoch failed", "worker_id", workerID, "err", err)
		}
		ws.epoch, ws.applied, ws.persisted, ws.acked = epoch, 0, 0, 0
	}
	return ws
}

// spoolStatusOf returns workerID's last reported spool status (nil when none).
func (h *Hub) spoolStatusOf(workerID string) *protocol.SpoolStatus {
	h.spoolMu.Lock()
	ws := h.spools[workerID]
	h.spoolMu.Unlock()
	if ws == nil {
		return nil
	}
	return ws.status.Load()
}

// startSpool answers worker.hello's spool section with worker.spool.resume and starts
// acknowledging. It runs before the connection's writer and read loop start.
func (h *Hub) startSpool(ctx, connCtx context.Context, wc *workerConn, hello protocol.WorkerHelloPayload) {
	wc.replayDoneCh = make(chan struct{})
	wc.activeChats = map[string]bool{}
	for _, c := range hello.ActiveChats {
		if c.TurnActive {
			wc.activeChats[c.ChatID] = true
		}
	}
	if hello.Spool == nil || h.store == nil {
		wc.replayDone.Store(true)
		close(wc.replayDoneCh)
		return
	}
	ws := h.workerSpoolFor(ctx, wc.id, hello.Spool.Epoch)
	ws.mu.Lock()
	applied := ws.applied
	ws.mu.Unlock()
	wc.spool = ws
	wc.helloLastSeq = hello.Spool.LastSeq
	if hello.Spool.LastSeq <= applied {
		wc.replayDone.Store(true)
		close(wc.replayDoneCh)
	} else {
		h.logger.Info("machine replays what it spooled", "worker_id", wc.id, "from_seq", applied+1, "to_seq", hello.Spool.LastSeq)
	}
	h.sendWorkerJSON(wc, protocol.MsgWorkerSpoolResume, protocol.SpoolResume{Epoch: hello.Spool.Epoch, AppliedSeq: applied})
	go h.spoolAckLoop(connCtx, wc)
}

// handleWorkerEnvelope applies one message from a daemon: spooled records at most once,
// in seq order.
func (h *Hub) handleWorkerEnvelope(ctx context.Context, wc *workerConn, env protocol.Envelope) {
	switch env.Type {
	case protocol.MsgWorkerSpoolCaughtUp:
		h.onSpoolCaughtUp(ctx, wc, env)
		return
	case protocol.MsgWorkerSpoolStatus:
		h.onSpoolStatus(ctx, wc, env)
		return
	}
	ws := wc.spool
	if env.Seq == 0 || ws == nil {
		h.handleWorkerMessage(ctx, wc, env)
		return
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if env.Seq <= ws.applied {
		ws.dups++
		return
	}
	mark := &store.SpoolMark{WorkerID: wc.id, Seq: env.Seq}
	h.handleWorkerMessage(store.WithSpoolMark(ctx, mark), wc, env)
	// Writes made later with this context (background work) are no longer guarded.
	written := !mark.Claim()
	ws.applied = env.Seq
	if written {
		ws.persisted = max(ws.persisted, env.Seq)
	} else if spoolPersistNow(env.Type) {
		// Statuses and session reports change state that a replay must not roll back.
		if err := h.store.AdvanceSpoolApplied(context.WithoutCancel(ctx), wc.id, env.Seq); err == nil {
			ws.persisted = max(ws.persisted, env.Seq)
		}
	}
}

// spoolPersistNow reports whether applying a record of msgType advances the stored seq at
// once (rather than with the next acknowledgement).
func spoolPersistNow(msgType string) bool {
	switch msgType {
	case protocol.MsgTaskStatus, protocol.MsgExternalSession, protocol.MsgOperatorCommand:
		return true
	}
	return false
}

// onSpoolCaughtUp ends the replay: records up to Seq the daemon did not send were
// superseded or compacted, so they count as applied.
func (h *Hub) onSpoolCaughtUp(ctx context.Context, wc *workerConn, env protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.SpoolAck](env)
	if err != nil || wc.spool == nil {
		return
	}
	ws := wc.spool
	ws.mu.Lock()
	ws.applied = max(ws.applied, p.Seq)
	dups := ws.dups
	ws.mu.Unlock()
	h.finishReplay(ctx, wc, dups)
}

// finishReplay runs once the daemon's replay is done: chats whose turn did not survive
// are failed now that every event they had is in.
func (h *Hub) finishReplay(ctx context.Context, wc *workerConn, dups uint64) {
	if !wc.replayDone.CompareAndSwap(false, true) {
		return
	}
	close(wc.replayDoneCh)
	h.logger.Info("machine finished replaying what it spooled", "worker_id", wc.id, "skipped_duplicates", dups)
	h.failInterruptedChats(context.WithoutCancel(ctx), wc)
	h.ackSpool(ctx, wc)
	h.broadcastWorker(ctx, wc.id)
}

// onSpoolStatus records the daemon's spool report and shows it in Fleet.
func (h *Hub) onSpoolStatus(ctx context.Context, wc *workerConn, env protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.SpoolStatus](env)
	if err != nil {
		return
	}
	ws := wc.spool
	if ws == nil {
		return
	}
	prev := ws.status.Swap(&p)
	if prev == nil || prev.Full != p.Full || prev.Replaying != p.Replaying || prev.ReplaySent != p.ReplaySent ||
		prev.Records != p.Records || prev.Bytes>>20 != p.Bytes>>20 {
		if p.Full && (prev == nil || !prev.Full) {
			h.logger.Warn("machine's spool is full: it no longer keeps vitals and activity while the hub is unreachable", "worker_id", wc.id, "reason", p.FullReason)
		}
		h.broadcastWorker(ctx, wc.id)
	}
}

// spoolAckLoop acknowledges applied records every spoolAckEvery until the connection ends.
func (h *Hub) spoolAckLoop(ctx context.Context, wc *workerConn) {
	t := time.NewTicker(spoolAckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.ackSpool(ctx, wc)
		}
	}
}

// ackSpool stores the applied seq and acknowledges it to the daemon.
func (h *Hub) ackSpool(ctx context.Context, wc *workerConn) {
	ws := wc.spool
	if ws == nil {
		return
	}
	ws.mu.Lock()
	applied, persisted, acked := ws.applied, ws.persisted, ws.acked
	ws.mu.Unlock()
	if applied <= acked {
		return
	}
	if applied > persisted {
		if err := h.store.AdvanceSpoolApplied(ctx, wc.id, applied); err != nil {
			if ctx.Err() == nil {
				h.logger.Warn("store applied spool seq failed", "worker_id", wc.id, "err", err)
			}
			return
		}
	}
	if !h.sendWorkerJSON(wc, protocol.MsgWorkerSpoolAck, protocol.SpoolAck{Seq: applied}) {
		return
	}
	ws.mu.Lock()
	ws.persisted = max(ws.persisted, applied)
	ws.acked = max(ws.acked, applied)
	ws.mu.Unlock()
}

// waitReplay blocks until wc's replay is done, the connection is no longer current, or
// ctx ends. It reports whether the replay is done.
func (h *Hub) waitReplay(ctx context.Context, wc *workerConn) bool {
	if wc.replayDoneCh == nil || wc.replayDone.Load() {
		return true
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-wc.replayDoneCh:
			return true
		case <-t.C:
			h.mu.RLock()
			current := h.workers[wc.id] == wc
			h.mu.RUnlock()
			if !current {
				return false
			}
		}
	}
}

// storeLateVitals stores a replayed heartbeat's vitals at its original time.
func (h *Hub) storeLateVitals(ctx context.Context, wc *workerConn, p protocol.WorkerHeartbeatPayload, seq uint64) {
	if p.Metrics == nil || p.At.IsZero() {
		return
	}
	cores, maxConc := 0, 0
	if w, err := h.store.GetWorker(ctx, wc.id); err == nil {
		maxConc = w.MaxConcurrent
		if f := decodeWorkerFacts(w.FactsJSON); f != nil {
			cores = f.CPUCores
		}
	}
	comps := HeatComponents(p.Metrics, cores, len(p.ActiveTaskIDs), maxConc)
	score, cause := rawHeat(comps)
	m := p.Metrics
	s := store.VitalsSample{
		WorkerID: wc.id, At: p.At.UTC(), N: max(p.Samples, 1),
		CPU: m.CPUPercent, Load: m.LoadAvg, Mem: float64(m.MemUsedMB), MemTotal: m.MemTotalMB, Swap: float64(m.SwapUsedMB),
		Heat: clamp01(score), HeatMax: clamp01(score), Cause: cause, SpoolSeq: seq,
	}
	if m.GPUPercent != nil {
		g := *m.GPUPercent
		s.GPU, s.GPUN = &g, s.N
	}
	if err := h.store.StoreLateVitals(ctx, []store.VitalsSample{s}, time.Now()); err != nil && err != store.ErrSpoolDuplicate {
		h.logger.Warn("store replayed vitals failed", "worker_id", wc.id, "at", p.At, "err", err)
	}
}

// decorateSpool adds the machine's spool status and wake grace to its fleet view.
func (h *Hub) decorateSpool(v *WorkerStatusView) {
	v.Spool = h.spoolStatusOf(v.ID)
	v.ReconnectingAfterSleep = h.sleepWake.machineInGrace(v.ID, time.Now())
}
