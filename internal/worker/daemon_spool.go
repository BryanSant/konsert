package worker

import (
	"context"
	"encoding/json/v2"
	"path/filepath"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/spool"
	"github.com/coder/websocket"
)

// Durable outbound spool wiring (KON-VZR9F, docs/design/spool.md). sendJSON appends
// spooled message types to d.spool; each connection's writer streams the spool from where
// the hub's worker.spool.resume says it stopped, so replayed and live records go out in
// one ordered stream.

// spoolDir is <state dir>/spool/<worker id> ("" without a state directory).
func spoolDir(stateDir, workerID string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "spool", safeFileName(workerID))
}

// openSpool opens the daemon's spool (nil when it cannot: messages are then sent live
// only and discarded while disconnected).
func (d *Daemon) openSpool(cfg config.WorkerConfig) *spool.Spool {
	dir := spoolDir(d.stateDir(), cfg.WorkerID)
	if dir == "" {
		d.logger.Warn("no state directory: hub-bound messages are not spooled")
		return nil
	}
	sp, err := spool.Open(spool.Options{
		Dir:         dir,
		MaxBytes:    int64(cfg.Spool.MaxMB) << 20,
		MaxAge:      time.Duration(cfg.Spool.MaxAgeDays) * 24 * time.Hour,
		Logger:      d.logger,
		MergeVitals: mergeVitals,
	})
	if err != nil {
		d.logger.Error("cannot open the outbound spool; hub-bound messages are not kept while disconnected", "dir", dir, "err", err)
		return nil
	}
	return sp
}

// spoolClass reports whether a worker → hub message is spooled and how.
func spoolClass(msgType string, payload any) (spool.Class, string, bool) {
	switch msgType {
	case protocol.MsgAgentChatEvent:
		if ev, ok := payload.(protocol.AgentChatEvent); ok && ev.Kind == protocol.EventElicitation {
			return spool.ClassQuestion, "", true
		}
		return spool.ClassChat, "", true
	case protocol.MsgTaskEvent:
		return spool.ClassTaskEvent, "", true
	case protocol.MsgTaskStatus:
		if p, ok := payload.(protocol.TaskStatusPayload); ok && protocol.IsTerminalTaskStatus(p.Status) {
			return spool.ClassFinalStatus, "", true
		}
		return spool.ClassStatus, "", true
	case protocol.MsgExternalSession, protocol.MsgOperatorCommand:
		return spool.ClassReport, "", true
	case protocol.MsgSessionActivity:
		key := ""
		if a, ok := payload.(protocol.SessionActivity); ok {
			key = "activity:" + a.Key()
		}
		return spool.ClassActivity, key, true
	case protocol.MsgWorkerHeartbeat:
		return spool.ClassVitals, "", true
	}
	return 0, "", false
}

// mergeVitals averages heartbeats into one (spool compaction): metrics weighted by their
// sample counts, the latest active task list, stamped with the period start.
func mergeVitals(start time.Time, recs []spool.Record) ([]byte, error) {
	var out protocol.WorkerHeartbeatPayload
	var m protocol.WorkerMetrics
	var n, gpuN float64
	var gpu float64
	var ts time.Time
	for _, r := range recs {
		env, err := protocol.DecodeEnvelope(r.Data)
		if err != nil {
			return nil, err
		}
		p, err := protocol.DecodePayload[protocol.WorkerHeartbeatPayload](env)
		if err != nil {
			return nil, err
		}
		ts = env.TS
		w := float64(max(p.Samples, 1))
		out.ActiveTaskIDs = p.ActiveTaskIDs
		if p.Metrics != nil {
			pm := p.Metrics
			m.CPUPercent += pm.CPUPercent * w
			m.LoadAvg += pm.LoadAvg * w
			m.MemUsedMB += uint64(float64(pm.MemUsedMB) * w)
			m.SwapUsedMB += uint64(float64(pm.SwapUsedMB) * w)
			m.MemTotalMB = max(m.MemTotalMB, pm.MemTotalMB)
			m.SwapTotalMB = max(m.SwapTotalMB, pm.SwapTotalMB)
			if pm.GPUPercent != nil {
				gpu += *pm.GPUPercent * w
				gpuN += w
			}
			n += w
		}
		out.Samples += int(w)
	}
	if n > 0 {
		m.CPUPercent /= n
		m.LoadAvg /= n
		m.MemUsedMB = uint64(float64(m.MemUsedMB) / n)
		m.SwapUsedMB = uint64(float64(m.SwapUsedMB) / n)
		if gpuN > 0 {
			g := gpu / gpuN
			m.GPUPercent = &g
		}
		out.Metrics = &m
		out.LoadAvg = m.LoadAvg
	}
	out.At = start.UTC()
	raw, err := json.Marshal(struct {
		Type    string                          `json:"type"`
		TS      time.Time                       `json:"ts"`
		Payload protocol.WorkerHeartbeatPayload `json:"payload"`
	}{protocol.MsgWorkerHeartbeat, ts, out}, protocol.JSONOptions)
	return raw, err
}

