package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

const (
	// directiveWarnBefore is how early a directive's expiry wakes the board pulse.
	directiveWarnBefore = 5 * time.Minute
	// directiveReapEvery is the safety pass that expires anything the single timer missed.
	directiveReapEvery = 30 * time.Second
	// directiveIdleDelay is how long the timer sleeps when nothing is scheduled to expire.
	directiveIdleDelay = 24 * time.Hour
	directiveReasonMax = 500

	AuditDirectiveCreate = "directive.create"
	AuditDirectiveEnd    = "directive.end"
	AuditDirectiveExpire = "directive.expire"

	directiveActionCreated = "created"
	directiveActionEnded   = "ended"
	directiveActionExpired = "expired"
)

// causeDirective attributes suspend/resume and expiry to the directive engine,
// not to a user hold.
var causeDirective = store.Cause{Actor: store.ActorSystem, Source: "directive", Confidence: store.ConfidenceCertain}

// DirectiveInput is a request to create a policy directive.
type DirectiveInput struct {
	Kind   string
	Target string
	Until  *time.Time
	Reason string
	Params store.DirectiveParams
}

func (h *Hub) directiveClock() time.Time {
	if h != nil && h.directiveNow != nil {
		return h.directiveNow()
	}
	return time.Now()
}

func (h *Hub) kickDirective() {
	if h == nil || h.directiveKick == nil {
		return
	}
	select {
	case h.directiveKick <- struct{}{}:
	default:
	}
}

// directiveLoop expires directives at until. One timer is reset to the next
// warning or expiry; a reaper pass covers a missed tick or a clock jump.
func (h *Hub) directiveLoop(ctx context.Context) {
	timer := time.NewTimer(h.nextDirectiveDelay(ctx))
	defer timer.Stop()
	reap := time.NewTicker(directiveReapEvery)
	defer reap.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.directiveKick:
			rearmTimer(timer, h.nextDirectiveDelay(ctx))
		case <-timer.C:
			h.runDirectivePass(ctx)
			rearmTimer(timer, h.nextDirectiveDelay(ctx))
		case <-reap.C:
			h.runDirectivePass(ctx)
			rearmTimer(timer, h.nextDirectiveDelay(ctx))
		}
	}
}

// rearmTimer resets timer to d. The caller must not also be receiving on timer.C.
func rearmTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

func (h *Hub) nextDirectiveDelay(ctx context.Context) time.Duration {
	d := h.nextDirectiveWake(ctx, h.directiveClock())
	if d <= 0 {
		// Something is already due. A short delay retries a failed expire
		// without spinning the loop.
		return time.Second
	}
	return d
}

// nextDirectiveWake is the wait until the next warning (until−5m) or expiry.
// Zero means a directive is already due. An idle fleet waits directiveIdleDelay.
func (h *Hub) nextDirectiveWake(ctx context.Context, now time.Time) time.Duration {
	list, err := h.store.ListDirectives(ctx, store.DirectiveFilter{OpenOnly: true, Limit: 1000})
	if err != nil {
		return directiveIdleDelay
	}
	h.directiveMu.Lock()
	defer h.directiveMu.Unlock()
	var next time.Time
	for _, d := range list {
		if d.Until == nil || d.EndedAt != nil {
			continue
		}
		if !now.Before(*d.Until) {
			return 0
		}
		wake := *d.Until
		if h.directiveWarned == nil || !warned(h.directiveWarned, d.ID) {
			if warnAt := d.Until.Add(-directiveWarnBefore); now.Before(warnAt) {
				wake = warnAt
			}
		}
		if next.IsZero() || wake.Before(next) {
			next = wake
		}
	}
	if next.IsZero() {
		return directiveIdleDelay
	}
	wait := next.Sub(now)
	if wait < 0 {
		return 0
	}
	return wait
}

func warned(m map[string]struct{}, id string) bool {
	_, ok := m[id]
	return ok
}

