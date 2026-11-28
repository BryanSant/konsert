package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/policy"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Break glass (SYR-ADQZX). A machine policy rule (max sessions, quiet hours, ...) can
// be suspended for a while with a policy-scope override (request_override). Like every
// override it needs the user's approval, unless the user turned on
// override_self_approve; it expires by itself and its uses are counted. Every time a
// rule or directive keeps work off a machine is counted (each piece of work at most
// once an hour), and GET /api/policy/report sets the blocks against the overrides that
// followed.

// overrideSelfApproveSetting lets the commander approve its own override requests.
const overrideSelfApproveSetting = "override_self_approve"

// policyBlockKeep is how long block counters are kept (the report reads a week).
const policyBlockKeep = 90 * 24 * time.Hour

// overrideRules validates the rules a policy override names.
func overrideRules(in []string) ([]string, error) {
	var out []string
	for _, r := range in {
		r = strings.TrimSpace(r)
		if r == "" || slices.Contains(out, r) {
			continue
		}
		if !slices.Contains(policy.Rules, r) {
			return nil, fmt.Errorf("unknown policy rule %q (want %s)", r, strings.Join(policy.Rules, ", "))
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("rules is required for scope policy: name the policy rules to suspend (%s)", strings.Join(policy.Rules, ", "))
	}
	return out, nil
}

func (h *Hub) overrideSelfApprove(ctx context.Context) bool {
	kv, err := h.store.GetAllSettings(ctx)
	return err == nil && kv[overrideSelfApproveSetting] == "true"
}

// policyLifts are the policy rules approved overrides suspend now, by machine, with
// the override that lifts each machine's rules (the first, for use counting).
type policyLifts map[string]struct {
	rules map[string]bool
	by    store.Override
	until time.Time
}

func (h *Hub) policyLifts(ctx context.Context, now time.Time) policyLifts {
	out := policyLifts{}
	for _, o := range h.activeOverrides(ctx, now) {
		if o.ScopeKind != store.OverrideScopePolicy || o.WorkerID == "" {
			continue
		}
		l, ok := out[o.WorkerID]
		if !ok {
			l.rules, l.by, l.until = map[string]bool{}, o, *o.ExpiresAt
		}
		for _, r := range o.Rules {
			l.rules[r] = true
		}
		if o.ExpiresAt.Before(l.until) {
			l.until = *o.ExpiresAt
		}
		out[o.WorkerID] = l
	}
	return out
}

// workerPolicyLifts is what the daemon gets in worker.directives: the rules approved
// overrides suspend on it, with when each lift ends.
func (h *Hub) workerPolicyLifts(ctx context.Context, workerID string) []protocol.PolicyLift {
	var out []protocol.PolicyLift
	for _, o := range h.activeOverrides(ctx, h.directiveClock()) {
		if o.ScopeKind == store.OverrideScopePolicy && o.WorkerID == workerID {
			out = append(out, protocol.PolicyLift{OverrideSID: o.SID, Rules: o.Rules, Until: *o.ExpiresAt})
		}
	}
	return out
}

// blockLog counts blocks in memory until the next flush; seen dedupes a piece of work
// within its hour.
type blockLog struct {
	mu      sync.Mutex
	seen    map[string]bool
	hour    time.Time
	pending []store.PolicyBlock
}

// noteBlock counts that rule kept subject (a task, ticket or chat; "" for a whole
// placement pass) off workerID, at most once per hour.
func (h *Hub) noteBlock(workerID, rule, subject, msg string) {
	now := time.Now().UTC()
	hour := now.Truncate(time.Hour)
	b := &h.blocks
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.hour.Equal(hour) {
		b.hour, b.seen = hour, map[string]bool{}
	}
	key := workerID + "|" + rule + "|" + subject
	if b.seen[key] {
		return
	}
	b.seen[key] = true
	b.pending = append(b.pending, store.PolicyBlock{Hour: hour, WorkerID: workerID, Rule: rule, N: 1, Subject: subject, Message: msg})
}

// flushBlocks writes the counted blocks.
func (h *Hub) flushBlocks(ctx context.Context) {
	if h.store == nil {
		return
	}
	h.blocks.mu.Lock()
	pending := h.blocks.pending
	h.blocks.pending = nil
	h.blocks.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := h.store.RecordPolicyBlocks(wctx, pending); err != nil {
		h.logger.Warn("policy blocks not stored", "n", len(pending), "err", err)
	}
}

// PolicyReportRow is one machine and rule over the report window.
type PolicyReportRow struct {
	WorkerID   string `json:"worker_id"`
	WorkerName string `json:"worker_name"`
	Rule       string `json:"rule"`
	Blocks     int    `json:"blocks"`
	// Overrides asked for this machine and rule in the window, and what became of them.
	Requested int `json:"overrides_requested"`
	Approved  int `json:"overrides_approved"`
	Denied    int `json:"overrides_denied"`
	Used      int `json:"overrides_used"`
	// Followed: some override request came after the first block (break glass used).
	Followed    bool      `json:"followed"`
	LastMessage string    `json:"last_message,omitempty"`
	LastBlock   time.Time `json:"last_block,omitzero"`
}

// PolicyReport is GET /api/policy/report.
type PolicyReport struct {
	Since     time.Time         `json:"since"`
	Rows      []PolicyReportRow `json:"rows"`
	Overrides []store.Override  `json:"overrides"`
	Totals    struct {
		Blocks, Requested, Approved, Denied, Used int
	} `json:"totals"`
}

// overrideRuleKeys maps an override to the report rules it answers: a policy override
// to the rules it lifts, a slot override to max sessions and throttles, a hold
// override to holds.
func overrideRuleKeys(o store.Override) []string {
	switch o.ScopeKind {
	case store.OverrideScopePolicy:
		return o.Rules
	case store.OverrideScopeMachineSlots:
		return []string{policy.RuleMaxSessions, store.DirectiveMachineThrottle}
	default:
		return []string{"hold"}
	}
}

// policyReport sets the blocks since since against the overrides requested since.
func (h *Hub) policyReport(ctx context.Context, since time.Time) (PolicyReport, error) {
	h.flushBlocks(ctx)
	rep := PolicyReport{Since: since.UTC()}
	blocks, err := h.store.ListPolicyBlocks(ctx, since)
	if err != nil {
		return rep, err
	}
	ovs, err := h.store.ListOverrides(ctx, store.OverrideFilter{Limit: 1000})
	if err != nil {
		return rep, err
	}
	type key struct{ worker, rule string }
	rows := map[key]*PolicyReportRow{}
	first := map[key]time.Time{}
	row := func(k key) *PolicyReportRow {
		r := rows[k]
		if r == nil {
			r = &PolicyReportRow{WorkerID: k.worker, WorkerName: h.workerLabel(ctx, k.worker), Rule: k.rule}
			rows[k] = r
		}
		return r
	}
	for _, b := range blocks {
		k := key{b.WorkerID, b.Rule}
		r := row(k)
		r.Blocks += b.N
		if b.Hour.After(r.LastBlock) || r.LastBlock.IsZero() {
			r.LastBlock, r.LastMessage = b.Hour, b.Message
		}
		if f, ok := first[k]; !ok || b.Hour.Before(f) {
			first[k] = b.Hour
		}
		rep.Totals.Blocks += b.N
	}
	for _, o := range ovs {
		if o.CreatedAt.Before(since) {
			continue
		}
		rep.Overrides = append(rep.Overrides, o)
		rep.Totals.Requested++
		approved := o.ApprovedAt != nil
		switch {
		case approved:
			rep.Totals.Approved++
		case o.Status == store.OverrideDenied:
			rep.Totals.Denied++
		}
		if o.UseCount > 0 {
			rep.Totals.Used++
		}
		for _, rule := range overrideRuleKeys(o) {
			k := key{o.WorkerID, rule}
			r := row(k)
			r.Requested++
			if approved {
				r.Approved++
			} else if o.Status == store.OverrideDenied {
				r.Denied++
			}
			if o.UseCount > 0 {
				r.Used++
			}
			// The first block's hour may start before the request in the same hour.
			if f, ok := first[k]; ok && !o.CreatedAt.Before(f) {
				r.Followed = true
			}
		}
	}
	for _, r := range rows {
		rep.Rows = append(rep.Rows, *r)
	}
	sort.Slice(rep.Rows, func(i, j int) bool {
		if rep.Rows[i].Blocks != rep.Rows[j].Blocks {
			return rep.Rows[i].Blocks > rep.Rows[j].Blocks
		}
		if rep.Rows[i].WorkerName != rep.Rows[j].WorkerName {
			return rep.Rows[i].WorkerName < rep.Rows[j].WorkerName
		}
		return rep.Rows[i].Rule < rep.Rows[j].Rule
	})
	return rep, nil
}

// handlePolicyReport serves GET /api/policy/report?days=7.
func (s *Server) handlePolicyReport(w http.ResponseWriter, r *http.Request) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 90 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "days must be 1-90"})
			return
		}
		days = n
	}
	rep, err := s.hub.policyReport(r.Context(), time.Now().Add(-time.Duration(days)*24*time.Hour))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
