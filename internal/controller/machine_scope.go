package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

// Operator labels, machine groups and Commander chat scope (SYR-FJA2T).
//
// A Commander chat's scope is a hard limit on where it may send work: the hub refuses a
// dispatch to a machine outside it, places unpinned work only on machines inside it, and
// the scheduler holds to it for work the chat queued. Offline machines stay in a scope
// and are skipped at dispatch like anywhere else.

// errOutOfScope is a dispatch to a machine outside a chat's scope or a ticket's Run on.
type errOutOfScope struct {
	machine string
	sc      machineScope
}

func (e *errOutOfScope) Error() string {
	if e.sc.ticket != "" {
		return fmt.Sprintf("%s is outside ticket %s's Run on (%s); pick a machine it allows or ask the user to change the ticket's Run on", e.machine, e.sc.ticket, e.sc.label)
	}
	return fmt.Sprintf("%s is outside this chat's machines (%s); pick a machine in scope or ask the user to change the chat's machines", e.machine, e.sc.label)
}

// machineScope is a resolved target: the machine IDs it names now and how to say it.
type machineScope struct {
	target store.MachineTarget
	ids    map[string]bool
	label  string
	// ticket is the ticket's short id when this is a ticket's Run on (SYR-0M4QQ).
	ticket string
}

func (m machineScope) all() bool { return m.target.IsAll() }

// in reports whether a machine is inside the scope.
func (m machineScope) in(workerID string) bool { return m.all() || m.ids[workerID] }

// resolveScope resolves t against the current fleet, labels and groups.
func (h *Hub) resolveScope(ctx context.Context, t store.MachineTarget) machineScope {
	sc := machineScope{target: t, ids: map[string]bool{}}
	if t.IsAll() {
		sc.label = "All machines"
		return sc
	}
	workers, _ := h.store.ListWorkers(ctx)
	workerIDs := make([]string, 0, len(workers))
	names := map[string]string{}
	for _, w := range workers {
		workerIDs = append(workerIDs, w.ID)
		names[w.ID] = w.Name
	}
	labels, _ := h.store.ListMachineLabels(ctx)
	groups := map[string][]string{}
	var groupName string
	if t.Kind == store.TargetGroup {
		if g, err := h.store.GetMachineGroup(ctx, t.ID); err == nil {
			groups[g.ID] = g.WorkerIDs
			groupName = g.Name
		}
	}
	for _, id := range store.ResolveTarget(t, workerIDs, labels, groups) {
		sc.ids[id] = true
	}
	switch t.Kind {
	case store.TargetMachine:
		sc.label = cmpOr(names[t.ID], t.ID)
	case store.TargetMachines:
		sc.label = fmt.Sprintf("%d machines", len(t.IDs))
		if len(t.IDs) == 1 {
			sc.label = cmpOr(names[t.IDs[0]], t.IDs[0])
		}
	case store.TargetGroup:
		sc.label = "group " + cmpOr(groupName, "(deleted)")
	case store.TargetLabel:
		sc.label = "label " + t.Label
	}
	return sc
}

// chatScope returns the scope of the Commander chat ctx comes from; all machines for
// anything else (an agent chat, the user, a ticket run).
func (h *Hub) chatScope(ctx context.Context) machineScope {
	o, ok := chatOriginFrom(ctx)
	if !ok || !o.commander {
		return machineScope{target: store.MachineTarget{Kind: store.TargetAll}, label: "All machines"}
	}
	return h.scopeOfChat(ctx, o.id)
}

// scopeOfChat returns a chat's stored scope, resolved.
func (h *Hub) scopeOfChat(ctx context.Context, chatID string) machineScope {
	t := store.MachineTarget{Kind: store.TargetAll}
	if chatID != "" {
		if st, err := h.store.GetChatScope(ctx, chatID); err == nil {
			t = st
		}
	}
	return h.resolveScope(ctx, t)
}

