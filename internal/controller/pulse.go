package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	pulseDebounceSetting        = "pulse_debounce_seconds"
	defaultPulseDebounceSeconds = 20
	minPulseDebounceSeconds     = 1
	maxPulseDebounceSeconds     = 3600

	// PulseResourceURI is the MCP resource URI returning the latest board pulse.
	PulseResourceURI = "syre://pulse"

	needsDecompositionLabel = "needs_decomposition"
)

type pulseState struct {
	mu               sync.Mutex
	pending          bool
	latest           string
	mcpCursor        int64
	mcpServers       []*mcp.Server
	decomposedPulsed map[string]bool
	idlePulsed       map[string]bool
}

// pulseTriggerMeta is stored in FleetEvent.MetaJSON for EventKindPulse rows.
type pulseTriggerMeta struct {
	Kind          string   `json:"kind"`
	ProjectID     string   `json:"project_id,omitempty"`
	ProjectName   string   `json:"project_name,omitempty"`
	TicketID      string   `json:"ticket_id,omitempty"`
	TicketSID     string   `json:"ticket_sid,omitempty"`
	UnblockedSIDs []string `json:"unblocked_sids,omitempty"`
	Followups     int      `json:"followups,omitzero"`
	Category      string   `json:"category,omitempty"`
	Summary       string   `json:"summary,omitempty"`
	ProjectLevel  bool     `json:"project_level,omitzero"`
	Autopilot     bool     `json:"autopilot,omitzero"`
}

func (h *Hub) pulseClock() time.Time {
	if h.pulseNow != nil {
		return h.pulseNow()
	}
	return time.Now()
}

// pulseDebounce returns the configured board pulse debounce window
// (Hub override -> controller setting pulse_debounce_seconds -> 20s default).
func (h *Hub) pulseDebounce(ctx context.Context) time.Duration {
	if h.PulseDebounce > 0 {
		return h.PulseDebounce
	}
	if saved, err := h.store.GetAllSettings(ctx); err == nil {
		if raw := strings.TrimSpace(saved[pulseDebounceSetting]); raw != "" {
			if sec, err := strconv.Atoi(raw); err == nil && sec >= minPulseDebounceSeconds && sec <= maxPulseDebounceSeconds {
				return time.Duration(sec) * time.Second
			}
		}
	}
	return time.Duration(defaultPulseDebounceSeconds) * time.Second
}

func (h *Hub) schedulePulseDelivery() {
	h.pulse.mu.Lock()
	if h.pulse.pending {
		h.pulse.mu.Unlock()
		return
	}
	h.pulse.pending = true
	h.pulse.mu.Unlock()

	started := h.goBackground(func(ctx context.Context) {
		t := time.NewTimer(h.pulseDebounce(ctx))
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.pulse.mu.Lock()
		h.pulse.pending = false
		h.pulse.mu.Unlock()
		h.deliverPulses(ctx)
	})
	if !started {
		h.pulse.mu.Lock()
		h.pulse.pending = false
		h.pulse.mu.Unlock()
	}
}

// RegisterPulseMCPServer registers an external MCP server to receive pulse notifications
// (notifications/message and notifications/resources/updated for syre://pulse).
func (h *Hub) RegisterPulseMCPServer(srv *mcp.Server) {
	if srv == nil {
		return
	}
	h.pulse.mu.Lock()
	defer h.pulse.mu.Unlock()
	if !slices.Contains(h.pulse.mcpServers, srv) {
		h.pulse.mcpServers = append(h.pulse.mcpServers, srv)
	}
}

// LatestPulse returns the most recently emitted board pulse, or a live snapshot when no
// pulse has been emitted yet.
func (h *Hub) LatestPulse(ctx context.Context) (string, error) {
	h.pulse.mu.Lock()
	latest := h.pulse.latest
	h.pulse.mu.Unlock()
	if latest != "" {
		return latest, nil
	}
	return h.buildBoardPulse(ctx, nil).Text(), nil
}

// LatestPulse returns the latest board pulse for MCP resource reads.
func (s *FleetService) LatestPulse(ctx context.Context) (string, error) {
	return s.hub.LatestPulse(ctx)
}

