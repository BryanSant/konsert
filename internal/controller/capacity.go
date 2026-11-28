package controller

import (
	"cmp"
	"context"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/capacity"
	"github.com/BryanSant/syre/internal/policy"
	"github.com/BryanSant/syre/internal/store"
)

// Capacity model (SYR-0AANJ): each online machine's headroom, from its last heartbeat,
// its facts and policy, the running managed sessions' work profiles and its shed
// cooldown. It is in the fleet snapshot; the placement scorer reads it.

// sessionEstimate is a running session's work key and expected peak, cached briefly:
// both change only when the session or its profile does.
type sessionEstimate struct {
	key  string
	peak uint64
	at   time.Time
}

type estimateCache struct {
	mu sync.Mutex
	m  map[string]sessionEstimate // by subject@class
}

const estimateTTL = time.Minute

// workerHeadroom is w's headroom (nil without a heartbeat carrying memory).
func (h *Hub) workerHeadroom(ctx context.Context, w store.Worker) *capacity.Headroom {
	m := h.WorkerMetrics(w.ID)
	if m == nil || m.MemTotalMB == 0 {
		return nil
	}
	now := time.Now()
	in := capacity.Inputs{TotalRAMMB: int(m.MemTotalMB), AvailRAMMB: int(m.MemTotalMB) - int(m.MemUsedMB),
		LoadAvg: m.LoadAvg, CPUPercent: m.CPUPercent, Now: now}
	if f := decodeWorkerFacts(w.FactsJSON); f != nil {
		in.CPUs, in.SurvivalReserveMB = f.CPUCores, int(f.SurvivalReserveMB)
	}
	if p := workerPolicy(w); p != nil {
		in.PolicyReserveMB, in.ReserveCPU = policy.ExtraReserveMB(p), p.ReserveCPU
	}
	if m.Load != nil {
		class := ""
		for _, sl := range m.Load.Sessions {
			if sl.TaskID == "" && sl.ChatID == "" {
				continue // observed: its use is in the available figure, its growth unknown
			}
			if class == "" {
				class = h.machineClassFor(ctx, w.ID)
			}
			s := capacity.Session{RSSMB: sl.RSSMB, Subject: taskSubject(sl.TaskID)}
			if sl.TaskID == "" {
				s.Subject = chatSubject(sl.ChatID)
			}
			s.WorkKey, s.ExpectedPeakMB = h.sessionEstimate(ctx, sl.TaskID, s.Subject, class, now)
			in.Sessions = append(in.Sessions, s)
		}
	}
	if until, ok := h.shedCooldown.until(w.ID); ok {
		in.ShedCooldownUntil = until
	}
	hr, ok := capacity.Compute(in)
	if !ok {
		return nil
	}
	return &hr
}

// sessionEstimate is a running task's work key and the peak its profile expects
// (the machine class's own, else the highest any class saw; 0: unknown). Chats have no
// work key yet: they count as the small class.
func (h *Hub) sessionEstimate(ctx context.Context, taskID, subject, class string, now time.Time) (string, uint64) {
	if taskID == "" || h.store == nil {
		return "", 0
	}
	ck := subject + "@" + class
	h.estimates.mu.Lock()
	if e, ok := h.estimates.m[ck]; ok && now.Sub(e.at) < estimateTTL {
		h.estimates.mu.Unlock()
		return e.key, e.peak
	}
	h.estimates.mu.Unlock()
	e := sessionEstimate{at: now}
	if task, err := h.store.GetTask(ctx, taskID); err == nil {
		var a store.Attempt
		if rows, err := h.store.ListAttempts(ctx, "", taskID); err == nil && len(rows) > 0 {
			a = rows[len(rows)-1]
		}
		e.key = cmpOr(task.WorkKey, h.workKeyFor(ctx, a, task))
		if profiles, err := h.store.ListWorkProfiles(ctx, e.key); err == nil {
			var own, any uint64
			for _, p := range profiles {
				if p.MachineClass == class {
					own = p.PeakRSSMaxMB
				}
				any = max(any, p.PeakRSSMaxMB)
			}
			e.peak = cmp.Or(own, any)
		}
	}
	h.estimates.mu.Lock()
	if h.estimates.m == nil || len(h.estimates.m) > 4096 {
		h.estimates.m = map[string]sessionEstimate{}
	}
	h.estimates.m[ck] = e
	h.estimates.mu.Unlock()
	return e.key, e.peak
}