// commanderScope is the scope of chatID when it is a Commander chat; all machines for
// any other chat.
func (h *Hub) commanderScope(ctx context.Context, chatID string) machineScope {
	if cs, err := h.store.GetChatSession(ctx, chatID); err == nil && cs.FleetTools {
		return h.scopeOfChat(ctx, chatID)
	}
	return h.resolveScope(ctx, store.MachineTarget{Kind: store.TargetAll})
}

// ticketScope is a ticket's Run on, resolved (all machines when it has none).
func (h *Hub) ticketScope(ctx context.Context, ticketID string) machineScope {
	t := store.MachineTarget{Kind: store.TargetAll}
	sid := ticketID
	if ticketID != "" {
		if st, err := h.store.GetTicketScope(ctx, ticketID); err == nil {
			t = st
		}
		if tk, err := h.store.GetTicket(ctx, ticketID); err == nil && tk.SID != "" {
			sid = tk.SID
		}
	}
	sc := h.resolveScope(ctx, t)
	sc.ticket = sid
	return sc
}

// scopeOverlap reports whether a and b (both resolved) share a machine.
func scopeOverlap(a, b machineScope) bool {
	if a.all() || b.all() {
		return true
	}
	for id := range a.ids {
		if b.ids[id] {
			return true
		}
	}
	return false
}

// applyScopes refuses a selector outside either scope or scopes that cannot both hold,
// then drops machines outside them; the drops are returned as rejections.
func applyScopes(workers, candidates []store.Worker, sel WorkerSelector, scopes ...machineScope) ([]store.Worker, []Rejection, error) {
	var live []machineScope
	for _, sc := range scopes {
		if !sc.all() {
			live = append(live, sc)
		}
	}
	for _, sc := range live {
		if err := scopeAllowsSelector(sc, workers, sel); err != nil {
			return nil, nil, err
		}
		if len(sc.ids) == 0 {
			if sc.ticket != "" {
				return nil, nil, fmt.Errorf("ticket %s's Run on (%s) names no machine right now; ask the user to change it", sc.ticket, sc.label)
			}
			return nil, nil, fmt.Errorf("this chat's machines (%s) name no machine right now; ask the user to change the chat's machines", sc.label)
		}
	}
	if len(live) == 2 && !scopeOverlap(live[0], live[1]) {
		return nil, nil, fmt.Errorf("this chat's machines (%s) and ticket %s's Run on (%s) share no machine; ask the user to change one of them", live[0].label, live[1].ticket, live[1].label)
	}
	var rejected []Rejection
	for _, sc := range live {
		var dropped []Rejection
		candidates, dropped = filterScope(candidates, sc)
		rejected = append(rejected, dropped...)
	}
	return candidates, rejected, nil
}

// filterScope drops machines outside sc, recording each as a rejection.
func filterScope(workers []store.Worker, sc machineScope) ([]store.Worker, []Rejection) {
	if sc.all() {
		return workers, nil
	}
	kept := make([]store.Worker, 0, len(workers))
	var rejected []Rejection
	for _, w := range workers {
		if sc.in(w.ID) {
			kept = append(kept, w)
			continue
		}
		reason := "user scope (" + sc.label + ")"
		if sc.ticket != "" {
			reason = "user scope (ticket: " + sc.label + ")"
		}
		rejected = append(rejected, Rejection{WorkerID: w.ID, WorkerName: w.Name, Reason: reason})
	}
	return kept, rejected
}

// scopeAllowsSelector refuses a selector that names a machine (by ID or name) outside sc.
func scopeAllowsSelector(sc machineScope, workers []store.Worker, sel WorkerSelector) error {
	for _, w := range workers {
		named := (sel.WorkerID != "" && w.ID == sel.WorkerID) || (sel.WorkerID == "" && sel.Name != "" && strings.EqualFold(w.Name, sel.Name))
		if named && !sc.in(w.ID) {
			return &errOutOfScope{machine: w.Name, sc: sc}
		}
	}
	return nil
}

