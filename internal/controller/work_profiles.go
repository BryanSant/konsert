package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Work profiles (SYR-VYFYX; docs/prd.md §5.4). When an attempt ends it is stamped with
// its work key, the machine class it ran on and its resource use from the metric
// history, and the profile of that (work key, machine class) is recomputed from the
// recent attempts (store.ProfileWindow). Profiles start unknown and fill in with runs;
// the capacity model and placement read them.

// workKeyFor is "same kind of work": project / epic (or the ticket when it has none,
// or "adhoc") / repo. Retries of a ticket and sibling tickets of an epic share one.
func (h *Hub) workKeyFor(ctx context.Context, a store.Attempt, task store.Task) string {
	project := "default"
	pid := task.ProjectID
	scope := "adhoc"
	if a.TicketID != "" {
		if tk, err := h.store.GetTicket(ctx, a.TicketID); err == nil {
			pid = cmpOr(pid, tk.ProjectID)
			scope = tk.SID
			if tk.ParentID != "" {
				if parent, err := h.store.GetTicket(ctx, tk.ParentID); err == nil {
					scope = parent.SID
				}
			}
		}
	} else if task.TicketID != "" {
		scope = task.TicketID // a ka ticket the task works on
	}
	if pid != "" {
		if p, err := h.store.GetProject(ctx, pid); err == nil && p.SID != "" {
			project = p.SID
		}
	}
	repo := "-"
	if task.RepoID != "" {
		if r, err := h.store.GetRepo(ctx, task.RepoID); err == nil && r.Name != "" {
			repo = r.Name
		}
	} else if dir := cmpOr(task.Workdir, task.EffectiveDir); dir != "" {
		repo = filepath.Base(filepath.Clean(dir))
	}
	return project + "/" + scope + "/" + repo
}

// machineClassFor groups machines that should perform alike: os / arch / RAM tier
// (the nearest power of two in GiB) and, when not the bare host, the isolation
// ("linux/amd64/32g", "linux/amd64/8g/container").
func (h *Hub) machineClassFor(ctx context.Context, workerID string) string {
	w, err := h.store.GetWorker(ctx, workerID)
	if err != nil {
		return "unknown"
	}
	class := cmpOr(w.OS, "unknown") + "/" + cmpOr(w.Arch, "unknown")
	var f protocol.MachineFacts
	if w.FactsJSON != "" && json.Unmarshal([]byte(w.FactsJSON), &f, protocol.JSONOptions) == nil && f.RAMTotalMB > 0 {
		gib := float64(f.RAMTotalMB) / 1024
		class += fmt.Sprintf("/%dg", int(math.Pow(2, math.Round(math.Log2(gib)))))
	}
	if iso := strings.TrimSpace(w.Isolation); iso != "" && iso != "host" {
		class += "/" + iso
	}
	return class
}

// attemptResources is an attempt's peak memory (MB) and CPU-seconds from its
// session's metric history over the attempt.
func (h *Hub) attemptResources(ctx context.Context, a store.Attempt) (uint64, float64) {
	if a.StartedAt == nil || a.EndedAt == nil {
		return 0, 0
	}
	subject := taskSubject(a.TaskID)
	if a.ChatID != "" {
		subject = chatSubject(a.ChatID)
	}
	h.flushMetrics(ctx, true) // the last minute is still open in memory
	rows, err := h.store.ListMetricSamples(ctx, store.MetricFilter{Subjects: []string{subject}, Period: store.MetricMinute,
		Since: a.StartedAt.Truncate(time.Minute), Until: a.EndedAt.Add(time.Minute)})
	if err != nil {
		return 0, 0
	}
	var peak, cpu float64
	for _, r := range rows {
		peak = max(peak, r.RSSMax)
		cpu += r.CPUAvg / 100 * 60
	}
	return uint64(peak), math.Round(cpu*10) / 10
}