// RegisterPulseMCPServer registers an external MCP server for pulse push notifications.
func (s *FleetService) RegisterPulseMCPServer(srv *mcp.Server) {
	s.hub.RegisterPulseMCPServer(srv)
}

// recordPulseTrigger persists an actionable pulse trigger as a FleetEvent(kind="pulse")
// and schedules debounced delivery.
func (h *Hub) recordPulseTrigger(ctx context.Context, meta pulseTriggerMeta, workerID, taskID, chatID string) {
	if h == nil || h.store == nil {
		return
	}
	summary := formatPulseTriggerSummary(meta)
	b, err := json.Marshal(meta, protocol.JSONOptions)
	if err != nil {
		return
	}
	pctx := withCause(ctx, store.Cause{Actor: store.ActorSystem, Source: "pulse", Confidence: store.ConfidenceCertain})
	h.recordFleetEvent(pctx, store.FleetEvent{
		Kind:     store.EventKindPulse,
		Action:   meta.Kind,
		WorkerID: workerID,
		TaskID:   taskID,
		ChatID:   chatID,
		Summary:  summary,
		MetaJSON: string(b),
	})
}

func formatPulseTriggerSummary(m pulseTriggerMeta) string {
	return protocol.BoardPulse{
		Triggers: []protocol.PulseTrigger{metaToProtocolTrigger(m)},
	}.Triggers[0].Kind + ": " + m.TicketSID + " " + m.Summary
}

func parsePulseMeta(e store.FleetEvent) (pulseTriggerMeta, bool) {
	if e.Kind != store.EventKindPulse || e.MetaJSON == "" {
		return pulseTriggerMeta{}, false
	}
	var m pulseTriggerMeta
	if err := json.Unmarshal([]byte(e.MetaJSON), &m, protocol.JSONOptions); err != nil {
		return pulseTriggerMeta{}, false
	}
	return m, true
}

func metaToProtocolTrigger(m pulseTriggerMeta) protocol.PulseTrigger {
	return protocol.PulseTrigger{
		Kind:          m.Kind,
		TicketSID:     m.TicketSID,
		UnblockedSIDs: m.UnblockedSIDs,
		Followups:     m.Followups,
		Category:      m.Category,
		Project:       m.ProjectName,
		Summary:       m.Summary,
	}
}

// pulseTicketTransition records a pulse trigger when a ticket transitions to an actionable
// state (review, failed, needs_you) or has followups in its digest, and checks whether the
// project's autopilot board went idle.
func (h *Hub) pulseTicketTransition(ctx context.Context, tk store.Ticket, task *store.Task, dig *protocol.RunDigest, fallbackCategory string) {
	if h == nil || h.store == nil {
		return
	}
	var proj store.Project
	if tk.ProjectID != "" {
		if p, err := h.store.GetProject(ctx, tk.ProjectID); err == nil {
			proj = p
		}
	}

	followups := 0
	if dig != nil {
		followups = len(dig.Followups)
	} else if task != nil {
		followups = len(task.Followups)
	}

	var workerID, taskID string
	if task != nil {
		workerID = task.WorkerID
		taskID = task.ID
	} else if tk.Lease != nil {
		workerID = tk.Lease.WorkerID
		taskID = tk.Lease.TaskID
	}

	var kind string
	var category string
	var summary string
	var unblocked []string
	switch tk.Status {
	case store.TicketStatusReview:
		kind = protocol.PulseTriggerReview
		unblocked = h.unblockedTicketSIDs(ctx, tk.ID, proj)
	case store.TicketStatusFailed:
		kind = protocol.PulseTriggerFailed
		if dig != nil && dig.Failure != nil && strings.TrimSpace(dig.Failure.Category) != "" {
			category = strings.TrimSpace(dig.Failure.Category)
		} else {
			category = strings.TrimSpace(fallbackCategory)
		}
	case store.TicketStatusNeedsYou:
		kind = protocol.PulseTriggerNeedsYou
		if dig != nil && dig.Question != nil && strings.TrimSpace(dig.Question.Text) != "" {
			summary = protocol.FormatPulseNeedsYouExcerpt(dig.Question.Text)
		}
		if dig != nil && dig.Failure != nil && strings.TrimSpace(dig.Failure.Category) != "" {
			category = strings.TrimSpace(dig.Failure.Category)
		} else {
			category = strings.TrimSpace(fallbackCategory)
		}
	default:
		if followups > 0 {
			kind = protocol.PulseTriggerFollowups
		}
	}

	globalOn := h.autopilotGlobal(ctx)
	fleetPaused := h.fleetPaused(ctx)
	projectPlaying := isProjectAutopilotPlaying(proj, globalOn, fleetPaused, h.projectDirectivePaused(ctx, proj.ID))

	if kind != "" {
		h.recordPulseTrigger(ctx, pulseTriggerMeta{
			Kind:          kind,
			ProjectID:     tk.ProjectID,
			ProjectName:   proj.Name,
			TicketID:      tk.ID,
			TicketSID:     tk.SID,
			UnblockedSIDs: unblocked,
			Followups:     followups,
			Category:      category,
			Summary:       summary,
			Autopilot:     projectPlaying,
		}, workerID, taskID, "")
	}

	if projectPlaying && tk.Status != store.TicketStatusRunning && tk.Status != store.TicketStatusNeedsYou {
		h.checkProjectBoardIdle(ctx, tk.ProjectID)
	}
}