// normalizeTarget checks t names machines and groups that exist, resolving machine
// names and short ids to machine IDs.
func (h *Hub) normalizeTarget(ctx context.Context, t store.MachineTarget) (store.MachineTarget, error) {
	t, err := t.Normalize()
	if err != nil {
		return t, err
	}
	machine := func(ref string) (string, error) {
		id := h.resolveMachine(ctx, ref)
		if _, err := h.store.GetWorker(ctx, id); err != nil {
			return "", fmt.Errorf("no machine %q", ref)
		}
		return id, nil
	}
	switch t.Kind {
	case store.TargetMachine:
		if t.ID, err = machine(t.ID); err != nil {
			return t, err
		}
	case store.TargetMachines:
		for i, ref := range t.IDs {
			if t.IDs[i], err = machine(ref); err != nil {
				return t, err
			}
		}
	case store.TargetGroup:
		g, err := h.findGroup(ctx, t.ID)
		if err != nil {
			return t, err
		}
		t.ID = g.ID
	}
	return t, nil
}

// findGroup finds a group by ID or name (any case).
func (h *Hub) findGroup(ctx context.Context, ref string) (store.MachineGroup, error) {
	ref = strings.TrimSpace(ref)
	if g, err := h.store.GetMachineGroup(ctx, ref); err == nil {
		return g, nil
	}
	groups, err := h.store.ListMachineGroups(ctx)
	if err != nil {
		return store.MachineGroup{}, err
	}
	for _, g := range groups {
		if strings.EqualFold(g.Name, ref) {
			return g, nil
		}
	}
	return store.MachineGroup{}, fmt.Errorf("no machine group %q", ref)
}

// broadcastMachineLabels sends one machine's labels to the browsers.
func (h *Hub) broadcastMachineLabels(ctx context.Context, workerID string) []string {
	all, _ := h.store.ListMachineLabels(ctx)
	labels := all[workerID]
	if labels == nil {
		labels = []string{}
	}
	h.broadcastFleetUpdate("machine_labels", machineLabelsUpdate{WorkerID: workerID, Labels: labels})
	return labels
}

type machineLabelsUpdate struct {
	WorkerID string   `json:"worker_id"`
	Labels   []string `json:"labels"`
}

// machineGroups lists the groups (never nil).
func (h *Hub) machineGroups(ctx context.Context) []store.MachineGroup {
	groups, _ := h.store.ListMachineGroups(ctx)
	if groups == nil {
		groups = []store.MachineGroup{}
	}
	return groups
}

func (h *Hub) broadcastMachineGroups(ctx context.Context) {
	h.broadcastFleetUpdate("machine_groups", h.machineGroups(ctx))
}

// labelMachine adds (or with remove, removes) labels on a machine.
func (h *Hub) labelMachine(ctx context.Context, workerID string, labels []string, remove bool) ([]string, error) {
	if _, err := h.store.GetWorker(ctx, workerID); err != nil {
		return nil, err
	}
	for _, raw := range labels {
		l, err := store.NormalizeLabel(raw)
		if err != nil {
			return nil, err
		}
		if remove {
			err = h.store.RemoveMachineLabel(ctx, workerID, l)
		} else {
			err = h.store.AddMachineLabel(ctx, workerID, l)
		}
		if err != nil {
			return nil, err
		}
	}
	return h.broadcastMachineLabels(ctx, workerID), nil
}

// saveGroup creates a group, or with id set renames it and replaces its members.
func (h *Hub) saveGroup(ctx context.Context, id, name string, machines []string) (store.MachineGroup, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return store.MachineGroup{}, errors.New("a group needs a name")
	}
	if len([]rune(name)) > 60 {
		return store.MachineGroup{}, errors.New("group name is longer than 60 characters")
	}
	members := make([]string, 0, len(machines))
	for _, ref := range machines {
		mid := h.resolveMachine(ctx, strings.TrimSpace(ref))
		if _, err := h.store.GetWorker(ctx, mid); err != nil {
			return store.MachineGroup{}, fmt.Errorf("no machine %q", ref)
		}
		if !slices.Contains(members, mid) {
			members = append(members, mid)
		}
	}
	var g store.MachineGroup
	var err error
	if id == "" {
		g, err = h.store.CreateMachineGroup(ctx, store.MachineGroup{Name: name, WorkerIDs: members, CreatedBy: describeActor(causeFrom(ctx))})
	} else {
		g, err = h.store.GetMachineGroup(ctx, id)
		if err == nil {
			g.Name, g.WorkerIDs = name, members
			err = h.store.UpdateMachineGroup(ctx, g)
		}
	}
	if errors.Is(err, store.ErrConflict) {
		return g, fmt.Errorf("a group named %q already exists", name)
	}
	if err != nil {
		return g, err
	}
	h.broadcastMachineGroups(ctx)
	return h.store.GetMachineGroup(ctx, g.ID)
}