// attemptEnded stamps attempt n of the ticket (or ad-hoc task) and refreshes its
// work profile.
func (h *Hub) attemptEnded(ctx context.Context, ticketID, taskID string, n int) {
	if h.store == nil || taskID == "" || n < 1 {
		return
	}
	rows, err := h.store.ListAttempts(ctx, ticketID, taskID)
	if err != nil {
		return
	}
	var a *store.Attempt
	for i := range rows {
		if rows[i].Attempt == n && (ticketID != "" || rows[i].TaskID == taskID) {
			a = &rows[i]
		}
	}
	if a == nil || a.EndedAt == nil || a.WorkerID == "" {
		return
	}
	task, err := h.store.GetTask(ctx, a.TaskID)
	if err != nil {
		return
	}
	key, class := cmpOr(task.WorkKey, h.workKeyFor(ctx, *a, task)), h.machineClassFor(ctx, a.WorkerID)
	peak, cpu := h.attemptResources(ctx, *a)
	if peak == 0 && task.PeakRSSMB > 0 {
		peak = task.PeakRSSMB
	}
	if err := h.store.SetAttemptProfileFields(ctx, a.ID, key, class, peak, cpu); err != nil {
		h.logger.Warn("attempt profile fields not stored", "attempt_id", a.ID, "err", err)
		return
	}
	h.refreshWorkProfile(ctx, key, class)
}

// refreshWorkProfile recomputes one profile from its recent attempts.
func (h *Hub) refreshWorkProfile(ctx context.Context, key, class string) {
	list, err := h.store.ListProfileAttempts(ctx, key, class, store.ProfileWindow)
	if err != nil {
		return
	}
	if err := h.store.UpsertWorkProfile(ctx, store.ComputeWorkProfile(key, class, list, time.Now())); err != nil {
		h.logger.Warn("work profile not stored", "work_key", key, "class", class, "err", err)
	}
}

// WorkProfiles returns the profiles of a work key ("" for all), or of the work key of
// a ticket or task reference.
func (s *FleetService) WorkProfiles(ctx context.Context, workKey, target string) (string, []store.WorkProfile, error) {
	if workKey == "" && target != "" {
		task, ticketID, err := s.resolveInspectTarget(ctx, target)
		if err != nil {
			return "", nil, err
		}
		a := store.Attempt{TicketID: ticketID, TaskID: task.ID}
		workKey = s.hub.workKeyFor(ctx, a, task)
	}
	list, err := s.store.ListWorkProfiles(ctx, workKey)
	return workKey, list, err
}

// handleWorkProfiles serves GET /api/work-profiles?work_key=…|target=….
func (s *Server) handleWorkProfiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key, list, err := s.service.WorkProfiles(r.Context(), strings.TrimSpace(q.Get("work_key")), strings.TrimSpace(q.Get("target")))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"work_key": key, "profiles": list})
}

// inspectProfile serves inspect kind=profile: target is a ticket or task (its work key)
// or a work key itself (it contains "/").
func (s *FleetService) inspectProfile(ctx context.Context, req InspectRequest) (InspectResult, error) {
	if req.Target == "" {
		return InspectResult{}, &inspectFailure{Status: http.StatusBadRequest, Msg: "target is required: a ticket, task or work key"}
	}
	key, target := "", req.Target
	if strings.Contains(req.Target, "/") {
		key, target = req.Target, ""
	}
	key, list, err := s.WorkProfiles(ctx, key, target)
	if err != nil {
		return InspectResult{}, err
	}
	res := InspectResult{Target: req.Target, Kind: req.Kind}
	text := "No attempts of work key " + key + " have ended yet: its profile is unknown."
	if len(list) > 0 {
		b, err := json.Marshal(map[string]any{"work_key": key, "profiles": list}, protocol.JSONOptions)
		if err != nil {
			return InspectResult{}, err
		}
		text = string(b)
	}
	applyPage(&res, protocol.PageText(text, req.MaxLines, req.Offset))
	return res, nil
}