// unblockedTicketSIDs returns SIDs of ready tickets blocked by ticketID whose blockers are
// now all satisfied (respecting the project's optimistic_chaining setting).
func (h *Hub) unblockedTicketSIDs(ctx context.Context, ticketID string, proj store.Project) []string {
	blocking, err := h.store.ListTicketBlocking(ctx, ticketID)
	if err != nil || len(blocking) == 0 {
		return nil
	}
	optimistic := proj.Autonomy.OptimisticChaining != nil && *proj.Autonomy.OptimisticChaining
	var sids []string
	for _, edge := range blocking {
		dep, err := h.store.GetTicket(ctx, edge.TicketID)
		if err != nil || dep.Status != store.TicketStatusReady || store.IsContainerKind(dep.Kind) {
			continue
		}
		edges, err := h.store.ListTicketEdges(ctx, dep.ID)
		if err != nil {
			continue
		}
		allSatisfied := true
		for _, e := range edges {
			blk, bErr := h.store.GetTicket(ctx, e.BlockedByID)
			if bErr != nil {
				allSatisfied = false
				break
			}
			if blk.Status == store.TicketStatusDone {
				continue
			}
			if blk.Status == store.TicketStatusReview && optimistic {
				continue
			}
			allSatisfied = false
			break
		}
		if allSatisfied {
			sids = append(sids, dep.SID)
		}
	}
	sort.Strings(sids)
	return sids
}

func hasNeedsDecompositionLabel(labels []string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), needsDecompositionLabel) {
			return true
		}
	}
	return false
}

// pulseTicketDecompositionCheck checks whether tk awaits decomposition (status ready with
// no repo binding, or label needs_decomposition) and records a pulse trigger once until
// the ticket's state changes.
func (h *Hub) pulseTicketDecompositionCheck(ctx context.Context, tk store.Ticket) {
	if h == nil || h.store == nil || store.IsContainerKind(tk.Kind) {
		return
	}
	needs := false
	switch tk.Status {
	case store.TicketStatusReady:
		if hasNeedsDecompositionLabel(tk.Labels) {
			needs = true
		} else if bindings, err := h.store.ListTicketRepoBindings(ctx, tk.ID); err == nil && len(bindings) == 0 {
			needs = true
		}
	case store.TicketStatusBacklog:
		if hasNeedsDecompositionLabel(tk.Labels) {
			needs = true
		}
	}

	h.pulse.mu.Lock()
	if h.pulse.decomposedPulsed == nil {
		h.pulse.decomposedPulsed = map[string]bool{}
	}
	if !needs {
		delete(h.pulse.decomposedPulsed, tk.ID)
		h.pulse.mu.Unlock()
		return
	}
	if h.pulse.decomposedPulsed[tk.ID] {
		h.pulse.mu.Unlock()
		return
	}
	h.pulse.decomposedPulsed[tk.ID] = true
	h.pulse.mu.Unlock()

	var projName string
	var autopilot bool
	if tk.ProjectID != "" {
		if p, err := h.store.GetProject(ctx, tk.ProjectID); err == nil {
			projName = p.Name
			autopilot = isProjectAutopilotPlaying(p, h.autopilotGlobal(ctx), h.fleetPaused(ctx), h.projectDirectivePaused(ctx, p.ID))
		}
	}
	h.recordPulseTrigger(ctx, pulseTriggerMeta{
		Kind:         protocol.PulseTriggerNeedsDecomposition,
		ProjectID:    tk.ProjectID,
		ProjectName:  projName,
		TicketID:     tk.ID,
		TicketSID:    tk.SID,
		ProjectLevel: true,
		Autopilot:    autopilot,
	}, "", "", "")
}