// spoolMessage appends an encoded envelope to the spool. It reports false when the
// message was not spooled (no spool, a write error, or a full spool for vitals and
// activity): the caller then sends it live if it can.
func (d *Daemon) spoolMessage(class spool.Class, key string, raw []byte) bool {
	if d.spool == nil {
		return false
	}
	if _, err := d.spool.Append(class, key, raw); err != nil {
		if err == spool.ErrFull {
			return true // dropped on purpose (counted in the spool status)
		}
		d.spoolWarnOnce.Do(func() {
			d.logger.Error("outbound spool write failed; sending without keeping", "err", err)
		})
		return false
	}
	return true
}

// spoolConn is one connection's position in the spool.
type spoolConn struct {
	mu          sync.Mutex
	cursor      uint64 // next seq to send; 0 until the hub resumed this connection
	start       uint64 // cursor at resume
	target      uint64 // last seq at resume: the replay ends after it
	caught      bool   // the replay reached target (worker.spool.caught_up sent)
	resetEpoch  string // non-empty when the spool was archived and reset to a fresh epoch
	gen         uint64 // incremented on resume/reset so an in-flight writeSpool cannot clobber a reset cursor
	resumedOnce bool
	resumed     chan struct{}
}

func newSpoolConn() *spoolConn { return &spoolConn{resumed: make(chan struct{})} }

// resume starts the stream after the hub's last applied seq.
func (c *spoolConn) resume(applied, last uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resumedOnce {
		return false
	}
	c.cursor, c.start, c.target = applied+1, applied+1, last
	c.caught = last <= applied
	c.gen++
	c.resumedOnce = true
	close(c.resumed)
	return true
}

// resumeReset starts a fresh epoch stream after archiving an orphaned spool: writeSpool
// sends worker.spool.caught_up{epoch: newEpoch, seq: 0} before any live records so the
// hub adopts the new epoch and finishes waiting for the old replay. Unlike resume, it
// resets the cursor even if the connection was previously resumed on the old epoch.
func (c *spoolConn) resumeReset(newEpoch string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cursor, c.start, c.target = 1, 1, 0
	c.caught = false
	c.resetEpoch = newEpoch
	c.gen++
	if !c.resumedOnce {
		c.resumedOnce = true
		close(c.resumed)
	}
	return true
}

func (c *spoolConn) position() (cursor, start, target uint64, caught bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cursor, c.start, c.target, c.caught
}

// handleSpoolMessage handles the hub's spool messages on connection sc. It reports
// whether env was one.
func (d *Daemon) handleSpoolMessage(sc *spoolConn, env protocol.Envelope) bool {
	switch env.Type {
	case protocol.MsgWorkerSpoolResume:
		p, err := protocol.DecodePayload[protocol.SpoolResume](env)
		if err != nil || d.spool == nil {
			return true
		}
		if p.HubID != "" {
			res, berr := d.spool.BindHub(p.HubID)
			if berr != nil {
				d.logger.Error("failed to bind outbound spool to hub", "hub_id", p.HubID, "err", berr)
			} else if res.Archived {
				oldHub := res.OldHubID
				if oldHub == "" {
					oldHub = "unknown"
				}
				d.logger.Warn("hub id changed; archived old spool and started a new epoch",
					"old_hub_id", oldHub, "new_hub_id", p.HubID,
					"archive", filepath.Base(res.ArchiveDir), "epoch", res.NewEpoch)
				if sc.resumeReset(res.NewEpoch) {
					// Kick a live heartbeat in the new epoch: serveConn's initial
					// kickHeartbeat may have landed in the archived old spool before
					// worker.spool.resume arrived.
					d.kickHeartbeat()
					d.kickSpoolStatus()
				}
				return true
			}
		}
		if p.Epoch != d.spool.Epoch() {
			// The hub tracks another epoch: it applied nothing of this one.
			p.AppliedSeq = 0
		}
		first, last := d.spool.Bounds()
		d.spool.Resumed(p.AppliedSeq)
		if sc.resume(p.AppliedSeq, last) {
			pending := uint64(0)
			if last > p.AppliedSeq {
				pending = last - p.AppliedSeq
			}
			if pending > 0 {
				d.logger.Info("replaying spooled messages to the hub", "from_seq", max(p.AppliedSeq+1, first), "to_seq", last, "records", pending)
			}
			d.kickSpoolStatus()
		}
		return true
	case protocol.MsgWorkerSpoolAck:
		if p, err := protocol.DecodePayload[protocol.SpoolAck](env); err == nil && d.spool != nil {
			d.spool.Ack(p.Seq)
		}
		return true
	}
	return false
}

// spoolBatch is how many records the writer sends between looks at the control queue.
const spoolBatch = 64

