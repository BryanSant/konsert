package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	commanderPolicyWritesSetting = "commander_policy_writes"
	policyWritesTighten          = "tighten"
	policyWritesConfirm          = "confirm"
	policyWritesNone             = "none"
)

// PolicyBackend is the set_policy surface on the Commander and external /mcp.
type PolicyBackend interface {
	SetPolicy(ctx context.Context, in SetPolicyInput) (SetPolicyResult, error)
}

// PolicyConstraints are the optional kind-specific fields of set_policy.
type PolicyConstraints struct {
	Fallback       string `json:"fallback,omitempty" jsonschema:"Agent to prefer when provider.disable blocks the target"`
	Scope          string `json:"scope,omitempty" jsonschema:"provider.disable scope: agent or account"`
	MaxConcurrency *int   `json:"max_concurrency,omitempty" jsonschema:"machine.throttle concurrency cap (0 accepts nothing new)"`
	CPUCeilingPct  *int   `json:"cpu_ceiling_pct,omitempty" jsonschema:"machine.throttle CPU ceiling percent"`
	HeavyOK        *bool  `json:"heavy_ok,omitempty" jsonschema:"machine.throttle: whether heavy work may still start"`
	SuspendRunning bool   `json:"suspend_running,omitempty" jsonschema:"machine.pause: also suspend work that is already running"`
}

// SetPolicyInput is set_policy(target, directive, until?, constraints?, reason).
// directive "end" with id (or target) asks to end a directive.
type SetPolicyInput struct {
	Target      string             `json:"target" jsonschema:"Machine, project, agent name (codex) or account id the directive applies to"`
	Directive   string             `json:"directive" jsonschema:"machine.pause, machine.throttle, machine.drain, provider.disable, autopilot.pause, or end"`
	Until       string             `json:"until,omitempty" jsonschema:"RFC3339 time the directive lasts until. Omit to leave it open-ended"`
	ID          string             `json:"id,omitempty" jsonschema:"Directive id or sid (dr-…) when directive is end"`
	Reason      string             `json:"reason,omitempty" jsonschema:"Why this policy change"`
	Constraints *PolicyConstraints `json:"constraints,omitempty" jsonschema:"Kind-specific fields: fallback, scope, max_concurrency, cpu_ceiling_pct, heavy_ok, suspend_running"`
}

// SetPolicyResult is a successful tool result. Loosens is true when the change
// was not applied and the Commander must call request_override. Pending is set
// when commander_policy_writes is confirm.
type SetPolicyResult struct {
	Applied *store.Directive      `json:"applied,omitempty"`
	Pending *store.PolicyProposal `json:"pending,omitempty"`
	Message string                `json:"message,omitempty"`
	Loosens bool                  `json:"loosens,omitempty"`
}

func (s *FleetService) SetPolicy(ctx context.Context, in SetPolicyInput) (SetPolicyResult, error) {
	return s.hub.SetPolicy(ctx, in)
}

func registerSetPolicyTool(srv *mcp.Server, backend PolicyBackend) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "set_policy",
		Description: setPolicyDesc,
		InputSchema: setPolicySchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SetPolicyInput) (*mcp.CallToolResult, any, error) {
		res, err := backend.SetPolicy(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return jsonToolResult(res)
	})
}

func (h *Hub) commanderPolicyWrites(ctx context.Context) string {
	if h == nil || h.store == nil {
		return policyWritesTighten
	}
	saved, err := h.store.GetAllSettings(ctx)
	if err != nil {
		return policyWritesTighten
	}
	switch strings.ToLower(strings.TrimSpace(saved[commanderPolicyWritesSetting])) {
	case policyWritesConfirm:
		return policyWritesConfirm
	case policyWritesNone:
		return policyWritesNone
	default:
		return policyWritesTighten
	}
}

func normalizePolicyWrites(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case policyWritesTighten, policyWritesConfirm, policyWritesNone:
		return strings.ToLower(strings.TrimSpace(v)), nil
	default:
		return "", fmt.Errorf("commander_policy_writes must be tighten, confirm, or none")
	}
}