// runDirectivePass warns once inside the last five minutes and expires directives
// whose until has passed. A clock jump past until expires and notifies expiry
// only (the missed warning is not sent). Safe for the timer and the reaper to
// call together: EndDirective is idempotent and the warning is marked once.
func (h *Hub) runDirectivePass(ctx context.Context) {
	h.directiveMu.Lock()
	now := h.directiveClock()
	list, err := h.store.ListDirectives(ctx, store.DirectiveFilter{OpenOnly: true, Limit: 1000})
	if err != nil {
		h.directiveMu.Unlock()
		return
	}
	if h.directiveWarned == nil {
		h.directiveWarned = map[string]struct{}{}
	}
	type step struct {
		d      store.Directive
		expire bool
	}
	var todo []step
	open := map[string]struct{}{}
	for _, d := range list {
		open[d.ID] = struct{}{}
		if d.Until == nil {
			continue
		}
		if !now.Before(*d.Until) {
			todo = append(todo, step{d: d, expire: true})
			continue
		}
		warnAt := d.Until.Add(-directiveWarnBefore)
		if !now.Before(warnAt) && !warned(h.directiveWarned, d.ID) {
			h.directiveWarned[d.ID] = struct{}{}
			todo = append(todo, step{d: d})
		}
	}
	for id := range h.directiveWarned {
		if _, ok := open[id]; !ok {
			delete(h.directiveWarned, id)
		}
	}
	h.directiveMu.Unlock()

	for _, s := range todo {
		if s.expire {
			h.expireDirective(ctx, s.d)
			continue
		}
		h.notifyDirectiveClock(ctx, s.d, false)
	}
}

func (h *Hub) expireDirective(ctx context.Context, d store.Directive) {
	ctx = withCause(context.WithoutCancel(ctx), causeDirective)
	cur, changed, err := h.closeDirective(ctx, d.ID, "system", directiveActionExpired, AuditDirectiveExpire)
	if err != nil || !changed {
		return
	}
	h.notifyDirectiveClock(ctx, cur, true)
}

func (h *Hub) notifyDirectiveClock(ctx context.Context, d store.Directive, expired bool) {
	word := "expiring"
	if expired {
		word = "expired"
	}
	projectID := ""
	if d.Kind == store.DirectiveAutopilotPause {
		projectID = d.Target
	}
	h.NotifyDirectiveExpiring(ctx, projectID, "", fmt.Sprintf("directive %s (%s) %s", d.SID, d.Kind, word))
}

// CreateDirective validates, stores and enforces a directive.
func (h *Hub) CreateDirective(ctx context.Context, in DirectiveInput) (store.Directive, error) {
	d, err := h.prepareDirective(ctx, in)
	if err != nil {
		return store.Directive{}, err
	}
	cause := causeFrom(ctx)
	d.Provenance = directiveProvenance(cause)
	d.Actor = directiveActor(cause)
	d.CreatedAt = h.directiveClock()
	stored, err := h.store.CreateDirective(context.WithoutCancel(ctx), d)
	if err != nil {
		return store.Directive{}, err
	}
	h.recordDirective(ctx, stored, directiveActionCreated, AuditDirectiveCreate)
	h.afterDirectiveChanged(ctx, stored, false)
	return stored, nil
}

// EndDirective ends an active directive and is idempotent when it has already ended.
func (h *Hub) EndDirective(ctx context.Context, id string) (store.Directive, error) {
	cause := causeFrom(ctx)
	d, changed, err := h.closeDirective(ctx, id, directiveActor(cause), directiveActionEnded, AuditDirectiveEnd)
	if err != nil || !changed {
		return d, err
	}
	return d, nil
}

// closeDirective stamps ended_at. changed is false when the row was already ended;
// the caller then skips effects. id may be unknown (ErrNotFound).
func (h *Hub) closeDirective(ctx context.Context, id, endedBy, action, auditAction string) (store.Directive, bool, error) {
	cur, err := h.store.GetDirective(ctx, id)
	if err != nil {
		return store.Directive{}, false, err
	}
	if cur.EndedAt != nil {
		return cur, false, nil
	}
	now := h.directiveClock()
	ok, err := h.store.EndDirective(context.WithoutCancel(ctx), cur.ID, endedBy, now)
	if err != nil {
		return store.Directive{}, false, err
	}
	if !ok {
		cur, err = h.store.GetDirective(ctx, cur.ID)
		return cur, false, err
	}
	h.directiveMu.Lock()
	delete(h.directiveWarned, cur.ID)
	h.directiveMu.Unlock()
	cur, err = h.store.GetDirective(ctx, cur.ID)
	if err != nil {
		return store.Directive{}, false, err
	}
	h.recordDirective(ctx, cur, action, auditAction)
	h.afterDirectiveChanged(ctx, cur, true)
	return cur, true, nil
}