// scopeSummary is fleet_status's view of the saved groups and, for a Commander chat, the
// machines it may send work to.
func (h *Hub) scopeSummary(ctx context.Context, workers []WorkerStatusView) (groups []string, scope string) {
	names := map[string]string{}
	for _, w := range workers {
		names[w.ID] = w.Name
	}
	nameList := func(ids []string) string {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			out = append(out, cmpOr(names[id], id))
		}
		return strings.Join(out, ", ")
	}
	for _, g := range h.machineGroups(ctx) {
		groups = append(groups, g.Name+": "+cmpOr(nameList(g.WorkerIDs), "(empty)"))
	}
	if o, ok := chatOriginFrom(ctx); ok && o.commander {
		sc := h.scopeOfChat(ctx, o.id)
		if sc.all() {
			return groups, "All machines"
		}
		var in []string
		for _, w := range workers {
			if sc.in(w.ID) {
				in = append(in, w.ID)
			}
		}
		scope = fmt.Sprintf("%s: %s. Work goes only to these machines; the user changes them in the chat.", sc.label, cmpOr(nameList(in), "no machines"))
	}
	return groups, scope
}

// --- REST ----------------------------------------------------------------------------

func (s *Server) registerMachineScopeRoutes() {
	s.mux.Handle("GET /api/machine-labels", s.requireBrowserAuth(s.handleListMachineLabels))
	s.mux.Handle("POST /api/workers/{id}/labels", s.requireBrowserAuth(s.byRef(ids.Machine, s.handleAddMachineLabel)))
	s.mux.Handle("DELETE /api/workers/{id}/labels/{label}", s.requireBrowserAuth(s.byRef(ids.Machine, s.handleRemoveMachineLabel)))
	s.mux.Handle("GET /api/machine-groups", s.requireBrowserAuth(s.handleListMachineGroups))
	s.mux.Handle("POST /api/machine-groups", s.requireBrowserAuth(s.handleSaveMachineGroup))
	s.mux.Handle("PUT /api/machine-groups/{id}", s.requireBrowserAuth(s.handleSaveMachineGroup))
	s.mux.Handle("DELETE /api/machine-groups/{id}", s.requireBrowserAuth(s.handleDeleteMachineGroup))
	s.mux.Handle("GET /api/chats/{id}/scope", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleGetChatScope)))
	s.mux.Handle("PUT /api/chats/{id}/scope", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleSetChatScope)))
	s.mux.Handle("GET /api/tickets/{id}/scope", s.requireBrowserAuth(s.byRef(ids.Ticket, s.handleGetTicketScope)))
	s.mux.Handle("PUT /api/tickets/{id}/scope", s.requireBrowserAuth(s.byRef(ids.Ticket, s.handleSetTicketScope)))
}

func (s *Server) handleListMachineLabels(w http.ResponseWriter, r *http.Request) {
	labels, err := s.store.ListMachineLabels(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"labels": labels})
}

func (s *Server) handleAddMachineLabel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	s.writeLabels(w, r, []string{body.Label}, false)
}

func (s *Server) handleRemoveMachineLabel(w http.ResponseWriter, r *http.Request) {
	s.writeLabels(w, r, []string{r.PathValue("label")}, true)
}

func (s *Server) writeLabels(w http.ResponseWriter, r *http.Request, labels []string, remove bool) {
	id := s.hub.resolveMachine(r.Context(), r.PathValue("id"))
	got, err := s.hub.labelMachine(r.Context(), id, labels, remove)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "machine not found"})
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, machineLabelsUpdate{WorkerID: id, Labels: got})
	}
}

