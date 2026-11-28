package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Placement rules and the placement log (SYR-880CP). A rule prefers, requires or avoids
// a machine or machine class for a scope; rules act only among the machines the
// policy admits (they beat scores, never policy). Every placement is logged with its
// candidates, rejections, the rules that applied and who decided.

// placementScope is what rules can be scoped to.
type placementScope struct {
	ProjectID, ProjectSID string
	Repo                  string // the repo's name
	Branch                string
	WorkKey               string
}

// placementScopeOf is the scope of work in projectID / repoID (or dir) on branch.
func (h *Hub) placementScopeOf(ctx context.Context, projectID, repoID, dir, branch, workKey string) placementScope {
	sc := placementScope{ProjectID: projectID, Branch: strings.TrimSpace(branch), WorkKey: workKey}
	if projectID != "" {
		if p, err := h.store.GetProject(ctx, projectID); err == nil {
			sc.ProjectSID = p.SID
		}
	}
	if repoID != "" {
		if r, err := h.store.GetRepo(ctx, repoID); err == nil {
			sc.Repo = r.Name
		}
	} else if dir = strings.TrimSpace(dir); dir != "" && dir != "~" {
		sc.Repo = filepathBase(dir)
	}
	return sc
}

func filepathBase(dir string) string {
	dir = strings.TrimRight(strings.ReplaceAll(dir, `\`, "/"), "/")
	if i := strings.LastIndexByte(dir, '/'); i >= 0 {
		return dir[i+1:]
	}
	return dir
}

// ruleApplies reports whether rule r's scope covers sc. A work key scope ending in "*"
// matches keys starting with what precedes it.
func ruleApplies(r store.PlacementRule, sc placementScope) bool {
	v := strings.TrimSpace(r.ScopeValue)
	switch r.ScopeKind {
	case store.RuleScopeAny:
		return true
	case store.RuleScopeProject:
		return v != "" && (strings.EqualFold(v, sc.ProjectID) || strings.EqualFold(v, sc.ProjectSID))
	case store.RuleScopeRepo:
		return v != "" && strings.EqualFold(v, sc.Repo)
	case store.RuleScopeBranch:
		return v != "" && v == sc.Branch
	case store.RuleScopeWorkKey:
		if prefix, ok := strings.CutSuffix(v, "*"); ok {
			return strings.HasPrefix(sc.WorkKey, prefix)
		}
		return v != "" && v == sc.WorkKey
	}
	return false
}

// placementRulesFor lists the active rules covering sc.
func (h *Hub) placementRulesFor(ctx context.Context, sc placementScope) []store.PlacementRule {
	all, err := h.store.ListPlacementRules(ctx, h.directiveClock())
	if err != nil {
		return nil
	}
	var out []store.PlacementRule
	for _, r := range all {
		if ruleApplies(r, sc) {
			out = append(out, r)
		}
	}
	return out
}

// placementOpts are the scorer inputs for work of scope sc expected to peak at peak.
func (h *Hub) placementOpts(ctx context.Context, sc placementScope, peak uint64, seed string, locality LocalityScoreFn) PlacementOpts {
	return PlacementOpts{
		Locality: locality,
		Headroom: h.headroomFn(ctx),
		Work:     placementWork(peak, seed),
		Rules:    h.placementRulesFor(ctx, sc),
		ClassOf:  func(w store.Worker) string { return h.machineClassFor(ctx, w.ID) },
	}
}

// PlacementDetail is a placement log entry's detail.
type PlacementDetail struct {
	PlacementOutcome
	PeakMB uint64 `json:"peak_mb,omitzero"`
	Known  bool   `json:"known,omitzero"`
}

// deciderOf names who placed work: the user, the Commander, or the scheduler.
func deciderOf(c store.Cause) string {
	switch c.Actor {
	case store.ActorUser:
		return "user"
	case store.ActorCommander:
		return "commander"
	}
	return "scheduler"
}

// logPlacement records a placement (chosen "" when nothing was chosen).
func (h *Hub) logPlacement(ctx context.Context, subject, workKey, decider, chosen string, out PlacementOutcome, w placementWorkInfo) {
	if h.store == nil {
		return
	}
	raw, err := json.Marshal(PlacementDetail{PlacementOutcome: out, PeakMB: w.peak, Known: w.peak > 0}, protocol.JSONOptions)
	if err != nil {
		return
	}
	if err := h.store.AppendPlacementLog(ctx, store.PlacementLogEntry{Subject: subject, WorkKey: workKey, Decider: decider,
		WorkerID: chosen, DetailJSON: string(raw)}); err != nil {
		h.logger.Warn("placement log write failed", "subject", subject, "err", err)
	}
}

type placementWorkInfo struct{ peak uint64 }

// placementLogKeep is how long the placement log is kept.
const placementLogKeep = 14 * 24 * time.Hour

// prunePlacementLog drops entries older than placementLogKeep.
func (h *Hub) prunePlacementLog(ctx context.Context) {
	if _, err := h.store.PrunePlacementLog(ctx, time.Now().Add(-placementLogKeep)); err != nil {
		h.logger.Warn("placement log prune failed", "err", err)
	}
}

// --- REST ---------------------------------------------------------------------------

// handleListPlacementRules serves GET /api/placement/rules[?all=1].
func (s *Server) handleListPlacementRules(w http.ResponseWriter, r *http.Request) {
	at := s.hub.directiveClock()
	if r.URL.Query().Get("all") != "" {
		at = time.Time{}
	}
	rules, err := s.store.ListPlacementRules(r.Context(), at)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if rules == nil {
		rules = []store.PlacementRule{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// handleCreatePlacementRule serves POST /api/placement/rules.
func (s *Server) handleCreatePlacementRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ScopeKind       string `json:"scope_kind"`
		ScopeValue      string `json:"scope_value"`
		Effect          string `json:"effect"`
		TargetKind      string `json:"target_kind"`
		Target          string `json:"target"`
		Evidence        string `json:"evidence"`
		DurationMinutes int    `json:"duration_minutes"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	cause := causeFrom(r.Context())
	rule, err := s.hub.createPlacementRule(r.Context(), store.PlacementRule{ScopeKind: body.ScopeKind, ScopeValue: body.ScopeValue,
		Effect: body.Effect, TargetKind: body.TargetKind, Target: body.Target, Evidence: body.Evidence}, body.DurationMinutes, cause)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

// handleEndPlacementRule serves POST /api/placement/rules/{id}/end.
func (s *Server) handleEndPlacementRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.EndPlacementRule(r.Context(), id, time.Now().UTC()); err != nil {
		writeStoreError(w, err)
		return
	}
	s.hub.recordAudit(r.Context(), store.AuditEvent{Action: AuditPlacementRuleEnd, Message: "Ended placement rule " + id})
	s.hub.kickScheduler()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePlacementLog serves GET /api/placement/log[?subject=task:<id>&limit=n].
func (s *Server) handlePlacementLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	rows, err := s.store.ListPlacementLog(r.Context(), store.PlacementLogFilter{Subject: q.Get("subject"), Limit: limit})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	type entry struct {
		store.PlacementLogEntry `json:",inline"`
		Detail                  PlacementDetail `json:"detail"`
	}
	out := make([]entry, 0, len(rows))
	for _, e := range rows {
		var d PlacementDetail
		_ = json.Unmarshal([]byte(e.DetailJSON), &d, protocol.JSONOptions)
		e.DetailJSON = ""
		out = append(out, entry{PlacementLogEntry: e, Detail: d})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// Audit actions of placement rules.
const (
	AuditPlacementRuleCreate = "placement_rule.create"
	AuditPlacementRuleEnd    = "placement_rule.end"
)

// createPlacementRule validates and stores a rule, with the caller as its provenance.
func (h *Hub) createPlacementRule(ctx context.Context, r store.PlacementRule, durationMinutes int, cause store.Cause) (store.PlacementRule, error) {
	r.ScopeKind = strings.TrimSpace(r.ScopeKind)
	if r.ScopeKind == "" {
		r.ScopeKind = store.RuleScopeAny
	}
	switch r.ScopeKind {
	case store.RuleScopeAny:
		r.ScopeValue = ""
	case store.RuleScopeProject, store.RuleScopeRepo, store.RuleScopeBranch, store.RuleScopeWorkKey:
		if strings.TrimSpace(r.ScopeValue) == "" {
			return r, fmt.Errorf("scope %s needs a scope_value", r.ScopeKind)
		}
	default:
		return r, fmt.Errorf("scope_kind: any, project, repo, branch or work_key")
	}
	switch r.Effect {
	case store.RuleEffectPrefer, store.RuleEffectRequire, store.RuleEffectAvoid:
	default:
		return r, fmt.Errorf("effect: prefer, require or avoid")
	}
	switch r.TargetKind {
	case store.RuleTargetMachine:
		id := h.resolveMachine(ctx, strings.TrimSpace(r.Target))
		w, err := h.store.GetWorker(ctx, id)
		if err != nil {
			return r, fmt.Errorf("unknown machine %q", r.Target)
		}
		r.Target = w.ID
	case store.RuleTargetClass:
		if strings.TrimSpace(r.Target) == "" {
			return r, fmt.Errorf("a class target such as linux/amd64/32g (a prefix of os/arch/RAM tier)")
		}
	default:
		return r, fmt.Errorf("target_kind: machine or class")
	}
	switch cause.Actor {
	case store.ActorUser:
		r.Provenance = store.RuleProvenanceUser
	case store.ActorCommander:
		r.Provenance = store.RuleProvenanceCommander
	default:
		if r.Provenance == "" {
			r.Provenance = store.RuleProvenanceLearned
		}
	}
	r.CreatedBy = describeActor(cause)
	if durationMinutes > 0 {
		exp := time.Now().UTC().Add(time.Duration(durationMinutes) * time.Minute)
		r.ExpiresAt = &exp
	}
	rule, err := h.store.CreatePlacementRule(ctx, r)
	if err != nil {
		return rule, err
	}
	h.recordAudit(withCause(ctx, cause), store.AuditEvent{Action: AuditPlacementRuleCreate,
		Message: fmt.Sprintf("Placement rule: %s %s %s for %s %s", rule.Effect, rule.TargetKind, rule.Target, rule.ScopeKind, rule.ScopeValue)})
	h.kickScheduler()
	return rule, nil
}

type dryRunKey struct{}

// withDryRun marks ctx as a placement dry run: nothing is created or counted.
func withDryRun(ctx context.Context) context.Context {
	return context.WithValue(ctx, dryRunKey{}, true)
}

func isDryRun(ctx context.Context) bool { v, _ := ctx.Value(dryRunKey{}).(bool); return v }

// PlacementExplanation is a placement dry run's answer (SYR-QY2TG): the machine the
// work would go to ("" when it would queue), and why.
type PlacementExplanation struct {
	WorkerID   string          `json:"worker_id,omitempty"`
	WorkerName string          `json:"worker_name,omitempty"`
	WorkKey    string          `json:"work_key"`
	Detail     PlacementDetail `json:"detail"`
}

// droppedBy lists the machines in before but not in after, as rejections for reason.
func droppedBy(before, after []store.Worker, reason string) []Rejection {
	kept := make(map[string]bool, len(after))
	for _, w := range after {
		kept[w.ID] = true
	}
	var out []Rejection
	for _, w := range before {
		if !kept[w.ID] {
			out = append(out, Rejection{WorkerID: w.ID, WorkerName: w.Name, Reason: reason})
		}
	}
	return out
}

// inspectPlacement answers inspect kind=placement: the latest placements of the task
// (and of its ticket), newest first.
func (s *FleetService) inspectPlacement(ctx context.Context, req InspectRequest, task store.Task, ticketID string) (InspectResult, error) {
	var entries []store.PlacementLogEntry
	for _, subject := range []string{taskSubject(task.ID), "ticket:" + ticketID} {
		if strings.HasSuffix(subject, ":") {
			continue
		}
		rows, err := s.store.ListPlacementLog(ctx, store.PlacementLogFilter{Subject: subject, Limit: 3})
		if err != nil {
			return InspectResult{}, err
		}
		entries = append(entries, rows...)
	}
	type view struct {
		At       time.Time       `json:"at"`
		Subject  string          `json:"subject"`
		WorkKey  string          `json:"work_key,omitempty"`
		Decider  string          `json:"decider"`
		WorkerID string          `json:"worker_id,omitempty"`
		Detail   PlacementDetail `json:"detail"`
	}
	out := make([]view, 0, len(entries))
	for _, e := range entries {
		v := view{At: e.At, Subject: e.Subject, WorkKey: e.WorkKey, Decider: e.Decider, WorkerID: e.WorkerID}
		_ = json.Unmarshal([]byte(e.DetailJSON), &v.Detail, protocol.JSONOptions)
		out = append(out, v)
	}
	raw, err := json.Marshal(out, protocol.JSONOptions)
	if err != nil {
		return InspectResult{}, err
	}
	return InspectResult{Target: req.Target, Kind: req.Kind, TaskID: task.ID, TaskSID: task.SID, TicketID: ticketID,
		WorkerID: task.WorkerID, Text: string(raw)}, nil
}

// handleExplainPlacement serves POST /api/placement/explain: a dispatch dry run that
// says where the work would go and why, dispatching nothing.
func (s *Server) handleExplainPlacement(w http.ResponseWriter, r *http.Request) {
	var req DispatchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.DryRun = true
	res, err := s.service.DispatchTask(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// withAccounts makes the scorer pick, on each machine, the agent account the work would
// use there (SYR-NCEHE). Shell work has none. Automatic placement (avoid) rules out a
// machine whose usable accounts are all exhausted until a reset; work the user started
// runs anyway, last, with the warning it already gets.
func (h *Hub) withAccounts(ctx context.Context, opts PlacementOpts, agent, accountID string, avoid bool) PlacementOpts {
	if agent == "" || agent == "shell" {
		return opts
	}
	now := time.Now()
	opts.Account = func(w store.Worker) (string, float64, bool, string) {
		id, q, known, why := h.placementAccount(ctx, w, agent, accountID, now)
		if why != "" && !avoid {
			return accountID, -1, true, "" // exhausted: last resort
		}
		return id, q, known, why
	}
	return opts
}

// chosenAccount is the account the scorer picked for workerID ("" when none).
func chosenAccount(out PlacementOutcome, workerID string) string {
	for _, s := range out.Ranked {
		if s.ID == workerID {
			return s.Account
		}
	}
	return ""
}