func (h *Hub) recordDirective(ctx context.Context, d store.Directive, action, auditAction string) {
	msg := h.directiveSummary(ctx, d, action)
	meta := auditMeta(map[string]any{
		"directive_id":  d.ID,
		"directive_sid": d.SID,
		"kind":          d.Kind,
		"target":        d.Target,
		"provenance":    d.Provenance,
		"until":         formatDirectiveUntil(d.Until),
		"reason":        d.Reason,
	})
	h.recordFleetEvent(ctx, store.FleetEvent{
		Kind:     store.EventKindDirective,
		Action:   action,
		WorkerID: directiveWorkerID(d),
		Summary:  msg,
		MetaJSON: meta,
	})
	h.recordAudit(ctx, store.AuditEvent{
		Action:   auditAction,
		WorkerID: directiveWorkerID(d),
		Message:  msg,
		MetaJSON: meta,
	})
	h.broadcastFleetUpdate("directive", d)
}

func (h *Hub) afterDirectiveChanged(ctx context.Context, d store.Directive, ended bool) {
	h.kickDirective()
	switch d.Kind {
	case store.DirectiveAutopilotPause:
		h.broadcastAutopilotUpdate(ctx)
		h.kickScheduler()
	default:
		if strings.HasPrefix(d.Kind, "machine.") {
			h.pushMachineDirectives(ctx, d.Target)
			h.kickScheduler()
			if d.Kind == store.DirectiveMachinePause && d.Params.SuspendRunning {
				if ended {
					h.resumeDirectiveSessions(ctx, d.Target)
				} else {
					h.suspendMachineForDirective(ctx, d.Target)
				}
			}
		}
	}
}

func (h *Hub) directiveSummary(ctx context.Context, d store.Directive, verb string) string {
	where := d.Target
	switch {
	case strings.HasPrefix(d.Kind, "machine."):
		where = h.workerLabel(ctx, d.Target)
	case d.Kind == store.DirectiveAutopilotPause:
		if p, err := h.store.GetProject(ctx, d.Target); err == nil && p.Name != "" {
			where = p.Name
		}
	}
	msg := fmt.Sprintf("Directive %s (%s) %s on %s", d.SID, d.Kind, verb, where)
	if d.Until != nil {
		msg += " until " + d.Until.UTC().Format(time.RFC3339)
	}
	if d.Reason != "" {
		msg += ": " + d.Reason
	}
	return msg
}

