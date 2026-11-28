package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/policy"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Machine policy (SYR-BJDY6). A machine's standing rules for new work, set by the user
// over REST (the UI and the CLI) and audited. MCP credentials never reach REST, so the
// Commander can read the policy (fleet_status) but not change it; it tightens through
// set_policy's timed directives instead, which the user can end. The hub checks the
// policy at admission and pushes it to the daemon with the directives, and the daemon
// checks it again before it starts anything.

// AuditMachinePolicy records a policy change, with the old and new policy.
const AuditMachinePolicy = "machine.policy"

// workerPolicy decodes a worker's stored policy (nil when none or unreadable).
func workerPolicy(w store.Worker) *protocol.MachinePolicy {
	if w.PolicyJSON == "" {
		return nil
	}
	var p protocol.MachinePolicy
	if err := json.Unmarshal([]byte(w.PolicyJSON), &p, protocol.JSONOptions); err != nil {
		return nil
	}
	return &p
}

// policyState is what the hub knows of w for a policy check: hosted is the sessions
// it counts there, load and memory come from the last heartbeat, cores and the
// survival reserve from its facts.
func (h *Hub) policyState(w store.Worker, work string, hosted int) policy.State {
	st := policy.State{Work: work, Hosted: hosted, LoadAvg: -1, AvailRAMMB: -1, UserIdleS: -1, Now: h.directiveClock()}
	if m := h.WorkerMetrics(w.ID); m != nil {
		st.LoadAvg = m.LoadAvg
		if m.Load != nil && m.Load.UserIdleS != nil {
			st.UserIdleS = *m.Load.UserIdleS
		}
		if m.MemTotalMB > 0 {
			st.AvailRAMMB = int(m.MemTotalMB) - int(m.MemUsedMB)
		}
	}
	if w.FactsJSON != "" {
		var f protocol.MachineFacts
		if json.Unmarshal([]byte(w.FactsJSON), &f, protocol.JSONOptions) == nil {
			st.CPUs = f.CPUCores
			st.SurvivalReserveMB = int(f.SurvivalReserveMB)
		}
	}
	return st
}

// policyViolation is the rule w's policy would refuse new work of class work for
// (nil: admitted), with the rules approved overrides lifted. A refusal is counted for
// the policy report under subject. The override returned let the work through: it
// counts as used once the work is placed.
func (h *Hub) policyViolation(ctx context.Context, w store.Worker, work string, hosted int, subject string) (*policy.Violation, *store.Override) {
	p := workerPolicy(w)
	if p == nil {
		return nil, nil
	}
	st := h.policyState(w, work, hosted)
	// Ticket runs are the scheduler's; other work is automatic unless a person started it.
	st.Automatic = work == protocol.WorkTicket || isAutomatic(causeFrom(ctx))
	v := policy.Check(p, st)
	if v == nil {
		return nil, nil
	}
	if l, ok := h.policyLifts(ctx, h.directiveClock())[w.ID]; ok {
		st.Lifted = l.rules
		if lv := policy.Check(p, st); lv == nil {
			o := l.by
			return nil, &o
		} else {
			v = lv
		}
	}
	if !isDryRun(ctx) {
		h.noteBlock(w.ID, v.Rule, subject, v.Message)
	}
	return v, nil
}

// liftUses maps a machine to the override that admitted work there.
type liftUses map[string]store.Override

// used counts the override that let work onto workerID, once the work is placed.
func (h *Hub) liftUsed(ctx context.Context, uses liftUses, workerID string) {
	if o, ok := uses[workerID]; ok {
		h.useOverride(ctx, o)
	}
}

// applyMachinePolicy drops the machines whose policy refuses new work of class work now
// (subject names it for the block counters) and caps the others' slots at their max
// sessions.
func (h *Hub) applyMachinePolicy(ctx context.Context, workers []store.Worker, counts map[string]int, work, subject string) ([]store.Worker, liftUses) {
	out := workers[:0:0]
	uses := liftUses{}
	for _, w := range workers {
		p := workerPolicy(w)
		if p == nil {
			out = append(out, w)
			continue
		}
		v, by := h.policyViolation(ctx, w, work, counts[w.ID], subject)
		if v != nil {
			continue
		}
		if by != nil {
			uses[w.ID] = *by
		}
		lifted := by != nil && slices.Contains(by.Rules, policy.RuleMaxSessions)
		if p.MaxSessions > 0 && !lifted && (w.MaxConcurrent <= 0 || p.MaxSessions < w.MaxConcurrent) {
			w.MaxConcurrent = p.MaxSessions
		}
		out = append(out, w)
	}
	return out, uses
}