// commanderActsFor is the user a Commander action is attributed to: the
// authenticated caller when that is a real user, otherwise the most recently
// used device's owner, otherwise "local".
func (h *Hub) commanderActsFor(ctx context.Context) string {
	if a, ok := ctx.Value(auditActorKey{}).(auditActor); ok {
		u := strings.TrimSpace(a.User)
		if u != "" && u != "commander" && u != "system" && !strings.HasPrefix(u, "agent-chat:") {
			return u
		}
	}
	if h != nil && h.store != nil {
		if devs, err := h.store.ListDevices(ctx); err == nil {
			var best store.Device
			for _, d := range devs {
				if d.RevokedAt != nil || strings.TrimSpace(d.CreatedBy) == "" {
					continue
				}
				if best.ID == "" || d.LastUsedAt.After(best.LastUsedAt) {
					best = d
				}
			}
			if best.CreatedBy != "" {
				return best.CreatedBy
			}
		}
	}
	return "local"
}

func (h *Hub) commanderPolicyCause(ctx context.Context) store.Cause {
	return store.Cause{
		Actor:      store.ActorUser,
		Source:     store.SourceCommander,
		ActorID:    h.commanderActsFor(ctx),
		Confidence: store.ConfidenceCertain,
	}
}

// SetPolicy applies a tightening directive, holds it for approval, or tells the
// Commander to call request_override when the change would loosen policy.
func (h *Hub) SetPolicy(ctx context.Context, in SetPolicyInput) (SetPolicyResult, error) {
	if h == nil || h.store == nil {
		return SetPolicyResult{}, errors.New("policy is unavailable")
	}
	if h.commanderPolicyWrites(ctx) == policyWritesNone {
		return SetPolicyResult{}, errors.New("commander_policy_writes is none: set_policy is read-only")
	}
	kind := strings.ToLower(strings.TrimSpace(in.Directive))
	if kind == "end" {
		return h.setPolicyEnd(ctx, in)
	}
	var until *time.Time
	if strings.TrimSpace(in.Until) != "" {
		t, err := parseDirectiveUntil(in.Until)
		if err != nil {
			return SetPolicyResult{}, errors.New("until must be RFC3339")
		}
		until = &t
	}
	prepared, err := h.prepareDirective(ctx, DirectiveInput{
		Kind: kind, Target: in.Target, Until: until, Reason: in.Reason, Params: constraintsParams(in.Constraints),
	})
	if err != nil {
		return SetPolicyResult{}, err
	}
	if h.policyLoosens(ctx, prepared) {
		return SetPolicyResult{
			Loosens: true,
			Message: "Raising a cap loosens operator policy. Call request_override and ask the user to approve it.",
		}, nil
	}
	cause := h.commanderPolicyCause(ctx)
	if h.commanderPolicyWrites(ctx) == policyWritesConfirm {
		p, err := h.proposePolicy(ctx, prepared, cause)
		if err != nil {
			return SetPolicyResult{}, err
		}
		return SetPolicyResult{
			Pending: &p,
			Message: "Waiting for approval. The change is a card in Needs you.",
		}, nil
	}
	stored, err := h.CreateDirective(withCause(ctx, cause), DirectiveInput{
		Kind: prepared.Kind, Target: prepared.Target, Until: prepared.Until, Reason: prepared.Reason, Params: prepared.Params,
	})
	if err != nil {
		return SetPolicyResult{}, err
	}
	return SetPolicyResult{
		Applied: &stored,
		Message: describeActor(cause) + " set " + prepared.Kind + " on " + prepared.Target,
	}, nil
}