// scanTicketsAwaitingDecomposition checks ready/backlog tickets in autopilot projects for
// items needing decomposition (no repo binding or needs_decomposition label).
func (h *Hub) scanTicketsAwaitingDecomposition(ctx context.Context, autopilotProjects map[string]store.Project) {
	for pid := range autopilotProjects {
		ready, err := h.store.ListTickets(ctx, store.TicketFilter{ProjectID: pid, Status: store.TicketStatusReady, Limit: 200})
		if err == nil {
			for _, tk := range ready {
				h.pulseTicketDecompositionCheck(ctx, tk)
			}
		}
		backlog, err := h.store.ListTickets(ctx, store.TicketFilter{ProjectID: pid, Status: store.TicketStatusBacklog, Limit: 200})
		if err == nil {
			for _, tk := range backlog {
				if hasNeedsDecompositionLabel(tk.Labels) {
					h.pulseTicketDecompositionCheck(ctx, tk)
				}
			}
		}
	}
}

// markProjectActive clears the board_idle latch when new work is dispatched or becomes
// active in projectID.
func (h *Hub) markProjectActive(projectID string) {
	if projectID == "" {
		return
	}
	h.pulse.mu.Lock()
	delete(h.pulse.idlePulsed, projectID)
	h.pulse.mu.Unlock()
}

// checkProjectBoardIdle triggers a project-level board_idle pulse and turns Play off
// automatically when an autopilot project has no dispatchable, running, or needs_you
// tickets left (review does not count as work left for autopilot).
func (h *Hub) checkProjectBoardIdle(ctx context.Context, projectID string) {
	if h == nil || h.store == nil || projectID == "" {
		return
	}
	proj, err := h.store.GetProject(ctx, projectID)
	if err != nil || !isProjectAutopilotPlaying(proj, h.autopilotGlobal(ctx), h.fleetPaused(ctx), h.projectDirectivePaused(ctx, proj.ID)) {
		h.markProjectActive(projectID)
		return
	}
	if disp, err := h.store.ListDispatchableTickets(ctx, projectID, 1); err != nil || len(disp) > 0 {
		h.markProjectActive(projectID)
		return
	}
	if running, err := h.store.ListTickets(ctx, store.TicketFilter{ProjectID: projectID, Status: store.TicketStatusRunning, Limit: 1}); err != nil || len(running) > 0 {
		h.markProjectActive(projectID)
		return
	}
	if needs, err := h.store.ListTickets(ctx, store.TicketFilter{ProjectID: projectID, Status: store.TicketStatusNeedsYou, Limit: 1}); err != nil || len(needs) > 0 {
		h.markProjectActive(projectID)
		return
	}

	h.pulse.mu.Lock()
	if h.pulse.idlePulsed == nil {
		h.pulse.idlePulsed = map[string]bool{}
	}
	if h.pulse.idlePulsed[projectID] {
		h.pulse.mu.Unlock()
		return
	}
	h.pulse.idlePulsed[projectID] = true
	h.pulse.mu.Unlock()

	// Turn Play off before recording the final pulse so the delivered pulse reflects
	// the final autopilot state.
	h.finishProjectAutopilot(ctx, proj)

	h.recordPulseTrigger(ctx, pulseTriggerMeta{
		Kind:         protocol.PulseTriggerBoardIdle,
		ProjectID:    proj.ID,
		ProjectName:  proj.Name,
		ProjectLevel: true,
		Autopilot:    true,
	}, "", "", "")
}

