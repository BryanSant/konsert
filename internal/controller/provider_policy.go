package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// AuditPlacementSubstitute records that placement ran work on a different agent
// than the one requested.
const AuditPlacementSubstitute = "placement.substitute"

// knownProviderAgents are the agent names a provider.disable target can mean
// as an agent-wide disable. Account ids that collide with these names are
// agent-wide unless scope is explicitly account.
var knownProviderAgents = []string{"claude", "codex", "grok", "agy", "cursor", "copilot", "mock"}

func canonicalAgent(name string) (string, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if slices.Contains(knownProviderAgents, name) {
		return name, true
	}
	return "", false
}

func providerAgentLabel(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "grok":
		return "Grok"
	case "agy":
		return "Antigravity"
	case "cursor":
		return "Cursor"
	case "copilot":
		return "Copilot"
	case "mock":
		return "Mock"
	default:
		name = strings.TrimSpace(name)
		if name == "" {
			return "Agent"
		}
		return name
	}
}

// providerIndex is the active provider.disable set plus the accounts workers
// have reported. An exhausted usage window with a future resets_at is included
// as an account disable so placement skips it as soon as the report is stored.
type providerIndex struct {
	now      time.Time
	agents   map[string]store.Directive
	accounts map[string]store.Directive
	reported []protocol.AgentAccount
}

func (h *Hub) loadProviderIndex(ctx context.Context) (providerIndex, error) {
	idx := providerIndex{
		now:      h.directiveClock(),
		agents:   map[string]store.Directive{},
		accounts: map[string]store.Directive{},
	}
	if h == nil || h.store == nil {
		return idx, nil
	}
	list, err := h.store.ListDirectives(ctx, store.DirectiveFilter{
		Kind: store.DirectiveProviderDisable, ActiveOnly: true, Now: idx.now, Limit: 1000,
	})
	if err != nil {
		return idx, err
	}
	for _, d := range list {
		if !d.ActiveAt(idx.now) {
			continue
		}
		if d.Params.Scope == store.DirectiveScopeAccount {
			idx.accounts[d.Target] = preferDirective(idx.accounts[d.Target], d)
			continue
		}
		key := strings.ToLower(d.Target)
		idx.agents[key] = preferDirective(idx.agents[key], d)
	}
	recs, err := h.store.ListAgentAccounts(ctx, "")
	if err != nil {
		return idx, err
	}
	for _, r := range recs {
		a, ok := decodeAccount(r)
		if !ok || a.ID == "" {
			continue
		}
		idx.reported = append(idx.reported, a)
		if _, exists := idx.accounts[a.ID]; exists {
			continue
		}
		u, exhausted := exhaustedWindow(a, idx.now)
		if !exhausted || u.ResetsAt == nil || !u.ResetsAt.After(idx.now) {
			continue
		}
		until := u.ResetsAt.UTC()
		idx.accounts[a.ID] = store.Directive{
			Kind:       store.DirectiveProviderDisable,
			Target:     a.ID,
			Until:      &until,
			Provenance: store.ProvenanceSystem,
			Reason:     "usage window exhausted",
			Params:     store.DirectiveParams{Scope: store.DirectiveScopeAccount},
		}
	}
	return idx, nil
}

// preferDirective keeps the tightest disable. An open-ended until hides the
// clock. A fallback on either row is kept. A zero cur means next is the first row.
func preferDirective(cur, next store.Directive) store.Directive {
	if cur.Kind == "" {
		return next
	}
	out := cur
	if out.Params.Fallback == "" {
		out.Params.Fallback = next.Params.Fallback
	}
	switch {
	case out.Until == nil || next.Until == nil:
		out.Until = nil
	case next.Until.Before(*out.Until):
		out.Until = next.Until
	}
	return out
}

func (idx providerIndex) accountDisabled(id string) bool {
	if id == "" {
		return false
	}
	_, ok := idx.accounts[id]
	return ok
}

// agentBlocked reports an agent-wide directive, or that every reported account
// is disabled by the operator (user or commander). A system usage-window
// directive is account-scoped and does not by itself retire the agent.
func (idx providerIndex) agentBlocked(agent string) bool {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" || agent == "shell" {
		return false
	}
	if _, ok := idx.agents[agent]; ok {
		return true
	}
	n, blocked := 0, 0
	for _, a := range idx.reported {
		if !strings.EqualFold(a.Agent, agent) {
			continue
		}
		n++
		d, ok := idx.accounts[a.ID]
		if ok && d.Provenance != store.ProvenanceSystem {
			blocked++
		}
	}
	return n > 0 && blocked == n
}