func formatDirectiveUntil(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func directiveWorkerID(d store.Directive) string {
	if strings.HasPrefix(d.Kind, "machine.") {
		return d.Target
	}
	return ""
}

func directiveProvenance(c store.Cause) string {
	switch c.Actor {
	case store.ActorUser:
		return store.ProvenanceUser
	case store.ActorCommander:
		return store.ProvenanceCommander
	default:
		return store.ProvenanceSystem
	}
}

func directiveActor(c store.Cause) string {
	if c.ActorID != "" {
		return c.ActorID
	}
	return describeActor(c)
}

func capDirectiveReason(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > directiveReasonMax {
		return string(r[:directiveReasonMax])
	}
	return s
}

func (h *Hub) prepareDirective(ctx context.Context, in DirectiveInput) (store.Directive, error) {
	kind := strings.TrimSpace(in.Kind)
	switch kind {
	case store.DirectiveProviderDisable:
		return store.Directive{}, errors.New("provider.disable is not available yet")
	case store.DirectiveMachinePause, store.DirectiveMachineThrottle, store.DirectiveMachineDrain, store.DirectiveAutopilotPause:
	default:
		return store.Directive{}, fmt.Errorf("unknown directive kind %q", kind)
	}
	target := strings.TrimSpace(in.Target)
	if target == "" {
		return store.Directive{}, errors.New("target is required")
	}
	if strings.HasPrefix(kind, "machine.") {
		target = h.resolveMachine(ctx, target)
		if _, err := h.store.GetWorker(ctx, target); err != nil {
			return store.Directive{}, fmt.Errorf("machine %q not found", strings.TrimSpace(in.Target))
		}
	} else {
		id := h.resolveProjectID(ctx, target)
		if id == "" {
			return store.Directive{}, fmt.Errorf("project %q not found", target)
		}
		target = id
	}
	if in.Until != nil {
		until := in.Until.UTC()
		if !h.directiveClock().Before(until) {
			return store.Directive{}, errors.New("until must be in the future")
		}
		in.Until = &until
	}
	params, err := normalizeDirectiveParams(kind, in.Params)
	if err != nil {
		return store.Directive{}, err
	}
	return store.Directive{
		Kind:   kind,
		Target: target,
		Params: params,
		Until:  in.Until,
		Reason: capDirectiveReason(in.Reason),
	}, nil
}

func normalizeDirectiveParams(kind string, p store.DirectiveParams) (store.DirectiveParams, error) {
	if p.CPUCeilingPct != nil {
		v := *p.CPUCeilingPct
		if v < 1 || v > 100 {
			return store.DirectiveParams{}, errors.New("cpu_ceiling_pct must be between 1 and 100")
		}
	}
	switch kind {
	case store.DirectiveMachineThrottle:
		if p.MaxConcurrency == nil || *p.MaxConcurrency < 0 {
			return store.DirectiveParams{}, errors.New("machine.throttle requires max_concurrency >= 0")
		}
		return p, nil
	case store.DirectiveMachinePause:
		return store.DirectiveParams{SuspendRunning: p.SuspendRunning}, nil
	default:
		return store.DirectiveParams{}, nil
	}
}

// resolveProjectID resolves a project id, sid or case-insensitive name.
func (h *Hub) resolveProjectID(ctx context.Context, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if id := resolveRef(ctx, h.store, ids.Project, ref); id != "" {
		if _, err := h.store.GetProject(ctx, id); err == nil {
			return id
		}
	}
	projects, err := h.store.ListProjects(ctx)
	if err != nil {
		return ""
	}
	for _, p := range projects {
		if strings.EqualFold(p.Name, ref) {
			return p.ID
		}
	}
	return ""
}

// projectDirectivePaused reports an active autopilot.pause on the project.
func (h *Hub) projectDirectivePaused(ctx context.Context, projectID string) bool {
	if h == nil || h.store == nil || projectID == "" {
		return false
	}
	list, err := h.store.ListDirectives(ctx, store.DirectiveFilter{
		Target: projectID, Kind: store.DirectiveAutopilotPause, ActiveOnly: true, Now: h.directiveClock(), Limit: 20,
	})
	return err == nil && len(list) > 0
}

// applyMachineDirectives drops paused or drained machines and caps throttled ones.
// Slot overrides have already raised MaxConcurrent; a throttle only tightens it.
// max_concurrency 0 removes the machine (the scheduler treats MaxConcurrent <= 0 as 1).
// workers.drained is a separate flag and is not changed here.
func (h *Hub) applyMachineDirectives(ctx context.Context, workers []store.Worker) []store.Worker {
	if len(workers) == 0 {
		return workers
	}
	list, err := h.store.ListDirectives(ctx, store.DirectiveFilter{ActiveOnly: true, Now: h.directiveClock(), Limit: 1000})
	if err != nil {
		h.logger.Warn("list directives for scheduling failed", "err", err)
		return nil
	}
	type cap struct {
		skip bool
		max  *int
	}
	byID := map[string]*cap{}
	for _, d := range list {
		if !strings.HasPrefix(d.Kind, "machine.") || !d.ActiveAt(h.directiveClock()) {
			continue
		}
		c := byID[d.Target]
		if c == nil {
			c = &cap{}
			byID[d.Target] = c
		}
		switch d.Kind {
		case store.DirectiveMachinePause, store.DirectiveMachineDrain:
			c.skip = true
		case store.DirectiveMachineThrottle:
			if d.Params.MaxConcurrency == nil {
				continue
			}
			v := *d.Params.MaxConcurrency
			if c.max == nil || v < *c.max {
				c.max = &v
			}
		}
	}
	if len(byID) == 0 {
		return workers
	}
	out := make([]store.Worker, 0, len(workers))
	for _, w := range workers {
		c := byID[w.ID]
		if c == nil {
			out = append(out, w)
			continue
		}
		if c.skip {
			continue
		}
		if c.max != nil {
			if *c.max <= 0 {
				continue
			}
			if w.MaxConcurrent <= 0 || *c.max < w.MaxConcurrent {
				w.MaxConcurrent = *c.max
			}
		}
		out = append(out, w)
	}
	return out
}

func (h *Hub) machineSuspendsRunning(ctx context.Context, workerID string) bool {
	list, err := h.store.ListDirectives(ctx, store.DirectiveFilter{
		Target: workerID, Kind: store.DirectiveMachinePause, ActiveOnly: true, Now: h.directiveClock(), Limit: 50,
	})
	if err != nil {
		return false
	}
	for _, d := range list {
		if d.Params.SuspendRunning && d.ActiveAt(h.directiveClock()) {
			return true
		}
	}
	return false
}

func (h *Hub) suspendMachineForDirective(ctx context.Context, workerID string) {
	ctx = withCause(ctx, causeDirective)
	tasks, chats := h.machineSessions(ctx, workerID)
	for _, t := range tasks {
		if !suspendable(t) || t.Status == protocol.TaskStatusSuspended {
			continue
		}
		if _, err := h.suspendTask(ctx, t, store.SuspendedByDirective); err != nil {
			h.logger.Warn("directive suspend failed", "worker_id", workerID, "task_id", t.ID, "err", err)
		}
	}
	for _, cs := range chats {
		if !chatBusy(cs.Status) {
			continue
		}
		if _, err := h.suspendChat(ctx, cs, store.SuspendedByDirective); err != nil {
			h.logger.Warn("directive suspend chat failed", "worker_id", workerID, "chat_id", cs.ID, "err", err)
		}
	}
}

func (h *Hub) resumeDirectiveSessions(ctx context.Context, workerID string) {
	if h.machineSuspendsRunning(ctx, workerID) {
		return
	}
	tasks, chats := h.machineSessions(ctx, workerID)
	for _, t := range tasks {
		if t.Status == protocol.TaskStatusSuspended && t.SuspendedBy == store.SuspendedByDirective {
			if _, err := h.resumeTask(ctx, t); err != nil {
				h.logger.Warn("directive resume failed", "worker_id", workerID, "task_id", t.ID, "err", err)
			}
		}
	}
	for _, cs := range chats {
		if cs.Status == protocol.ChatStatusSuspended && cs.SuspendedBy == store.SuspendedByDirective {
			if _, err := h.resumeChat(ctx, cs); err != nil {
				h.logger.Warn("directive resume chat failed", "worker_id", workerID, "chat_id", cs.ID, "err", err)
			}
		}
	}
}

// deliverWorkerDirectives pushes this machine's active directives and, when a
// suspend_running pause is still active, freezes managed sessions again (a
// reconnect re-adopts them as running).
func (h *Hub) deliverWorkerDirectives(ctx context.Context, wc *workerConn) {
	if wc == nil {
		return
	}
	h.pushWorkerDirectives(ctx, wc)
	if h.machineSuspendsRunning(ctx, wc.id) {
		h.suspendMachineForDirective(ctx, wc.id)
	}
}

func (h *Hub) pushMachineDirectives(ctx context.Context, workerID string) {
	if wc := h.workerConnFor(workerID); wc != nil {
		h.pushWorkerDirectives(ctx, wc)
	}
}

func (h *Hub) pushWorkerDirectives(ctx context.Context, wc *workerConn) {
	now := h.directiveClock()
	list, err := h.store.ListDirectives(ctx, store.DirectiveFilter{
		Target: wc.id, ActiveOnly: true, Now: now, Limit: 100,
	})
	if err != nil {
		h.logger.Warn("list worker directives failed", "worker_id", wc.id, "err", err)
		return
	}
	payload := protocol.WorkerDirectivesPayload{Directives: []protocol.WorkerDirective{}}
	for _, d := range list {
		if !strings.HasPrefix(d.Kind, "machine.") || !d.ActiveAt(now) {
			continue
		}
		payload.Directives = append(payload.Directives, protocol.WorkerDirective{
			ID: d.ID, SID: d.SID, Kind: d.Kind, Until: d.Until,
			Params: protocol.WorkerDirectiveParams{
				MaxConcurrency: d.Params.MaxConcurrency,
				CPUCeilingPct:  d.Params.CPUCeilingPct,
				HeavyOK:        d.Params.HeavyOK,
				SuspendRunning: d.Params.SuspendRunning,
			},
		})
	}
	h.sendWorkerJSON(wc, protocol.MsgWorkerDirectives, payload)
}

// directiveFleetPhrase is the pulse notable for one machine: pause beats drain
// beats throttle. A workers.drained flag still reads "drained" when no drain
// directive is active. loc formats until as HH:MM.
func (h *Hub) directiveFleetPhrase(w store.Worker, dirs []store.Directive, now time.Time, loc *time.Location) string {
	var pauseUntil, drainUntil *time.Time
	pause, drain := false, false
	pauseOpen, drainOpen := false, false
	var throttleMax *int
	var throttleUntil *time.Time
	throttleOpen := false
	for i := range dirs {
		d := dirs[i]
		if d.Target != w.ID || !d.ActiveAt(now) {
			continue
		}
		switch d.Kind {
		case store.DirectiveMachinePause:
			pause = true
			if d.Until == nil {
				pauseOpen = true
			} else {
				pauseUntil = soonerUntil(pauseUntil, d.Until)
			}
		case store.DirectiveMachineDrain:
			drain = true
			if d.Until == nil {
				drainOpen = true
			} else {
				drainUntil = soonerUntil(drainUntil, d.Until)
			}
		case store.DirectiveMachineThrottle:
			if d.Params.MaxConcurrency == nil {
				continue
			}
			v := *d.Params.MaxConcurrency
			if throttleMax == nil || v < *throttleMax {
				n := v
				throttleMax = &n
				throttleUntil = d.Until
				throttleOpen = d.Until == nil
			} else if v == *throttleMax {
				if d.Until == nil {
					throttleOpen = true
				} else {
					throttleUntil = soonerUntil(throttleUntil, d.Until)
				}
			}
		}
	}
	switch {
	case pause:
		if pauseOpen {
			pauseUntil = nil
		}
		return "paused" + untilClock(pauseUntil, loc)
	case drain || w.Drained:
		if drain {
			if drainOpen {
				drainUntil = nil
			}
			return "drained" + untilClock(drainUntil, loc)
		}
		return "drained"
	case throttleMax != nil:
		if throttleOpen {
			throttleUntil = nil
		}
		return fmt.Sprintf("throttled max=%d", *throttleMax) + untilClock(throttleUntil, loc)
	default:
		return ""
	}
}

// soonerUntil keeps the earliest deadline. A nil until (open-ended) does not
// replace a concrete one: the phrase names the soonest time the restriction lifts.
func soonerUntil(cur, next *time.Time) *time.Time {
	if next == nil {
		return cur
	}
	if cur == nil || next.Before(*cur) {
		return next
	}
	return cur
}

func untilClock(t *time.Time, loc *time.Location) string {
	if t == nil {
		return ""
	}
	if loc == nil {
		loc = time.Local
	}
	return " until " + t.In(loc).Format("15:04")
}

// --- REST ---------------------------------------------------------------------------------

func (s *Server) handleListDirectives(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.DirectiveFilter{Kind: strings.TrimSpace(q.Get("kind"))}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		f.Limit = n
	}
	if target := strings.TrimSpace(q.Get("target")); target != "" {
		f.Target = s.resolveDirectiveTarget(r.Context(), target)
	}
	active := strings.TrimSpace(q.Get("active"))
	if active == "" || active == "1" || strings.EqualFold(active, "true") {
		f.ActiveOnly = true
		f.Now = s.hub.directiveClock()
	}
	list, err := s.store.ListDirectives(r.Context(), f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []store.Directive{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"directives": list})
}