// NotifyDirectiveExpiring is the hook point for expiring policy directives (A6) and
// overrides. When chatID is empty, the trigger is project/fleet-level and reaches all
// commanders; when chatID is set, it targets that commander.
func (h *Hub) NotifyDirectiveExpiring(ctx context.Context, projectID, chatID, summary string) {
	if h == nil || h.store == nil {
		return
	}
	var projName string
	if projectID != "" {
		if p, err := h.store.GetProject(ctx, projectID); err == nil {
			projName = p.Name
		}
	}
	h.recordPulseTrigger(ctx, pulseTriggerMeta{
		Kind:         protocol.PulseTriggerDirectiveExpiring,
		ProjectID:    projectID,
		ProjectName:  projName,
		Summary:      strings.TrimSpace(summary),
		ProjectLevel: chatID == "",
	}, "", "", chatID)
}

// buildBoardPulse assembles the current fleet and board snapshot along with the batched
// triggers from pulseEvents.
func (h *Hub) buildBoardPulse(ctx context.Context, pulseEvents []store.FleetEvent) protocol.BoardPulse {
	p := protocol.BoardPulse{
		AutopilotState: "off",
		Time:           h.pulseClock().Local().Format("15:04"),
	}

	// Autopilot state
	if h.fleetPaused(ctx) {
		p.AutopilotState = "paused"
	} else {
		globalOn := h.autopilotGlobal(ctx)
		if projects, err := h.store.ListProjects(ctx); err == nil {
			for _, proj := range projects {
				if isProjectAutopilotPlaying(proj, globalOn, false, h.projectDirectivePaused(ctx, proj.ID)) {
					p.AutopilotState = "on"
					break
				}
			}
		}
	}

	// Fleet state
	if workers, err := h.store.ListWorkers(ctx); err == nil {
		p.FleetTotal = len(workers)
		sort.Slice(workers, func(i, j int) bool { return workers[i].Name < workers[j].Name })
		heldWorkers := map[string]bool{}
		if holds, hErr := h.store.ListHolds(ctx, store.HoldFilter{ActiveOnly: true}); hErr == nil {
			for _, hd := range holds {
				if hd.WorkerID != "" && hd.ScopeKind == store.HoldScopeMachine {
					heldWorkers[hd.WorkerID] = true
				}
			}
		}
		var dirs []store.Directive
		if list, dErr := h.store.ListDirectives(ctx, store.DirectiveFilter{OpenOnly: true, Limit: 1000}); dErr == nil {
			dirs = list
		}
		loc := h.pulseClock().Location()
		now := h.directiveClock()
		for _, w := range workers {
			if h.IsWorkerConnected(w.ID) {
				p.FleetUp++
			}
			var states []string
			if phrase := h.directiveFleetPhrase(w, dirs, now, loc); phrase != "" {
				states = append(states, phrase)
			}
			if heldWorkers[w.ID] {
				states = append(states, "held")
			}
			if len(states) > 0 {
				p.FleetNotable = append(p.FleetNotable, w.Name+" "+strings.Join(states, ", "))
			}
		}
		p.FleetNotable = append(p.FleetNotable, h.providerWaitPhrases(ctx, loc)...)
	}

	// Board counts
	if tickets, err := h.store.ListTickets(ctx, store.TicketFilter{Limit: 1000}); err == nil {
		for _, tk := range tickets {
			if store.IsContainerKind(tk.Kind) {
				continue
			}
			switch tk.Status {
			case store.TicketStatusReady:
				p.Ready++
			case store.TicketStatusRunning:
				p.Running++
			case store.TicketStatusNeedsYou:
				p.NeedsYou++
				if len(p.NeedsYouSIDs) < 3 {
					p.NeedsYouSIDs = append(p.NeedsYouSIDs, tk.SID)
				}
			case store.TicketStatusReview:
				p.Review++
			}
		}
	}
	if disp, err := h.store.ListDispatchableTickets(ctx, "", 1000); err == nil {
		p.Dispatchable = len(disp)
	}

	// Coalesce triggers
	p.Triggers = coalescePulseTriggers(pulseEvents)
	return p
}