// taskWork is a task's work class.
func taskWork(ticketID string) string {
	if ticketID != "" {
		return protocol.WorkTicket
	}
	return protocol.WorkTask
}

// errPolicyRefused wraps a violation for a dispatch pinned to a machine.
type errPolicyRefused struct {
	machine string
	v       *policy.Violation
}

func (e *errPolicyRefused) Error() string { return fmt.Sprintf("%s: %s", e.machine, e.v.Message) }

func (s *Server) handleGetWorkerPolicy(w http.ResponseWriter, r *http.Request) {
	wk, err := s.store.GetWorker(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": policyOrEmpty(workerPolicy(wk))})
}

// handleSetWorkerPolicy replaces a machine's policy (an empty one clears it). Only the
// user sets it: MCP credentials are refused before this handler.
func (s *Server) handleSetWorkerPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var next protocol.MachinePolicy
	if !decodeJSON(w, r, &next) {
		return
	}
	if err := policy.Validate(&next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	wk, err := s.store.GetWorker(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	prev := policyOrEmpty(workerPolicy(wk))
	raw := ""
	if !reflect.DeepEqual(next, protocol.MachinePolicy{}) {
		b, err := json.Marshal(next, protocol.JSONOptions)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		raw = string(b)
	}
	if err := s.store.SetWorkerPolicy(r.Context(), id, raw); err != nil {
		writeStoreError(w, err)
		return
	}
	s.hub.afterPolicyChanged(r.Context(), wk, prev, next)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "policy": next})
}

func policyOrEmpty(p *protocol.MachinePolicy) protocol.MachinePolicy {
	if p == nil {
		return protocol.MachinePolicy{}
	}
	return *p
}

// afterPolicyChanged audits the change, pushes the policy to the machine and lets the
// scheduler re-place work it now refuses or admits.
func (h *Hub) afterPolicyChanged(ctx context.Context, wk store.Worker, prev, next protocol.MachinePolicy) {
	meta, _ := json.Marshal(map[string]any{"before": prev, "after": next}, protocol.JSONOptions)
	h.recordAudit(ctx, store.AuditEvent{
		Action:    AuditMachinePolicy,
		WorkerID:  wk.ID,
		WorkerSID: wk.SID,
		Message:   "machine policy for " + wk.Name + " updated",
		MetaJSON:  string(meta),
	})
	h.pushMachineDirectives(ctx, wk.ID)
	if updated, err := h.store.GetWorker(ctx, wk.ID); err == nil {
		view := toWorkerView(updated, h.ActiveTaskCounts()[wk.ID], h.WorkerMetrics(wk.ID))
		if view.Status == "online" && !h.IsWorkerConnected(wk.ID) {
			view.Status = "offline"
		}
		h.broadcastFleetUpdate("worker", view)
	}
	h.kickScheduler()
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "worker not found"})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// SetWorkerPolicy replaces a machine's policy over PUT /api/workers/{id}/policy (the
// CLI's syre policy; an empty policy clears it).
func (c *RemoteFleetClient) SetWorkerPolicy(ctx context.Context, workerID string, p protocol.MachinePolicy) (protocol.MachinePolicy, error) {
	var res struct {
		Policy protocol.MachinePolicy `json:"policy"`
	}
	err := c.doJSON(ctx, http.MethodPut, "/api/workers/"+url.PathEscape(workerID)+"/policy", p, &res)
	return res.Policy, err
}

// PolicyRefusal is why a machine's policy refuses new work now.
type PolicyRefusal struct {
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// policyRefusal checks p for work a person starts now (the rules every piece of work
// meets; an interactive machine's "in use" hold is for automatic work only, and the
// guardrails' survival reserve is the floor under the RAM rule). nil: it takes work.
func policyRefusal(p *protocol.MachinePolicy, hosted int, m *protocol.WorkerMetrics, f *protocol.MachineFacts, now time.Time) *PolicyRefusal {
	if p == nil {
		return nil
	}
	st := policy.State{Hosted: hosted, LoadAvg: -1, AvailRAMMB: -1, UserIdleS: -1, Now: now}
	if m != nil {
		st.LoadAvg = m.LoadAvg
		if m.MemTotalMB > 0 {
			st.AvailRAMMB = int(m.MemTotalMB) - int(m.MemUsedMB)
		}
	}
	if f != nil {
		st.CPUs = f.CPUCores
		st.SurvivalReserveMB = int(f.SurvivalReserveMB)
	}
	v := policy.Check(p, st)
	if v == nil {
		return nil
	}
	return &PolicyRefusal{Rule: v.Rule, Message: strings.TrimPrefix(v.Message, "machine policy: ")}
}
