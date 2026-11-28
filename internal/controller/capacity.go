package controller

import (
	"cmp"
	"context"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/capacity"
	"github.com/BryanSant/syre/internal/policy"
	"github.com/BryanSant/syre/internal/protocol"
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
		e.peak = h.expectedPeak(ctx, e.key, class)
	}
	h.estimates.mu.Lock()
	if h.estimates.m == nil || len(h.estimates.m) > 4096 {
		h.estimates.m = map[string]sessionEstimate{}
	}
	h.estimates.m[ck] = e
	h.estimates.mu.Unlock()
	return e.key, e.peak
}

// expectedPeak is the memory work of key is expected to peak at: its profile's highest
// peak on the machine class, else on any class ("" class: any); 0 when unknown.
func (h *Hub) expectedPeak(ctx context.Context, key, class string) uint64 {
	if key == "" || h.store == nil {
		return 0
	}
	profiles, err := h.store.ListWorkProfiles(ctx, key)
	if err != nil {
		return 0
	}
	var own, any uint64
	for _, p := range profiles {
		if class != "" && p.MachineClass == class {
			own = p.PeakRSSMaxMB
		}
		any = max(any, p.PeakRSSMaxMB)
	}
	return cmp.Or(own, any)
}

// headroomFn is workerHeadroom bound to ctx, for the placement scorer.
func (h *Hub) headroomFn(ctx context.Context) func(store.Worker) *capacity.Headroom {
	return func(w store.Worker) *capacity.Headroom { return h.workerHeadroom(ctx, w) }
}

// accountRemaining is an account's remaining share of its tightest usage window
// (known false without usage reports).
func accountRemaining(a protocol.AgentAccount) (float64, bool) {
	if len(a.Usage) == 0 {
		return 0, false
	}
	used := 0.0
	for _, u := range a.Usage {
		used = max(used, u.UsedPercent)
	}
	return max(0, 1-used/100), true
}

// placementAccount picks the agent account work would use on a machine (SYR-NCEHE):
// the one asked for, else the signed-in account of the agent with the most quota left,
// skipping exhausted ones until their reset. why says when every usable account there
// is exhausted.
func (h *Hub) placementAccount(ctx context.Context, w store.Worker, agent, accountID string, now time.Time) (string, float64, bool, string) {
	if agent == "" || agent == "shell" {
		return "", 0, false, ""
	}
	accts := h.workerAccounts(ctx, w.ID)
	if accountID != "" {
		a, ok := findAccount(accts, agent, accountID)
		if !ok {
			return accountID, 0, false, ""
		}
		if u, exhausted := exhaustedWindow(a, now); exhausted {
			return "", 0, false, exhaustedReason(a, u)
		}
		q, known := accountRemaining(a)
		return a.ID, q, known, ""
	}
	var best *protocol.AgentAccount
	bestQ, bestKnown := -1.0, false
	var spent []string
	for i, a := range accts {
		if !strings.EqualFold(a.Agent, agent) || a.Auth.State == protocol.AuthLoggedOut {
			continue
		}
		if u, exhausted := exhaustedWindow(a, now); exhausted {
			spent = append(spent, exhaustedReason(a, u))
			continue
		}
		q, known := accountRemaining(a)
		if !known {
			q = 0.5
		}
		// Most quota left; the default account breaks a tie.
		if best == nil || q > bestQ || q == bestQ && a.IsDefault && !best.IsDefault {
			best, bestQ, bestKnown = &accts[i], q, known
		}
	}
	if best == nil {
		if len(spent) > 0 {
			return "", 0, false, strings.Join(spent, "; ")
		}
		return "", 0, false, "" // no account reported: the worker decides
	}
	if !bestKnown {
		bestQ = 0
	}
	return best.ID, bestQ, bestKnown, ""
}

func exhaustedReason(a protocol.AgentAccount, u protocol.AgentRateLimit) string {
	msg := cmpOr(a.Label, a.ID) + " has used its " + u.Window + " window"
	if u.ResetsAt != nil {
		msg += " (resets " + u.ResetsAt.UTC().Format("15:04 MST") + ")"
	}
	return msg
}