func coalescePulseTriggers(events []store.FleetEvent) []protocol.PulseTrigger {
	if len(events) == 0 {
		return nil
	}
	var out []protocol.PulseTrigger
	var decompSIDs []string
	decompSeen := map[string]bool{}
	idleSeen := map[string]bool{}
	for _, e := range events {
		m, ok := parsePulseMeta(e)
		if !ok {
			continue
		}
		switch m.Kind {
		case protocol.PulseTriggerNeedsDecomposition:
			if m.TicketSID != "" && !decompSeen[m.TicketSID] {
				decompSeen[m.TicketSID] = true
				decompSIDs = append(decompSIDs, m.TicketSID)
			}
		case protocol.PulseTriggerBoardIdle:
			key := m.ProjectID
			if key == "" {
				key = m.ProjectName
			}
			if !idleSeen[key] {
				idleSeen[key] = true
				out = append(out, metaToProtocolTrigger(m))
			}
		default:
			out = append(out, metaToProtocolTrigger(m))
		}
	}
	if len(decompSIDs) > 0 {
		out = append(out, protocol.PulseTrigger{
			Kind:       protocol.PulseTriggerNeedsDecomposition,
			TicketSIDs: decompSIDs,
		})
	}
	return out
}

// deliverPulses runs after the pulse debounce window expires:
//   - updates the latest pulse and pushes MCP notifications to external MCP commanders;
//   - starts a turn in every idle internal Commander chat whose work or projects are affected
//     (busy commanders receive it prepended to their next prompt via noteForPrompt).
func (h *Hub) deliverPulses(ctx context.Context) {
	// 1. External MCP commanders + latest pulse snapshot.
	h.pulse.mu.Lock()
	afterID := h.pulse.mcpCursor
	h.pulse.mu.Unlock()

	evs, err := h.store.ListFleetEvents(ctx, store.FleetEventFilter{
		Kind:    store.EventKindPulse,
		AfterID: afterID,
		Limit:   200,
	})
	if err == nil && len(evs) > 0 {
		// ListFleetEvents returns newest first; reverse to chronological order.
		slices.Reverse(evs)
		maxID := evs[len(evs)-1].ID
		pulseText := h.buildBoardPulse(ctx, evs).Text()

		h.pulse.mu.Lock()
		if maxID > h.pulse.mcpCursor {
			h.pulse.mcpCursor = maxID
		}
		h.pulse.latest = pulseText
		servers := append([]*mcp.Server(nil), h.pulse.mcpServers...)
		h.pulse.mu.Unlock()

		h.notifyPulseMCPServers(ctx, servers)
	}

	// 2. Internal Commander chats (idle -> new turn; busy -> left for noteForPrompt).
	for _, cs := range h.commanderChats(ctx) {
		if !chatIdle(cs.Status) || cs.WorkerID == "" || h.workerConnFor(cs.WorkerID) == nil {
			continue
		}
		events := h.undeliveredEvents(ctx, cs)
		if len(events) == 0 {
			continue
		}
		work := h.commanderWorkOf(ctx, cs.ID)
		hasPulse := false
		hasNonPulse := false
		for _, e := range events {
			if e.Kind == store.EventKindPulse {
				if work.affectsPulse(cs.ID, e) {
					hasPulse = true
				}
			} else if work.affects(cs.ID, e) {
				hasNonPulse = true
			}
		}
		if !hasPulse {
			continue
		}
		dctx := withCause(ctx, store.Cause{Actor: store.ActorSystem, Source: "pulse", Confidence: store.ConfidenceCertain})
		turnID := ids.UUID()
		note := h.takeNote(dctx, cs.ID, turnID)
		if note == "" {
			continue
		}
		if hasNonPulse {
			note += "\nThe user is in charge: don't undo or work around their actions. If this affects work you started, tell the user what is paused or blocked and ask (ask_user) whether to restart, wait or change plans."
		}
		if err := h.sendAgentChat(dctx, cs, chatSendRequest{SessionID: cs.ID, Content: note, TurnID: turnID}); err != nil {
			h.logger.Warn("commander pulse delivery failed", "chat_id", cs.ID, "err", err)
			continue
		}
		h.recordFleetEvent(dctx, store.FleetEvent{
			Kind: store.EventKindNotice, Action: "commander_pulse", ChatID: cs.ID, WorkerID: cs.WorkerID,
			Summary: fmt.Sprintf("Syre pushed a board pulse to the commander (chat %s)", cs.SID),
		})
	}
}

func (h *Hub) notifyPulseMCPServers(ctx context.Context, servers []*mcp.Server) {
	for _, srv := range servers {
		if srv == nil {
			continue
		}
		_ = srv.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{
			URI: PulseResourceURI,
		})
	}
}