func (h *Hub) setPolicyEnd(ctx context.Context, in SetPolicyInput) (SetPolicyResult, error) {
	ref := strings.TrimSpace(in.ID)
	if ref == "" {
		ref = strings.TrimSpace(in.Target)
	}
	if ref == "" {
		return SetPolicyResult{}, errors.New("id is required to end a directive")
	}
	id := resolveRef(ctx, h.store, ids.Directive, ref)
	d, err := h.store.GetDirective(ctx, id)
	if err != nil {
		return SetPolicyResult{}, err
	}
	if d.EndedAt != nil || !d.ActiveAt(h.directiveClock()) {
		return SetPolicyResult{}, errors.New("directive is not active")
	}
	return SetPolicyResult{
		Loosens: true,
		Message: "Ending a directive loosens operator policy. Call request_override and ask the user to approve it.",
	}, nil
}

// policyLoosens reports a throttle whose cap is higher than the machine's
// effective cap. Creating pause, drain, provider.disable or autopilot.pause
// only restricts.
func (h *Hub) policyLoosens(ctx context.Context, d store.Directive) bool {
	if d.Kind != store.DirectiveMachineThrottle || d.Params.MaxConcurrency == nil {
		return false
	}
	w, err := h.store.GetWorker(ctx, d.Target)
	if err != nil {
		return false
	}
	effective := w.MaxConcurrent
	if effective <= 0 {
		effective = 1
	}
	now := h.directiveClock()
	list, err := h.store.ListDirectives(ctx, store.DirectiveFilter{
		Target: d.Target, Kind: store.DirectiveMachineThrottle, ActiveOnly: true, Now: now, Limit: 50,
	})
	if err == nil {
		for _, cur := range list {
			if !cur.ActiveAt(now) || cur.Params.MaxConcurrency == nil {
				continue
			}
			if *cur.Params.MaxConcurrency < effective {
				effective = *cur.Params.MaxConcurrency
			}
		}
	}
	return *d.Params.MaxConcurrency > effective
}

func constraintsParams(c *PolicyConstraints) store.DirectiveParams {
	if c == nil {
		return store.DirectiveParams{}
	}
	return store.DirectiveParams{
		MaxConcurrency: c.MaxConcurrency,
		CPUCeilingPct:  c.CPUCeilingPct,
		HeavyOK:        c.HeavyOK,
		SuspendRunning: c.SuspendRunning,
		Fallback:       c.Fallback,
		Scope:          c.Scope,
	}
}

func (h *Hub) proposePolicy(ctx context.Context, d store.Directive, cause store.Cause) (store.PolicyProposal, error) {
	chatID := ""
	if o, ok := chatOriginFrom(ctx); ok {
		chatID = o.id
	}
	p := store.PolicyProposal{
		ChatID:    chatID,
		Kind:      d.Kind,
		Target:    d.Target,
		Params:    d.Params,
		Until:     d.Until,
		Reason:    d.Reason,
		Summary:   policyProposalSummary(d, h.directiveClock().Location()),
		Status:    store.PolicyProposalPending,
		ActorID:   cause.ActorID,
		CreatedAt: h.directiveClock(),
	}
	stored, err := h.store.CreatePolicyProposal(ctx, p)
	if err != nil {
		return store.PolicyProposal{}, err
	}
	h.broadcastFleetUpdate("policy_proposal", stored)
	return stored, nil
}

func policyProposalSummary(d store.Directive, loc *time.Location) string {
	clock := ""
	if d.Until != nil {
		if loc == nil {
			loc = time.Local
		}
		clock = " until " + d.Until.In(loc).Format("15:04")
	}
	switch d.Kind {
	case store.DirectiveProviderDisable:
		what := providerAgentLabel(d.Target) + " disabled" + clock
		if d.Params.Scope == store.DirectiveScopeAccount {
			what = "Account " + d.Target + " disabled" + clock
		}
		if d.Params.Fallback != "" {
			what += " → fallback " + providerAgentLabel(d.Params.Fallback)
		}
		return what
	case store.DirectiveMachinePause:
		return "Pause " + d.Target + clock
	case store.DirectiveMachineDrain:
		return "Drain " + d.Target + clock
	case store.DirectiveMachineThrottle:
		n := "?"
		if d.Params.MaxConcurrency != nil {
			n = fmt.Sprintf("%d", *d.Params.MaxConcurrency)
		}
		return "Throttle " + d.Target + " to " + n + clock
	case store.DirectiveAutopilotPause:
		return "Pause autopilot on " + d.Target + clock
	default:
		return d.Kind + " on " + d.Target + clock
	}
}