// writeSpool sends the next batch of spooled records on connection sc. It reports
// whether it sent or skipped anything (call again at once).
func (d *Daemon) writeSpool(sc *spoolConn, write func(outboundMsg) error) (bool, error) {
	if d.spool == nil {
		return false, nil
	}
	sc.mu.Lock()
	cursor, target, caught, resetEpoch, gen := sc.cursor, sc.target, sc.caught, sc.resetEpoch, sc.gen
	sc.mu.Unlock()
	if cursor == 0 {
		return false, nil
	}
	if !caught && cursor > target {
		raw, err := protocol.NewEnvelope(protocol.MsgWorkerSpoolCaughtUp, "", protocol.SpoolAck{Seq: target, Epoch: resetEpoch})
		if err == nil {
			if err := write(outboundMsg{msgType: websocket.MessageText, data: raw}); err != nil {
				return false, err
			}
		}
		sc.mu.Lock()
		if sc.gen == gen {
			sc.caught = true
			sc.resetEpoch = ""
		}
		sc.mu.Unlock()
		d.kickSpoolStatus()
		return true, nil
	}
	recs, next := d.spool.Read(cursor, spoolBatch, 1<<20)
	for _, r := range recs {
		sc.mu.Lock()
		stale := sc.gen != gen
		sc.mu.Unlock()
		if stale {
			return true, nil
		}
		if err := write(outboundMsg{msgType: websocket.MessageText, data: protocol.WithSeq(r.Data, r.Seq)}); err != nil {
			return false, err
		}
		d.spool.MarkSent(r.Seq)
		sc.mu.Lock()
		if sc.gen != gen {
			sc.mu.Unlock()
			return true, nil
		}
		sc.cursor = r.Seq + 1
		sc.mu.Unlock()
	}
	sc.mu.Lock()
	if sc.gen != gen {
		sc.mu.Unlock()
		return true, nil
	}
	sc.cursor = max(sc.cursor, next)
	cursor = sc.cursor
	sc.mu.Unlock()
	if !caught && cursor > target {
		raw, err := protocol.NewEnvelope(protocol.MsgWorkerSpoolCaughtUp, "", protocol.SpoolAck{Seq: target})
		if err == nil {
			if err := write(outboundMsg{msgType: websocket.MessageText, data: raw}); err != nil {
				return false, err
			}
		}
		sc.mu.Lock()
		if sc.gen == gen {
			sc.caught = true
		}
		sc.mu.Unlock()
		d.kickSpoolStatus()
	}
	return len(recs) > 0, nil
}

// spoolStatus is the spool's state for status reports, with sc's replay progress.
func (d *Daemon) spoolStatus(sc *spoolConn) *protocol.SpoolStatus {
	if d.spool == nil {
		return nil
	}
	st := d.spool.Status()
	out := &protocol.SpoolStatus{
		Bytes: st.Bytes, BudgetBytes: st.BudgetBytes, Records: st.Records, OldestUnsentAt: st.OldestUnsentAt,
		LastSeq: st.LastSeq, AckedSeq: st.AckedSeq, Full: st.Full != "", FullReason: st.Full, Dropped: st.Dropped,
	}
	if st.Err != nil {
		out.Error = st.Err.Error()
	}
	if sc != nil {
		cursor, start, target, caught := sc.position()
		if cursor != 0 && !caught && target >= start {
			out.Replaying = true
			out.ReplayTotal = target - start + 1
			if cursor > start {
				out.ReplaySent = min(cursor-start, out.ReplayTotal)
			}
		}
	}
	return out
}

func (d *Daemon) kickSpoolStatus() {
	if d.spoolKick == nil {
		return
	}
	select {
	case d.spoolKick <- struct{}{}:
	default:
	}
}

func (d *Daemon) kickHeartbeat() {
	if d.hbKick == nil {
		return
	}
	select {
	case d.hbKick <- struct{}{}:
	default:
	}
}

// spoolStatusLoop reports the spool's state to the hub (worker.spool.status, not
// spooled) and to `konsertd status`: every 2 s while replaying, otherwise every 30 s or
// when something changed, until ctx (the connection) ends.
func (d *Daemon) spoolStatusLoop(ctx context.Context, sc *spoolConn) {
	if d.spool == nil {
		return
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var last protocol.SpoolStatus
	var lastAt time.Time
	report := func(force bool) {
		st := d.spoolStatus(sc)
		d.link.setSpool(st)
		changed := st.Replaying != last.Replaying || st.Full != last.Full || st.ReplaySent != last.ReplaySent ||
			st.Records != last.Records || (st.OldestUnsentAt.IsZero() != last.OldestUnsentAt.IsZero())
		// A busy spool changes every tick: outside a replay, once in 10 s is enough.
		since := time.Since(lastAt)
		if !force && !(changed && (st.Replaying || since >= 10*time.Second)) && since < 30*time.Second {
			return
		}
		last, lastAt = *st, time.Now()
		d.sendJSON(protocol.MsgWorkerSpoolStatus, st)
	}
	select {
	case <-ctx.Done():
		return
	case <-sc.resumed:
	}
	report(true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			report(false)
		case <-d.spoolKick:
			report(true)
		}
	}
}

// spoolDisconnectedLoop keeps `konsertd status` current while no hub is connected.
func (d *Daemon) spoolDisconnectedLoop(ctx context.Context) {
	if d.spool == nil {
		return
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.sendMu.RLock()
			connected := d.queues != nil
			d.sendMu.RUnlock()
			if !connected {
				d.link.setSpool(d.spoolStatus(nil))
			}
		}
	}
}