func (s *Server) handleListMachineGroups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"groups": s.hub.machineGroups(r.Context())})
}

func (s *Server) handleSaveMachineGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string   `json:"name"`
		WorkerIDs []string `json:"worker_ids"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	if id != "" {
		if _, err := s.store.GetMachineGroup(r.Context(), id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "machine group not found"})
			return
		}
	}
	g, err := s.hub.saveGroup(r.Context(), id, body.Name, body.WorkerIDs)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	status := http.StatusOK
	if id == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, g)
}

func (s *Server) handleDeleteMachineGroup(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteMachineGroup(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "machine group not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.broadcastMachineGroups(r.Context())
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// chatScopeView is a chat's scope with what it names now.
type chatScopeView struct {
	ChatID     string              `json:"chat_id"`
	Scope      store.MachineTarget `json:"scope"`
	Label      string              `json:"label"`
	MachineIDs []string            `json:"machine_ids"`
}

func (h *Hub) chatScopeView(ctx context.Context, chatID string) chatScopeView {
	sc := h.scopeOfChat(ctx, chatID)
	v := chatScopeView{ChatID: chatID, Scope: sc.target, Label: sc.label, MachineIDs: []string{}}
	if sc.all() {
		return v
	}
	workers, _ := h.store.ListWorkers(ctx)
	for _, w := range workers {
		if sc.in(w.ID) {
			v.MachineIDs = append(v.MachineIDs, w.ID)
		}
	}
	return v
}

func (s *Server) handleGetChatScope(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetChatSession(r.Context(), id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "chat not found"})
		return
	}
	writeJSON(w, http.StatusOK, s.hub.chatScopeView(r.Context(), id))
}

func (s *Server) handleSetChatScope(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	cs, err := s.store.GetChatSession(ctx, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "chat not found"})
		return
	}
	if !cs.FleetTools {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "only a Commander chat has machines to choose; an agent chat runs on its own machine"})
		return
	}
	var body store.MachineTarget
	if !decodeJSON(w, r, &body) {
		return
	}
	t, err := s.hub.normalizeTarget(ctx, body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.store.SetChatScope(ctx, id, t); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	v := s.hub.chatScopeView(ctx, id)
	s.hub.broadcastFleetUpdate("chat_scope", v)
	writeJSON(w, http.StatusOK, v)
}

// ticketScopeView is a ticket's Run on with what it names now (SYR-0M4QQ).
type ticketScopeView struct {
	TicketID   string              `json:"ticket_id"`
	Scope      store.MachineTarget `json:"scope"`
	Label      string              `json:"label"`
	MachineIDs []string            `json:"machine_ids"`
}

func (h *Hub) ticketScopeView(ctx context.Context, ticketID string) ticketScopeView {
	sc := h.ticketScope(ctx, ticketID)
	v := ticketScopeView{TicketID: ticketID, Scope: sc.target, Label: sc.label, MachineIDs: []string{}}
	if sc.all() {
		return v
	}
	workers, _ := h.store.ListWorkers(ctx)
	for _, w := range workers {
		if sc.in(w.ID) {
			v.MachineIDs = append(v.MachineIDs, w.ID)
		}
	}
	return v
}

func (s *Server) handleGetTicketScope(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetTicket(r.Context(), id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "ticket not found"})
		return
	}
	writeJSON(w, http.StatusOK, s.hub.ticketScopeView(r.Context(), id))
}

func (s *Server) handleSetTicketScope(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := s.store.GetTicket(ctx, id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "ticket not found"})
		return
	}
	var body store.MachineTarget
	if !decodeJSON(w, r, &body) {
		return
	}
	t, err := s.hub.normalizeTarget(ctx, body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.store.SetTicketScope(ctx, id, t); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	v := s.hub.ticketScopeView(ctx, id)
	s.hub.broadcastFleetUpdate("ticket_scope", v)
	// A ticket that waited for a machine may fit one now.
	s.hub.kickScheduler()
	writeJSON(w, http.StatusOK, v)
}