// ApprovePolicyProposal applies a pending confirm-mode change with the cause
// captured when the Commander asked.
func (h *Hub) ApprovePolicyProposal(ctx context.Context, id string) (store.Directive, error) {
	p, err := h.policyProposal(ctx, id)
	if err != nil {
		return store.Directive{}, err
	}
	if p.Status != store.PolicyProposalPending {
		return store.Directive{}, errors.New("proposal is not pending")
	}
	cause := store.Cause{
		Actor: store.ActorUser, Source: store.SourceCommander, ActorID: p.ActorID, Confidence: store.ConfidenceCertain,
	}
	d, err := h.CreateDirective(withCause(ctx, cause), DirectiveInput{
		Kind: p.Kind, Target: p.Target, Until: p.Until, Reason: p.Reason, Params: p.Params,
	})
	if err != nil {
		return store.Directive{}, err
	}
	by := directiveActor(causeFrom(ctx))
	ok, err := h.store.DecidePolicyProposal(ctx, p.ID, store.PolicyProposalApproved, by, d.ID, h.directiveClock())
	if err != nil {
		return d, err
	}
	if ok {
		if cur, gErr := h.store.GetPolicyProposal(ctx, p.ID); gErr == nil {
			h.broadcastFleetUpdate("policy_proposal", cur)
		}
	}
	return d, nil
}

// DenyPolicyProposal drops a pending confirm-mode change.
func (h *Hub) DenyPolicyProposal(ctx context.Context, id string) (store.PolicyProposal, error) {
	p, err := h.policyProposal(ctx, id)
	if err != nil {
		return store.PolicyProposal{}, err
	}
	if p.Status != store.PolicyProposalPending {
		return store.PolicyProposal{}, errors.New("proposal is not pending")
	}
	by := directiveActor(causeFrom(ctx))
	ok, err := h.store.DecidePolicyProposal(ctx, p.ID, store.PolicyProposalDenied, by, "", h.directiveClock())
	if err != nil {
		return store.PolicyProposal{}, err
	}
	if !ok {
		return store.PolicyProposal{}, errors.New("proposal is not pending")
	}
	cur, err := h.store.GetPolicyProposal(ctx, p.ID)
	if err != nil {
		return store.PolicyProposal{}, err
	}
	h.broadcastFleetUpdate("policy_proposal", cur)
	return cur, nil
}

func (h *Hub) policyProposal(ctx context.Context, ref string) (store.PolicyProposal, error) {
	id := resolveRef(ctx, h.store, ids.Policy, strings.TrimSpace(ref))
	return h.store.GetPolicyProposal(ctx, id)
}

func (s *Server) handleSetPolicy(w http.ResponseWriter, r *http.Request) {
	var body SetPolicyInput
	if !decodeJSON(w, r, &body) {
		return
	}
	res, err := s.hub.SetPolicy(r.Context(), body)
	if err != nil {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, store.ErrNotFound):
			code = http.StatusNotFound
		case strings.Contains(err.Error(), "read-only"):
			code = http.StatusForbidden
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleListPolicyProposals(w http.ResponseWriter, r *http.Request) {
	f := store.PolicyProposalFilter{Status: strings.TrimSpace(r.URL.Query().Get("status"))}
	list, err := s.store.ListPolicyProposals(r.Context(), f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []store.PolicyProposal{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": list})
}

func (s *Server) handleApprovePolicyProposal(w http.ResponseWriter, r *http.Request) {
	d, err := s.hub.ApprovePolicyProposal(r.Context(), r.PathValue("id"))
	if err != nil {
		writePolicyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleDenyPolicyProposal(w http.ResponseWriter, r *http.Request) {
	p, err := s.hub.DenyPolicyProposal(r.Context(), r.PathValue("id"))
	if err != nil {
		writePolicyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func writePolicyErr(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, store.ErrNotFound) {
		code = http.StatusNotFound
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