// accountsHeld reports that every reported account of the agent is disabled.
// An agent-wide directive is left to agentBlocked.
func (idx providerIndex) accountsHeld(agent string) (store.Directive, bool) {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" || agent == "shell" {
		return store.Directive{}, false
	}
	if _, ok := idx.agents[agent]; ok {
		return store.Directive{}, false
	}
	var d store.Directive
	n := 0
	for _, a := range idx.reported {
		if !strings.EqualFold(a.Agent, agent) {
			continue
		}
		n++
		ad, ok := idx.accounts[a.ID]
		if !ok {
			return store.Directive{}, false
		}
		if n == 1 {
			d = ad
			continue
		}
		d = preferDirective(d, ad)
	}
	return d, n > 0
}

func (idx providerIndex) blockInfo(agent string) (store.Directive, bool) {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if d, ok := idx.agents[agent]; ok {
		return d, true
	}
	if !idx.agentBlocked(agent) {
		return store.Directive{}, false
	}
	var d store.Directive
	found := false
	for _, a := range idx.reported {
		if !strings.EqualFold(a.Agent, agent) {
			continue
		}
		ad, ok := idx.accounts[a.ID]
		if !ok {
			continue
		}
		if !found {
			d = ad
			found = true
			continue
		}
		d = preferDirective(d, ad)
	}
	return d, found
}

func (idx providerIndex) otherAccount(agent, skipID string) (protocol.AgentAccount, bool) {
	var loggedIn, anyUsable *protocol.AgentAccount
	for i := range idx.reported {
		a := idx.reported[i]
		if !strings.EqualFold(a.Agent, agent) || a.ID == skipID {
			continue
		}
		if idx.accountDisabled(a.ID) || !accountChatUsable(a) {
			continue
		}
		if a.IsDefault && accountLoggedIn(a) {
			return a, true
		}
		if accountLoggedIn(a) && loggedIn == nil {
			loggedIn = &idx.reported[i]
		}
		if anyUsable == nil {
			anyUsable = &idx.reported[i]
		}
	}
	if loggedIn != nil {
		return *loggedIn, true
	}
	if anyUsable != nil {
		return *anyUsable, true
	}
	return protocol.AgentAccount{}, false
}