func (s *Server) handleCreateDirective(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind   string                `json:"kind"`
		Target string                `json:"target"`
		Until  string                `json:"until"`
		Reason string                `json:"reason"`
		Params store.DirectiveParams `json:"params"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	var until *time.Time
	if strings.TrimSpace(body.Until) != "" {
		t, err := parseDirectiveUntil(body.Until)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "until must be RFC3339"})
			return
		}
		until = &t
	}
	d, err := s.hub.CreateDirective(r.Context(), DirectiveInput{
		Kind: body.Kind, Target: body.Target, Until: until, Reason: body.Reason, Params: body.Params,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) handleEndDirective(w http.ResponseWriter, r *http.Request) {
	d, err := s.hub.EndDirective(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "directive not found"})
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, d)
	}
}

func parseDirectiveUntil(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// resolveDirectiveTarget maps an id, sid or machine/project name to an id.
// An unknown ref is returned unchanged so the list filter simply matches nothing.
func (s *Server) resolveDirectiveTarget(ctx context.Context, ref string) string {
	if id := resolveRef(ctx, s.store, ids.Machine, ref); id != ref {
		return id
	}
	if id := resolveRef(ctx, s.store, ids.Project, ref); id != ref {
		return id
	}
	if id := s.hub.resolveMachine(ctx, ref); id != ref {
		return id
	}
	if id := s.hub.resolveProjectID(ctx, ref); id != "" && id != ref {
		return id
	}
	return ref
}