func (idx providerIndex) enabledAccounts(accts []protocol.AgentAccount) []protocol.AgentAccount {
	out := make([]protocol.AgentAccount, 0, len(accts))
	for _, a := range accts {
		if idx.accountDisabled(a.ID) || idx.agentBlocked(a.Agent) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// providerDecision is where a preferred agent and account should run.
// Wait means a pinned ticket stays ready. An empty Agent with Wait false means
// any eligible agent.
type providerDecision struct {
	Agent        string
	AccountID    string
	Substitution string
	Wait         bool
	BlockedAgent string
	Until        *time.Time
}

func decideProvider(idx providerIndex, agent, accountID string, pinned bool, workers []store.Worker) providerDecision {
	agent = strings.TrimSpace(agent)
	accountID = strings.TrimSpace(accountID)
	if canon, ok := canonicalAgent(agent); ok {
		agent = canon
	}
	if agent == "shell" {
		return providerDecision{Agent: agent, AccountID: accountID}
	}

	if accountID != "" && idx.accountDisabled(accountID) {
		d := idx.accounts[accountID]
		if agent == "" {
			agent = agentOfAccount(idx, accountID)
		}
		// An operator disable of the whole agent wins over a usage window on one account.
		if !idx.agentBlocked(agent) {
			if fb := usableFallback(idx, d.Params.Fallback, workers); fb != "" {
				return providerDecision{
					Agent: fb, Substitution: substitutionNote(accountID, fb, d.Until), Until: d.Until,
				}
			}
			if alt, ok := idx.otherAccount(agent, accountID); ok {
				return providerDecision{
					Agent: alt.Agent, AccountID: alt.ID,
					Substitution: substitutionNote(accountID, alt.ID, d.Until), Until: d.Until,
				}
			}
			// A usage window with no sibling account stays on the requested agent.
			// The exhausted account id is not selected. An explicit chat keeps the
			// account the user aimed at, because this decision has no substitution.
			if !pinned && d.Provenance == store.ProvenanceSystem {
				return providerDecision{Agent: agent, Until: d.Until}
			}
			accountID = ""
			blocked := agent
			if blocked == "" {
				blocked = d.Target
			}
			if pinned {
				return providerDecision{Wait: true, Agent: agent, BlockedAgent: blocked, Until: d.Until}
			}
			return providerDecision{BlockedAgent: blocked, Until: d.Until}
		}
	}

	if agent != "" && idx.agentBlocked(agent) {
		d, _ := idx.blockInfo(agent)
		if fb := usableFallback(idx, d.Params.Fallback, workers); fb != "" {
			return providerDecision{
				Agent: fb, Substitution: substitutionNote(agent, fb, d.Until), Until: d.Until,
			}
		}
		if pinned {
			return providerDecision{Wait: true, Agent: agent, BlockedAgent: agent, Until: d.Until}
		}
		return providerDecision{BlockedAgent: agent, Until: d.Until}
	}
	// Every account is in a usage window. A pinned ticket waits. An unpinned
	// task keeps the agent it asked for, so placement does not hop to another
	// installed agent solely because the subscription is exhausted.
	if pinned && agent != "" {
		if d, held := idx.accountsHeld(agent); held {
			return providerDecision{Wait: true, Agent: agent, BlockedAgent: agent, Until: d.Until}
		}
	}
	return providerDecision{Agent: agent, AccountID: accountID}
}

func agentOfAccount(idx providerIndex, accountID string) string {
	for _, a := range idx.reported {
		if a.ID == accountID {
			if canon, ok := canonicalAgent(a.Agent); ok {
				return canon
			}
			return strings.ToLower(strings.TrimSpace(a.Agent))
		}
	}
	return ""
}

func usableFallback(idx providerIndex, fb string, workers []store.Worker) string {
	fb = strings.TrimSpace(fb)
	if fb == "" {
		return ""
	}
	canon, ok := canonicalAgent(fb)
	if !ok {
		canon = strings.ToLower(fb)
	}
	if canon == "" || idx.agentBlocked(canon) {
		return ""
	}
	for _, w := range workers {
		if slices.Contains(w.Agents, canon) {
			return canon
		}
	}
	return ""
}

func substitutionNote(from, to string, until *time.Time) string {
	msg := fmt.Sprintf("substituted %s for %s", to, from)
	if until != nil {
		return msg + " (" + from + " disabled until " + until.UTC().Format(time.RFC3339) + ")"
	}
	return msg + " (" + from + " disabled)"
}

func (h *Hub) decideProvider(ctx context.Context, agent, accountID string, pinned bool, workers []store.Worker) providerDecision {
	idx, err := h.loadProviderIndex(ctx)
	if err != nil {
		h.logger.Warn("list provider directives failed", "err", err)
		return providerDecision{Agent: strings.TrimSpace(agent), AccountID: strings.TrimSpace(accountID)}
	}
	return decideProvider(idx, agent, accountID, pinned, workers)
}

// filterProviderWorkers keeps machines that can run agent. An empty agent keeps
// machines that still have some agent placement has not disabled.
func filterProviderWorkers(workers []store.Worker, agent string, idx providerIndex) []store.Worker {
	if agent == "shell" {
		return workers
	}
	out := make([]store.Worker, 0, len(workers))
	for _, w := range workers {
		if agent != "" {
			if slices.Contains(w.Agents, agent) && !idx.agentBlocked(agent) {
				out = append(out, w)
			}
			continue
		}
		if pickAllowedAgent(w, idx) != "" || len(w.Agents) == 0 {
			out = append(out, w)
		}
	}
	return out
}

// pickAllowedAgent chooses an installed agent placement has not disabled.
// Agents with non-exhausted accounts are preferred over usage-held agents.
func pickAllowedAgent(w store.Worker, idx providerIndex) string {
	for _, allowHeld := range []bool{false, true} {
		allowed := func(a string) bool {
			if idx.agentBlocked(a) {
				return false
			}
			if !allowHeld {
				if _, held := idx.accountsHeld(a); held {
					return false
				}
			}
			return true
		}
		for _, pref := range preferredAgentOrder {
			if slices.Contains(w.Agents, pref) && allowed(pref) {
				return pref
			}
		}
		for _, a := range w.Agents {
			if allowed(a) {
				return a
			}
		}
	}
	return ""
}

func (h *Hub) noteProviderSubstitution(ctx context.Context, taskID, ticketID, note string) {
	note = strings.TrimSpace(note)
	if note == "" || h == nil {
		return
	}
	h.recordAudit(ctx, store.AuditEvent{
		Action:   AuditPlacementSubstitute,
		TaskID:   taskID,
		Message:  note,
		MetaJSON: auditMeta(map[string]any{"task_id": taskID, "ticket_id": ticketID, "substitution": note}),
	})
}

func (h *Hub) recordAttemptSubstitution(ctx context.Context, taskID, ticketID, workerID, agent, accountID, note string, attemptN int) {
	if note == "" || taskID == "" {
		return
	}
	h.noteProviderSubstitution(ctx, taskID, ticketID, note)
	if attemptN < 1 {
		attemptN = h.adHocAttemptNumber(ctx, taskID, 0)
	}
	h.beginAttempt(ctx, store.Attempt{
		TicketID:     ticketID,
		TaskID:       taskID,
		Attempt:      attemptN,
		WorkerID:     workerID,
		AccountID:    accountID,
		Agent:        agent,
		Substitution: note,
	})
}

// providerWaitPhrases names pinned ready tickets whose agent is disabled and
// has no usable fallback: "waiting: codex disabled until 14:00".
func (h *Hub) providerWaitPhrases(ctx context.Context, loc *time.Location) []string {
	if h == nil || h.store == nil {
		return nil
	}
	tickets, err := h.store.ListTickets(ctx, store.TicketFilter{Status: store.TicketStatusReady, Limit: 200})
	if err != nil || len(tickets) == 0 {
		return nil
	}
	idx, err := h.loadProviderIndex(ctx)
	if err != nil {
		return nil
	}
	workers := h.schedulingWorkers(ctx)
	seen := map[string]struct{}{}
	var out []string
	for _, tk := range tickets {
		agent := strings.TrimSpace(tk.Hints.Agent)
		if agent == "" || tk.Status != store.TicketStatusReady {
			continue
		}
		dec := decideProvider(idx, agent, "", true, workers)
		if !dec.Wait {
			continue
		}
		name := dec.BlockedAgent
		if name == "" {
			name = agent
		}
		phrase := fmt.Sprintf("waiting: %s disabled%s", name, untilClock(dec.Until, loc))
		if _, ok := seen[phrase]; ok {
			continue
		}
		seen[phrase] = struct{}{}
		out = append(out, phrase)
	}
	return out
}

func (h *Hub) schedulingWorkers(ctx context.Context) []store.Worker {
	all, err := h.store.ListWorkers(ctx)
	if err != nil {
		return nil
	}
	out := make([]store.Worker, 0, len(all))
	for _, w := range all {
		if h.IsWorkerConnected(w.ID) && !w.Drained {
			out = append(out, w)
		}
	}
	return out
}

// applyProviderPlacement rewrites a preferred agent and account for one
// placement. pinned is true only when a ticket hint names the agent.
func (h *Hub) applyProviderPlacement(ctx context.Context, agent, accountID string, pinned bool, workers []store.Worker) (providerDecision, providerIndex) {
	idx, err := h.loadProviderIndex(ctx)
	if err != nil {
		h.logger.Warn("list provider directives failed", "err", err)
		return providerDecision{Agent: strings.TrimSpace(agent), AccountID: strings.TrimSpace(accountID)}, idx
	}
	return decideProvider(idx, agent, accountID, pinned, workers), idx
}

// retargetChat moves a chat off an operator-disabled agent or account. A usage
// window alone leaves an explicit account in place: the prompt is still sent,
// with the exhausted-window warning. A non-empty blocked message means an
// operator disable left nothing eligible.
func (h *Hub) retargetChat(ctx context.Context, cs *store.ChatSession) (note, blocked string) {
	if cs == nil {
		return "", ""
	}
	agent := strings.TrimSpace(cs.Agent)
	accountID := strings.TrimSpace(cs.AccountID)
	if (agent == "" && accountID == "") || agent == "shell" {
		return "", ""
	}
	workers := h.schedulingWorkers(ctx)
	dec := h.decideProvider(ctx, agent, accountID, false, workers)
	if dec.Wait {
		return "", h.disabledChatMessage(dec)
	}
	if dec.Substitution != "" {
		cs.Agent = dec.Agent
		cs.AccountID = dec.AccountID
		if cs.WorkerID != "" && !h.workerCanRun(ctx, cs.WorkerID, cs.Agent, cs.AccountID) {
			cs.WorkerID = ""
		}
		return dec.Substitution, ""
	}
	// Placement cleared the agent. An account that is still the one requested
	// stays put, so a logged-out account reports its own sign-in error.
	if dec.BlockedAgent != "" && dec.Agent == "" {
		if wid, a, ok := h.defaultAgentTarget(ctx); ok {
			from := agent
			if from == "" {
				from = accountID
			}
			cs.WorkerID = wid
			applyAccountDefaults(cs, a)
			return substitutionNote(from, a.Agent, dec.Until), ""
		}
		return "", h.disabledChatMessage(dec)
	}
	return "", ""
}

func (h *Hub) disabledChatMessage(dec providerDecision) string {
	name := providerAgentLabel(dec.BlockedAgent)
	if name == "Agent" && dec.BlockedAgent != "" {
		name = dec.BlockedAgent
	}
	return name + " is disabled" + untilClock(dec.Until, h.directiveClock().Location())
}

func (h *Hub) workerCanRun(ctx context.Context, workerID, agent, accountID string) bool {
	if workerID == "" {
		return false
	}
	if accountID != "" {
		if _, ok := h.lookupAccount(ctx, workerID, agent, accountID); ok {
			return true
		}
	}
	w, err := h.store.GetWorker(ctx, workerID)
	if err != nil {
		return false
	}
	return agent == "" || slices.Contains(w.Agents, agent)
}

// syncUsageDirectives creates, deduplicates and ends system provider.disable
// rows from a worker's account report. An exhausted window with resets_at
// disables that account until the reset. A later report that is not exhausted
// ends the system directive early. User-created directives are left alone.
func (h *Hub) syncUsageDirectives(ctx context.Context, accounts []protocol.AgentAccount) {
	if h == nil || h.store == nil {
		return
	}
	ctx = withCause(context.WithoutCancel(ctx), causeDirective)
	now := h.directiveClock()
	list, err := h.store.ListDirectives(ctx, store.DirectiveFilter{
		Kind: store.DirectiveProviderDisable, ActiveOnly: true, Now: now, Limit: 1000,
	})
	if err != nil {
		h.logger.Warn("list usage directives failed", "err", err)
		return
	}
	byAccount := map[string][]store.Directive{}
	for _, d := range list {
		if d.Provenance != store.ProvenanceSystem || d.Params.Scope != store.DirectiveScopeAccount || !d.ActiveAt(now) {
			continue
		}
		byAccount[d.Target] = append(byAccount[d.Target], d)
	}
	for _, a := range accounts {
		if a.ID == "" {
			continue
		}
		u, exhausted := exhaustedWindow(a, now)
		want := exhausted && u.ResetsAt != nil && u.ResetsAt.After(now)
		var keep *store.Directive
		for i := range byAccount[a.ID] {
			d := byAccount[a.ID][i]
			if want && keep == nil && d.Until != nil && d.Until.Unix() == u.ResetsAt.Unix() {
				keep = &byAccount[a.ID][i]
				continue
			}
			if _, err := h.EndDirective(ctx, d.ID); err != nil {
				h.logger.Warn("end usage directive failed", "account_id", a.ID, "err", err)
			}
		}
		if !want || keep != nil {
			continue
		}
		until := u.ResetsAt.UTC()
		reason := "usage window exhausted"
		if u.Window != "" {
			reason = "usage window " + u.Window + " exhausted"
		}
		if _, err := h.CreateDirective(ctx, DirectiveInput{
			Kind:   store.DirectiveProviderDisable,
			Target: a.ID,
			Until:  &until,
			Reason: reason,
			Params: store.DirectiveParams{Scope: store.DirectiveScopeAccount},
		}); err != nil {
			h.logger.Warn("create usage directive failed", "account_id", a.ID, "err", err)
		}
	}
}

// resolveProviderTarget classifies a provider.disable target. An explicit scope
// wins. A known agent name is agent-wide. An account id that is not an agent
// name is account-scoped.
func (h *Hub) resolveProviderTarget(ctx context.Context, target, scope string) (string, string, error) {
	target = strings.TrimSpace(target)
	scope = strings.ToLower(strings.TrimSpace(scope))
	if target == "" {
		return "", "", fmt.Errorf("target is required")
	}
	switch scope {
	case "", store.DirectiveScopeAgent, store.DirectiveScopeAccount:
	default:
		return "", "", fmt.Errorf("scope must be agent or account")
	}
	if scope == store.DirectiveScopeAccount {
		return target, store.DirectiveScopeAccount, nil
	}
	if canon, ok := canonicalAgent(target); ok {
		return canon, store.DirectiveScopeAgent, nil
	}
	if scope == store.DirectiveScopeAgent {
		return strings.ToLower(target), store.DirectiveScopeAgent, nil
	}
	if h.accountIDKnown(ctx, target) {
		return target, store.DirectiveScopeAccount, nil
	}
	return strings.ToLower(target), store.DirectiveScopeAgent, nil
}

func (h *Hub) accountIDKnown(ctx context.Context, id string) bool {
	if h == nil || h.store == nil || id == "" {
		return false
	}
	recs, err := h.store.ListAgentAccounts(ctx, "")
	if err != nil {
		return false
	}
	for _, r := range recs {
		if r.AccountID == id {
			return true
		}
		if a, ok := decodeAccount(r); ok && a.ID == id {
			return true
		}
	}
	return false
}
